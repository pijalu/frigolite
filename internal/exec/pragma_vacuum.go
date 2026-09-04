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
//
// P8.INCRVACUUM.phase7: when a transaction is active, the file-truncating
// step is skipped. The journal/rollback machinery does not yet capture the
// BEFORE image of the truncated tail page, so a ROLLBACK would leave the
// file shorter than the btree expects. In a transaction we still yield the
// row but do NOT call runIncrVacuumStep or DecrementFreelistCount — the
// chain still references the page, so a count decrement would create a
// header.count / chain-walked-count mismatch. The file is actually shrunk
// at COMMIT (AutoVacuumCommit on FULL mode / IncrVacuumStep on
// INCREMENTAL-mode COMMITs).
func (e *Engine) IncrementalVacuum(schema string, limit int64) *execpragma.Result {
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
	// Transactional guard (P8.INCRVACUUM.phase7): yield the row and
	// return without modifying the file or the chain count. The vacuum
	// is performed at COMMIT (or the user's next incremental_vacuum
	// after ROLLBACK). For non-transactional callers the step loop
	// runs below.
	if e.tx.inTransaction {
		return &execpragma.Result{Columns: []string{"incremental_vacuum"}, Rows: [][]interface{}{{int64(1)}}}
	}
	// btree.c sqlite3BtreeIncrVacuum + pragma.c PragTyp_INCREMENTAL_VACUUM:
	// the VDBE loops OP_IncrVacuum — one incrVacuumStep per iteration —
	// until SQLITE_DONE or the N-step limit. Each step (truncate a free
	// tail page, or relocate the live last page onto the lowest free
	// page) maintains the freelist count, the freelist chain, and the
	// header page count itself; the pragma adds no bookkeeping of its
	// own (a DecrementFreelistCount here would double-count against
	// Truncate's own truncatedFree adjustment — "Freelist: size is N
	// but should be M").
	total := int64(0)
	var rows [][]interface{}
	for total < limit {
		steps, err := e.runIncrVacuumStep(ctx, false)
		if err != nil {
			// A failed step (e.g. the relocation orphan branch)
			// ends the loop; whatever steps already completed are
			// reported. The `db eval {PRAGMA incremental_vacuum}`
			// callback chain terminates on the last row.
			_ = err
			break
		}
		if steps == 0 {
			break // SQLITE_DONE — no more work
		}
		total += int64(steps)
		rows = append(rows, []interface{}{int64(1)})
	}
	if total == 0 {
		return &execpragma.Result{}
	}
	return &execpragma.Result{Columns: []string{"incremental_vacuum"}, Rows: rows}
}

// runIncrVacuumStep performs a single btree.c incrVacuumStep: the last
// page of the file is either on the freelist (just truncate) or in
// use (relocate to a low free page via AllocatePageLE + RelocatePage,
// then truncate). The step does one page of work. bCommit mirrors the
// C parameter: false for PRAGMA incremental_vacuum, true for the
// autocommit drain (autoVacuumCommit).
//
// Reference: btree.c sqlite3BtreeIncrVacuum / incrVacuumStep
// (~line 6780 / 4010).
func (e *Engine) runIncrVacuumStep(ctx *DatabaseContext, bCommit bool) (int, error) {
	bt := btree.NewBTree(ctx.Pager, 1, true)
	steps, err := bt.IncrVacuumStep(1, bCommit)
	return steps, err
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
	// P8.INCRVACUUM.phase9.s: drain the freelist past nVac when the only
	// remaining work is a final ptrmap-page truncation (autovacuum-9.2
	// expects the file to drop to 1 page after dropping all tables;
	// nFree counts only the freelist chain, not the trailing ptrmap).
	// The first nVac iterations honour the callback's batch cap; the
	// extra iterations only run as far as the work itself allows
	// (the loop breaks when no progress is made).
	for i := uint32(0); ; i++ {
		npBefore := ctx.Pager.NumPages()
		_, err := e.runIncrVacuumStep(ctx, true)
		if err != nil {
			break
		}
		npAfter := ctx.Pager.NumPages()
		if npAfter >= npBefore {
			break
		}
		totalSteps++
		if i+1 >= nVac && ctx.Pager.NumPages() <= 1 {
			break
		}
	}
	// P8.INCRVACUUM.T5: mirror autoVacuumCommit's post-drain step
	// (src/btree.c:4247-4252): when the whole freelist was drained
	// (nVac==nFree), the chain header is zeroed — chain entries that
	// survive on disk are intentionally garbage at that point (every
	// former free page was relocated into or truncated away), and the
	// zeroed trunk/count make that legal for integrity_check. Without
	// this, header.trunk could point above the truncation point and
	// later chain walks (checkFreelistCount) would read truncated
	// pages. A callback-capped batch (nVac<nFree) keeps the chain:
	// remaining free pages below the truncation point are still
	// reachable through it, exactly as in SQLite.
	if nVac == nFree {
		ctx.Pager.ZeroFreelistChain()
	}
	return int(totalSteps), nil
}

// SetAutovacuumPagesCallback registers (or clears, if nil) a
// callback fired by AutoVacuumCommit before each batch. The callback
// signature mirrors btree.c sqlite3_autovacuum_pages:
//
//	cb(schema, fileSize, nFree, pageSize) -> nVac
//
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
