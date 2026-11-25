# PLAN — insert DML parity residue (2026-11-25)

## State

`FRIGOLITE_TEST=insert` had exactly two result-visible leaves:

- `insert-17.15`: `UNIQUE constraint failed: t3.a` where sqlite3 3.54 reports
  `UNIQUE constraint failed: t3.d`.
- `insert-5.3`: `[lsearch $x OpenEphemeral]>0` evaluated to 0 because the
  EXPLAIN stream omitted C's temp-table spool opcode.

Both predate S66. No row loss was reported; the first leaf is an error-text
regression and the second is an EXPLAIN-opcode-set gap.

## Hypothesis and source check

1. `insert-17.15` builds its trigger OLD/NEW rows through
   `internal/execdml/insert_conflict.go:buildTriggerRowMap`, which calls
   `wrapValueForRowMap`. When the incoming value is already a
   `*util.ColumnValue` (or a `*collatedValue`), wrapping it again nests the
   affinity layer. The partial-index predicate `c<=d` then compares wrappers
   instead of scalars and evaluates `3<=2` as TRUE for `t3bpi`. The spurious
   partial-index entry changes which REPLACE conflict is reported (IPK alias
   `t3.a` instead of the real `t3d` index on `d`).
2. `internal/execquery/explain_rows.go:emitInsertRows` renders the driving
   SELECT's plan nodes and the write cursors, but never renders the
   `OP_OpenEphemeral` that C emits for `INSERT ... SELECT` when the SELECT
   reads the destination (`../sqlite/src/insert.c:1151-1189`).
3. The fixture captured `EXPLAIN` with `resultAll`, but the original TCL uses
   `set x [execsql {...}]`; `execsql` is `$db eval` (`../sqlite/test/tester.tcl:1445-1448`),
   whose result is a flat list of all columns. `lsearch $x OpenEphemeral`
   probes individual values, so the faithful harness capture is `resultFlat`.

## Changes (ordered)

1. Add `frigolite_insert_parity_native_test.go` pins for both leaves:
   - exact error text for the trigger/partial-index/REPLACE sequence;
   - `OpenEphemeral` presence in `EXPLAIN INSERT INTO t4 SELECT x+2 FROM t4`.
2. `internal/execdml/insert_select.go:wrapValueForRowMap`: return an existing
   `*util.ColumnValue` or `*collatedValue` unchanged, so affinity/collation
   is applied once. Keep the existing wrap path for raw scalars.
3. `internal/execquery/explain_rows.go:emitInsertRows`: when a SELECT source
   table name matches the INSERT target, emit one `OpenEphemeral` row with
   `nColumn` from the SELECT's column count, after the driving plan rows and
   before the write opens.
4. `testdata/insert.json` `insert-5.3`: `resultAll` -> `resultFlat` to model
   TCL `execsql`'s flat `db eval` list. This fixes the fixture interpretation;
   it does not weaken or skip the assertion.

## Verification

- `go test -run 'TestInsert17_15UniqueColumnName|TestInsert53ExplainOpenEphemeral' -count=1 .`
  (native pins must pass).
- `/usr/bin/sqlite3 :memory:` oracle: same trigger sequence -> `UNIQUE
  constraint failed: t3.d`; same EXPLAIN -> `OpenEphemeral nColumn=1`.
- `FRIGOLITE_TEST=insert go test -run '^TestSQLiteSuite$' -count=1 .`
  (both leaves pass).
- `go test -run TestSOLID_ ./...` and `go vet` on the touched packages.
- Full 12-shard harness (`tools/run_json_shards.sh`) leaf-name diff against
  `/tmp/t30_full_gate.log`: NEW=0, with documented flakes allowed as GONE.
- Update `TODO.md`, `.agents/lessons_learned.md`, and a dated handoff; commit.

## Risk / rollback

The wrapping guard only affects callers that pass an already-wrapped value
(one call site, trigger row maps); raw scalars keep the old path. The
EXPLAIN emitter addition is display-only and cannot alter DML results. If a
family gate reports NEW failures, revert the two engine hunks independently.
