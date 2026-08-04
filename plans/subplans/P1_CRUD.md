# Sub-Plan: P1 — CRUD Core (8 sub-goals)

> **Prerequisite**: G0 (grammar) complete or in progress.
> **Structure**: 8 sub-goals, each with pre-tests (P6 protocol) then TCL test fixes.

Each sub-goal below is a SEPARATE goal with freshContext:true. The handover note
carries state between them.

## Common: Pre-Test Protocol (P6)

Before running TCL tests, write hand-written Go tests that isolate the feature.
Compare against `/usr/bin/sqlite3` (the oracle) for expected output. This cleanly
separates engine bugs from transpiler bugs.

### Oracle comparison helper (write once, reuse):
```bash
# Get expected output from real SQLite:
echo "SELECT ..." | sqlite3 :memory:
```

---

## G1.CREATE — CREATE TABLE / DDL

### Goal
```
Objective: All CREATE TABLE functionality works — column types, constraints,
WITHOUT ROWID, STRICT, IF NOT EXISTS, CREATE TABLE AS SELECT.
Completion criterion: testgen packages select1, types, strict, tableopts,
without_rowid all PASS; hand-written pre-tests in frigolite_p1_create_test.go PASS.
Verify command: go test -tags testgen ./testgen/select1/ ./testgen/types/ ./testgen/strict/ ./testgen/without_rowid/ ./testgen/tableopts/ -count=1 && go test -run TestP1Create -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_create_test.go`
Test cases (each compared against sqlite3 oracle):
- CREATE TABLE with all column types (INTEGER, TEXT, REAL, BLOB, NUMERIC)
- Column constraints: PRIMARY KEY, NOT NULL, UNIQUE, DEFAULT, CHECK, REFERENCES
- Table constraints: PRIMARY KEY(a,b), UNIQUE(a), CHECK(expr), FOREIGN KEY
- WITHOUT ROWID tables
- STRICT tables
- IF NOT EXISTS
- CREATE TABLE AS SELECT
- AUTOINCREMENT
- Generated columns (GENERATED ALWAYS AS)
- Type affinity rules (TEXT/INTEGER/REAL/BLOB → storage class)

### Steps
1. **Write pre-test** → `frigolite_p1_create_test.go` with all cases above.
   Run against frigolite; record failures. Compare against sqlite3.
   Commit: `G1.CREATE.1: add CREATE TABLE pre-test suite`
2. **Fix STRICT table enforcement** — STRICT tables must reject wrong-type values.
   SQLite ref: `src/build.c` (TF_Strict flag). Fix: `internal/exec/`.
   Verify: `go test -run TestP1Create -count=1 .`
   Commit: `G1.CREATE.2: enforce STRICT table type checking`
3. **Fix generated columns** — parse + eval GENERATED ALWAYS AS (expr).
   Grammar rules: 423–424 in parse.y. Fix: `internal/parse/parser.go` + `internal/exec/`.
   Commit: `G1.CREATE.3: implement generated columns`
4. **Fix WITHOUT ROWID ordering** — PK defines row order.
   Verify: `go test -tags testgen ./testgen/without_rowid/ -count=1`
   Commit: `G1.CREATE.4: fix WITHOUT ROWID primary key ordering`
5. **Run TCL tests** — fix any transpiler-specific issues uncovered.
   Commit: `G1.CREATE.5: all CREATE TABLE TCL tests green`

---

## G1.INSERT — INSERT / UPSERT / VALUES

### Goal
```
Objective: All INSERT functionality — VALUES, multi-row, INSERT...SELECT, DEFAULT
VALUES, UPSERT (ON CONFLICT), OR IGNORE/REPLACE.
Completion criterion: testgen packages insert, values, valuesfault, default_pkg
all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/insert/ ./testgen/values/ ./testgen/valuesfault/ ./testgen/default_pkg/ -count=1 && go test -run TestP1Insert -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_insert_test.go`
- INSERT single row with all columns
- INSERT with column list (subset)
- INSERT multi-row VALUES
- INSERT...SELECT
- INSERT DEFAULT VALUES
- INSERT OR IGNORE (conflict handling)
- INSERT OR REPLACE
- REPLACE INTO
- UPSERT: ON CONFLICT DO NOTHING
- UPSERT: ON CONFLICT(a) DO UPDATE SET b=excluded.b
- INSERT with RETURNING (if not done in G1.UPDATE)
- Column count validation (too many/few columns = error)
- Type affinity on INSERT (value coerced to column type)

