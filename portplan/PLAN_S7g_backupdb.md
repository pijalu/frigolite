# PLAN S7g — the `sqlite3_backup` command object (walbak-3.x)

## Failure class

All 8 walbak-3.x failures are **converter artifacts**, one root cause:
walbak-3.$tn.2/.4 use test_backup.c's command object
(`sqlite3_backup B db2 main db main` + `B step 10000` + `B finish`),
which the converter silently dropped (unknown command). The setups
survived (setup_2..5: db2 opens `:memory:` / `""` / `test.db2` with the
page-size pragmas and 3.4's pre-existing `xx` table), so every 3.x.2
SELECT / 3.x.3 INSERT on db2 fails `no such table: t1` — the backup that
should have copied t1 never ran.

## C contract (test_backup.c:32-150)

- `sqlite3_backup CMDNAME DESTHANDLE DESTNAME SRCHANDLE SRCNAME` —
  sqlite3_backup_init; **copies nothing**; creates CMDNAME.
- Methods: `step npage` (returns the errname), `finish` (releases and
  **deletes the command**), `remaining`, `pagecount` (ints).
- backup.c:378-383: SQLITE_READONLY only for a WAL/memdb destination with
  a page-size mismatch. 3.4's mismatched destination (2048, rollback,
  non-empty) is allowed; the copy is a raw page-image with
  truncate-round-up (backup.c:449-457). frigolite's logical copy
  (frigolite_backup.go NewBackup/Finish, operates on LIVE connections)
  satisfies the engine-visible contract: db2 must hold t1 and keep
  working afterward.
- backup.c's page-1 image carries the source's version bytes: a WAL
  source leaves a file-backed rollback destination reporting WAL
  (walbak-3.x.7's `{wal}` on db2 — subtests not in the fixture, but the
  parity is engine-visible). Mirror with SetJournalMode("wal") on the
  live file-backed destination when the source pager is in WAL mode
  (same parity the S7f backup_file unit applies to fresh destinations).

## Changes

1. `tools/tclconvert/tcl/interp.go` — Stmt fields `DestDB`, `SrcConn`,
   `SrcDB` (Database already routes the destination connection); Interp
   field `backupHandles map[string]backupHandleInfo`.
2. `tools/tclconvert/tcl/interp_stmt.go` — `commandSqlite3Backup` (5
   args; stores the handle; emits nothing).
3. `tools/tclconvert/tcl/interp_cmds.go` — register the command; the
   named-object fallthrough (before `i.procs[cmd]`, like
   `i.testvfsNames[cmd]`) dispatches the handle's methods:
   `step N` emits one `backup_db` step (the harness copy runs to
   completion — C's step returns DONE once the budget covers the source);
   `finish` emits nothing (C copies nothing at finish) and deletes the
   handle; `remaining`/`pagecount` report no emission (no converted
   script consumes them).
4. `tools/tclconvert/main.go` — TestStep mirror fields + the `backup_db`
   whitelist entry.
5. `frigolite_harness_test.go` — `case "backup_db"` beside `backup_file`:
   resolve dest/src connections ("db" → the main executor, else
   namedDBs), `srcConn.NewBackup(dstConn, step.DestDB, step.SrcDB)` +
   `Finish()`, and the WAL-source → file-backed-dest SetJournalMode("wal")
   parity guarded by `Pager.IsMemory()`.
6. Regenerate `testdata/walbak.json`.

## Scope

Only walbak.json is regenerated. The same dropped-step artifact exists
wherever backup*.test/pagerfault.test/multiplex3.test used the construct
(converter now supports it; regenerating those fixtures is their own
unit — census note).

## Verification

- `go test ./tools/tclconvert/...` — green, incl. the two new emission
  tests (TestSQLite3BackupCommandObjectEmitsBackupDB,
  TestSQLite3BackupHandleSurvivesAcrossSteps).
- `FRIGOLITE_TEST=walbak` — **9 → 0** (96 subtests run, zero FAIL/SKIP):
  the 8 walbak-3.x "no such table: t1" failures fixed, and the 9th
  baseline failure (a walbak-2.x body whose dropped `B step` block made
  the do_test value empty) now honestly asserts and passes — the
  regeneration recovered 16 backup_db steps: 3.x's four `B step 10000` +
  2.x's step/finish pairs with return-code expectations
  (walbak-2.6/2.10's SQLITE_BUSY asserted against a real busy gate: the
  source's open write transaction).
- Engine: the existing `Backup.Step` (frigolite_backup_step.go: page
  budget + setDestPgsz + restart rule) serves the runner unchanged; the
  one engine change is Finish's BUSY guard (backup.c:599-603 — finish
  rolls back, never copies, returns the last step's code) so a finish
  after a BUSY step does not read the source's uncommitted state.
- `FRIGOLITE_TEST=wal` sweep: **138 failures = 147 − 9** (the walbak block
  this unit fixed; zero new). wal-12.4 absent (the leak fix), walfault-11
  present (874.71s, the carded faultsim 512-trial-cap residue). The
  failure-name list is the new baseline:
  portplan/baseline_wal_sweep_2026-11-07_S7g.txt.
- Commit; TODO.md / lessons_learned.md records.
