// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sortexec

import (
	"sync"
	"sync/atomic"

	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/channel"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"go.uber.org/zap"
)

type parallelSortSpillHelper struct {
	cond             *sync.Cond
	spillStatus      int
	sortedRowsInDisk []*chunk.DataInDiskByChunks
	sortExec         *SortExec

	lessRowFunc   func(chunk.Row, chunk.Row) int
	errOutputChan chan rowWithError
	finishCh      chan struct{}

	fieldTypes    []*types.FieldType
	tmpSpillChunk *chunk.Chunk

	bytesConsumed atomic.Int64
	bytesLimit    atomic.Int64
}

func newParallelSortSpillHelper(sortExec *SortExec, fieldTypes []*types.FieldType, finishCh chan struct{}, lessRowFunc func(chunk.Row, chunk.Row) int, errOutputChan chan rowWithError) *parallelSortSpillHelper {
	return &parallelSortSpillHelper{
		cond:          sync.NewCond(new(sync.Mutex)),
		spillStatus:   notSpilled,
		sortExec:      sortExec,
		lessRowFunc:   lessRowFunc,
		errOutputChan: errOutputChan,
		finishCh:      finishCh,
		fieldTypes:    fieldTypes,
	}
}

func (p *parallelSortSpillHelper) close() {
	for _, inDisk := range p.sortedRowsInDisk {
		inDisk.Close()
	}

	if p.tmpSpillChunk != nil {
		p.tmpSpillChunk.Destroy(spillChunkSize, p.fieldTypes)
	}
}

func (p *parallelSortSpillHelper) isNotSpilledNoLock() bool {
	return p.spillStatus == notSpilled
}

func (p *parallelSortSpillHelper) isInSpillingNoLock() bool {
	return p.spillStatus == inSpilling
}

func (p *parallelSortSpillHelper) isSpillNeeded() bool {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.spillStatus == needSpill
}

func (p *parallelSortSpillHelper) isSpillTriggered() bool {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return len(p.sortedRowsInDisk) > 0
}

func (p *parallelSortSpillHelper) setInSpilling() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	p.spillStatus = inSpilling
}

func (p *parallelSortSpillHelper) setNeedSpillNoLock() {
	p.spillStatus = needSpill
}

func (p *parallelSortSpillHelper) setNotSpilled() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	p.spillStatus = notSpilled
}

func (p *parallelSortSpillHelper) spill() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = util.GetRecoverError(r)
		}
	}()

	p.setInSpilling()

	// Spill is done, broadcast to wake up all sleep goroutines
	defer p.cond.Broadcast()
	defer p.setNotSpilled()

	select {
	case <-p.finishCh:
		return nil
	default:
	}

	workerNum := len(p.sortExec.Parallel.workers)
	workerWaiter := &sync.WaitGroup{}
	workerWaiter.Add(workerNum)
	sortedRowsIters := make([]*chunk.Iterator4Slice, workerNum)
	errChannel := make(chan error, workerNum)
	for i := range workerNum {
		go func(idx int) {
			defer func() {
				if r := recover(); r != nil {
					errChannel <- util.GetRecoverError(r)
				}
				workerWaiter.Done()
			}()

			err := injectErrorForIssue59655(10)
			if err != nil {
				errChannel <- err
				return
			}

			sortedRows, err := p.sortExec.Parallel.workers[idx].sortLocalRows()
			if err != nil {
				errChannel <- err
				return
			}
			sortedRowsIters[idx] = chunk.NewIterator4Slice(sortedRows)
			injectParallelSortRandomFail(200)
		}(i)
	}

	workerWaiter.Wait()
	close(errChannel)
	for err := range errChannel {
		return err
	}

	totalRows := 0
	for i := range sortedRowsIters {
		totalRows += sortedRowsIters[i].Len()
	}

	source := &memorySource{sortedRowsIters: sortedRowsIters}
	merger := newMultiWayMerger(source, p.lessRowFunc)
	_ = merger.init()
	return p.spillImpl(merger)
}

func (p *parallelSortSpillHelper) releaseMemory() {
	totalReleasedMemory := int64(0)
	for _, worker := range p.sortExec.Parallel.workers {
		totalReleasedMemory += worker.totalMemoryUsage
		worker.totalMemoryUsage = 0
	}
	p.sortExec.memTracker.Consume(-totalReleasedMemory)
}

func (p *parallelSortSpillHelper) spillTmpSpillChunk(inDisk *chunk.DataInDiskByChunks) error {
	err := inDisk.Add(p.tmpSpillChunk)
	if err != nil {
		return err
	}
	p.tmpSpillChunk.Reset()
	return nil
}

func (p *parallelSortSpillHelper) initForSpill() {
	if p.tmpSpillChunk == nil {
		p.tmpSpillChunk = chunk.NewChunkFromPoolWithCapacity(p.fieldTypes, spillChunkSize)
	}
}

func (p *parallelSortSpillHelper) spillImpl(merger *multiWayMerger) error {
	logutil.BgLogger().Info(spillInfo, zap.Int64("consumed", p.bytesConsumed.Load()), zap.Int64("quota", p.bytesLimit.Load()))
	p.initForSpill()
	p.tmpSpillChunk.Reset()
	inDisk := chunk.NewDataInDiskByChunks(p.fieldTypes)
	inDisk.GetDiskTracker().AttachTo(p.sortExec.diskTracker)

	spilledRowChannel := make(chan chunk.Row, 10000)
	errChannel := make(chan error, 1)

	// We must wait the finish of the following goroutine,
	// or we will exit `spillImpl` function in advance and
	// this will cause data race.
	defer channel.Clear(spilledRowChannel)

	wg := &util.WaitGroupWrapper{}
	wg.RunWithRecover(
		func() {
			defer close(spilledRowChannel)
			injectParallelSortRandomFail(200)

			err := injectErrorForIssue59655(500)
			if err != nil {
				errChannel <- err
				return
			}

			for {
				row, err := merger.next()
				if err != nil {
					errChannel <- err
					break
				}
				if row.IsEmpty() {
					break
				}
				spilledRowChannel <- row
			}
		},
		func(r any) {
			if r != nil {
				errChannel <- util.GetRecoverError(r)
			}
		})

	var (
		row chunk.Row
		ok  bool
		err error
	)

OuterLoop:
	for {
		select {
		case <-p.finishCh:
			break OuterLoop
		case row, ok = <-spilledRowChannel:
			if !ok {
				if p.tmpSpillChunk.NumRows() > 0 {
					err = p.spillTmpSpillChunk(inDisk)
					if err != nil {
						break OuterLoop
					}
				}

				injectParallelSortRandomFail(200)

				if inDisk.NumRows() > 0 {
					p.sortedRowsInDisk = append(p.sortedRowsInDisk, inDisk)
					p.releaseMemory()
				}
				break OuterLoop
			}
		}

		p.tmpSpillChunk.AppendRow(row)
		if p.tmpSpillChunk.IsFull() {
			err = p.spillTmpSpillChunk(inDisk)
			if err != nil {
				break
			}
		}
	}

	wg.Wait()
	close(errChannel)
	if err != nil {
		return err
	}
	return <-errChannel
}
