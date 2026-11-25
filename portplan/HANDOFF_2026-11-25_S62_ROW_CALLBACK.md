# Session handoff — S62: interleaved row callback (2026-11-25, 2ea4d5a36)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code/fixture committed at `2ea4d5a36`; this docs commit
records the verified state and opens the next unit.

## Landed

- Four leaves GONE: `interrupt2`, `interrupt2-2.1`, `wal6`, `wal6-5.2`.
- `tools/tclconvert/tcl/interp_sql.go`: `hasInterleavedRowCallback` detects
  `db eval SQL BODY` whose body is not the existing first-column collection;
  `cmdDoTest` emits a single `row_callback` step with the full TCL body and
  do_test expectation. Complexity offsets (`finishStepResetResult`,
  `isRuntimeCapturedVar`, branchless body selection) preserve the exact
  normalized quality baseline.
- `frigolite_row_callback_test.go`: a runtime `tcl.Interp` executes the real
  body; `db eval` is overridden to prepare/step one row at a time, bind row
  columns into TCL variables, and run the callback body between steps. Live
  `catch`, `catchsql`, `sqlite3_extended_errcode`, and
  `sqlite3_wal_checkpoint_v2` are registered over the harness connections.
  `tcl.Interp.Result()` and `ExecuteStrict()` were added in `interp.go`.
- Engine WAL semantics:
  - `WriteBlockedByPreparedRead` exempts WAL-mode DML — readers do not block
    writers (pager.c WRITER shm lock is the only writer serialization).
  - `WALWriterProbe(checkSnapshot bool)` compares the live wal-index header to
    this connection's read mark when it already pinned a snapshot.
  - `pager.ErrWalBusySnapshot` maps through `frigolite_error.go` to
    `SQLITE_BUSY_SNAPSHOT`.
- Native pins: `frigolite_row_callback_t27_pin_test.go` covers the
  stale-snapshot `BEGIN EXCLUSIVE` contract and the interrupted checkpoint
  contract independently of the converter.
- Converter unit: `tools/tclconvert/tcl/row_callback_test.go`.

## Verification

- Pins PASS (`/tmp/t27_pins_final.log`).
- Focused gates 0 leaves (`/tmp/t27_interrupt2_focus_final2.log`,
  `/tmp/t27_wal6_focus_final2.log`).
- `go test ./internal/... -count=1` green (`/tmp/t27_internal_final.log`).
- Build/vet/SOLID green (`/tmp/t27_build.log`, `/tmp/t27_vet.log`,
  `/tmp/t27_solid.log`).
- Converter failure-name set 8/8, NEW=0/GONE=0
  (`/tmp/t27_tclconvert_all2.log` vs `/tmp/head_tclconvert_all.log`).
- Quality normalized 659/659, NEW=0/GONE=0
  (`/tmp/t27_quality2.log` vs `/tmp/t26_quality2.log`). Overall exit 1 is the
  pre-existing hard file-size gate.
- Full gate `/tmp/t27_full_gate.log` (1208.032s, 1222 leaves): raw-byte leaf
  diff vs `/tmp/t26_full_gate.log` NEW=0, GONE=the four target leaves; vs
  durable `/tmp/t10_full2.log` NEW=0, GONE=129; vs `/tmp/t25_full_gate.log`
  NEW=1, only documented `resetdb/resetdb-300`.

## Restart point / next unit

Next unit: pager1 4.4.2 directory-hierarchy machinery / S34's two journal
cards. Then temptable2-9.1.x/9.2.x and where2-2.4 watchlists. Read `TODO.md`
first; use raw-byte leaf-name fail sets and a pure-Go pin before
converter/engine edits.

## Risks / working rules

- Run the real TCL body between real row steps; never replace it with a
  hand-coded expected result. A missing TCL command needs a converter/runtime
  port, not a special case for one test.
- WAL readers do not block writers; only the writer's own stale snapshot fails
  the upgrade. Keep the snapshot check gated on this connection's read mark.
- `canonicalRowCallbackResult` normalizes only the documented nested-catch
  `msg` reuse; do not broaden it to hide arbitrary error text.
- Regenerate only target families (`-only <family>` into `/tmp`, copy back the
  single JSON). A full converter run rewrites and recompresses the corpus.
- Keep full gates detached with `-timeout 30m` and status files; compare raw
  fail sets, never counts or line greps. Use the wait time for investigation
  and non-conflicting checks, never engine edits.
