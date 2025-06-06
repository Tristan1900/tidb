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
	"container/heap"
	"context"
	"testing"

	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/dbterror/exeerrors"
	"github.com/pingcap/tidb/pkg/util/memory"
	"github.com/pingcap/tidb/pkg/util/sqlkiller"
	"github.com/stretchr/testify/require"
)

// topNChunkHeap implements heap.Interface.
type topNChunkHeap struct {
	compareRow func(chunk.Row, chunk.Row) int
	greaterRow func(chunk.Row, chunk.Row) bool

	// rowChunks is the chunks to store row values.
	rowChunks *chunk.List
	// rowPointer store the chunk index and row index for each row.
	rowPtrs []chunk.RowPtr

	isInitialized bool
	isRowPtrsInit bool

	memTracker *memory.Tracker

	totalLimit uint64
	idx        int

	fieldTypes []*types.FieldType
}

func (h *topNChunkHeap) init(topnExec *TopNExec, memTracker *memory.Tracker, totalLimit uint64, idx int, greaterRow func(chunk.Row, chunk.Row) bool, fieldTypes []*types.FieldType) {
	h.memTracker = memTracker

	// The schema of TopN keep same with its children without inline projection. After inline projection, TopN will have its own schema,
	// so TopN can not be used to construct chunks, but children information needs to be used instead.
	// Row size of new chunk list may not be enough to hold the result set from child executor when inline projection occurs.
	// To avoid this problem, we use child executor's schmea to build new chunk list by default.
	ch := topnExec.Children(0)
	h.rowChunks = chunk.NewList(exec.RetTypes(ch), ch.InitCap(), ch.MaxChunkSize())
	h.rowChunks.GetMemTracker().AttachTo(h.memTracker)
	h.rowChunks.GetMemTracker().SetLabel(memory.LabelForRowChunks)

	h.compareRow = topnExec.compareRow
	h.greaterRow = greaterRow

	h.totalLimit = totalLimit
	h.idx = idx
	h.isInitialized = true

	h.fieldTypes = fieldTypes
}

func (h *topNChunkHeap) initPtrs() {
	h.memTracker.Consume(int64(chunk.RowPtrSize * h.rowChunks.Len()))
	h.initPtrsImpl()
}

func (h *topNChunkHeap) initPtrsImpl() {
	h.rowPtrs = make([]chunk.RowPtr, 0, h.rowChunks.Len())
	for chkIdx := range h.rowChunks.NumChunks() {
		rowChk := h.rowChunks.GetChunk(chkIdx)
		for rowIdx := range rowChk.NumRows() {
			h.rowPtrs = append(h.rowPtrs, chunk.RowPtr{ChkIdx: uint32(chkIdx), RowIdx: uint32(rowIdx)})
		}
	}
	h.isRowPtrsInit = true
}

func (h *topNChunkHeap) clear() {
	h.rowChunks.Clear()
	h.memTracker.Consume(int64(-chunk.RowPtrSize * len(h.rowPtrs)))
	h.rowPtrs = nil
	h.isRowPtrsInit = false
	h.isInitialized = false
	h.idx = 0
}

func (h *topNChunkHeap) update(heapMaxRow chunk.Row, newRow chunk.Row) {
	if h.greaterRow(heapMaxRow, newRow) {
		// Evict heap max, keep the next row.
		h.rowPtrs[0] = h.rowChunks.AppendRow(newRow)
		heap.Fix(h, 0)
	}
}

func (h *topNChunkHeap) processChk(chk *chunk.Chunk) {
	for i := range chk.NumRows() {
		heapMaxRow := h.rowChunks.GetRow(h.rowPtrs[0])
		newRow := chk.GetRow(i)
		h.update(heapMaxRow, newRow)
	}
}

// doCompaction rebuild the chunks and row pointers to release memory.
// If we don't do compaction, in a extreme case like the child data is already ascending sorted
// but we want descending top N, then we will keep all data in memory.
// But if data is distributed randomly, this function will be called log(n) times.
func (h *topNChunkHeap) doCompaction(topnExec *TopNExec) error {
	// The schema of TopN keep same with its children without inline projection. After inline projection, TopN will have its own schema,
	// so TopN can not be used to construct chunks, but children information needs to be used instead.
	// Row size of new chunk list may not be enough to hold the result set from child executor when inline projection occurs.
	// To avoid this problem, we use child executor's schmea to build new chunk list by default.
	ch := topnExec.Children(0)
	newRowChunks := chunk.NewList(exec.RetTypes(ch), ch.InitCap(), ch.MaxChunkSize())
	newRowPtrs := make([]chunk.RowPtr, 0, h.rowChunks.Len())
	for _, rowPtr := range h.rowPtrs {
		newRowPtr := newRowChunks.AppendRow(h.rowChunks.GetRow(rowPtr))
		newRowPtrs = append(newRowPtrs, newRowPtr)
	}
	newRowChunks.GetMemTracker().SetLabel(memory.LabelForRowChunks)
	h.memTracker.ReplaceChild(h.rowChunks.GetMemTracker(), newRowChunks.GetMemTracker())
	h.rowChunks = newRowChunks

	h.memTracker.Consume(int64(chunk.RowPtrSize * (len(newRowPtrs) - len(h.rowPtrs))))
	h.rowPtrs = newRowPtrs
	return nil
}

func (h *topNChunkHeap) keyColumnsCompare(i, j chunk.RowPtr) int {
	rowI := h.rowChunks.GetRow(i)
	rowJ := h.rowChunks.GetRow(j)
	return h.compareRow(rowI, rowJ)
}

// Less implement heap.Interface, but since we mantains a max heap,
// this function returns true if row i is greater than row j.
func (h *topNChunkHeap) Less(i, j int) bool {
	rowI := h.rowChunks.GetRow(h.rowPtrs[i])
	rowJ := h.rowChunks.GetRow(h.rowPtrs[j])
	return h.greaterRow(rowI, rowJ)
}

func (h *topNChunkHeap) Len() int {
	return len(h.rowPtrs)
}

func (*topNChunkHeap) Push(any) {
	// Should never be called.
}

func (h *topNChunkHeap) Pop() any {
	h.rowPtrs = h.rowPtrs[:len(h.rowPtrs)-1]
	// We don't need the popped value, return nil to avoid memory allocation.
	return nil
}

func (h *topNChunkHeap) Swap(i, j int) {
	h.rowPtrs[i], h.rowPtrs[j] = h.rowPtrs[j], h.rowPtrs[i]
}

// TestKillSignalInTopN is for test
func TestKillSignalInTopN(t *testing.T, topnExec *TopNExec) {
	ctx := context.Background()
	err := topnExec.Open(ctx)
	require.NoError(t, err)

	chkHeap := &topNChunkHeap{}
	// Offset of heap in worker should be 0, as we need to spill all data
	chkHeap.init(topnExec, topnExec.memTracker, topnExec.Limit.Offset+topnExec.Limit.Count, 0, topnExec.greaterRow, topnExec.RetFieldTypes())
	srcChk := exec.TryNewCacheChunk(topnExec.Children(0))
	err = exec.Next(ctx, topnExec.Children(0), srcChk)
	require.NoError(t, err)
	chkHeap.rowChunks.Add(srcChk)

	topnExec.Ctx().GetSessionVars().SQLKiller.SendKillSignal(sqlkiller.QueryInterrupted)
	err = topnExec.spillHelper.spillHeap(chkHeap)
	require.ErrorIs(t, err, exeerrors.ErrQueryInterrupted)
}