### Steps
- [x] 1. **Write pre-test** → `frigolite_p1_insert_test.go` (VALUES, column list, multi-row, INSERT...SELECT, DEFAULT VALUES, OR IGNORE, OR REPLACE, UPSERT DO NOTHING / DO UPDATE excluded.*, column-count validation).
   Commit: `G1.INSERT.1: add INSERT pre-test suite`
- [x] 2. **Fix column affinity on INSERT** — multi-word type names (NATIONAL CHARACTER, LONG INTEGER, DOUBLE PRECISION) parse correctly; ApplyColumnAffinity coerces values.
   Commit: `G1.INSERT.2: enforce column affinity on INSERT`
- [x] 3. **Fix UPSERT** — ON CONFLICT DO UPDATE with excluded.* references (excluded pseudo-table populated in buildUpdatedRow).
   Commit: `G1.INSERT.3: implement UPSERT (ON CONFLICT DO UPDATE)`
- [x] 4. **Fix column count validation** — INSERT with wrong column count errors (too few/too many/column-list mismatch); OR IGNORE swallowed via cloneStmtsWithValues OrIgnore fix.
   Commit: `G1.INSERT.4: enforce INSERT column count validation`
- [x] 5. **Run TCL tests** — insert, values, valuesfault, default_pkg all green (default_pkg fixed: multi-word affinity, AUTOINCREMENT sequence, int64 overflow→REAL, non-constant DEFAULT rejection).
   Commit: `G1.INSERT.5: all INSERT TCL tests green`

---

## G1.SELECT — SELECT (basic + variants)

### Goal
```
Objective: All basic SELECT functionality — projection, WHERE, DISTINCT, ORDER BY,
LIMIT/OFFSET, aliases, star expansion, scalar subqueries, type coercion.
Completion criterion: testgen packages select2–selectH all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/select2/ ./testgen/select3/ ./testgen/select4/ ./testgen/select5/ ./testgen/select6/ ./testgen/select7/ ./testgen/select8/ ./testgen/select9/ ./testgen/selectA/ ./testgen/selectB/ ./testgen/selectC/ ./testgen/selectD/ ./testgen/selectE/ ./testgen/selectF/ ./testgen/selectG/ ./testgen/selectH/ -count=1 && go test -run TestP1Select -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_select_test.go`
- SELECT * / SELECT col / SELECT t.col / SELECT expr AS alias
- SELECT with WHERE (all comparison operators)
- SELECT DISTINCT
- ORDER BY (ASC/DESC, multiple columns, by alias, by position)
- LIMIT / OFFSET / LIMIT a,b
- Star expansion (SELECT *, SELECT t.*)
- SELECT with expressions (arithmetic, functions)
- UNION / UNION ALL / INTERSECT / EXCEPT (basic — full impl in G2.SETOP)
- Float formatting (1.0 not 1 for REAL values)
- Empty result sets
- Column naming (expr without alias)

### Steps
1. **Write pre-test** → `frigolite_p1_select_test.go`.
   Commit: `G1.SELECT.1: add SELECT pre-test suite`
2. **Fix float formatting** — REAL values always show `.0` for whole numbers.
   Fix: `testgen/*/helpers_test.go` flatten() function (harness fix, P2-allowed).
   Commit: `G1.SELECT.2: fix REAL float formatting in test harness`
3. **Fix LEFT JOIN view column resolution** (select1).
   SQLite ref: `src/select.c`.
   Commit: `G1.SELECT.3: fix LEFT JOIN view column resolution`
4. **Fix compound SELECT affinity** — UNION/INTERSECT column types from first SELECT.
   Commit: `G1.SELECT.4: fix compound SELECT column affinity`
5. **Fix EXPLAIN QUERY PLAN output** — EQP multi-node emission (selectD, whereE, whereH).
   Commit: `G1.SELECT.5: fix EXPLAIN QUERY PLAN output format`
6. **Run TCL tests** — fix per-package issues.
   Commit: `G1.SELECT.N: <package> TCL tests green`

---

## G1.WHERE — WHERE clauses / filtering

