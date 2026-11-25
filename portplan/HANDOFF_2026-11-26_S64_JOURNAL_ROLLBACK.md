# Session handoff — S64: rollback journal finalization (2026-11-26, landed)

Commit: `6d3ba760e` (`fix(pager): finalize rollback journals and rewind only owned WAL frames`).
Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code/fixture changes are in `internal/pager/journal.go`,
`internal/pager/pager.go`, `internal/pager/pagersnapshot.go`,
`internal/pager/pagerwal.go`, `internal/pager/wal.go`,
`internal/exec/transaction.go`, `internal/exec/ddl_forward.go`, and the two
native pins (`frigolite_s34_journal_t29_pin_test.go`,
`frigolite_temptable2_t29_pin_test.go`).

## Landed

- Both S34 latent cards closed:
  - ROLLBACK finalizes the DELETE-mode rollback journal, so no
    `test.db-journal` residue survives.
  - `db close` mid-transaction rolls back instead of flushing, matching
    `sqlite3Close → sqlite3RollbackAll`.
- `Pager.EndRollbackJournal()` reuses
  `finalizeRollbackJournalLockedMulti(false)` (DELETE unlink, TRUNCATE/PERSIST
  finalization, WAL skip). Savepoint ROLLBACK TO does not call it.
- `Engine.rollbackOpenTx()` finalizes journals for all attached pagers after
  DDL undo and before restoring snapshots; a finalization error is reported
  after the rollback hook. `Engine.Close()` calls `rollbackOpenTx()` for an
  open transaction before FTS flush/lock release/pager close.
- WAL rollback now rewinds only frames written by THIS connection's write
  transaction. `walWriter` records frame count and checksum chain when it
  acquires the WRITER lock (`markWriteStartLocked`), and `Pager.Restore`
  calls `rewindOwnWALFramesLocked()`. A read-only close has no writer lock,
  so it cannot truncate frames another connection committed after its read
  snapshot (the C `sqlite3WalSavepoint` / write-savepoint boundary).
- Native pins: `TestS34RollbackRemovesDeleteJournal`,
  `TestS34CloseMidTxnRollsBack`, `TestS64CloseReadOnlyWALPreservesOtherFrames`,
  `TestS64TempPageCount`.

## Verification

- Oracle CLI (`/tmp/t29_oracle`) established the two S34 expected behaviors.
- Pins RED before fix (`/tmp/t29_pins0.log`), GREEN after
  (`/tmp/t29_pins1.log`, `/tmp/t29_pins2.log`, `/tmp/t29_fix2_pins.log`).
- Controlled bisect isolated the three full-gate regressions: close rollback
  caused both `wal3` leaves; the concurrent focused-gate run (two `go test`
  processes in one worktree) fabricated the `temptable2-4.1.1` mismatch,
  which vanished on a sequential rerun and is absent from a pure-Go pin.
- Sequential family gates: `wal3`, `temptable2`, `pager1`, `journal`,
  `rollback`, `savepoint`, `trans`, `pragma2`, `sharedA`, `e_walckpt` — all
  NEW=0/GONE=0 versus `/tmp/t28_full_gate.log` leaf sets
  (`/tmp/t29_fix_*.log`).
- `go test ./internal/... -count=1` green (`/tmp/t29_fix_internal.log`).
- Quality normalized NEW=0/GONE=0 vs `/tmp/t28_quality2.log`
  (`/tmp/t29_fix2_quality.log`); helper extraction kept `Pager.Restore` at
  its baseline complexity.
- Detached full gate `/tmp/t29_full_gate2.log` (status file
  `/tmp/t29_full_gate2.status`, 1219 leaves): raw-byte leaf diff NEW=0 vs
  `/tmp/t28_full_gate.log` and durable `/tmp/t10_full2.log`; sole GONE vs
  t28 is `resetdb/resetdb-300` (known load flap).
- Converter baseline unchanged (no converter or fixture edits in S64).

## Restart point / next unit

S65: `temptable2-4.1.4` and `5.1.2`. The TCL asserts boolean range
expressions (`expr ($n >280 && $n <300)` and `expr ($n >270 && $n <290)`)
after `set n [db one {PRAGMA temp.page_count}]`, but the converter folds them
to a literal `expect: "1"` because the unbraced parenthesized expression has
multiple TCL words and `cmdDoTest`'s trailing-expr branch requires exactly
two. The harness already supports `runtime_expr` and `ExpectedExpr`. Fix the
multi-word `expr` capture, then adjudicate the engine's page counts
(316/304) against those ranges. After that: where2-2.4 watchlist, then the
performance phase.

## Risks / working rules

- Journal finalization fires xClose/xDelete hook events; the affected family
  gates and full gate stayed name-set clean.
- Close now fires the rollback hook for an open transaction, matching C.
- WAL rewind uses the writer-lock boundary; if a future path writes frames
  without taking the WRITER lock, it must also call `markWriteStartLocked`.
- Never run two `go test` processes in one worktree: sequential gates are the
  only valid evidence. Keep full gates detached with `-timeout 30m` and
  status files; compare raw leaf-name fail sets, never counts or line greps.
