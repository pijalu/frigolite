// PRAGMA incremental_vacuum (btree.c sqlite3BtreeIncrVacuum): the corruption
// guard runs first (an oversized header freelist count, or a final size
// beyond the current size, is a malformed database), then the vacuum step
// work. This engine never populates the on-disk freelist (freed pages are
// not recycled), so a nonzero freelist count here describes only a corrupted
// header — reported exactly as SQLite reports it.

package exec

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/pager"
)

// IncrementalVacuum implements PRAGMA incremental_vacuum. For each call with
// nFree>0 it consumes one free page from the on-disk freelist (decrementing
// the header count) and yields one row. Without an actual page-relocation
// pass the file does not shrink (sqlite3PagerMovepage / relocatePage /
// btree.c incrVacuumStep); this engine surfaces one row per free-page so
// `db eval {PRAGMA incremental_vacuum}` callback chains terminate, but the
// file size stays the same. Tests asserting post-vacuum file size require
// the full page-swap implementation (P8.INCRVACUUM follow-up).
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
	// Consume one free page: decrement header count and emit a row.
	ctx.Pager.DecrementFreelistCount(1)
	return &execpragma.Result{Columns: []string{"incremental_vacuum"}, Rows: [][]interface{}{{int64(1)}}}
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
