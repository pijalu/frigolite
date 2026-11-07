# PLAN S8 — the checkpoint copyback interrupt check (wal.c:2289-2292) + the regen-drift battery

## Regeneration triage (queue item 3)

- **interrupt2.json** — regenerated; the converter now captures the
  testvfs surface (register_testvfs/filter/script + write_cb's body +
  runtime_expr per-trial arms + 4 wal_checkpoint_v2 steps). Unskipped it
  from unsupportedTestFiles (the entry predates the testvfs/interrupt
  surface; the tvfs hook registers `sqlite3_interrupt` already).
- **e_walauto.test** — its core reads the -shm bytes directly
  (read_nbackfill/read_mxframe binary scans + a `while {1}` threshold
  loop): not expressible as engine steps. Carded unsupported; the
  regenerated fixture reverted (the old one was internally broken:
  INSERTs without the CREATE).
- **wal5.test** — the converter captures nothing ("no tests captured";
  lock_common.tcl primitives). The old green 2-exec fixture stands;
  carded converter residue.

## interrupt2's 8 failures → ONE engine gap

The xWrite hook fires per copyback page (wal.go:1106, walvfs-3.1's
surface) and the pragma path surfaces the interrupt flag post-hoc
(pragma_state.go:421/437) — but the copyback loop itself never checks
the flag, so it RUNS TO COMPLETION despite the interrupt (C aborts at
wal.c:2289-2292 per page). Consequences: the WAL drains fully (1.x.1's
later trials 10/15/20 never see the interrupt — "expected error but got
success"), the 1.x.3 truncate trials see an empty WAL ({0 0 0}), and
2.1's reader-loop trial degenerates (plus its own converter artifact:
the per-row callback body was dropped).

## Changes (C-fundamental: wal.c:2289-2292)

1. `internal/pager` — `Pager.interruptProbe func() bool` +
   `SetInterruptProbe` (mirrors SetBusyHandler; installed once, read
   without the mutex like vfsHook) + `interruptFired()` + the sentinel
   `ErrCheckpointInterrupted`. `backfillFrameRangeLocked` checks the
   flag at the LOOP HEAD per page and returns the sentinel — the szDb
   truncate and the nBackfill advance are skipped (the err gates both,
   wal.c:2308).
2. `internal/exec` — bind the probe at NewEngine (main pager) and
   AppendDBList (ATTACHed pagers): `pg.SetInterruptProbe(e.IsInterrupted)`.
   `Engine.WalCheckpoint` maps the sentinel to the "interrupted" Result
   error in both the NULL-zDb loop and the single-schema path (the
   post-check stays: a non-copyback interrupt still surfaces).
3. Native pin: TestCheckpointInterruptPin — the hook interrupts at the
   5th copyback write; the pragma fails "interrupted"; a clean
   checkpoint then backfills from nBackfill=0 (all 36 pages again); a
   TRUNCATE on a drained WAL reports {0 0 0}.

## Verification (results)

- Native pin `TestCheckpointInterruptPin` — green: the 5th copyback write
  interrupts the pragma ("interrupted"), the clean checkpoint re-backfills
  from the unadvanced mark, and a TRUNCATE on a drained log reports
  {0 0 0} with zero copyback writes.
- interrupt2: **36/37 pass** (was 27/35 pre-fix). The whole 1.x block
  (interrupt-during-checkpoint for delays 5/10/15/20, both the pragma and
  the checkpoint_v2 catch shape "SQLITE_INTERRUPT - interrupted") and
  section 3 (the db_save/db_restore snapshot rounds) pass.
- Residue (converter artifacts, carded): interrupt2-2.1 — the per-row
  `db eval` callback that arms the interrupt at row 990 and interleaves a
  checkpoint mid-scan needs an interleaved row-callback step surface (the
  fixture dropped the body).
- walvfs: the 9 failures are exactly the S7g baseline set (zero
  regressions from the copyback behavior change).
- wal sweep vs the 138 baseline: **136 = 138 − 2 (e_walauto's broken
  fixture left via the unsupported card), zero new** (normalized
  name-set diff). New baseline:
  portplan/baseline_wal_sweep_2026-11-07_S8.txt. interrupt2 does not
  match the "wal" substring — its 36/37 runs under the interrupt2
  filter.
