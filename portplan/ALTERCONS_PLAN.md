# ALTERCONS focused repair plan

## Reproduction and evidence

- Full-suite run retained at `/tmp/frigolite-full-current/`: JSON output `full.json` (75,902,516 bytes), text/timing `full.stderr`, start/end/exit markers. `go test ./... -json` exited 1 in 382.47s; 2,221 failing test events across 346 fixture families.
- Earliest event: `TestSQLiteSuite/altercons/altercons-1.18.2` (`13:29:21 CEST`). Exact isolated repro: `FRIGOLITE_TEST=altercons go test . -run '^TestSQLiteSuite$' -count=1 -json`; 1.18.2 fails querying `sqlite_schema.sql` with `malformed database schema (t1) - incomplete input`.
- Fixture sequence creates `t1(a, b, c -- comment\n CONSTRAINT abc NOT NULL\n )`, drops `abc`, then inspects stored CREATE TABLE SQL. Expected `CREATE TABLE t1(a, b, c)`.

## Full-suite first-failure repair slice

- First refreshed failure was altercons-2.1: DROP named UNIQUE must fail with `constraint may not be dropped: ccc`. Added native `TestAlterDropUniqueConstraintRejected`; fix checks key-constraint type before rewrite. Exact leaf passes.
- Next family leaf 3.6.2: dropping NOT NULL rewrote full column, changing CHECK text and moving named constraint. Added native preservation regression and changed nullable rewrite to remove only top-level NOT NULL text.
- Sequential altercons repairs: 5.3.2.3 preserves ON CONFLICT; 5.3.3.3 preserves action casing; 5.3.5.3 preserves single-quoted identifier; 5.4.2 now returns SQLite's unquoted missing-column error. Each exact leaf passes after corresponding native test (where applicable).
- Latest full-run first failure 2.1.1 was fixture loss: source `altercons2.test:72-80` registers authorizer `xAuth` denying SQLITE_ALTER_TABLE; testdata lacked auth step. Added explicit marker/native denial pin; exact 2.0/2.1.1/2.1.2 passes.
- Latest family follow-up 11.1.2: ALTER ADD CHECK with trailing `--comment` stores comment before rebuilt `)`, yielding incomplete schema. Native RED/green `TestAlterAddCheckWithTrailingLineCommentKeepsSchemaValid`; exact 11.1 sequence passes.
- 12.7 failed escaped constraint name `a"b`; token-based `sqlHasConstraintName` and doubled-quote serialization fix now pass native `TestDropConstraintWithEscapedQuotedName`.
- Full `FRIGOLITE_TEST=altercons2 go test . -run '^TestSQLiteSuite$' -count=1` now passes after these repairs.
- altercons3 fixture correction: source `reset_db` boundaries were omitted before 2.0/3.0/4.x; added markers. 4.x SQL/expected strings also carried spurious terminal quote absent from source; corrected those fixture artifacts. Current isolated family first failure 5.2: DROP named constraint from malformed writable_schema SQL should splice named clause, leaving truncated prefix.
- Latest full run `altercons3/2.0` exposed missing `reset_db` markers in testdata; added source-aligned resets, then isolated 4.x cases exposed spurious terminal `"` in generated SQL and expectations (source lines 59-81 contain no such quotes). Repair only these generated fixture artifacts, then rerun family.
- Refreshed full suite: `/tmp/frigolite-full-refresh-20261017T1420/full.json` (75,964,696 bytes), stderr empty, 347s, exit 1. Earliest current failure `altercons-6.4.2`: ADD duplicate constraint name `abc` unexpectedly succeeds. Focused latest altercons family output `/tmp/altercons-after-eighthfix.txt` had same first failure.

## Generated-column RawSQL defect plan

1. Reproduce natively and inspect stored SQL immediately after CREATE and after DROP.
2. Trace preprocessing and token stream; compare SQLite schema text behavior/source contract.
3. Remove redundant preprocessing only if existing token-level handling provides equivalent parser input while preserving original RawSQL.
4. Verify native regression, exact altercons-1.22.2, focused family result, internal/execddl, diff check.

## Repair plan

1. Read the corresponding SQLite `altercons` TCL source and current converter output/provenance; compare command ordering and SQL text against fixture.
2. Trace `dropTableConstraint` in `internal/execddl/alter_modify.go` and helpers in `alter_modify_sql.go`; identify whether malformed retained schema SQL originates in constraint-range selection or reconstruction.
3. Add native regression reproducing this CREATE/DROP/query flow and asserting stored SQL is parseable and equals expected schema form.
4. Implement smallest source-faithful constraint removal fix; preserve comments/whitespace semantics expected by SQLite.
5. Run regression, `FRIGOLITE_TEST=altercons go test . -run '^TestSQLiteSuite$' -count=1`, then update full-suite inventory after family passes.


## First defect repair evidence

- Native regression `internal/execddl/alter_constraint_test.go` initially failed: output retained `NULL` from `NOT NULL`, and line-comment text survived in the column prefix. `skipColumnConstraintTail` now consumes the `NOT NULL` pair, and column-constraint removal truncates preceding `--` comment text before rebuilding.
- `go test ./internal/execddl -run '^TestDropColumnConstraintAfterLineComment$' -count=1` passes. Exact first leaf `altercons-1.18.2` no longer appears in focused suite output.
- Native regression isolated loss before ALTER: preprocessing stripped GENERATED/STORED before RawSQL capture, storing `CREATE TABLE t1(a,b,c CONSTRAINT abc  AS (b+1))`; DROP then naturally left only AS. Removed redundant preprocessing; token-level parser compatibility remains.
- Validation: native end-to-end passes; exact `FRIGOLITE_TEST=altercons-1.22.2 go test . -run '^TestSQLiteSuite$' -count=1` passes. Broad `FRIGOLITE_TEST=altercons ...` still has unrelated failing cases (2.1, 3.6.2, etc.) but no 1.22.2 failure. `go test ./internal/execddl`, parser suite and `git diff --check` pass.