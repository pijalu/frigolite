# Plan — S64: finalize rollback journal and roll back on close (2026-11-25)

Status: LANDED at `6d3ba760e`; full gate NEW=0 vs t28/t10.

Mode: debug — two latent S34 cards reproduce with native pure-Go pins; neither
is currently red in the JSON suite, so the fix is driven by SQLite oracle
contracts rather than a harness assertion.

## Evidence

- Oracle (`/tmp/t29_oracle`, SQLite CLI):
  - `PRAGMA journal_mode=delete; CREATE; BEGIN; INSERT; ROLLBACK` leaves NO
    `db1-journal`.
  - `CREATE; INSERT(1); BEGIN; INSERT(2); close` leaves count 1, not 2, and
    no journal.
- Pins before fix (`/tmp/t29_pins0.log`):
  - `TestS34RollbackRemovesDeleteJournal`: journal still present after ROLLBACK.
  - `TestS34CloseMidTxnRollsBack`: count 2 after close/reopen (implicit COMMIT).
- C contracts:
  - pager.c `pager_playback → pager_end_transaction` finalizes the journal;
    DELETE unlinks, TRUNCATE/PERSIST finalize their sidecar.
  - main.c:1374-1379 `sqlite3Close → sqlite3RollbackAll` rolls an open
    transaction back and fires the rollback hook (main.c:1520-1523).
- `portplan/PLAN_S34_regress.md` §Carded lists exactly these two residues.

## Change (one coherent slice)

1. `internal/pager/journal.go`: add `Pager.EndRollbackJournal()` — the
   full-transaction-rollback journal finalizer. It skips WAL pagers and calls
   `finalizeRollbackJournalLockedMulti(false)` so mode-specific behavior is
   identical to COMMIT finalization (DELETE unlink, TRUNCATE/PERSIST finalize).
   Savepoint ROLLBACK TO does NOT call it, matching C's `isSavepnt` playback.
2. `internal/exec/transaction.go`: `rollbackOpenTx` calls
   `EndRollbackJournal()` for every attached pager after DDL undo and before
   `restoreTxSnapshots`; a finalization error is returned after the rollback
   hook, matching C's error propagation without masking the page restore.
3. `internal/exec/ddl_forward.go`: `Engine.Close()` calls `rollbackOpenTx()`
   when `e.tx.inTransaction`, before FTS flush/lock release/pager close. C's
   close rolls back first, so the pager close sees no dirty pages and cannot
   turn the open transaction into a COMMIT.
4. Pure-Go pins `frigolite_s34_journal_t29_pin_test.go` cover both contracts.

## Regression triage (2026-11-26, in progress)

The first full gate (`/tmp/t29_full_gate.log`) and focused gates
(`/tmp/t29_wal3_focused.log`, `/tmp/t29_temptable2_focused.log`) reproduced
three NEW leaves deterministically; they are NOT flakes:

- `wal3/wal3-9.3`: `exec error: pager: checkpoint: wal frame 7 missing
  (short read)` on `PRAGMA wal_checkpoint` after closing db0..db48.
- `wal3/wal3-9.4`: same error for frame 53 after closing db49.
- `temptable2/temptable2-4.1.1`: `PRAGMA temp.page_count` returned 10, want 9.

Hypotheses, in order:

1. `Engine.Close()`'s new rollback calls `rollbackOpenTx()` on read-only
   explicit transactions. `Pager.Restore()` then truncates the SHARED WAL
   back to that connection's snapshot frame, deleting frames committed by
   other connections. C's `sqlite3BtreeRollback` ends a WAL read transaction
   without rewinding the shared log; only frames written by the rolling-back
   connection may be undone.
2. `rollbackOpenTx()`'s new `EndRollbackJournal()` loop touches attached
   in-memory/temp pagers or implicit transaction rollbacks, adding a temp
   page (10 vs 9). C's `pager_end_transaction` is gated on an open pager
   write transaction (`eState >= PAGER_WRITER_LOCKED || eLock >= RESERVED`);
   FRIGOLITE's call is currently ungated.

Controlled-bisect results:

- Disabling only `Engine.Close()` rollback removes both `wal3` leaves and
  leaves `temptable2-4.1.1` failing: close rollback is the WAL cause.
- Disabling only the journal-finalization loop removes the temp leaf and
  leaves both `wal3` leaves failing: the journal loop is not the WAL cause.
- A sequential recheck of `temptable2` on the restored tree showed only the
  two pre-existing leaves (4.1.4, 5.1.2); the earlier temp mismatch came
  from running two `go test` processes concurrently in one worktree. The
  full gate still runs one process and will be the final arbiter.
- A pure-Go `:memory:` pin of `temptable2-4.1.1` is GREEN on the S64 tree.

Chosen WAL fix: capture the WAL frame count and checksum chain when the
connection first acquires the WRITER lock (its real write-savepoint
boundary, C's `sqlite3WalSavepoint`), and have `Pager.Restore` rewind only
to that captured point. A read-only close has no writer lock and therefore
rewinds nothing, while a rolled-back write still removes only its own
uncommitted frames. The existing journal finalization and close rollback
remain in place; no test is skipped or weakened.

## Verify

- Pins GREEN after fix; affected family gates (`pager1`, `journal`,
  `rollback`, `savepoint`, `trans`, `pragma2`, `sharedA`, `e_walckpt`) have no
  NEW/GONE leaf sets versus `/tmp/t28_full_gate.log`.
- `wal3` and `temptable2` focused gates have no NEW/GONE leaves versus
  `/tmp/t28_full_gate.log`.
- `go test ./internal/... -count=1`.
- Quality normalized diff vs `/tmp/t28_quality2.log` NEW=0.
- Detached full gate; raw-byte leaf diff vs `/tmp/t28_full_gate.log` and
  durable `/tmp/t10_full2.log`, NEW=0.

## Final verification

- Code commit: `6d3ba760e`; pre-commit build/SOLID checks passed.
- Pins GREEN (`/tmp/t29_fix2_pins.log`) including the read-only WAL-close pin.
- Sequential family gates (`wal3`, `temptable2`, `pager1`, `journal`,
  `rollback`, `savepoint`, `trans`, `pragma2`, `sharedA`, `e_walckpt`):
  NEW=0/GONE=0 versus t28 leaf sets.
- `go test ./internal/... -count=1` green (`/tmp/t29_fix_internal.log`).
- Quality normalized NEW=0/GONE=0 (`/tmp/t29_fix2_quality.log`).
- Detached full gate `/tmp/t29_full_gate2.log`: 1219 leaves, NEW=0 vs t28 and
  t10; sole GONE vs t28 is `resetdb/resetdb-300` (known load flap).

## Risks

- Calling `EndRollbackJournal` on every attached pager is harmless when no
  journal file exists, but it can fire xClose/xDelete VFS hook events. The
  affected family gates and full gate must confirm no hook-sequence regressions.
- `Engine.Close` now fires the rollback hook for an open transaction (C does
  too); any test harness that assumed silence on close must be revisited only
  if the full gate shows a NEW failure.
- WAL-mode close keeps its existing writer-release path; this fix targets the
  non-WAL journal sidecar only.
