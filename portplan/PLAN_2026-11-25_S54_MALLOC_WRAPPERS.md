# Plan — S54 slice 3: tclconvert malloc_common wrappers (2026-11-25)

## Problem

`fts3malloc` fails at `fts3_malloc-2.0` with `no such table: ft1`. SQLite's
`fts3malloc.test` creates `ft1`…`ft7` through `do_write_test` from
`malloc_common.tcl`; the JSON converter has no handler for that proc, so its
CREATEs never reach the fixture and the later unconditional `DROP TABLE ft1`
fails. `fts3cov` uses the same wrapper family.

## Exact changes

1. `tools/tclconvert/tcl/interp_cmds.go`
   - Register `do_write_test` and `do_error_test` wrappers.
   - `do_write_test name table sql` emits the SQL as a successful `execsql`
     step (`do_execsql_test name sql {}`); the OOM loop in malloc_common is
     not reproducible in the JSON harness, matching tcl2go's existing
     `emitFTSWriteExec` treatment.
   - `do_error_test name sql msg` emits a `catchsql` step expecting
     `1 {msg}` (tcl2go `processFTSErrorTest` semantics).
2. Unit test in `tools/tclconvert/tcl` for both wrapper shapes.
3. Regenerate `testdata/fts3malloc.json` (and only that family), then run
   focused `fts3malloc`; compare raw fail-set vs `/tmp/t16_full.log`.

## Verification

- New converter unit test green; converter full-package failure-name set vs
  clean HEAD unchanged (8 baseline failures).
- Focused `fts3malloc`: NEW=0 vs `/tmp/t16_full.log`; target leaves gone.
- Quality staticcheck/gocognit/gocyclo NEW=0.
- Detached full gate `-timeout 30m`; raw diff vs `/tmp/t10_full2.log`
  NEW=0.

## Risks / non-goals

- No attempt to emulate SQLite's OOM injection; only the completed SQL and
  error messages that downstream tests observe are materialized.
- No engine change unless a pure-Go pin proves one is needed.
- Do not hand-edit JSON; regenerate with `-only`.

## Execution record (committed 7aced89b1)

Added `commandDoWriteTest` / `commandDoErrorTest` to `tclconvert` and a unit
test for their emitted exec/catch steps. Regenerated only
`testdata/fts3malloc.json`.

Verification:

- `go test ./tools/tclconvert/tcl -run '^TestMallocCommonWrappers$'` — green.
- Converter full-package failure-name set vs clean HEAD: 8 vs 8, NEW=0.
- Focused `fts3malloc` raw diff vs `/tmp/t16_full.log`: NEW=0, GONE=5.
- `resetdb-300` adjudicated as the documented flake: present in durable t10,
  absent in t16, present in the focused re-run and in `/tmp/t17_full.log`.
- Detached full gate `/tmp/t17_full.log` (1183.785s, 1287 leaves): raw-byte
  diff vs durable `/tmp/t10_full2.log` NEW=0, GONE=64; quality NEW=0.
