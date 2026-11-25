# Plan — S60: quote DQS dbconfig fixture support (2026-11-25)

Mode: debug — seven `quote-2.*` leaves are reproduced; the engine already has the
correct DQS contract, so the gap is in the JSON converter/harness path.

## Evidence

- Focused `/tmp/t25_quote_focus0.log`: leaves = `quote`, `quote-2.1.1`,
  `2.1.2`, `2.1.3`, `2.1.4`, `2.2`, `2.5`.
- SQLite `quote.test:95-96` sets `SQLITE_DBCONFIG_DQS_DDL 0` and
  `SQLITE_DBCONFIG_DQS_DML 1` immediately before section 2. `testdata/quote.json`
  contains no step for either command; the engine therefore runs section 2
  with its default DQS settings (DDL=true) and accepts the double-quoted
  strings.
- Direct pure-Go probe with `db.SetDQS(false, true)` produced all four required
  errors verbatim, so no engine fix is needed.
- `tools/tcl2go/processmisc.go:16-68` already handles these two dbconfig flags
  and emits `db.SetDQS(false, true)`; `testgen/quote/quote_test.go:238-239`
  contains both calls. `tools/tclconvert/tcl/interp_cmds.go:2293-2298` only
  handles `DEFENSIVE` and silently drops DQS flags.

## Change (one coherent slice)

1. **Pin (engine contract).** Add `frigolite_quote_dqs_t25_pin_test.go`:
   `db.SetDQS(false, true)`, then assert the four `no such column: "X" - should
   this be a string literal in single-quotes?` errors, the `writable_schema`
   bypass creating the legacy schema, the CHECK failure on `'null'`, the `w`
   row, and the stored `sqlite_master` SQL. Run it before converter edits and
   record that it is GREEN (engine already correct).

2. **Carry DQS through the JSON path.**
   - `tools/tclconvert/tcl/interp.go`: add `dqsDDL`/`dqsDML` state to `Interp`,
     defaulting to true (SQLite defaults).
   - `tools/tclconvert/tcl/interp_cmds.go`: extend `commandDBConfig` to parse
     `SQLITE_DBCONFIG_DQS_DDL` / `_DML`, update the two state bits, and emit
     `Stmt{Type: "set_dqs", SQL: "<ddl>,<dml>", TestName: i.curTest}`. Keep
     `DEFENSIVE` as-is.
   - `tools/tclconvert/main.go` needed no change: `set_dqs` carries a
     non-empty `SQL` value, so the existing `step.SQL != ""` keeps it.
     Adding it to `groupToTestCase`'s long whitelist was tried and reverted
     because it only raised the already-over-threshold gocyclo by 1.
   - `frigolite_harness_test.go`: add a `step.Type == "set_dqs"` arm beside
     `defensive`; parse `SQL` with `strconv.ParseBool` and call
     `db.SetDQS(ddl, dml)`.
   - Regenerate only `testdata/quote.json` with
     `go run ./tools/tclconvert -testdir ../sqlite/test -outdir /tmp/t25 -only quote`
     and install it after checking step counts. Converter grouping folds
     the two consecutive DQS calls into `repeat ×2 {set_dqs false,true}`;
     the harness expands repeats before dispatch. The regenerated fixture
     has 35 tests and 38 top-level steps.

3. **Verify (all checked).**
   - Pin GREEN before converter edits; focused `quote` gate
     `/tmp/t25/quote_focus1.log`: 0 leaves.
   - `go test ./internal/... -count=1` green (`/tmp/t25_internal.log`).
   - Converter full package `/tmp/t25_tclconvert_all2.log`: 8/8 failure
     names unchanged vs `/tmp/head_tclconvert_all.log`, NEW=0/GONE=0.
   - Quality `/tmp/t25_quality2.log`: normalized staticcheck/gocognit/gocyclo
     NEW=0/GONE=0 vs `/tmp/t24_quality.log`; overall exit 1 remains the
     pre-existing file-size gate.
   - Full gate `/tmp/t25_full_gate.log` (1231 leaves, 1146s): raw-byte leaf
     diff vs `/tmp/t24_full_gate.log` is NEW=0 and GONE=the seven target
     leaves; vs durable `/tmp/t10_full2.log` NEW=0, GONE=120 (prior slices).
     No `quote` leaves remain.

4. **Land.** Code/fixture commit `2324b3a18`; plan/handoff/TODO/lessons
   commit follows. Goal todo t25 done and next unit (`unhex`) opened.

## Carded / risks

- Do not change engine DDL validation; it already matches SQLite with DQS off.
- DQS state must be emitted at the exact source point (after the section-2
  reopen), so regenerate the fixture rather than hand-editing a global default.
- `set_dqs` is a harness-control step, not SQL; keep it out of query/exec
  assertions and never let it consume an argument slot.
- Full gates are detached; compare raw fail sets, never counts or line greps.
