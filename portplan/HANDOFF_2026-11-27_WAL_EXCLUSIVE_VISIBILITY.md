# HANDOFF — 2026-11-27 — WAL exclusive-mode SELECT visibility

## Status

COMPLETE pending the final full-harness name-set diff. Review TODO item 2
is resolved in `TODO.md`.

## Reproduction and diagnosis

Pre-fix, `go test -run '^TestWalMVCCExclusiveModeVisibility$' -count=1 .`
failed at `frigolite_walmvcc_test.go:45`: c2's
`SELECT count(*) FROM t1` returned `database is locked` after c1 had set
`PRAGMA locking_mode=EXCLUSIVE` and inserted a row. The same failure existed
at HEAD `5b5472021`.

A temporary diagnostic test (removed before commit) dumped the lock registry
at each step:

```
after open:    c1 heap=false wal=true, c2 heap=false wal=true, excl=false walshared=true
after pragma:  c1 heap=false wal=true, c2 heap=false wal=true, excl=false walshared=true
after insert:  c1 heap=false wal=true, c2 heap=false wal=true, excl=true  walshared=true
after c2 read: c2 err=database is locked
```

So the WAL was in the NORMAL (shared) wal-index form, not the heap/no-shm
form. `Engine.walBeginStmtWrite` (old `internal/exec/locks.go:337`) took the
process-global EXCLUSIVE file hold on c1's first insert; c2's later SELECT
was refused by `crossConnFineGrainedCheck`'s `ExclusiveLockedByOther`.

The sqlite3 3.54 oracle (`python3 sqlite3`, two connections, WAL set before
locking_mode) also returns `database is locked` for c2, i.e. C's file lock
excludes the late reader. This unit pins Frigolite's shared-wal-index
adaptation instead: a peer already attached to the index keeps reading the
committed frames while the writer's WAL shm locks are no-ops. The fixture
side (e_wal 2.2.2/2.3.2/3.2.2/3.2.5) still requires the file hold for a
connection opened after the exclusive write, and requires a fresh
acquisition to fail when a peer holds the WAL-file-SHARED mark.

Diagnostic probe of the fixture analogue (`PRAGMA locking_mode=EXCLUSIVE;
PRAGMA locking_mode=NORMAL; PRAGMA locking_mode=EXCLUSIVE; INSERT`) showed
the same heap=false/excl=true state; the discriminator is attachment timing,
not the pragma sequence: in the harness the second connection is opened
*after* the exclusive insert, while `openWalPair` opens it *before*.

## Fix

1. `internal/pager/wallocks.go` — two exported read-only seams:
   - `WALIndexShared()` reports `WALIndex.RefCount() > 1`, the same
     sole-connection proof already used by close-checkpoint, WAL→DELETE and
     heap conversion.
   - `WALExclusiveActive()` reports `walWriter.exclusiveMode` (C's
     WAL_EXCLUSIVE_MODE), true from the first exclusive-mode write until
     locking_mode=normal.
2. `internal/exec/wallocks_exclusive.go` (new) — `walExclusiveFileLock`
   extracts the registry gate out of `walBeginStmtWrite` (restoring its
   pre-change gocognit/gocyclo baseline):
   - a fresh exclusive acquisition still reports `database is locked` when
     another connection holds EXCLUSIVE, WAL-file-SHARED or a shared read
     transaction (e_wal-3.2.5);
   - a later write by an already-active exclusive WAL writer is allowed
     despite the peer's WAL-file-SHARED mark;
   - the global EXCLUSIVE file hold is taken only when no other connection
     is attached to the wal-index. A peer opened after the first exclusive
     commit still sees the hold and is blocked (e_wal 2.2.2/3.2.2).
3. `frigolite_walmvcc_test.go` — `TestWalMVCCExclusiveLatePeerBlocked`
   pins the fixture-side boundary so future changes cannot silently remove
   it while fixing the visibility case.

No shm lock, frame lookup, read-mark or checkpoint logic was changed; WAL
writes remain serialized on the wal-index mutex.

## Verification

- `go test -run '^TestWalMVCCExclusiveModeVisibility$' -count=1 .`: PASS
  (counts 1 and 3 on both connections; `integrity_check=ok`).
- `go test -run '^TestWalMVCCExclusive' -count=1 .`: PASS (visibility plus
  the new late-peer pin).
- `FRIGOLITE_TEST=e_wal go test -run '^TestSQLiteSuite$' -count=1 .`: PASS.
- `go test -run TestSOLID_ ./...`: PASS.
- `go vet ./internal/exec/... ./internal/pager/...`: clean.
- `tools/quality_gate.sh internal/exec/locks.go internal/exec/wallocks_exclusive.go
  internal/pager/wallocks.go`: unchanged repo-wide baseline debt only
  (staticcheck U1000/S1031/SA4014 findings, pre-existing `CrossConnLockError`
  complexity, and `locks.go` >1000 lines); `walBeginStmtWrite` is back to its
  HEAD gocognit 24 / gocyclo 18.
- Full harness: 12 shards (`/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.uDfLf9/shard-*.log`)
  completed (all 12 status files present; no timeout). Leaf FAIL-name set
  diffed against `/tmp/t30_full_gate.log`: NEW=0, GONE=6
  (`resetdb-100/300` flakes, the `temptable2` family-level + two leaves that
  passed this run, `vacuum-into-500` flake). A single unsharded run timed out
  at 25m inside `vacuummem/1.0` — a pre-existing slow family, not a failure
  signature; the shards each finished under their 10m default.
- `go test -skip '^TestSQLiteSuite$' -count=1 ./...`: only the documented
  pre-existing native/tool failures remain; no new failure from this fix.

## Residual risk

Frigolite uses an in-process lock registry rather than POSIX file locks. A
separate process opening the same WAL file while this connection is in
shared-index exclusive mode is outside the registry's view; the file hold is
intentionally omitted in that mode so an already-attached peer can read.
External-process exclusivity remains governed by the OS locks the pager
takes elsewhere, not by this gate.
