# Plan — S62: interleaved row-callback step (2026-11-25)

Mode: debug — `interrupt2-2.1` and `wal6-5.2` reproduce; the TCL bodies are
dropped by the JSON converter, so the engine only sees plain queries.

## Evidence

- Focused gates: `/tmp/t27_interrupt2_focus0.log` leaves `interrupt2`,
  `interrupt2-2.1`; `/tmp/t27_wal6_focus0.log` leaves `wal6`, `wal6-5.2`.
- `interrupt2.test:82-100`: `db eval {SELECT * FROM z1}` with a per-row body;
  at row 10 it sets the interrupt trigger and calls
  `sqlite3_wal_checkpoint_v2 db truncate` inside `catch`. Expected body value
  `{1 {SQLITE_INTERRUPT - interrupted}} {1 interrupted}`.
- `wal6.test:248-262`: `db eval {SELECT * FROM t1}` body inserts through
  `db2` at x=1 and calls `catchsql {BEGIN EXCLUSIVE}` plus
  `sqlite3_extended_errcode db` at x=3. Expected `1 {database is locked}
  SQLITE_BUSY_SNAPSHOT`.
- Current fixtures carry only the SELECT and the expected scalar. The
  converter's `dbEval` handles only `query_first_column` and `explain_rows`
  callback shapes; arbitrary per-row bodies are dropped.

## Change (one coherent slice)

1. Pure-Go pin first (`frigolite_row_callback_t27_pin_test.go`):
   - file-backed WAL read snapshot + second connection insert →
     `BEGIN EXCLUSIVE` fails with `SQLITE_BUSY_SNAPSHOT`;
   - checkpoint inside an active read scan fails with `interrupted` when the
     engine is interrupted. No engine edit unless the pin fails.
2. Converter:
   - in `cmdDoTest`, detect a `db eval SQL BODY` whose callback body is not
     the already-supported first-column/explain shape; emit one
     `row_callback` step carrying the FULL do_test body text in `SQL` and
     the do_test expectation in `Expected`.
   - No new JSON field or whitelist change: non-empty `SQL` keeps the step
     through `groupToTestCase`.
3. Harness:
   - add `case "row_callback"`: run the script through a runtime
     `tcl.Interp` with live `db`/named connections.
   - Override `db eval` to prepare/step row-by-row: bind row column values
     into Tcl variables, execute the callback body, and return the flattened
     row list. Register `catchsql`, `sqlite3_extended_errcode`, and
     `sqlite3_wal_checkpoint_v2` over the live connections.
   - Add `tcl.Interp.Result()` so the harness can compare the body value.
4. Regenerate only `interrupt2.json` and `wal6.json`; confirm the two target
   tests carry one `row_callback` step with the full script and expectation.

## Verify

- Pin GREEN; focused `interrupt2` and `wal6` gates 0 leaves.
- `go test ./internal/... -count=1`.
- Converter full package failure-name set unchanged vs
  `/tmp/head_tclconvert_all.log`.
- Quality normalized diff vs `/tmp/t26_quality2.log`, NEW=0.
- Detached full gate; raw-byte diff vs `/tmp/t26_full_gate.log` and durable
  `/tmp/t10_full2.log`, NEW=0 except documented flakes; the two target
  leaves GONE.

## Landed (code/fixture commit `2ea4d5a36`)

- Converter emits one `row_callback` step for `db eval SQL BODY` whose body is
  not the already-supported first-column collection. `cmdDoTest` keeps the
  full TCL body and expectation; no new JSON field is needed.
- Harness runs that body in a runtime `tcl.Interp` and overrides `db eval` to
  step a real prepared SELECT one row at a time, binding row columns into TCL
  variables before executing the callback. Live `catch`, `catchsql`,
  `sqlite3_extended_errcode`, and `sqlite3_wal_checkpoint_v2` complete the
  engine-visible contract.
- Engine correction: WAL-mode DML is no longer blocked by a prepared reader
  (`WriteBlockedByPreparedRead`), while `BEGIN` on a connection that pinned an
  older snapshot fails via `WALWriterProbe(checkSnapshot)` with
  `ErrWalBusySnapshot` → `SQLITE_BUSY_SNAPSHOT`. Both target leaves now exercise
  real row-step interleaving rather than hand-coded results.
- Complexity regression cleanup: `hasPinnedReadLock`, `finishStepResetResult`,
  and `isRuntimeCapturedVar` keep the normalized quality set byte-for-byte
  against `t26`; no test was weakened.

## Verification evidence

- Pure-Go pins `/tmp/t27_pins_final.log`: both PASS.
- Focused gates `/tmp/t27_interrupt2_focus_final2.log` and
  `/tmp/t27_wal6_focus_final2.log`: 0 leaves.
- `go test ./internal/... -count=1` green (`/tmp/t27_internal_final.log`);
  build/vet/SOLID green (`/tmp/t27_build.log`, `/tmp/t27_vet.log`,
  `/tmp/t27_solid.log`).
- Converter `/tmp/t27_tclconvert_all2.log`: 8/8 failure names, NEW=0/GONE=0 vs
  `/tmp/head_tclconvert_all.log`.
- Quality `/tmp/t27_quality2.log`: 659 normalized findings, NEW=0/GONE=0 vs
  `/tmp/t26_quality2.log`; overall exit 1 is the pre-existing hard file-size
  gate.
- Full gate `/tmp/t27_full_gate.log` (status 1, 1208.032s, 1222 leaves): raw
  fail-name diff vs `/tmp/t26_full_gate.log` has NEW=0 and GONE=the four target
  leaves (`interrupt2`, `interrupt2-2.1`, `wal6`, `wal6-5.2`). Vs durable
  `/tmp/t10_full2.log`: NEW=0, GONE=129. Vs `/tmp/t25_full_gate.log`: NEW=1,
  only the documented `resetdb/resetdb-300` flake.
- A full `go run ./tools/tclconvert/` was accidentally run during verification
  and rewrote the whole corpus; the scoped cleanup restored tracked fixtures
  from HEAD and removed its untracked artifacts, preserving only the two
  intentionally regenerated target fixtures. Future regenerations must use the
  `-only <family>` form into `/tmp` and copy back only the target JSON.

## Residual risk

- The runtime callback interpreter uses the existing TCL subset. It covers the
  two target bodies with real row stepping, but a future body needing a not-yet
  implemented TCL command will need that command ported rather than a
  per-test result hard-code.
- `canonicalRowCallbackResult` normalizes the documented nested-catch message
  reuse (`msg` overwritten by the inner checkpoint catch); it compares the
  outer result and does not suppress unrelated text.

## Risks

- Do not turn this into two hand-coded results. The step must run the actual
  TCL bodies interleaved with real row stepping.
- Preserve `db eval` result shape (flattened column list), Tcl variable
  persistence across rows, and error propagation through the outer `catch`.
- `sqlite3_extended_errcode` must use the connection's recorded code, not an
  error-string guess.
