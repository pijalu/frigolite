# Parity repair session — compare-query, date lifecycle, hexio (2026-10-18)

This plan scopes the next work items from `handover.md` and
`portplan/FULL_SUITE_FAILURE_PLAN.md`. Workspace contains many unrelated edits;
only the files named per item may be touched.

## Evidence base

- Latest full run: `/tmp/frigolite-current-full-suite6.log` (exit 1); 284 harness
  families failing, ~1531 failing test records. Per-family first-failure digest
  generated at `/tmp/family-first-failures.txt`.
- Many families fail at their first setup step with "table t1 already exists" or
  missing-table errors: a fixture-age/staleness signature previously traced to
  converter regressions. Established repair: regenerate only the affected
  fixture from `../sqlite/test`, after verifying the fresh output against source.
- Isolated `FRIGOLITE_TEST=date` on the current fixture fails `date-14.1`
  (7 mis-grouped steps, "table t1 already exists"). `testdata/date.json` is a
  17-test stale fixture; fresh conversion produces 1430 ordered tests and
  without further changes reduces date failures to `setup_4`, `date-15.1`,
  `date-15.2`.

## Item 1 — converter runtime result comparison (`compare_query`)

Authoritative pattern (many files): `do_execsql_test NAME {SQL1} [db eval {SQL2}]`
and `do_test NAME { body } [db eval {SQL2}]` compare the engine result of the
body against a second query executed at runtime. Examples:
`../sqlite/test/update2.test:38,53` (`SELECT * FROM t1` vs
`SELECT a, repeat(b,100) FROM t2`), `../sqlite/test/analyze5.test:186-197`.

Currently the converter substitutes `[db eval {...}]` offline, which (a) appends
a spurious `query` step and (b) returns the *previous* command's result as the
expectation (update2 fixture carries bogus `expect:"10"`). The harness already
implements `compare_query` (`frigolite_harness_test.go:2614`, step fields
`SQL2`/`Expect=="ok"`) but the converter never emits it.

Plan:
1. Add converter unit tests (fail first):
   - `do_execsql_test u-1.1 {SELECT a FROM t1} [db eval {SELECT a FROM t2}]`
     → single `compare_query` step, SQL=first, AuxSQL=second, Expected=ok.
   - same for `do_test u-1.2 { SELECT ... } [db eval {...}]`.
   - literal expectations unchanged.
2. Implement in `tools/tclconvert/tcl/interp_sql.go`:
   - `extractCompareQueryWord(rawWord)` recognising `[db eval { … }]` /
     `[execsql { … }]` as the trailing expectation word;
   - `cmdDoExecSQL`/`cmdDoTest` skip offline evaluation of that word, attach
     `AuxSQL` + `Expected="ok"`, type `compare_query`.
3. Add `Stmt.AuxSQL` (tcl) and JSON `auxSql` in `tools/tclconvert/main.go`:
   `TestStep.AuxSQL`; harness `TestStep.SQL2` already reads `sql2` — emit `sql2`
   for AuxSQL (keep harness unchanged).
4. Regenerate `testdata/update2.json` from source; verify isolated update2.
5. Regression: update2-1.1.2/1.2.2 compare both queries at runtime.

## Item 2 — date family lifecycle

Source `../sqlite/test/date.test:520-546` (`date-14.1`, `date-14.2.$i`) requires:
`db close`; `hexio_write test.db OFFSET HEX`; `sqlite3 db test.db`;
`db eval SELECT * FROM t1`; datetime values must round-trip the patched double.

Plan:
1. Converter: handle `sqlite3 -has-codec` returning `0` (source wraps 14.x in
   `if {0==[sqlite3 -has-codec]}`; the substitution currently yields empty and
   the body is dropped).
2. Converter: add `hexio_write FILE OFFSET HEXDATA` capturing a typed step
   (`hexio_write`, FileTarget=file, FileOffset, FileData=hex bytes).
   Make `format %02x $i` evaluable (existing `format` handler).
3. Harness: execute `hexio_write` by patching the bytes of the resolved
   lifecycle file at the offset (same path mapping as `write_file`/`reopen_db`).
