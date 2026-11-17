# PLAN S35d — walckptnoop (B8): 7 leaves, ~150-250 LOC

Unit S35d: the `PRAGMA wal_checkpoint = noop` surface + the fixture-UDF round
trip. Extends TRIAGE_wal_residue_2026-11-16.md §B8. Branches off feature/perf
AFTER the s35-wal-small merge lands.

## C contract (3.51 oracle tree, verified 2026-11-18)

- `SQLITE_CHECKPOINT_NOOP = -1` (sqlite.h.in:10060) — parses from
  pragma.c:2391 (`else if( sqlite3StrICmp(zRight,"noop")==0 )`). C's ordering
  NOOP(-1) < PASSIVE(0) is load-bearing: `eMode<=PASSIVE` passes xBusyHandler=0
  (pager.c sqlite3PagerCheckpoint's argument choice).
- **sqlite3WalCheckpoint (wal.c:4300-4373)**: NOOP skips BOTH the exclusive
  CKPT-lock block (`if( eMode!=SQLITE_CHECKPOINT_NOOP )`, wal.c:4318) and the
  walCheckpoint() run (`else if( eMode2!=SQLITE_CHECKPOINT_NOOP )`,
  wal.c:4370) — but STILL runs walIndexReadHdr (recovery/adopt) and, on
  rc==OK|BUSY, fills `*pnLog = hdr.mxFrame` and
  `*pnCkpt = walCkptInfo()->nBackfill` (wal.c:4372-4377). So NOOP reports
  {0, mxFrame, nBackfill} WITHOUT touching the log. Fixture pin: walckptnoop
  -1.1 wants {0 298 0} — mxFrame 298, nBackfill 0.
- **The in-txn veto (btree.c sqlite3BtreeCheckpoint)**:
  `if( pBt->inTransaction!=TRANS_NONE ) rc = SQLITE_LOCKED;` — BEFORE the
  pager call, EVERY mode (NOOP included). vdbe.c OP_Checkpoint maps
  rc!=OK && rc!=BUSY → abort_due_to_error, so the statement fails with C's
  SQLITE_LOCKED text "database table is locked" (1.7's catch want).
- The {0 -1 -1} no-WAL answer (pPager->pWal==0) is already ported
  (pragma_state.go WalCheckpoint's ctx==nil branch).

## Our divergence (probed 2026-11-18)

- internal/exec/pragma_state.go WalCheckpoint: the mode switch maps
  full/restart/truncate; unknown (incl. "noop") DEFAULTS to WalCkptPassive —
  a passive checkpoint RUNS (wrong numbers + wrong side effects).
- internal/pager/wal.go WalCheckpointMode: enum {Passive 0, Full 1, Restart
  2, Truncate 3} — no NOOP; checkpoint() unconditionally takes locks/runs.
- The in-txn veto: the engine's WalCheckpoint has no inTransaction check (the
  S7c veto exists on the API path only — internal/exec/hooks.go).
- walckptnoop-1.0 (the harness class): `db func myrandomblob myrandomblob` —
  the fixture registers a TCL proc as a UDF; the harness registers it without
  the fixture-interp proc round trip, so the body's `[expr % 256]` TCL leaks
  as SQL ("unexpected character '%'") — the S25 push/pull harnessTclFunction
  pattern is the fix; 1.0's cascade takes 1.1-1.5 with it (t1 gets its rows).

## Delivery

1. **Engine — the NOOP mode** (internal/pager/wal.go + pagerwal.go):
   - Add `WalCkptNoop WalCheckpointMode = -1` (C's ordering: NOOP < PASSIVE;
     the enum values stay C's).
   - `(*Pager) CheckpointMode` / `(*walWriter) checkpoint`: NOOP skips the
     lock acquisition AND the passes — but still refreshes the wal-index
     header (the walIndexReadHdr equivalent: walInitHeader/adopt path) and
     reports {0, hdr.MxFrame, ckptInfo.nBackfill}. No dirty-page work, no
     frame copy, no -wal truncation.
2. **Engine — the in-txn veto** (internal/exec/pragma_state.go WalCheckpoint):
   before calling CheckpointMode, refuse when the database context's btree is
   in a transaction (any mode incl. NOOP) with C's SQLITE_LOCKED text
   "database table is locked" — statement-failing (the catch path), matching
   vdbe.c's abort_due_to_error. Use the same inTransaction state the S7c API
   veto reads (hooks.go) — one source of truth, no duplicate txn tracking.
3. **Pragma parse**: the mode switch gains "noop" (case-insensitive,
   ToLower-trimmed like its siblings).
4. **Harness — the UDF round trip** (walckptnoop-1.0): extend the
   register_function handler with the S25 harnessTclFunction push/pull — the
   fixture proc body registers as a runtime callback UDF (arg-count honored,
   TCL body evaluated in the fixture interp via the callback runtime), NOT
   as SQL. Check S25's existing register_function coverage first: if the
   round trip exists but rejects proc bodies (non-SQL), extend it; do not
   duplicate machinery.
5. Tests: pure-Go probe for the NOOP report triple ({0 mxFrame nBackfill}
   with a live WAL; no -wal shrink; a second NOOP returns identical numbers)
   and the in-txn veto text; then the family sweep.

## Verify

- FRIGOLITE_TEST=walckptnoop family sweep — 7→0, python raw byte scan
  name-set diff vs the pre-fix set {1.0,1.1,1.2,1.3,1.4,1.5,1.7}.
- wal/wal2/walvfs/walckptnoop/e_walckpt identity sweeps (the mode enum change
  touches every checkpoint caller — FULL family universe: e_walckpt,
  walckpt, notify1, snapshot*).
- go test -run 'TestSOLID_' ./... green; go build ./... clean.
- Full gate at the unit merge (coordinator).

## Baseline (worktree 0b932e20e, the s35 merge)

walckptnoop {1.0,1.1,1.2,1.3,1.4,1.5,1.7} = 7 leaves (the 1268-baseline
subset).
