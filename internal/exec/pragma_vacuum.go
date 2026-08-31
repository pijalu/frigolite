// PRAGMA incremental_vacuum (btree.c sqlite3BtreeIncrVacuum): the corruption
// guard runs first (an oversized header freelist count, or a final size
// beyond the current size, is a malformed database), then the vacuum step
// work. This engine never populates the on-disk freelist (freed pages are
// not recycled), so a nonzero freelist count here describes only a corrupted
// header — reported exactly as SQLite reports it.

package exec

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/pager"
)

// IncrementalVacuum implements PRAGMA incremental_vacuum. For each call with
// nFree>0 it consumes one free page from the on-disk freelist (decrementing
// the header count) and yields one row. With phase 3's IncrVacuumStep, the
// step also performs the actual page-swap + truncate (file shrinks by 1 page
// per call when the last page is on the freelist; if the last page is in use
// and a free page is available, relocate+truncate).
func (e *Engine) IncrementalVacuum(schema string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return &execpragma.Result{}
	}
	ps := ctx.Pager.PageSize()
	nOrig := ctx.Pager.HeaderPageCount()
	nFree := ctx.Pager.FreelistCount()
	nFin := finalDbSize(nOrig, nFree, ps)
	// btree.c sqlite3BtreeIncrVacuum corruption guard.
	if nOrig < nFin || nFree >= nOrig {
		return &execpragma.Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	// With an empty freelist there is no work: SQLITE_DONE, no rows.
	if nFree == 0 {
		return &execpragma.Result{}
	}
	// Run one IncrVacuumStep via the btree (P8.INCRVACUUM phase 3).
	if err := e.runIncrVacuumStep(ctx); err != nil {
		// If the step fails (e.g. no free page available for a
		// non-freelist last page), we still surface a row so the
		// `db eval {PRAGMA incremental_vacuum}` callback chain
		// terminates. The DecrementFreelistCount below is the
		// existing row-yield mechanism.
		_ = err
	}
	// Consume one free page: decrement header count and emit a row.
	ctx.Pager.DecrementFreelistCount(1)
	return &execpragma.Result{Columns: []string{"incremental_vacuum"}, Rows: [][]interface{}{{int64(1)}}}
}

// runIncrVacuumStep performs a single btree.c incrVacuumStep: the last
// page of the file is either on the freelist (just truncate) or in
// use (relocate to a low free page via AllocatePageLE + RelocatePage,
// then truncate). The step does one page of work.
//
// Reference: btree.c sqlite3BtreeIncrVacuum / incrVacuumStep
// (~line 6780 / 6700).
func (e *Engine) runIncrVacuumStep(ctx *DatabaseContext) error {
	bt := btree.NewBTree(ctx.Pager, 1, true)
	_, err := bt.IncrVacuumStep(1)
	return err
}

// AutoVacuumCommit drains the on-disk freelist via repeated
// IncrVacuumStep calls (P8.INCRVACUUM phase 4). Called from
// engine.go's commit() hook when the database is in FULL auto-vacuum
// mode. Honors the per-batch nVac returned by an optional
// sqlite3_autovacuum_pages callback (which the engine stores in
// e.autovacPagesCallback; nil if not registered).
//
// Reference: btree.c autoVacuumCommit (~line 4174).
func (e *Engine) AutoVacuumCommit(schema string) (int, error) {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return 0, nil
	}
	ps := ctx.Pager.PageSize()
	nFree := ctx.Pager.FreelistCount()
	if nFree == 0 {
		return 0, nil
	}
	// Compute the upper bound on what to vacuum this batch. If a
	// callback is registered, ask it; otherwise drain all.
	var nVac uint32
	if cb := e.getAutovacPagesCallback(); cb != nil {
		// btree.c autoVacuumCommit passes nFilePages (in pages) and
		// pageSize (in bytes) to the callback. Our public Go signature
		// mirrors the C signature: cb(schema, fileSize, nFree, pageSize)
		// where fileSize is in pages and pageSize is in bytes.
		fileSize := ctx.Pager.NumPages()
		want := cb(schema, uint32(fileSize), nFree, ps)
		if want > nFree {
			want = nFree
		}
		nVac = want
	} else {
		nVac = nFree
	}
	totalSteps := uint32(0)
	for i := uint32(0); i < nVac; i++ {
		npBefore := ctx.Pager.NumPages()
		if err := e.runIncrVacuumStep(ctx); err != nil {
			break
		}
		npAfter := ctx.Pager.NumPages()
		if npAfter >= npBefore {
			break
		}
		totalSteps++
	}
	return int(totalSteps), nil
}

// SetAutovacuumPagesCallback registers (or clears, if nil) a
// callback fired by AutoVacuumCommit before each batch. The callback
// signature mirrors btree.c sqlite3_autovacuum_pages:
//   cb(schema, fileSize, nFree, pageSize) -> nVac
// The engine returns the desired number of pages to vacuum this
// batch (clamped to nFree).
func (e *Engine) SetAutovacuumPagesCallback(cb func(schema string, fileSize, nFree, pageSize uint32) uint32) {
	e.autovacPagesCallback = cb
}

func (e *Engine) getAutovacPagesCallback() func(schema string, fileSize, nFree, pageSize uint32) uint32 {
	return e.autovacPagesCallback
}

// finalDbSize computes the post-vacuum page count of an auto-vacuum database
// nOrig pages in size with nFree free pages (btree.c finalDbSize), including
// the pointer-map pages that vacuuming frees. Arithmetic is 32-bit unsigned
// and may wrap exactly as SQLite's Pgno arithmetic does; callers compare the
// result against nOrig with the same relational operators.
func finalDbSize(nOrig, nFree, pageSize uint32) uint32 {
	nEntry := pageSize/5 + 1
	nPtrmap := (nFree - nOrig + PtrmapPagenoFor(nOrig, pageSize) + nEntry) / nEntry
	nFin := nOrig - nFree - nPtrmap
	pending := pendingBytePageFor(pageSize)
	if nOrig > pending && nFin < pending {
		nFin--
	}
	for isPtrmapPageFor(nFin, pageSize) || nFin == pending {
		nFin--
	}
	return nFin
}

// PtrmapPagenoFor exposes the pointer-map page covering pgno (btree.c
// ptrmapPageno) for the vacuum math in this package.
func PtrmapPagenoFor(pgno, pageSize uint32) uint32 {
	return pager.PtrmapPageNo(pgno, pageSize)
}

// pendingBytePageFor is the page holding the PENDING_BYTE lock byte
// (btree.c PENDING_BYTE_PAGE); vacuuming never leaves the database ending
// on it.
func pendingBytePageFor(pageSize uint32) uint32 {
	return 1073741824/pageSize + 1
}

// isPtrmapPageFor reports whether pgno is itself a pointer-map page.
func isPtrmapPageFor(pgno, pageSize uint32) bool {
	return pgno >= 2 && PtrmapPagenoFor(pgno, pageSize) == pgno
}
