# HANDOFF — 2026-11-25 — Concurrent DML / hot-journal race

## Status

COMPLETE. The two native thread-concurrency contract tests now pass; the
fix is a rollback-journal lock/recovery parity change, not a WAL-path change.

## Root cause (instrumented, not inferred)

Temporary stack-gated probes (`FRIGOLITE_TRACE_GATE`) at every candidate
CANTOPEN/READONLY gate produced one exact failure path:

- `TestNativeThreadReaderDuringWrites`: reader CANTOPEN came from
  `internal/pager/journal.go` `errCantOpenJournal`. `hotJournalProbe` read a
  journal that existed at probe time; a concurrent writer committed and
  unlinked it before the read/write reopen. `os.OpenFile` returned
  `ENOENT`, which Frigolite mapped unconditionally to SQLITE_CANTOPEN.
- Writer "attempt to write a readonly database" was downstream: after the
  reader's `t.Fatalf`, the test's deferred `Close` + TempDir cleanup removed
  the database file under the still-running writer, so
  `openRollbackJournalLocked`'s `databaseFileMoved` guard fired.
- `TestNativeThreadConcurrentWritersSerialize` failed on the same
  `errCantOpenJournal` path: two autocommit writers had no cross-connection
  write mark, so both used the same rollback-journal path and a later
  statement could probe a journal the other had just deleted.
- None of the suspected WAL gates (`walOpenRefused`, `WALAttachRefusal`,
  `SetShmUnavailable`) fired.

## Fix

1. `internal/pager/journal.go`: a journal that vanishes between the probe
   and the read/write reopen is a C ticket-#3883 false-positive
   (`src/pager.c hasHotJournal`): skip playback and return nil. Genuine
   open failures (unreadable journal) keep SQLITE_CANTOPEN.
2. `internal/exec/engine_core.go` + `internal/exec/locks.go`: an outermost
   autocommit write to a rollback-journal database acquires the registry's
   write-transaction mark for the statement, mirroring C's RESERVED lock held
   from first page write to implicit commit. The mark makes writers serialize
   and makes `RecoverHotJournalIfNeeded` skip a live journal. WAL writes are
   exempt — C serializes those through the `-shm` WRITER byte.
3. `internal/lockreg/lockreg.go`: `TrySetWriteTx` atomically checks and sets
   the write marks on all attached files. This closes the check-then-set
   window where two writers both passed `CrossConnLockError`; a loser gets
   clean `database is locked`, not a partial mark.

## Verification

- Repro before fix: `TestNativeThreadConcurrentWritersSerialize` and
  `TestNativeThreadReaderDuringWrites` failed on the paths above
  (`/tmp/conc_repro_initial.log`, `/tmp/trace_reader2.log`,
  `/tmp/trace_writers.log`).
- After fix: both tests plus `TestWalLockWriterSerialization` pass with
  `-count=20` (`/tmp/conc_and_wal_20.log`); the goal verify command passes.
- Full JSON harness: 12 shards (`/tmp/conc2_shard_*.log`) merged leaf fail
  names vs `/tmp/t30_baseline_leaves.txt`: NEW=0, GONE=4 (the documented
  baseline-flake/density supersessions).
- `go test -run TestSOLID_ ./...`: green.
- `go test -skip '^TestSQLiteSuite$' -count=1 ./...`: remaining root failures
  are the pre-existing Review TODO items (WAL MVCC/lock protocol,
  locking_mode, harness `$t$tcl`, pending-byte test) plus the nine documented
  tclconvert unit-test drifts; no new failures from this fix.
- `tools/quality_gate.sh` on the four changed files reports only the known
  repo-wide baseline debt (staticcheck findings, pre-existing complexity in
  `Exec`/`CrossConnLockError`/`hotJournalProbe`, and the two >1000-line
  files). The new registry helpers are below the thresholds.

## Residual risk

Frigolite still uses an in-process lock registry rather than OS file locks.
An EXTERNAL process deleting the journal after our successful reopen but
before playback remains outside this fix; C closes that window with an
EXCLUSIVE POSIX lock. The in-process race that caused both native failures is
closed by the statement-scoped write mark.