4. Harness: `set sqlite_current_time [db eval {SELECT strftime('%s',…)}]`
   (source line 402) must emit a `query_assign` with Variable
   `sqlite_current_time`; the query_assign handler sets `function.SetNowFunc`
   from the evaluated result instead of requiring a literal marker.
5. Harness: support Tcl UDF body `after N` (`date-15.1/15.2` `sleeper`) by
   sleeping N ms and returning NULL, so the within-statement `now` constancy
   assertions run with real elapsed time.
6. Regenerate only `testdata/date.json`; verify isolated `date` family,
   including date-14.1/14.2.x and 15.1/15.2; run `tools/tclconvert/tcl` tests and
   `git diff --check`.

## Verification cadence

- Per item: converter unit tests, isolated family run
  (`FRIGOLITE_TEST=<family> go test . -run '^TestSQLiteSuite$' -count=1`),
  `go test ./tools/tclconvert/... -count=1`, `git diff --check`.
- After each item lands: serialized full-suite background run with unique
  `/tmp` log/exit paths (no overlapping runs).
- Record durable findings in `.agents/lessons_learned.md`.

## Explicitly out of scope here (next items, ordered by earliest log failure)

- `e_update`/`e_delete` `DROP TRIGGER tr1` (source-backed inspection).
- Fixture regeneration for other stale families, one root cause at a time.

## Results (2026-10-18)

- Item 1 (compare_query): converter now emits a real `compare_query` step for
  `[db eval {...}]` trailing expectations (regressions
  `TestDoExecSQLRuntimeComparisonCapturesCompareQuery`,
  `TestDoTestRuntimeComparisonCapturesCompareQuery`). Regenerated
  `testdata/update2.json` (37 cases); `update2-1.1.2`/`1.2.2` compare both
  queries at runtime; isolated `FRIGOLITE_TEST=update2` passes.
- Item 2 (date lifecycle): regenerated `testdata/date.json` (1943 cases) with
  `query_assign` for `sqlite_current_time`, `hexio_write` lifecycle steps,
  real `date-14.1`/`14.2.*` assertions (via `runtime_expr`), and
  `date-15.1/15.2` sleeper support. Isolated `date` family has zero failures.
- Converter: trailing expressions over runtime-captured vars emit
  `runtime_expr` (raw expr + expected), evaluated by the new harness Tcl-expr
  subset evaluator (`frigolite_harness_expr_test.go`); unit tests
  `TestDoTestFinalExprWithRuntimeVarEmitsRuntimeExpr` /
  `TestDoTestFinalExprWithoutRuntimeVarStaysOffline` pass.
- Harness: `string repeat` UDF (`db func repeat [list string repeat]`);
  `sqlite3 db FILE` and `open_named_db` normalize `file:`/URI operands to the
  fixture-local basename; `copy_file` marker supported.
- Item 3 (reset vs reopen): resolved by convention — `sqlite3 db FILE` always
  emits data-preserving `reopen_db` (never `reset_db`); converter regression
  renamed to `TestSQLite3ReopenAfterForcedeleteURI`; converter tests pass.
- `8_3_names.test` is guarded by `ifcapable !8_3_names { finish_test; return }`;
  the capability (SQLITE_ENABLE_8_3_NAMES FAT name mangling) is modeled absent
  like utf16. Fresh conversion now skips the file ("Processed 0/1 files
  (skipped 1...)"), so fixture `testdata/8_3_names.json` was removed (no
  skip-list edits). Coverage note: legacy oracle fixture `f_8_3_names.json`
  (passing, 16 cases) continues to exercise the SQL-visible portions. Gaps
  observed before removal: section-2 `.nal` journal naming (N/A without the
  compile option) and `8_3_names-5.6` WAL read snapshot across a writer commit
  (queued P7.WAL snapshot work).
- Verification: `go test ./tools/tclconvert/... -count=1` passes; isolated
  `8_3_names` and `update2` pass; `date` filter matches only
  `e_update-2.2.X` (next item); `git diff --check` clean.
- Full-suite run after these changes: `/tmp/frigolite-full-suite7.log`
  (exit marker `/tmp/frigolite-full-suite7.exit`).

## e_update residuals — plan and authority correction (2026-10-18)