### Goal
```
Objective: All WHERE clause functionality — comparison, logical ops, IN, BETWEEN,
LIKE, GLOB, IS NULL, IS NOT NULL, EXISTS, collation.
Completion criterion: testgen packages where–whereN all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/where/ ./testgen/whereA/ ./testgen/whereB/ ./testgen/whereC/ ./testgen/whereD/ ./testgen/whereE/ ./testgen/whereF/ ./testgen/whereG/ ./testgen/whereH/ ./testgen/whereI/ ./testgen/whereJ/ ./testgen/whereK/ ./testgen/whereL/ ./testgen/whereM/ ./testgen/whereN/ -count=1 && go test -run TestP1Where -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_where_test.go`
- WHERE with =, <>, !=, <, >, <=, >=
- WHERE with AND, OR, NOT (three-valued logic with NULL)
- WHERE col IN (1,2,3) / NOT IN
- WHERE col BETWEEN x AND y / NOT BETWEEN
- WHERE col LIKE 'pattern%' / NOT LIKE / ESCAPE
- WHERE col GLOB 'pattern'
- WHERE col IS NULL / IS NOT NULL
- WHERE EXISTS (subquery)
- WHERE col IN (subquery)
- WHERE with COLLATE (NOCASE, BINARY, RTRIM)
- WHERE with type affinity (INTEGER vs TEXT comparison)
- NULLS FIRST/LAST in ORDER BY

### Steps
1. **Write pre-test** → `frigolite_p1_where_test.go`.
   Commit: `G1.WHERE.1: add WHERE pre-test suite`
2. **Fix three-valued logic** — NULL in AND/OR/NOT/comparison.
   SQLite ref: `src/expr.c` (sqlite3ExprIfTrue/IfFalse).
   Commit: `G1.WHERE.2: fix NULL three-valued logic in WHERE`
3. **Fix COLLATE in WHERE** — NOCASE/BINARY/RTRIM comparison.
   Commit: `G1.WHERE.3: implement COLLATE in WHERE comparisons`
4. **Fix type affinity in comparisons** — INTEGER vs TEXT coercion.
   Commit: `G1.WHERE.4: fix type affinity in WHERE comparisons`
5. **Fix NULLS FIRST/LAST** — ORDER BY null placement.
   Commit: `G1.WHERE.5: implement NULLS FIRST/LAST in ORDER BY`
6. **Run TCL tests**.
   Commit: `G1.WHERE.N: <package> TCL tests green`

---

## G1.UPDATE — UPDATE / RETURNING

### Goal
```
Objective: All UPDATE functionality — SET, WHERE, multi-column, OR IGNORE/REPLACE,
UPDATE...FROM, RETURNING.
Completion criterion: testgen packages update, returning all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/update/ ./testgen/returning/ -count=1 && go test -run TestP1Update -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_update_test.go`
- UPDATE SET col = value (single column)
- UPDATE SET col1=v1, col2=v2 (multi-column)
- UPDATE SET col = expr (expression)
- UPDATE ... WHERE (conditional)
- UPDATE OR IGNORE (conflict handling)
- UPDATE OR REPLACE
- UPDATE...FROM (join in UPDATE)
- UPDATE ... RETURNING * / RETURNING col
- UPDATE with DEFAULT values
- UPDATE affecting 0 rows

### Steps
1. **Write pre-test** → `frigolite_p1_update_test.go`.
   Commit: `G1.UPDATE.1: add UPDATE pre-test suite`
2. **Implement RETURNING** — INSERT/UPDATE/DELETE projection.
   SQLite ref: `src/parse.y:1110` (returning rule), `src/insert.c`.
   Note: T1.7 in TCL_RESEARCH_TIER1.md may have partial work (check handover).
   Commit: `G1.UPDATE.2: implement RETURNING clause`
3. **Fix UPDATE...FROM** — join syntax in UPDATE statements.
   Commit: `G1.UPDATE.3: implement UPDATE...FROM clause`
4. **Run TCL tests**.
   Commit: `G1.UPDATE.4: all UPDATE TCL tests green`

---

## G1.DELETE — DELETE

### Goal
```
Objective: All DELETE functionality — WHERE, RETURNING, DELETE with subqueries,
DELETE with ORDER BY/LIMIT (if supported).
Completion criterion: testgen packages delete_, delete2, delete3, delete4,
delete_pkg all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/delete_/ ./testgen/delete2/ ./testgen/delete3/ ./testgen/delete4/ ./testgen/delete_pkg/ -count=1 && go test -run TestP1Delete -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_delete_test.go`
- DELETE FROM t (all rows)
- DELETE FROM t WHERE condition
- DELETE ... RETURNING *
- DELETE with subquery in WHERE
- DELETE affecting 0 rows
- DELETE with foreign key cascade
- DELETE from WITHOUT ROWID table

### Steps
1. **Write pre-test** → `frigolite_p1_delete_test.go`.
   Commit: `G1.DELETE.1: add DELETE pre-test suite`
2. **Fix delete3** — if not already fixed (T1.1 may have done it; verify).
   Commit: `G1.DELETE.2: verify/fix delete3`
