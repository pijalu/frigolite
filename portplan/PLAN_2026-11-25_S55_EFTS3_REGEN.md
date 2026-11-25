# Plan — S55 slice 2: regenerate e_fts3 with wrapper support (2026-11-25)

## Problem

`e_fts3` has 8 leaves in `/tmp/t19_full2.log`:

- `setup_1`–`setup_4`: `SELECT * FROM docs WHERE 1` — `no such table: docs`
- `e_fts3-4.4`, `e_fts3-4.5`: `SELECT optimize(t4) FROM t4 LIMIT 1` — no `t4`
- `setup_8`: `SELECT root FROM ta_segdir WHERE rowid = 2` — no `ta_segdir`

All three tables are created by SQLite source `e_fts3.test` through
`ddl_test`/`write_test`, which call `do_write_test` via `uplevel`. The checked
fixture has only 25 tests and omits those DDL/write steps; it predates the
`do_write_test`/`do_error_test` converter support landed in the fts3malloc
slice.

## Evidence

- Failure block in `/tmp/t19_full2.log` starts at `TestSQLiteSuite/e_fts3`.
- SQLite source `../sqlite/test/e_fts3.test`: `ddl_test 1.2.2.1` creates
  `docs`, `ddl_test 4.1` creates `t4`, `ddl_test 10.1.1` creates `ta`.
- Temp regeneration `/tmp/e_fts3_regen/e_fts3.json` has 432 tests and contains
  all three CREATEs plus the write steps that populate `docs`.

No engine change is indicated: the engine already executes the DDL when the
fixture carries it. This is a converter/fixture slice.

## Exact changes

1. Regenerate only `testdata/e_fts3.json` with the current converter:
   `go run ./tools/tclconvert -testdir ../sqlite/test -outdir testdata -only e_fts3`.
2. Do not hand-edit the JSON. If leaves remain, triage the first failing step
   with a pure-Go pin before any engine edit.
3. Add a converter unit test only if a new converter behavior is needed.

## Verification

- Focused `FRIGOLITE_TEST=e_fts3` gate: raw leaf-fail diff vs
  `/tmp/t19_full2.log` NEW=0, all 8 target leaves GONE.
- Converter package raw failure-name set unchanged vs
  `/tmp/t19_tclconvert_all.log`.
- Quality staticcheck/gocognit/gocyclo NEW=0 vs `/tmp/t19_quality3.log`.
- Detached full gate `-timeout 30m`; raw-byte leaf fail-set diff vs
  `/tmp/t19_full2.log` and durable `/tmp/t10_full2.log`.
- Code commit, then docs/handoff/lessons/TODO commit.

## Execution record (2026-11-25)

- Code commit `d96724729`: regenerated `testdata/e_fts3.json` only.
- Focused gate `/tmp/t20_focus.log`: exit 0; all 8 `e_fts3` leaves GONE vs
  `/tmp/t19_full2.log`, NEW=0.
- Quality `/tmp/t20_quality.log`: staticcheck/gocognit/gocyclo NEW=0 vs
  `/tmp/t19_quality3.log`; converter failure-name set unchanged vs
  `/tmp/t19_tclconvert_all.log`.
- Full gate `/tmp/t20_full.log` (1268 leaves): 8 `e_fts3` leaves GONE.
  NEW vs t19 are the documented `atof1-2.40` and `resetdb-300` flakes;
  NEW vs durable t10 is only `atof1-2.40`. Focused `/tmp/t20_atof1_focus.log`
  green; `resetdb-300` absent from `/tmp/t20_resetdb_focus.log`.

## Risks / non-goals

- No engine change unless a pure-Go pin fails.
- Full gates use raw-byte leaf-name sets only, never counts or line greps.
- `tools/tclconvert` is ignored; force-add converter files if any change.
