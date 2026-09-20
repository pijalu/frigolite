// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// This file implements the sqlite3_backup_step state machine: page-budget
// accounting (Step/stepAdvance), backup.c setDestPgsz application on the
// first step (stepSetDestPgsz and its FullImageReplace / page-size-adopt
// arms), and the source-change restart rule.

package frigolite

// Step advances the backup by nPages pages (nPages < 0 copies the whole
// database, nPages == 0 copies nothing). It returns the SQLite result code
// string: "SQLITE_DONE" when the backup has completed, "SQLITE_OK" when
// pages remain, "SQLITE_BUSY" when the source or destination is locked, and
// "SQLITE_READONLY" for a populated in-memory destination with a mismatched
// page size. An empty in-memory destination adopts the source page size on its
// first step, matching SQLite's backup.c setDestPgsz behavior.
func (b *Backup) Step(nPages int) string {
	if b == nil {
		return "SQLITE_ERROR"
	}
	if b.finished {
		return b.rc
	}
	if b.done {
		b.rc = "SQLITE_DONE"
		return b.rc
	}

	if !b.stepSetDestPgsz() {
		return b.rc
	}

	// Lock checks: an open write transaction on the source, an exclusive lock
	// on the source by another connection, or a write transaction on the
	// destination by another connection all return SQLITE_BUSY. A missing
	// destination schema (detached mid-backup, backup5-3.3) returns
	// SQLITE_ERROR with "unknown database <name>" on the destination.
	if b.dst.engine.GetDB(b.dstSchema) == nil {
		b.rc = "SQLITE_ERROR"
		b.lastErr = "unknown database " + b.dstSchema
		b.dst.engine.SetLastErr(b.lastErr, "SQLITE_ERROR")
		return b.rc
	}
	if rc := b.checkBusy(); rc != "" {
		b.rc = rc
		return b.rc
	}

	b.stepRestartIfSourceChanged()
	b.stepAdvance(nPages)
	return b.rc
}

// stepSetDestPgsz applies backup.c's setDestPgsz on the first step. An empty
// destination (memory or file) adopts the source page size before any
// page is written; an in-memory destination cannot be resized once it
// holds pages and a mismatch fails with SQLITE_READONLY. A populated
// file-backed destination proceeds: frigolite rebuilds it logically,
// so the destination page size adapts through the copy itself.
// A FullImageReplace destination (the VACUUM copy-back) mirrors backup.c's
// whole-image overwrite: the destination is reset EMPTY first (at the
// pending page size when KeepDestPageSize is set — vacuumRebuild has
// already applied it — otherwise at the source's), so free pages are
// reclaimed and the rebuilt image is compact.
//
// It reports false when it has already set b.rc/b.lastErr and the step must
// return immediately.
func (b *Backup) stepSetDestPgsz() bool {
	if b.FullImageReplace && b.copied == 0 && !b.done {
		return b.resetDestForFullReplace()
	} else if b.dstPageMismatch() {
		return b.adoptDestPageSize()
	}
	return true
}

// resetDestForFullReplace mirrors backup.c's whole-image overwrite for the
// VACUUM copy-back: the destination is reset EMPTY first (at the pending page
// size when KeepDestPageSize is set — vacuumRebuild has already applied it —
// otherwise at the source's), so free pages are reclaimed and the rebuilt
// image is compact. It reports false when b.rc/b.lastErr were set and the
// step must return immediately.
func (b *Backup) resetDestForFullReplace() bool {
	srcCtx := b.src.engine.GetDB(b.srcSchema)
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if srcCtx == nil || dstCtx == nil {
		b.rc = "SQLITE_ERROR"
		b.lastErr = "unknown database"
		return false
	}
	if b.KeepDestPageSize {
		return true
	}
	dstCtx.Pager.ResetToEmpty(srcCtx.Pager.PageSize())
	if !dstCtx.IsMemory {
		_ = dstCtx.Pager.Flush()
	}
	dstCtx.Schema.InvalidateCache()
	return true
}

// adoptDestPageSize applies setDestPgsz when the destination's page size
// differs from the source's: an in-memory destination cannot be resized once
// it holds pages and a mismatch fails with SQLITE_READONLY; an empty
// destination adopts the source page size. A populated file-backed
// destination proceeds: frigolite rebuilds it logically, so the destination
// page size adapts through the copy itself. It reports false when
// b.rc/b.lastErr were set and the step must return immediately.
func (b *Backup) adoptDestPageSize() bool {
	srcCtx := b.src.engine.GetDB(b.srcSchema)
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if srcCtx == nil || dstCtx == nil {
		b.rc = "SQLITE_ERROR"
		b.lastErr = "unknown database"
		return false
	}
	if dstCtx.IsMemory {
		if dstCtx.Pager.NumPages() > 1 {
			b.rc = "SQLITE_READONLY"
			b.lastErr = "attempt to write a readonly database"
			return false
		}
		if !b.KeepDestPageSize {
			dstCtx.Pager.ResetToEmpty(srcCtx.Pager.PageSize())
		}
		dstCtx.Schema.InvalidateCache()
		return true
	}
	if !b.KeepDestPageSize && (dstCtx.Pager.OpenedEmpty() || dstCtx.Pager.NumPages() == 0) {
		// setDestPgsz for a file destination never written to:
		// re-create it at the source page size. ResetToEmpty +
		// immediate Flush keeps the on-disk image self-consistent
		// (canonical header inside page 1) so the per-statement file
		// checks never observe a truncated/zeroed image.
		dstCtx.Pager.ResetToEmpty(srcCtx.Pager.PageSize())
		_ = dstCtx.Pager.Flush()
		dstCtx.Schema.InvalidateCache()
	}
	return true
}

// stepRestartIfSourceChanged restarts the backup when the source was modified
// since the backup started (or since the last restart): in-memory sources
// restart, file-backed sources continue (their previously copied pages remain
// valid snapshots).
func (b *Backup) stepRestartIfSourceChanged() {
	if !b.hasChange {
		return
	}
	srcCtx := b.src.engine.GetDB(b.srcSchema)
	if srcCtx == nil {
		return
	}
	cc, ok := srcCtx.Pager.FileChangeCounter()
	if !ok || cc == b.initChange {
		return
	}
	if srcCtx.IsMemory {
		b.copied = 0
		b.initChange = cc
	}
}

// stepAdvance accounts the page budget for this step and completes the
// logical copy when the budget covers the source: nPages < 0 copies the whole
// database, nPages == 0 copies nothing (SQLITE_OK unless already complete).
func (b *Backup) stepAdvance(nPages int) {
	if nPages == 0 {
		if b.copied >= b.currentPagecount() {
			b.done = true
			b.rc = "SQLITE_DONE"
			return
		}
		b.rc = "SQLITE_OK"
		return
	}
	if nPages < 0 {
		nPages = b.currentPagecount()
	}
	total := b.currentPagecount()
	b.copied += nPages
	if b.copied < total {
		b.rc = "SQLITE_OK"
		return
	}
	b.copied = total
	b.done = true
	b.rc = "SQLITE_DONE"
	if err := b.copyLocked(); err != nil {
		b.lastErr = err.Error()
		b.rc = "SQLITE_ERROR"
	} else if b.sourceEmpty() {
		b.resetEmptyDestination()
	}
}