3. **Fix DELETE RETURNING** — projection of deleted rows.
   Commit: `G1.DELETE.3: implement DELETE RETURNING`
4. **Fix delete_pkg** — DELETE WHERE scalar subquery ORDER BY LIMIT OFFSET.
   Commit: `G1.DELETE.4: fix DELETE with scalar subquery`
5. **Run TCL tests**.
   Commit: `G1.DELETE.5: all DELETE TCL tests green`

---

## G1.TYPES — Types & Affinity

### Goal
```
Objective: Correct type affinity, CAST, NULL handling, typeof(), storage classes.
Completion criterion: testgen packages affinity, cast, numcast, types, intpkey,
intreal, nulls, null all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/affinity/ ./testgen/cast/ ./testgen/numcast/ ./testgen/types/ ./testgen/intpkey/ ./testgen/intreal/ ./testgen/nulls/ ./testgen/null/ -count=1 && go test -run TestP1Types -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_types_test.go`
- Type affinity: TEXT/INTEGER/REAL/BLOB/NUMERIC column coercion
- CAST(expr AS type) for all type combinations
- CAST string to REAL (numeric prefix parse)
- typeof() for all storage classes
- NULL handling: NULL + expr = NULL, NULL = x = NULL
- Integer PRIMARY KEY (rowid alias)
- INTEGER overflow → REAL promotion
- Large integer comparison (int64 vs float64)

### Steps
1. **Write pre-test** → `frigolite_p1_types_test.go`.
   Commit: `G1.TYPES.1: add types & affinity pre-test suite`
2. **Fix affinity on INSERT/comparison** — applyAffinity(value, affinity).
   Commit: `G1.TYPES.2: implement column affinity application`
3. **Fix CAST string→REAL** — numeric prefix parsing (sqlite3AtoF).
   Commit: `G1.TYPES.3: fix CAST string-to-REAL numeric prefix parse`
4. **Fix integer PRIMARY KEY extremes** — MinInt64/MaxInt64 handling.
   Commit: `G1.TYPES.4: fix integer PRIMARY KEY extreme values`
5. **Run TCL tests**.
   Commit: `G1.TYPES.5: all types TCL tests green`

---

## G1.EXPR — Expressions

### Goal
```
Objective: All expression evaluation — arithmetic, comparison, logical, CASE,
BETWEEN, IN, LIKE, GLOB, IS NULL, scalar functions, NULL propagation.
Completion criterion: testgen packages expr, between, coalesce, literal, istrue,
cse, subtype all PASS; pre-tests PASS.
Verify command: go test -tags testgen ./testgen/expr/ ./testgen/between/ ./testgen/coalesce/ ./testgen/literal/ ./testgen/istrue/ ./testgen/cse/ ./testgen/subtype/ -count=1 && go test -run TestP1Expr -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p1_expr_test.go`
- Arithmetic: +, -, *, /, % (REM)
- Bitwise: &, |, <<, >>, ~
- Comparison: =, ==, !=, <>, <, >, <=, >=
- Logical: AND, OR, NOT (three-valued)
- String concat: ||
- CASE WHEN ... THEN ... ELSE ... END
- BETWEEN / NOT BETWEEN
- IN (list) / NOT IN (list) / IN (subquery)
- LIKE / GLOB / NOT LIKE / ESCAPE
- IS NULL / IS NOT NULL / IS / IS NOT
- CAST
- Scalar functions (abs, length, etc. — minimal, full in G4)
- NULL propagation in all operators
- Boolean literals: TRUE / FALSE (SQLite 3.23+)

### Steps
1. **Write pre-test** → `frigolite_p1_expr_test.go`.
   Commit: `G1.EXPR.1: add expression pre-test suite`
2. **Fix NULL propagation** — arithmetic/comparison with NULL → NULL.
   Commit: `G1.EXPR.2: fix NULL propagation in expressions`
3. **Fix bool rendering** — TRUE/FALSE → 1/0 (not Go bool).
   Note: T1.3 may have done this for cse; verify and extend.
   Commit: `G1.EXPR.3: fix boolean expression rendering (true→1, false→0)`
4. **Fix BETWEEN** — correctly handle NULL operands.
   Commit: `G1.EXPR.4: fix BETWEEN with NULL operands`
5. **Implement custom test functions** — int(), implies_nonnull_row, etc.
   These are TCL test-harness functions, not engine functions.
   Fix: `tools/tcl2go/gen.go` or testgen helpers.
   Commit: `G1.EXPR.5: implement test-harness custom functions`
6. **Run TCL tests**.
   Commit: `G1.EXPR.6: all expression TCL tests green`