Fresh fixture regeneration (from the pinned `../sqlite` 3.51 tree) cleared
`e_update-2.2.X` but exposed 19 leaf failures. Analysis split them into:

- Category A (converter `{*}` argument expansion missing): `1.8.X.2` (x12) and
  `2.3.0`. `[list {*}$data]` / `[list {*}{...}]` is tokenized as literal `*`
  plus a substituted word, yielding `* {1 one ...}` expectations. Fix in
  `tools/tclconvert/tcl`: `RawWord.Expand`, detection in `ParseCommands`,
  splicing `splitList(value)` in `execCommand`/`evalAllWords`.
- Category B (lossy expected-list serialization): `1.4.1`, `1.7.1`.
  `commandDoSelectTests` joins `splitList(expected)` with spaces, dropping
  empty-element braces (`{{} {} 1 i}` -> `"  1 i"`). Fix: canonical
  `tclList(splitList(...))` (empty elements stay `{}`). The harness then needs
  TCL's empty-value equivalence: expected `{}`/`NULL` matches both a NULL cell
  ("NULL") and an empty text cell ("") — add a single-space-joined
  element-wise fallback in `matchExpectation`.
- Category C (temp-trigger qualified DML targets): initially diagnosed as an
  engine gap (pinned 3.51 parse.y `trnm` rejects `UPDATE main.t1` inside a
  trigger whose ON table is temp; old fixture comment cited e_update-2.1.3 as
  allowed). Evidence reverses the authority call:
  - `tools/tclconvert/main.go:76` and `tools/oracle_generate.py` both default
    to `ori/sqlite/test`; the corpus was regenerated from that tree (T26).
  - `ori/sqlite/test/e_update.test` (60 cases) contains the newer text
    "Update: Unless the trigger is in the temp schema." with `2.1.3` as an
    allowed `CREATE TRIGGER ... DROP TRIGGER` exec; pinned `../sqlite`
    (3.51.0) still lists that SQL under `-error`.
  - `ori/sqlite/test/temptrigger.test` documents "temp triggers may
    INSERT/UPDATE/DELETE to fully qualified table names"; `testdata/
    temptrigger.json` `8.1.2`/`8.2.2`/`8.3.2` currently pass in the harness,
    and the local sqlite3 3.54 oracle accepts the statements.
  - Therefore corpus authority is `ori/sqlite/test`, the engine's current
    temp-trigger behavior is correct, and `testdata/e_update.json` must be
    regenerated from `ori`, not the 3.51 tree. No engine change; add a native
    pin for the allowed/rejected split.
- Plan order: converter A+B with unit tests; regenerate e_update from ori;
  harness equivalence fallback; isolated `e_update` + `temptrigger` + `trigger1`
  reruns; converter tests; `git diff --check`; docs; next full suite.

## Session status (2026-10-19)
- All item-1/item-2 converter repairs landed and verified (converter suites green):
  `%d` error-template formatting, `ifcapable EXPR BODY else ELSEBODY`, `{*}` expansion,
  canonical `{}` serialization, `open_empty_db` for `sqlite3 db ""`/`:memory:`.
- Fixtures regenerated from `ori/sqlite/test` and green: `e_update`, `e_delete`,
  `e_insert`, `trigger1`, `trigger7`, `triggerupfrom` (incl. malformed-schema-on-ATTACH
  2.4), `temptrigger` (65 cases incl. section 9).
- Harness: `:memory:` preserved by `routeFixtureAttachPaths`; `open_empty_db` step
  implemented and counted as a lifecycle signal.
- Engine: attach-time `sqlite3Init` validation (`ValidateAttachedSchema`, row + trigger
  checks before registration; failed ATTACH rolls back), attach.c `fixSelectCb`
  name-comparison semantics in `checkLoadedTableRefCtx`, fire-time TEMP-trigger exemption
  and owning-schema validation, trigger grammar/temp-name restrictions.
- Regression check: no new failures vs run7 (`/tmp/run7-fails.txt`) for `trigger*`,
  `view*`, `fkey*`, `alter*`.
- Run8 launched: `/tmp/frigolite-full-suite8.log`, exit `/tmp/frigolite-full-suite8.exit`.
  Next session step is run8 triage (compare failing sets with `comm -13`).
