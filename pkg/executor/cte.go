// Copyright 2021 PingCAP, Inc.
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

package executor

import (
	"bytes"
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/executor/join"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/pingcap/tidb/pkg/util/cteutil"
	"github.com/pingcap/tidb/pkg/util/dbterror/exeerrors"
	"github.com/pingcap/tidb/pkg/util/disk"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/memory"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var _ exec.Executor = &CTEExec{}

// CTEExec implements CTE.
// Following diagram describes how CTEExec works.
//
// `iterInTbl` is shared by `CTEExec` and `CTETableReaderExec`.
// `CTETableReaderExec` reads data from `iterInTbl`,
// and its output will be stored `iterOutTbl` by `CTEExec`.
//
// When an iteration ends, `CTEExec` will move all data from `iterOutTbl` into `iterInTbl`,
// which will be the input for new iteration.
// At the end of each iteration, data in `iterOutTbl` will also be added into `resTbl`.
// `resTbl` stores data of all iteration.
/*
                                   +----------+
                     write         |iterOutTbl|
       CTEExec ------------------->|          |
          |                        +----+-----+
    -------------                       | write
    |           |                       v
 other op     other op             +----------+
 (seed)       (recursive)          |  resTbl  |
                  ^                |          |
                  |                +----------+
            CTETableReaderExec
                   ^
                   |  read         +----------+
                   +---------------+iterInTbl |
                                   |          |
                                   +----------+
*/
type CTEExec struct {
	exec.BaseExecutor

	chkIdx   int
	producer *cteProducer

	// limit in recursive CTE.
	cursor         uint64
	meetFirstBatch bool
}

// Open implements the Executor interface.
func (e *CTEExec) Open(ctx context.Context) (err error) {
	e.reset()
	if err := e.BaseExecutor.Open(ctx); err != nil {
		return err
	}

	e.producer.resTbl.Lock()
	defer e.producer.resTbl.Unlock()

	if e.producer.checkAndUpdateCorColHashCode() {
		err = e.producer.reset()
		if err != nil {
			return err
		}
	}
	if e.producer.openErr != nil {
		return e.producer.openErr
	}
	if !e.producer.hasCTEResult() && !e.producer.executorOpened {
		if err = e.producer.openProducerExecutor(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Next implements the Executor interface.
func (e *CTEExec) Next(ctx context.Context, req *chunk.Chunk) (err error) {
	e.producer.resTbl.Lock()
	defer e.producer.resTbl.Unlock()
	if !e.producer.hasCTEResult() {
		// in case that another CTEExec call close without generate CTE result.
		if !e.producer.executorOpened {
			if err = e.producer.openProducerExecutor(ctx, e); err != nil {
				return err
			}
		}
		if err = e.producer.genCTEResult(ctx); err != nil {
			return err
		}
	}
	return e.producer.getChunk(e, req)
}

func setFirstErr(firstErr error, newErr error, msg string) error {
	if newErr != nil {
		logutil.BgLogger().Error("cte got error", zap.Any("err", newErr), zap.String("extra msg", msg))
		if firstErr == nil {
			firstErr = newErr
		}
	}
	return firstErr
}

// Close implements the Executor interface.
func (e *CTEExec) Close() (firstErr error) {
	func() {
		e.producer.resTbl.Lock()
		defer e.producer.resTbl.Unlock()
		if e.producer.executorOpened {
			failpoint.Inject("mock_cte_exec_panic_avoid_deadlock", func(v failpoint.Value) {
				ok := v.(bool)
				if ok {
					// mock an oom panic, returning ErrMemoryExceedForQuery for error identification in recovery work.
					panic(exeerrors.ErrMemoryExceedForQuery)
				}
			})
			// closeProducerExecutor() only close seedExec and recursiveExec, will not touch resTbl.
			// It means you can still read resTbl after call closeProducerExecutor().
			// You can even call all three functions(openProducerExecutor/genCTEResult/closeProducerExecutor) in CTEExec.Next().
			// Separating these three function calls is only to follow the abstraction of the volcano model.
			err := e.producer.closeProducerExecutor()
			firstErr = setFirstErr(firstErr, err, "close cte producer error")
			if !e.producer.hasCTEResult() {
				// CTE result is not generated, in this case, we reset it
				err = e.producer.reset()
				firstErr = setFirstErr(firstErr, err, "close cte producer error")
			}
		}
	}()
	err := e.BaseExecutor.Close()
	firstErr = setFirstErr(firstErr, err, "close cte children error")
	return
}

func (e *CTEExec) reset() {
	e.chkIdx = 0
	e.cursor = 0
	e.meetFirstBatch = false
}

type cteProducer struct {
	// executorOpened is used to indicate whether the executor(seedExec/recursiveExec) is opened.
	// when executorOpened is true, the executor is opened, otherwise it means the executor is
	// not opened or is already closed.
	executorOpened bool

	// cteProducer is shared by multiple operators, so if the first operator tries to open
	// and got error, the second should return open error directly instead of open again.
	// Otherwise there may be resource leak because Close() only clean resource for the last Open().
	openErr error

	ctx sessionctx.Context

	seedExec      exec.Executor
	recursiveExec exec.Executor

	// `resTbl` and `iterInTbl` are shared by all CTEExec which reference to same the CTE.
	// `iterInTbl` is also shared by CTETableReaderExec.
	resTbl     cteutil.Storage
	iterInTbl  cteutil.Storage
	iterOutTbl cteutil.Storage

	hashTbl join.BaseHashTable

	// UNION ALL or UNION DISTINCT.
	isDistinct bool
	curIter    int
	hCtx       *join.HashContext
	sel        []int

	// Limit related info.
	hasLimit bool
	limitBeg uint64
	limitEnd uint64

	memTracker  *memory.Tracker
	diskTracker *disk.Tracker

	// Correlated Column.
	corCols         []*expression.CorrelatedColumn
	corColHashCodes [][]byte
}

func (p *cteProducer) openProducerExecutor(ctx context.Context, cteExec *CTEExec) (err error) {
	defer func() {
		p.openErr = err
		p.executorOpened = true
	}()
	if p.seedExec == nil {
		return errors.New("seedExec for CTEExec is nil")
	}
	if err = exec.Open(ctx, p.seedExec); err != nil {
		return err
	}

	p.resetTracker()
	p.memTracker = memory.NewTracker(cteExec.ID(), -1)
	p.diskTracker = disk.NewTracker(cteExec.ID(), -1)
	p.memTracker.AttachTo(p.ctx.GetSessionVars().StmtCtx.MemTracker)
	p.diskTracker.AttachTo(p.ctx.GetSessionVars().StmtCtx.DiskTracker)

	if p.recursiveExec != nil {
		if err = exec.Open(ctx, p.recursiveExec); err != nil {
			return err
		}
		// For non-recursive CTE, the result will be put into resTbl directly.
		// So no need to build iterOutTbl.
		// Construct iterOutTbl in Open() instead of buildCTE(), because its destruct is in Close().
		recursiveTypes := p.recursiveExec.RetFieldTypes()
		p.iterOutTbl = cteutil.NewStorageRowContainer(recursiveTypes, cteExec.MaxChunkSize())
		if err = p.iterOutTbl.OpenAndRef(); err != nil {
			return err
		}
	}

	if p.isDistinct {
		p.hashTbl = join.NewConcurrentMapHashTable()
		p.hCtx = &join.HashContext{
			AllTypes: cteExec.RetFieldTypes(),
		}
		// We use all columns to compute hash.
		p.hCtx.KeyColIdx = make([]int, len(p.hCtx.AllTypes))
		for i := range p.hCtx.KeyColIdx {
			p.hCtx.KeyColIdx[i] = i
		}
	}
	return nil
}

func (p *cteProducer) closeProducerExecutor() (firstErr error) {
	err := exec.Close(p.seedExec)
	firstErr = setFirstErr(firstErr, err, "close seedExec err")

	if p.recursiveExec != nil {
		err = exec.Close(p.recursiveExec)
		firstErr = setFirstErr(firstErr, err, "close recursiveExec err")

		// `iterInTbl` and `resTbl` are shared by multiple operators,
		// so will be closed when the SQL finishes.
		if p.iterOutTbl != nil {
			err = p.iterOutTbl.DerefAndClose()
			firstErr = setFirstErr(firstErr, err, "deref iterOutTbl err")
		}
	}
	// Reset to nil instead of calling Detach(),
	// because ExplainExec still needs tracker to get mem usage info.
	p.memTracker = nil
	p.diskTracker = nil
	p.executorOpened = false
	return
}

func (p *cteProducer) getChunk(cteExec *CTEExec, req *chunk.Chunk) (err error) {
	req.Reset()
	if p.hasLimit {
		return p.nextChunkLimit(cteExec, req)
	}
	if cteExec.chkIdx < p.resTbl.NumChunks() {
		res, err := p.resTbl.GetChunk(cteExec.chkIdx)
		if err != nil {
			return err
		}
		// Need to copy chunk to make sure upper operator will not change chunk in resTbl.
		// Also we ignore copying rows not selected, because some operators like Projection
		// doesn't support swap column if chunk.sel is no nil.
		req.SwapColumns(res.CopyConstructSel())
		cteExec.chkIdx++
	}
	return nil
}

func (p *cteProducer) nextChunkLimit(cteExec *CTEExec, req *chunk.Chunk) error {
	if !cteExec.meetFirstBatch {
		for cteExec.chkIdx < p.resTbl.NumChunks() {
			res, err := p.resTbl.GetChunk(cteExec.chkIdx)
			if err != nil {
				return err
			}
			cteExec.chkIdx++
			numRows := uint64(res.NumRows())
			if newCursor := cteExec.cursor + numRows; newCursor >= p.limitBeg {
				cteExec.meetFirstBatch = true
				begInChk, endInChk := p.limitBeg-cteExec.cursor, numRows
				if newCursor > p.limitEnd {
					endInChk = p.limitEnd - cteExec.cursor
				}
				cteExec.cursor += endInChk
				if begInChk == endInChk {
					break
				}
				tmpChk := res.CopyConstructSel()
				req.Append(tmpChk, int(begInChk), int(endInChk))
				return nil
			}
			cteExec.cursor += numRows
		}
	}
	if cteExec.chkIdx < p.resTbl.NumChunks() && cteExec.cursor < p.limitEnd {
		res, err := p.resTbl.GetChunk(cteExec.chkIdx)
		if err != nil {
			return err
		}
		cteExec.chkIdx++
		numRows := uint64(res.NumRows())
		if cteExec.cursor+numRows > p.limitEnd {
			numRows = p.limitEnd - cteExec.cursor
			req.Append(res.CopyConstructSel(), 0, int(numRows))
		} else {
			req.SwapColumns(res.CopyConstructSel())
		}
		cteExec.cursor += numRows
	}
	return nil
}

func (p *cteProducer) hasCTEResult() bool {
	return p.resTbl.Done()
}

// genCTEResult generates the result of CTE, and stores the result in resTbl.
// This is a synchronous function, which means it will block until the result is generated.
func (p *cteProducer) genCTEResult(ctx context.Context) (err error) {
	if p.resTbl.Error() != nil {
		return p.resTbl.Error()
	}
	resAction := setupCTEStorageTracker(p.resTbl, p.ctx, p.memTracker, p.diskTracker)
	iterInAction := setupCTEStorageTracker(p.iterInTbl, p.ctx, p.memTracker, p.diskTracker)
	var iterOutAction *chunk.SpillDiskAction
	if p.iterOutTbl != nil {
		iterOutAction = setupCTEStorageTracker(p.iterOutTbl, p.ctx, p.memTracker, p.diskTracker)
	}

	failpoint.Inject("testCTEStorageSpill", func(val failpoint.Value) {
		if val.(bool) && vardef.EnableTmpStorageOnOOM.Load() {
			defer resAction.WaitForTest()
			defer iterInAction.WaitForTest()
			if iterOutAction != nil {
				defer iterOutAction.WaitForTest()
			}
		}
	})

	if err = p.computeSeedPart(ctx); err != nil {
		p.resTbl.SetError(err)
		return err
	}
	if err = p.computeRecursivePart(ctx); err != nil {
		p.resTbl.SetError(err)
		return err
	}
	p.resTbl.SetDone()
	return nil
}

func (p *cteProducer) computeSeedPart(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil && err == nil {
			err = util.GetRecoverError(r)
		}
	}()
	failpoint.Inject("testCTESeedPanic", nil)
	p.curIter = 0
	p.iterInTbl.SetIter(p.curIter)
	chks := make([]*chunk.Chunk, 0, 10)
	for {
		if p.limitDone(p.iterInTbl) {
			break
		}
		chk := exec.TryNewCacheChunk(p.seedExec)
		if err = exec.Next(ctx, p.seedExec, chk); err != nil {
			return
		}
		if chk.NumRows() == 0 {
			break
		}
		if chk, err = p.tryDedupAndAdd(chk, p.iterInTbl, p.hashTbl); err != nil {
			return
		}
		chks = append(chks, chk)
	}
	// Initial resTbl is empty, so no need to deduplicate chk using resTbl.
	// Just adding is ok.
	for _, chk := range chks {
		if err = p.resTbl.Add(chk); err != nil {
			return
		}
	}
	p.curIter++
	p.iterInTbl.SetIter(p.curIter)

	return
}

func (p *cteProducer) computeRecursivePart(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil && err == nil {
			err = util.GetRecoverError(r)
		}
	}()
	failpoint.Inject("testCTERecursivePanic", nil)
	if p.recursiveExec == nil || p.iterInTbl.NumChunks() == 0 {
		return
	}

	if p.curIter > p.ctx.GetSessionVars().CTEMaxRecursionDepth {
		return exeerrors.ErrCTEMaxRecursionDepth.GenWithStackByArgs(p.curIter)
	}

	if p.limitDone(p.resTbl) {
		return
	}

	var iterNum uint64
	for {
		chk := exec.TryNewCacheChunk(p.recursiveExec)
		if err = exec.Next(ctx, p.recursiveExec, chk); err != nil {
			return
		}
		if chk.NumRows() == 0 {
			if iterNum%1000 == 0 {
				// To avoid too many logs.
				p.logTbls(ctx, err, iterNum, zapcore.DebugLevel)
			}
			iterNum++
			failpoint.Inject("assertIterTableSpillToDisk", func(maxIter failpoint.Value) {
				if iterNum > 0 && iterNum < uint64(maxIter.(int)) && err == nil {
					if p.iterInTbl.GetDiskBytes() == 0 && p.iterOutTbl.GetDiskBytes() == 0 && p.resTbl.GetDiskBytes() == 0 {
						p.logTbls(ctx, err, iterNum, zapcore.InfoLevel)
						panic("assert row container spill disk failed")
					}
				}
			})

			if err = p.setupTblsForNewIteration(); err != nil {
				return
			}
			if p.limitDone(p.resTbl) {
				break
			}
			if p.iterInTbl.NumChunks() == 0 {
				break
			}
			// Next iteration begins. Need use iterOutTbl as input of next iteration.
			p.curIter++
			p.iterInTbl.SetIter(p.curIter)
			if p.curIter > p.ctx.GetSessionVars().CTEMaxRecursionDepth {
				return exeerrors.ErrCTEMaxRecursionDepth.GenWithStackByArgs(p.curIter)
			}
			// Make sure iterInTbl is setup before Close/Open,
			// because some executors will read iterInTbl in Open() (like IndexLookupJoin).
			if err = exec.Close(p.recursiveExec); err != nil {
				return
			}
			if err = exec.Open(ctx, p.recursiveExec); err != nil {
				return
			}
		} else {
			if err = p.iterOutTbl.Add(chk); err != nil {
				return
			}
		}
	}
	return
}

func (p *cteProducer) setupTblsForNewIteration() (err error) {
	num := p.iterOutTbl.NumChunks()
	chks := make([]*chunk.Chunk, 0, num)
	// Setup resTbl's data.
	for i := range num {
		chk, err := p.iterOutTbl.GetChunk(i)
		if err != nil {
			return err
		}
		// Data should be copied in UNION DISTINCT.
		// Because deduplicate() will change data in iterOutTbl,
		// which will cause panic when spilling data into disk concurrently.
		if p.isDistinct {
			chk = chk.CopyConstruct()
		}
		chk, err = p.tryDedupAndAdd(chk, p.resTbl, p.hashTbl)
		if err != nil {
			return err
		}
		chks = append(chks, chk)
	}

	// Setup new iteration data in iterInTbl.
	if err = p.iterInTbl.Reopen(); err != nil {
		return err
	}
	setupCTEStorageTracker(p.iterInTbl, p.ctx, p.memTracker, p.diskTracker)

	if p.isDistinct {
		// Already deduplicated by resTbl, adding directly is ok.
		for _, chk := range chks {
			if err = p.iterInTbl.Add(chk); err != nil {
				return err
			}
		}
	} else {
		if err = p.iterInTbl.SwapData(p.iterOutTbl); err != nil {
			return err
		}
	}

	// Clear data in iterOutTbl.
	if err = p.iterOutTbl.Reopen(); err != nil {
		return err
	}
	setupCTEStorageTracker(p.iterOutTbl, p.ctx, p.memTracker, p.diskTracker)
	return nil
}

func (p *cteProducer) reset() error {
	p.curIter = 0
	p.hashTbl = nil
	p.executorOpened = false
	p.openErr = nil

	// Normally we need to setup tracker after calling Reopen(),
	// But reopen resTbl means we need to call genCTEResult() again, it will setup tracker.
	if err := p.resTbl.Reopen(); err != nil {
		return err
	}
	return p.iterInTbl.Reopen()
}

func (p *cteProducer) resetTracker() {
	if p.memTracker != nil {
		p.memTracker.Reset()
		p.memTracker = nil
	}
	if p.diskTracker != nil {
		p.diskTracker.Reset()
		p.diskTracker = nil
	}
}

// Check if tbl meets the requirement of limit.
func (p *cteProducer) limitDone(tbl cteutil.Storage) bool {
	return p.hasLimit && uint64(tbl.NumRows()) >= p.limitEnd
}

func setupCTEStorageTracker(tbl cteutil.Storage, ctx sessionctx.Context, parentMemTracker *memory.Tracker,
	parentDiskTracker *disk.Tracker) (actionSpill *chunk.SpillDiskAction) {
	memTracker := tbl.GetMemTracker()
	memTracker.SetLabel(memory.LabelForCTEStorage)
	memTracker.AttachTo(parentMemTracker)

	diskTracker := tbl.GetDiskTracker()
	diskTracker.SetLabel(memory.LabelForCTEStorage)
	diskTracker.AttachTo(parentDiskTracker)

	if vardef.EnableTmpStorageOnOOM.Load() {
		actionSpill = tbl.ActionSpill()
		failpoint.Inject("testCTEStorageSpill", func(val failpoint.Value) {
			if val.(bool) {
				actionSpill = tbl.(*cteutil.StorageRC).ActionSpillForTest()
			}
		})
		ctx.GetSessionVars().MemTracker.FallbackOldAndSetNewAction(actionSpill)
	}
	return actionSpill
}

func (p *cteProducer) tryDedupAndAdd(chk *chunk.Chunk,
	storage cteutil.Storage,
	hashTbl join.BaseHashTable) (res *chunk.Chunk, err error) {
	if p.isDistinct {
		if chk, err = p.deduplicate(chk, storage, hashTbl); err != nil {
			return nil, err
		}
	}
	return chk, storage.Add(chk)
}

// Compute hash values in chk and put it in hCtx.hashVals.
// Use the returned sel to choose the computed hash values.
func (p *cteProducer) computeChunkHash(chk *chunk.Chunk) (sel []int, err error) {
	numRows := chk.NumRows()
	p.hCtx.InitHash(numRows)
	// Continue to reset to make sure all hasher is new.
	for i := numRows; i < len(p.hCtx.HashVals); i++ {
		p.hCtx.HashVals[i].Reset()
	}
	sel = chk.Sel()
	var hashBitMap []bool
	if sel != nil {
		hashBitMap = make([]bool, chk.Capacity())
		for _, val := range sel {
			hashBitMap[val] = true
		}
	} else {
		// Length of p.sel is init as MaxChunkSize, but the row num of chunk may still exceeds MaxChunkSize.
		// So needs to handle here to make sure len(p.sel) == chk.NumRows().
		if len(p.sel) < numRows {
			tmpSel := make([]int, numRows-len(p.sel))
			for i := range tmpSel {
				tmpSel[i] = i + len(p.sel)
			}
			p.sel = append(p.sel, tmpSel...)
		}

		// All rows is selected, sel will be [0....numRows).
		// e.sel is setup when building executor.
		sel = p.sel
	}

	for i := range chk.NumCols() {
		if err = codec.HashChunkSelected(p.ctx.GetSessionVars().StmtCtx.TypeCtx(), p.hCtx.HashVals,
			chk, p.hCtx.AllTypes[i], i, p.hCtx.Buf, p.hCtx.HasNull,
			hashBitMap, false); err != nil {
			return nil, err
		}
	}
	return sel, nil
}

// Use hashTbl to deduplicate rows, and unique rows will be added to hashTbl.
// Duplicated rows are only marked to be removed by sel in Chunk, instead of really deleted.
func (p *cteProducer) deduplicate(chk *chunk.Chunk,
	storage cteutil.Storage,
	hashTbl join.BaseHashTable) (chkNoDup *chunk.Chunk, err error) {
	numRows := chk.NumRows()
	if numRows == 0 {
		return chk, nil
	}

	// 1. Compute hash values for chunk.
	chkHashTbl := join.NewConcurrentMapHashTable()
	selOri, err := p.computeChunkHash(chk)
	if err != nil {
		return nil, err
	}

	// 2. Filter rows duplicated in input chunk.
	// This sel is for filtering rows duplicated in cur chk.
	selChk := make([]int, 0, numRows)
	for i := range numRows {
		key := p.hCtx.HashVals[selOri[i]].Sum64()
		row := chk.GetRow(i)

		hasDup, err := p.checkHasDup(key, row, chk, storage, chkHashTbl)
		if err != nil {
			return nil, err
		}
		if hasDup {
			continue
		}

		selChk = append(selChk, selOri[i])

		rowPtr := chunk.RowPtr{ChkIdx: uint32(0), RowIdx: uint32(i)}
		chkHashTbl.Put(key, rowPtr)
	}
	chk.SetSel(selChk)
	chkIdx := storage.NumChunks()

	// 3. Filter rows duplicated in RowContainer.
	// This sel is for filtering rows duplicated in cteutil.Storage.
	selStorage := make([]int, 0, len(selChk))
	for i := range selChk {
		key := p.hCtx.HashVals[selChk[i]].Sum64()
		row := chk.GetRow(i)

		hasDup, err := p.checkHasDup(key, row, nil, storage, hashTbl)
		if err != nil {
			return nil, err
		}
		if hasDup {
			continue
		}

		rowIdx := len(selStorage)
		selStorage = append(selStorage, selChk[i])

		rowPtr := chunk.RowPtr{ChkIdx: uint32(chkIdx), RowIdx: uint32(rowIdx)}
		hashTbl.Put(key, rowPtr)
	}

	chk.SetSel(selStorage)
	return chk, nil
}

// Use the row's probe key to check if it already exists in chk or storage.
// We also need to compare the row's real encoding value to avoid hash collision.
func (p *cteProducer) checkHasDup(probeKey uint64,
	row chunk.Row,
	curChk *chunk.Chunk,
	storage cteutil.Storage,
	hashTbl join.BaseHashTable) (hasDup bool, err error) {
	entry := hashTbl.Get(probeKey)

	for ; entry != nil; entry = entry.Next {
		ptr := entry.Ptr
		var matchedRow chunk.Row
		if curChk != nil {
			matchedRow = curChk.GetRow(int(ptr.RowIdx))
		} else {
			matchedRow, err = storage.GetRow(ptr)
		}
		if err != nil {
			return false, err
		}
		isEqual, err := codec.EqualChunkRow(p.ctx.GetSessionVars().StmtCtx.TypeCtx(),
			row, p.hCtx.AllTypes, p.hCtx.KeyColIdx,
			matchedRow, p.hCtx.AllTypes, p.hCtx.KeyColIdx)
		if err != nil {
			return false, err
		}
		if isEqual {
			return true, nil
		}
	}
	return false, nil
}

func getCorColHashCode(corCol *expression.CorrelatedColumn) (res []byte) {
	return codec.HashCode(res, *corCol.Data)
}

// Return true if cor col has changed.
func (p *cteProducer) checkAndUpdateCorColHashCode() bool {
	var changed bool
	for i, corCol := range p.corCols {
		newHashCode := getCorColHashCode(corCol)
		if !bytes.Equal(newHashCode, p.corColHashCodes[i]) {
			changed = true
			p.corColHashCodes[i] = newHashCode
		}
	}
	return changed
}

func (p *cteProducer) logTbls(ctx context.Context, err error, iterNum uint64, lvl zapcore.Level) {
	logutil.Logger(ctx).Log(lvl, "cte iteration info",
		zap.Int64("iterInTbl mem usage", p.iterInTbl.GetMemBytes()), zap.Int64("iterInTbl disk usage", p.iterInTbl.GetDiskBytes()),
		zap.Int64("iterOutTbl mem usage", p.iterOutTbl.GetMemBytes()), zap.Int64("iterOutTbl disk usage", p.iterOutTbl.GetDiskBytes()),
		zap.Int64("resTbl mem usage", p.resTbl.GetMemBytes()), zap.Int64("resTbl disk usage", p.resTbl.GetDiskBytes()),
		zap.Int("resTbl rows", p.resTbl.NumRows()), zap.Uint64("iteration num", iterNum), zap.Error(err))
}
