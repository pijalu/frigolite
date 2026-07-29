# Frigolite — Master Plan: Complete SQLite Feature Parity

> **Status**: ACTIVE — rewriting from ground-truth measurements.
> **Goal**: **ALL 1002 harness test files green** (1002/1002), with SOLID
> architecture and good performance. No skip-list entries remain.
> **Execution**: Sequential — one phase = one goa goal. Each phase is
> self-contained: an agent with no prior context can implement it from this
> document plus the referenced SQLite C source.

---

## Verified Ground Truth (measured before rewrite)

### Test Results (the real numbers)
| Metric | Count | % |
|--------|-------|---|
| Total test files | 1002 | 100% |
| Skipped (unsupported) | 134 | 13.4% |
| Skipped (slow) | 3 | 0.3% |
| Active test files | 865 | 86.3% |
| **PASS (file-level)** | **213** | **24.6%** |
| **FAIL (file-level)** | **656** | **75.4%** |

### Failure Reasons (by frequency across all sub-tests)
| Reason | Count | Notes |
|--------|-------|-------|
| result mismatch | 4980 | Query returns wrong data |
| query error | 2875 | Most are "no such table" |
| expected error but got success | 1357 | Wrong data accepted |
| exec error | 1105 | Execution fails unexpectedly |
| error mismatch | 594 | Wrong error message |

### Top Error Patterns (the blockers)
| Pattern | Count | Root cause |
|---------|-------|------------|
| "no such table: t1" | 1305 | JOIN bugs, subquery bugs, expected errors |
| "no such table" (other) | 1342 | Same + temp tables, views |
| "unknown function" | 307 | Missing SQL functions |
| "database disk image is malformed" | 76 | Corruption / format bugs |
| "btree" errors | 63 | B-tree bugs |
| UNIQUE constraint failures | ~57 | Constraint handling bugs |
| "attached databases must use same text encoding" | 47 | ATTACH bug |
| "unrecognized date/time" | 14 | Date/time function bugs |

### Known Broken Features (verified by isolated testing)
| Feature | Status | Example |
|---------|--------|---------|
| `JOIN ... USING` | **BROKEN** — returns empty | `SELECT * FROM t1 JOIN t2 USING(a)` → [] |
| `LEFT JOIN` | **BROKEN** — returns NULLs instead of matches | `SELECT * FROM t1 LEFT JOIN t2 ON ...` → all NULLs |
| `RIGHT JOIN` | **BROKEN** — returns empty | |
| `FULL JOIN` | **BROKEN** — returns empty | |
| `NATURAL JOIN` | **BROKEN** — returns cross-join | |
| Scalar subquery | **BROKEN** — returns NULL | `SELECT a, (SELECT d FROM t2 WHERE c=t1.a) FROM t1` |
| `EXISTS` subquery | **BROKEN** — returns empty | |
| Comma-join with derived table | **BROKEN** — "no such table: " | `SELECT * FROM t1, (SELECT ...)` |
| Window functions | **BROKEN** — returns NULL | `row_number() OVER (...)` |
| FTS3/4/5 | **NOT IMPLEMENTED** | 55+ fts test files fail |
| RIGHT/FULL JOIN execution | **NOT IMPLEMENTED** | Parsed but not executed |
| JSON functions (partial) | **MISSING** | json_valid, json_remove, json_type |
| WAL mode | **NOT IMPLEMENTED** | Rollback journal only |

### Architecture (verified)
- **13 internal packages**, correctly layered (SOLID test passes).
- `internal/exec/` has 13 files, 10,481 total lines. Biggest: `select.go`
  (2999), `alter.go` (2188), `ddl.go` (1398), `expression.go` (1195).
- No panics, no timeouts in the full suite (~13s).

### Performance (M4 Pro, benchtime=1000x)
| Benchmark | Current | Target | Status |
|-----------|---------|--------|--------|
| TestSQLiteSuite | ~13s | <60s | **MET** |
| BenchmarkInsert | ~2100-2900 ns/op | <2000 | Close |
| BenchmarkSelect | ~88-98K ns/op | <100K | **MET** |

---

## Development Principles (Binding)

1. **1002/1002 is the goal** — the "unsupported" skip list is a tracking
   mechanism, never an acceptable end state. Every test that passes on SQLite
   must pass on Frigolite.
2. **Test surface is sacred** — never weaken an assertion or delete a test to
   pass. Setup/teardown MAY change; functional scope MAY NOT.
3. **Go stdlib first** — no CGO, no external Go modules, no `sqlite3` at runtime.
4. **SQLite is the oracle** — `/usr/bin/sqlite3` (v3.51.0) MAY be used at
   *test-generation time* to capture expected output; never at *test-run time*.
   The SQLite C source at `/Users/muaddib/dev/sqlite/src/` is the behavioural
   spec.
5. **SOLID design** — new subsystems get their own `internal/` package; update
   `internalLayers` in `frigolite_solid_test.go` when adding packages. One
   responsibility per package.
6. **Complexity gate** — target cognitive complexity **<15** per function.
   Gate stays at 90 during feature work (G01–G12); enforced in G13.
7. **Regression prevention** — after each step: `go build ./...` + the step's
   verify command. After each phase: `go test -run TestSOLID_ ./...` + full suite.
8. **Commit after every step** — see "Per-Step Protocol" below.

### Per-Step Protocol (MANDATORY for every step in every phase)

After completing each numbered step within a phase:

1. **Run the verify command** for that step (from the `Verify` section).
2. **Run `go build ./...`** — must compile.
3. **Commit** with message: `G<NN>.<step>: <description of what changed>`.
4. **Update this plan** — mark the step done (change `[ ]` to `[x]`), update
   the "Live Metrics" table with the new pass count, and note any findings or
   deviations in the step's "Notes" line. This is how the next agent (or
   continuation turn) knows exactly where things stand.

This ensures: every step is independently verifiable, every change is
recoverable, and the plan always reflects ground truth.

---

## Phase Dependency Chain

```
G01  JOIN & Subquery Engine Fix     ← highest impact: flips hundreds of tests
 │
 ├── G02  Expression & Type Affinity  ← fixes e_* and expr* tests
 │
 ├── G03  SQL Functions Completion    ← fixes json1, func, date/time
 │
 ├── G04  Constraint Enforcement      ← fixes UNIQUE/NOT NULL/CHECK/FK
 │
 ├── G05  Window Functions            ← fixes window* (17 skipped + 3 active)
 │
 ├── G06  ALTER TABLE Completeness    ← fixes altertab* (3 skipped + active)
 │
 ├── G07  Query Planner & ANALYZE     ← fixes where*, analyze*, eqp*
 │
 ├── G08  ATTACH / DETACH             ← fixes attach* (many skipped)
 │
 ├── G09  FTS3/4/5                   ← fixes fts3*/fts4*/fts5* (~76 files)
 │
 ├── G10  Virtual Tables & Remaining  ← fixes vtab*, bestindex*, misc
 │
 ├── G11  Corruption & Edge Cases     ← fixes corrupt*, tkt*, bigfile*
 │
 ├── G12  Remove All Skip-List Entries ← zero skips, fix remaining
 │
 └── G13  Quality & SOLID Final       ← complexity <15, full green, perf
```

**Sequential order**: G01 → G02 → G03 → G04 → G05 → G06 → G07 → G08 →
G09 → G10 → G11 → G12 → G13

---

## Live Metrics (updated after each step)

| Phase | Step | Description | PASS/Total | Delta | Notes |
|-------|------|-------------|------------|-------|-------|
| (start) | — | Baseline measurement | 213/865 | — | 75.4% failure rate |

---

## Key Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source | `/Users/muaddib/dev/sqlite/src/` |
| SQLite FTS3/4 source | `/Users/muaddib/dev/sqlite/ext/fts3/` |
| SQLite FTS5 source | `/Users/muaddib/dev/sqlite/ext/fts5/` |
| SQLite TCL tests (source) | `ori/sqlite/test/` |
| Frigolite test data (JSON) | `testdata/*.json` (1002 files) |
| Frigolite harness | `frigolite_harness_test.go` |
| Frigolite test helpers | `frigolite_test.go` |
| SOLID tests | `frigolite_solid_test.go` |
| Quality gates | `Makefile` (`make quality`) |
| sqlite3 binary (oracle) | `/usr/bin/sqlite3` (v3.51.0) |
| Internal packages | `internal/` (13 packages) |
| Exec engine (main) | `internal/exec/select.go` (2999 lines), `internal/exec/engine.go` (703) |
| JOIN execution | `internal/exec/select.go` — `execJoins` (~line 800) |
| Parser | `internal/sql/parser.go`, `internal/sql/ast.go` |
| Expression eval | `internal/exec/expression.go` |

---

## Common Verify Commands

```bash
# Full harness suite (file-level results)
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v . 2>&1 | \
  grep -E "^    --- (PASS|FAIL): TestSQLiteSuite/[^/]+$" | \
  awk '/PASS/{p++} /FAIL/{f++} END{printf "PASS=%d FAIL=%d TOTAL=%d\n",p,f,p+f}'

# Filtered harness (specific test file)
FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s . 2>&1 | tail -30

# SOLID architecture
go test -run TestSOLID_ -count=1 ./...

# Quality gate
make quality

# Build
go build ./...

# Specific sub-test (e.g. select1-17.1)
FRIGOLITE_TEST=select1 go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | grep "17.1"
```

---

## G01 — JOIN & Subquery Engine Fix (HIGHEST IMPACT)

> **goa goal**: `G01: JOIN & subquery engine fix — USING/LEFT/RIGHT/FULL/NATURAL joins, scalar subqueries, EXISTS, derived tables in comma-joins`
>
> **Objective**: Fix the 7 known-broken JOIN/subquery patterns. This is the
> single highest-impact phase: these bugs cascade into "no such table" and
> "result mismatch" errors across hundreds of tests.
>
> **Completion criterion**: At least 350/865 tests pass (up from 213). No new
> regressions. `go build ./...` passes. SOLID test passes.
>
> **SQLite reference**: `src/where.c` (WHERE-loop builder, join planning),
> `src/select.c` (SELECT statement compilation, aggregate handling, subquery
> flattening), `src/expr.c` (expression evaluation, subquery resolution).

### Problem Analysis (verified by isolated tests)

Seven patterns are broken:

1. **`JOIN ... USING`** — returns empty result set. The executor's
   `buildJoinedRow`/`execJoins` doesn't apply USING conditions correctly.
2. **`LEFT JOIN`** — returns NULLs for rows that should match. The ON condition
   evaluation for correlation (e.g., `t1.a = t2.c`) doesn't resolve correlated
   column references between the two join sides.
3. **`RIGHT JOIN`** — returns empty. Not implemented in executor.
4. **`FULL JOIN`** — returns empty. Not implemented in executor.
5. **`NATURAL JOIN`** — returns cross-join result (no join condition applied).
6. **Scalar subquery** — `SELECT a, (SELECT d FROM t2 WHERE c=t1.a) FROM t1`
   returns NULL for the subquery column. Correlated column resolution fails.
7. **Comma-join with derived table** — `SELECT * FROM t1, (SELECT ...)` fails
   with "no such table: " (empty name).

### Step G01.1 — Map the current JOIN execution code

**Objective**: Read and document the current join execution path before making changes.

- [ ] Read `internal/exec/select.go` — locate `execJoins`, `buildJoinedRow`,
      `collectUsingColumns`, `buildLeftJoinRow`. Document line numbers and data flow.
- [ ] Read `internal/sql/ast.go` — `JoinClause`, `JoinType`, `CommaJoin`,
      `Using` fields. Document the AST shape.
- [ ] Read `internal/sql/parser.go` — `parseJoinClause`, `parseJoinType`,
      `parseNaturalJoinType`. Confirm what's parsed vs what's executed.
- [ ] Write a debug test that exercises each of the 7 broken patterns and
      records the current (wrong) output. Keep it as a regression baseline.

**Verify**:
```bash
go test -run TestDebugJoins -v -count=1 -timeout 10s . 2>&1
```
**Commit + update plan**: Commit the debug test. Update Live Metrics. Mark step done.

### Step G01.2 — Fix comma-join with derived table (empty "no such table")

**Objective**: `SELECT * FROM t1, (SELECT * FROM t2)` must work.

- [ ] **Reproduce**: Create a focused test: `SELECT * FROM t1, (SELECT * FROM t2 WHERE y=2)`.
- [ ] **Localize**: In `execJoins` (select.go), find where `CommaJoin: true` is
      handled. The derived table (subquery in FROM) is likely not recognized as
      a table source — it's being looked up as a regular table by name, and the
      subquery's alias/name is empty → "no such table: ".
- [ ] **Fix**: In the comma-join path, check if `join.Table` is a subquery
      (`Subquery` field in the AST). If so, materialize it (like the main FROM
      subquery path) and use the result as the right side of the join.
- [ ] **Test**: The reproduction test must return correct results.

**SQLite ref**: `src/select.c` `sqlite3SrcListLookup()` — SQLite treats
subqueries in FROM as virtual tables that are materialized.

**Verify**:
```bash
FRIGOLITE_TEST=select1 go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | grep -E "17\.[0-9]"
go build ./...
```
**Commit + update plan**: `G01.2: fix comma-join with derived table`. Update Live Metrics.

### Step G01.3 — Fix JOIN USING

**Objective**: `SELECT * FROM t1 JOIN t2 USING(a)` returns matched rows.

- [ ] **Reproduce**: `SELECT * FROM t1 JOIN t2 USING(a)` with t1(a,b), t2(a,c).
- [ ] **Localize**: `execJoins` → `collectUsingColumns` → the ON condition
      generated for USING. The join condition for USING(a) is `t1.a = t2.a`. But
      the executor likely fails to evaluate this, OR the column name resolution
      fails because both tables have a column named "a".
- [ ] **Fix**: Ensure USING generates a proper join condition and that column
      resolution distinguishes `t1.a` from `t2.a` by table qualifier, not just
      by column name.
- [ ] **Verify**: USING join returns the correct matched rows, with the USING
      column appearing once in the output.

**SQLite ref**: `src/select.c` — USING clause adds equality predicates and
collapses the joined columns.

**Verify**:
```bash
FRIGOLITE_TEST=select1 go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | tail -10
go build ./...
```
**Commit + update plan**: `G01.3: fix JOIN USING`. Update Live Metrics.

### Step G01.4 — Fix LEFT JOIN (correlation)

**Objective**: `SELECT * FROM t1 LEFT JOIN t2 ON t1.a=t2.c` matches correctly.

- [ ] **Reproduce**: `SELECT * FROM t1 LEFT JOIN t2 ON t1.a=t2.c` with data
      where matches exist. Current: returns NULLs. Expected: matched rows.
- [ ] **Localize**: The LEFT JOIN path evaluates the ON condition. The ON
      condition references columns from BOTH tables (e.g., `t1.a = t2.c`). The
      current evaluator likely can't resolve correlated column references across
      the two row sources during the join condition check.
- [ ] **Fix**: The join condition evaluation must have access to BOTH the left
      row and the right row simultaneously. Create a combined row context
      (column map) that includes columns from both tables, then evaluate the ON
      expression against it.
- [ ] **Verify**: LEFT JOIN returns matched rows + unmatched left rows with NULLs.

**SQLite ref**: `src/where.c` — join condition is a WHERE-like filter applied
per row-pair. `src/select.c` — LEFT JOIN NULL-padding.

**Verify**:
```bash
FRIGOLITE_TEST=join go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | tail -20
go build ./...
```
**Commit + update plan**: `G01.4: fix LEFT JOIN correlation`. Update Live Metrics.

### Step G01.5 — Implement RIGHT JOIN and FULL JOIN

**Objective**: `RIGHT JOIN` and `FULL JOIN` work correctly.

- [ ] **Reproduce**: `SELECT * FROM t1 RIGHT JOIN t2 ON t1.a=t2.c` and
      `SELECT * FROM t1 FULL JOIN t2 ON t1.a=t2.c`.
- [ ] **Approach**: RIGHT JOIN = LEFT JOIN with swapped operands. FULL JOIN =
      LEFT JOIN + unmatched right rows. Implement both by reusing the fixed
      LEFT JOIN logic from G01.4.
- [ ] **Implementation**:
  - RIGHT JOIN: swap left/right tables, do LEFT JOIN, swap columns back.
  - FULL JOIN: do LEFT JOIN, then iterate the right table for unmatched rows
    (right rows where no left match exists), pad left with NULLs.
- [ ] **Verify**: Both return correct results matching SQLite.

**SQLite ref**: SQLite added RIGHT/FULL JOIN in 3.39.0.
`src/where.c` — `WHERE_RIGHT_JOIN` handling.

**Verify**:
```bash
FRIGOLITE_TEST=join go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | tail -20
go build ./...
```
**Commit + update plan**: `G01.5: implement RIGHT/FULL JOIN`. Update Live Metrics.

### Step G01.6 — Fix NATURAL JOIN

**Objective**: `SELECT * FROM t1 NATURAL JOIN t2` matches on common columns.

- [ ] **Reproduce**: `SELECT * FROM t1 NATURAL JOIN t2` with t1(a,b), t2(a,c).
      Current: cross-join. Expected: match on column `a`.
- [ ] **Fix**: NATURAL JOIN = INNER JOIN with USING on all common column names.
      When `JoinType == "NATURAL"`, auto-generate USING conditions for columns
      that exist in both tables.
- [ ] **Verify**: Returns matched rows, common columns collapsed.

**SQLite ref**: `src/select.c` — NATURAL JOIN converts to USING on all common
columns during parsing.

**Verify**:
```bash
FRIGOLITE_TEST=join go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | tail -20
go build ./...
```
**Commit + update plan**: `G01.6: fix NATURAL JOIN`. Update Live Metrics.

### Step G01.7 — Fix scalar subqueries (correlated)

**Objective**: `SELECT a, (SELECT d FROM t2 WHERE c=t1.a) FROM t1` works.

- [ ] **Reproduce**: Scalar correlated subquery returns NULL instead of value.
- [ ] **Localize**: In `evalExpr` / `evalScalarSubquery` (expression.go or
      select.go). The subquery is evaluated, but the correlated column `t1.a`
      from the outer query is not passed into the subquery's context. The
      subquery executes against a fresh context without the outer row.
- [ ] **Fix**: Implement correlated subquery context: when evaluating a scalar
      subquery, pass the outer row's column map into the subquery's evaluation
      context so `t1.a` resolves to the outer row's value.
- [ ] **Verify**: Scalar subquery returns correct correlated values.

**SQLite ref**: `src/select.c` — correlated subqueries are not flattened; they
execute with access to the outer query's row via `pParse->pEList` resolution.

**Verify**:
```bash
FRIGOLITE_TEST=select go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | tail -20
go build ./...
```
**Commit + update plan**: `G01.7: fix scalar subquery correlation`. Update Live Metrics.

### Step G01.8 — Fix EXISTS subqueries

**Objective**: `SELECT a FROM t1 WHERE EXISTS(SELECT 1 FROM t2 WHERE c=t1.a)` works.

- [ ] **Reproduce**: EXISTS subquery returns empty result.
- [ ] **Localize**: The WHERE clause evaluation path. EXISTS is a boolean
      expression — it should be true if the subquery returns at least one row.
      The current path likely treats EXISTS as a table name lookup or fails to
      evaluate the subquery.
- [ ] **Fix**: Implement EXISTS evaluation in `evalExpr`: execute the subquery
      (with correlated context from G01.7), return true if ≥1 row, false otherwise.
- [ ] **Verify**: EXISTS returns correct boolean filtering.

**SQLite ref**: `src/expr.c` — `sqlite3ExprIfTrue` / EXISTS handling.

**Verify**:
```bash
FRIGOLITE_TEST=existsexpr go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 30s . 2>&1 | tail -20
go build ./...
```
**Commit + update plan**: `G01.8: fix EXISTS subquery`. Update Live Metrics.

### Step G01.9 — Run full suite, measure impact, fix regressions

- [ ] Run the full harness suite, count PASS/FAIL.
- [ ] For any regressions (previously passing tests now failing), bisect and fix.
- [ ] Target: **≥350/865 PASS** (was 213). If not met, investigate remaining
      JOIN-related failures.

**Verify**:
```bash
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v . 2>&1 | \
  grep -E "^    --- (PASS|FAIL): TestSQLiteSuite/[^/]+$" | \
  awk '/PASS/{p++} /FAIL/{f++} END{printf "PASS=%d FAIL=%d TOTAL=%d\n",p,f,p+f}'
go test -run TestSOLID_ -count=1 ./...
go build ./...
```
**Commit + update plan**: `G01.9: full suite measurement + regression fixes`. Update Live Metrics with final count.

---

## G02 — Expression & Type Affinity

> **goa goal**: `G02: expression evaluation and type affinity — fix e_* tests, expr*, type coercion, COLLATE, comparison`
>
> **Objective**: Fix expression evaluation bugs causing "result mismatch" in
> the e_* and expr* test files. Correct type affinity, comparison semantics,
> COLLATE, and edge cases.
>
> **Completion criterion**: At least 400/865 tests pass. No new regressions.
>
> **SQLite reference**: `src/expr.c` (expression evaluation), `src/resolve.c`
> (name resolution), `src/vdbeapi.c` (affinity application).

### Step G02.1 — Baseline e_* and expr* failures

- [ ] Run `FRIGOLITE_TEST=e_ go test ...` and `FRIGOLITE_TEST=expr go test ...`
- [ ] Categorize failures: type affinity, comparison, COLLATE, arithmetic, CAST.
- [ ] Document top 5 failure patterns.
- [ ] **Commit + update plan**.

### Step G02.2 — Fix type affinity in expressions

**Problem**: SQLite applies type affinity (TEXT, NUMERIC, INTEGER, REAL, BLOB)
to values before comparison and arithmetic. Frigolite may not apply affinity
correctly in all cases.

- [ ] Identify affinity bugs by comparing with SQLite output.
- [ ] Fix `applyAffinity` / column affinity application in `expression.go`.
- [ ] **Verify**: `FRIGOLITE_TEST=affinity go test ...`
- [ ] **Commit + update plan**.

### Step G02.3 — Fix comparison semantics (NULL ordering, type coercion)

**Problem**: NULL handling in comparisons, numeric vs text comparison ordering.

- [ ] Test NULL comparison: `NULL = NULL` → NULL, `NULL IS NULL` → true.
- [ ] Test numeric/text comparison: `'1' < '2'` vs `1 < 2`.
- [ ] Fix `compareValues` / `evalBinaryOp` in expression.go.
- [ ] **Verify**: `FRIGOLITE_TEST=expr go test ...`
- [ ] **Commit + update plan**.

### Step G02.4 — Fix COLLATE clause support

- [ ] **Verify**: `FRIGOLITE_TEST=collate go test ...`
- [ ] **Commit + update plan**.

### Step G02.5 — Fix CAST expression

- [ ] Ensure CAST applies affinity and type conversion correctly.
- [ ] **Verify**: `FRIGOLITE_TEST=cast go test ...`
- [ ] **Commit + update plan**.

### Step G02.6 — Full suite measurement

- [ ] Run full suite, measure PASS count, fix regressions.
- [ ] Target: **≥400/865 PASS**.
- [ ] **Commit + update plan**.

---

## G03 — SQL Functions Completion

> **goa goal**: `G03: complete SQL function library — JSON functions, date/time, missing scalar functions`
>
> **Objective**: Implement all missing SQL functions that harness tests require.
> 307 "unknown function" errors across ~10 function families.
>
> **Completion criterion**: Zero "unknown function" errors in the harness. At
> least 450/865 tests pass.
>
> **SQLite reference**: `src/func.c` (built-in functions), `ext/misc/json.c`
> (JSON1 extension), `src/date.c` (date/time functions).

### Step G03.1 — Audit missing functions

- [ ] Run full suite, extract all "unknown function" errors.
- [ ] Build a list: function name, type (scalar/aggregate/window), count.
- [ ] **Commit + update plan** with the complete list.

### Step G03.2 — Implement JSON functions (json_valid, json_remove, json_type, json_extract, json_array_length, json_array_insert, json_quote, json_error_position)

- [ ] Create/extend `internal/function/json.go` (or `json1.go`).
- [ ] Reference: `/Users/muaddib/dev/sqlite/ext/misc/json.c`.
- [ ] Implement each: json_valid, json_remove, json_type, json_extract,
      json_array_length, json_array_insert, json_quote, json_error_position,
      and any others in the JSON1 family found in Step G03.1.
- [ ] **Verify**: `FRIGOLITE_TEST=json1 go test ...`
- [ ] **Commit + update plan** after each function or family.

### Step G03.3 — Fix date/time functions

- [ ] Fix "unrecognized date/time" errors (14 occurrences).
- [ ] Review `internal/function/` date functions against `src/date.c`.
- [ ] **Verify**: `FRIGOLITE_TEST=date go test ...` and
      `FRIGOLITE_TEST=strftime go test ...`
- [ ] **Commit + update plan**.

### Step G03.4 — Implement remaining missing scalar functions

- [ ] `changes()`, `octet_length()`, `intreal()`, `sqlite_compileoption_used()`,
      and others found in G03.1.
- [ ] **Verify**: full suite — zero "unknown function" errors.
- [ ] **Commit + update plan**.

### Step G03.5 — Full suite measurement

- [ ] Target: **≥450/865 PASS**.
- [ ] **Commit + update plan**.

---

## G04 — Constraint Enforcement

> **goa goal**: `G04: constraint enforcement — UNIQUE, NOT NULL, CHECK, FOREIGN KEY correctness`
>
> **Objective**: Fix constraint handling causing UNIQUE/NOT NULL/CHECK failures
> (~57 UNIQUE, 6 NOT NULL, 8 CHECK errors) and "expected error but got success"
> (1357 occurrences — many are constraint violations that should fail but succeed).
>
> **Completion criterion**: At least 500/865 tests pass.
>
> **SQLite reference**: `src/insert.c` (UNIQUE/NOT NULL checks),
> `src/update.c` (constraint checks during UPDATE), `src/fkey.c` (foreign keys).

### Step G04.1 — Audit constraint failures

- [ ] Run full suite, extract all constraint-related errors.
- [ ] Categorize: UNIQUE (expected vs unexpected), NOT NULL, CHECK, FK.
- [ ] **Commit + update plan**.

### Step G04.2 — Fix UNIQUE constraint enforcement

- [ ] Ensure INSERT/UPDATE with duplicate key in UNIQUE column fails with the
      correct error message format.
- [ ] Handle multi-column UNIQUE constraints.
- [ ] Handle UNIQUE with NULL (multiple NULLs allowed).
- [ ] **Verify**: `FRIGOLITE_TEST=unique go test ...` and
      `FRIGOLITE_TEST=conflict go test ...`
- [ ] **Commit + update plan**.

### Step G04.3 — Fix NOT NULL constraint enforcement

- [ ] **Verify**: `FRIGOLITE_TEST=notnull go test ...`
- [ ] **Commit + update plan**.

### Step G04.4 — Fix CHECK constraint enforcement

- [ ] Implement CHECK constraint evaluation during INSERT/UPDATE.
- [ ] **Verify**: `FRIGOLITE_TEST=check go test ...`
- [ ] **Commit + update plan**.

### Step G04.5 — Fix FOREIGN KEY enforcement

- [ ] Implement FK constraint checks (if tests require it).
- [ ] **Verify**: `FRIGOLITE_TEST=fkey go test ...`
- [ ] **Commit + update plan**.

### Step G04.6 — Full suite measurement

- [ ] Target: **≥500/865 PASS**.
- [ ] **Commit + update plan**.

---

## G05 — Window Functions

> **goa goal**: `G05: window functions — ROW_NUMBER, RANK, DENSE_RANK, LAG, LEAD, FIRST_VALUE, LAST_VALUE, NTH_VALUE, aggregate windows, FILTER`
>
> **Objective**: Implement full window function support. Remove all 17 window
> test files from the skip list and make them pass. Fix the 3 active window
> test files (win3*).
>
> **Completion criterion**: All window* test files pass. At least 520/865 tests pass.
>
> **SQLite reference**: `src/window.c` (canonical implementation).
> Key functions: `sqlite3WindowCodeStep()`, `windowAggFinal()`,
> `windowAggValue()`.
>
> **Test data**: `testdata/window1.json`–`windowE.json`, `windowerr.json`,
> `windowfault.json`, `windowpushd.json`, `win3*.json`.

### Step G05.1 — Remove window tests from skip list

- [ ] Remove all `window*` entries from `unsupportedTestFiles` in
      `frigolite_harness_test.go`.
- [ ] Run the window tests, capture all failures.
- [ ] **Commit + update plan**.

### Step G05.2 — Implement window function execution pipeline

- [ ] Create `internal/exec/window.go`.
- [ ] Implement the standard pipeline:
      1. Partition rows by PARTITION BY expressions.
      2. Sort each partition by ORDER BY expressions.
      3. Compute window-frame bounds.
      4. Evaluate window aggregate over the frame.
- [ ] **Verify**: `FRIGOLITE_TEST=window1 go test ...`
- [ ] **Commit + update plan**.

### Step G05.3 — Implement built-in window functions

- [ ] ROW_NUMBER, RANK, DENSE_RANK, LAG, LEAD, FIRST_VALUE, LAST_VALUE, NTH_VALUE.
- [ ] **Verify**: `FRIGOLITE_TEST=window go test ...`
- [ ] **Commit + update plan**.

### Step G05.4 — Implement aggregates-as-windows

- [ ] SUM, AVG, COUNT, MIN, MAX as window functions.
- [ ] **Verify**: window tests.
- [ ] **Commit + update plan**.

### Step G05.5 — Full suite measurement

- [ ] Target: **≥520/865 PASS**.
- [ ] **Commit + update plan**.

---

## G06 — ALTER TABLE Completeness

> **goa goal**: `G06: ALTER TABLE — RENAME correctness, ADD/DROP COLUMN, token-level rename in triggers/views`
>
> **Objective**: All altertab* test files pass (3 skipped + active failures).
>
> **Completion criterion**: All alter* tests pass. At least 540/865 tests pass.
>
> **SQLite reference**: `src/alter.c` — `sqlite3AlterRenameTable()`,
> `reloadTableSchema()`, `renameTokenFind()`.
>
> **Test data**: `testdata/altertab.json`, `altertab2.json`, `altertab3.json`,
> `alterlegacy.json`, `alterauth.json`, `altercorrupt.json`, `altercol*.json`.

### Step G06.1 — Remove alter tests from skip list

- [ ] Remove `altercorrupt`, `altertab2`, `altertab3` from `unsupportedTestFiles`.
- [ ] Run alter tests, capture failures.
- [ ] **Commit + update plan**.

### Step G06.2 — Fix token-level rename

- [ ] Use `internal/rename` package for token-level identifier replacement in
      trigger/view SQL.
- [ ] **Verify**: `FRIGOLITE_TEST=altertab go test ...`
- [ ] **Commit + update plan**.

### Step G06.3 — Fix ADD/DROP COLUMN

- [ ] **Verify**: `FRIGOLITE_TEST=altercol go test ...`
- [ ] **Commit + update plan**.

### Step G06.4 — Full suite measurement

- [ ] Target: **≥540/865 PASS**.
- [ ] **Commit + update plan**.

---

## G07 — Query Planner & ANALYZE

> **goa goal**: `G07: query planner — ANALYZE, cost-based index selection, EXPLAIN QUERY PLAN, auto-index`
>
> **Objective**: Index selection uses statistics. EQP reports correct plans.
> `analyze*`, `where*`, `eqp*` tests pass.
>
> **Completion criterion**: All analyze*, eqp* tests pass. At least 580/865 tests pass.
>
> **SQLite reference**: `src/analyze.c`, `src/where.c` (`whereLoopBuilder`),
> `src/wherecode.c`.

### Step G07.1 — Implement ANALYZE

- [ ] Create `sqlite_stat1` table, store per-index statistics.
- [ ] **Verify**: `FRIGOLITE_TEST=analyze go test ...`
- [ ] **Commit + update plan**.

### Step G07.2 — Cost-based index selection

- [ ] Use stats to choose between full-scan and index lookup.
- [ ] **Verify**: `FRIGOLITE_TEST=where go test ...`
- [ ] **Commit + update plan**.

### Step G07.3 — EXPLAIN QUERY PLAN

- [ ] Output `SEARCH ... USING INDEX` / `SCAN` correctly.
- [ ] **Verify**: `FRIGOLITE_TEST=eqp go test ...`
- [ ] **Commit + update plan**.

### Step G07.4 — Auto-index for joins

- [ ] Create temporary ephemeral indexes for join columns without an index.
- [ ] **Verify**: `FRIGOLITE_TEST=autoindex go test ...`
- [ ] **Commit + update plan**.

### Step G07.5 — Full suite measurement

- [ ] Target: **≥580/865 PASS**.
- [ ] **Commit + update plan**.

---

## G08 — ATTACH / DETACH

> **goa goal**: `G08: ATTACH/DETACH — multi-database dispatch, schema resolution, file-backed attached databases`
>
> **Objective**: All attach* tests pass (currently 93/181 pass).
>
> **Completion criterion**: All attach* tests pass. At least 620/865 tests pass.
>
> **SQLite reference**: `src/attach.c`, `src/build.c` (`sqlite3LocateTable`).
>
> **Test data**: `testdata/attach*.json`.

### Step G08.1 — Fix ATTACH encoding check

- [ ] Fix false positive "attached databases must use the same text encoding" (47 occurrences).
- [ ] **Verify**: `FRIGOLITE_TEST=attach go test ...`
- [ ] **Commit + update plan**.

### Step G08.2 — Fix multi-database schema resolution

- [ ] `db.table` notation resolves to the correct attached database.
- [ ] **Verify**: `FRIGOLITE_TEST=attach go test ...`
- [ ] **Commit + update plan**.

### Step G08.3 — Fix harness test ordering (if needed)

- [ ] Fix JSON test ordering or harness cleanup to handle section reordering.
- [ ] **Commit + update plan**.

### Step G08.4 — Full suite measurement

- [ ] Target: **≥620/865 PASS**.
- [ ] **Commit + update plan**.

---

## G09 — FTS3/4/5 (Full-Text Search)

> **goa goal**: `G09: FTS3/4/5 — shadow tables, segment store, MATCH query, BM25 ranking`
>
> **Objective**: Implement FTS3/4/5 virtual table module. All fts3*/fts4*/fts5*
> tests pass (~76 files total — the single largest feature gap).
>
> **Completion criterion**: All FTS test files pass. At least 700/865 tests pass.
>
> **SQLite reference**: `ext/fts3/` (FTS3/4 source), `ext/fts5/` (FTS5 source).
> Key files: `fts3_write.c` (index write), `fts3.c` (query), `fts5_index.c`.
>
> **Test data**: `testdata/fts3*.json`, `testdata/fts4*.json`, `testdata/fts5*.json`.

### Step G09.1 — Remove FTS tests from skip list, audit

- [ ] Remove all `fts3*`, `fts4*` entries from `unsupportedTestFiles`.
- [ ] Run FTS tests, capture all failures.
- [ ] **Commit + update plan**.

### Step G09.2 — Implement FTS shadow table architecture

- [ ] `CREATE VIRTUAL TABLE ft USING fts3(col)` creates `<ft>_content`,
      `<ft>_segdir`, `<ft>_segments`, `<ft>_stat` shadow tables.
- [ ] **Verify**: shadow tables created on FTS table creation.
- [ ] **Commit + update plan**.

### Step G09.3 — Implement FTS insert path (tokenize + write postings)

- [ ] Tokenize each row's text, write postings to the current segment.
- [ ] Update `<ft>_content` with rowid and column values.
- [ ] **Verify**: `FRIGOLITE_TEST=fts3aa go test ...`
- [ ] **Commit + update plan**.

### Step G09.4 — Implement MATCH query

- [ ] Parse MATCH expression (AND/OR/NOT/phrase/prefix).
- [ ] Look up postings, intersect/union doclists, return matching rowids.
- [ ] Rank by BM25 (FTS5) or older FTS3 ranking.
- [ ] **Verify**: `FRIGOLITE_TEST=fts3aa go test ...`
- [ ] **Commit + update plan**.

### Step G09.5 — Implement segment merge

- [ ] Merge small segments into larger ones (b-tree-style merge).
- [ ] **Verify**: `FRIGOLITE_TEST=fts3sort go test ...`
- [ ] **Commit + update plan**.

### Step G09.6 — FTS5-specific features

- [ ] Enhanced query syntax, column filters, `rank` function.
- [ ] **Verify**: `FRIGOLITE_TEST=fts5 go test ...`
- [ ] **Commit + update plan**.

### Step G09.7 — Full suite measurement

- [ ] Target: **≥700/865 PASS**.
- [ ] **Commit + update plan**.

---

## G10 — Virtual Tables & Remaining Features

> **goa goal**: `G10: virtual tables and remaining features — vtab modules, bestindex, WITHOUT ROWID, remaining missing features`
>
> **Objective**: Fix vtab*, bestindex*, without* tests. Implement remaining
> virtual table features and fix miscellaneous failures.
>
> **Completion criterion**: At least 750/865 tests pass.
>
> **SQLite reference**: `src/vtab.c`, `src/loadext.c`.

### Step G10.1 — Fix virtual table xBestIndex

- [ ] **Verify**: `FRIGOLITE_TEST=bestindex go test ...`
- [ ] **Commit + update plan**.

### Step G10.2 — Fix WITHOUT ROWID tables

- [ ] **Verify**: `FRIGOLITE_TEST=without go test ...`
- [ ] Fix WITHOUT ROWID execution.
- [ ] **Commit + update plan**.

### Step G10.3 — Implement remaining vtab modules

- [ ] `zipfile`, `dbstat`, `fsdir`, `json_each`, `json_tree` (if tests require).
- [ ] **Commit + update plan**.

### Step G10.4 — Full suite measurement

- [ ] Target: **≥750/865 PASS**.
- [ ] **Commit + update plan**.

### Step G10.5 — Implement missing features from the failing set

- [ ] Review all remaining "result mismatch" failures and fix them.
- [ ] **Commit + update plan**.

---

## G11 — Corruption & Edge Cases

> **goa goal**: `G11: corruption handling, edge cases, ticket tests`
>
> **Objective**: Fix corrupt* tests (14 files), tkt* tests (~73 files total),
> bigfile*/bigrow* tests. These require robust error handling, precise error
> messages, and edge-case correctness.
>
> **Completion criterion**: At least 850/865 tests pass.
>
> **SQLite reference**: Each `tkt*` file references a SQLite ticket number; the
> fix is documented in the SQLite commit that closed the ticket.

### Step G11.1 — Fix corruption handling (corrupt* tests)

- [ ] **Verify**: `FRIGOLITE_TEST=corrupt go test ...`
- [ ] **Commit + update plan**.

### Step G11.1b — Fix corruption message format

- [ ] Match SQLite's exact error messages for corrupt databases.
- "database disk image is malformed" (76 occurrences) — verify these are
  expected vs actual.
- [ ] **Commit + update plan**.

### Step G11.2 — Fix ticket tests (tkt*)

- [ ] Remove all `tkt*` entries from `unsupportedTestFiles`.
- [ ] Run tkt* tests, fix each one (they are isolated bug fixes).
- [ ] **Commit + update plan** after each batch of fixes.

### Step G11.3 — Fix bigfile/bigrow/bigsort tests

- [ ] **Verify**: `FRIGOLITE_TEST=big go test ...`
- [ ] **Commit + update plan**.

### Step G11.4 — Full suite measurement

- [ ] Target: **≥850/865 PASS**.
- [ ] **Commit + update plan**.

---

## G12 — Remove All Skip-List Entries

> **goa goal**: `G12: zero skip-list entries — all 1002 test files active and green`
>
> **Objective**: Remove ALL remaining entries from `unsupportedTestFiles` and
> `slowTestFiles`. Every one of the 1002 test files runs and passes.
>
> **Completion criterion**: 1002/1002 test files PASS. Zero skip-list entries.

### Step G12.1 — Remove remaining unsupported entries

- [ ] List all remaining entries in `unsupportedTestFiles`.
- [ ] Remove them one category at a time: threads, WAL, misc.
- [ ] Run tests for each category, fix failures.
- [ ] **Commit + update plan** after each category.

### Step G12.2 — Remove slow test entries

- [ ] Remove `emptytable`, `indexexpr1`, `joinD` from `slowTestFiles`.
- [ ] Ensure they complete within timeout.
- [ ] **Commit + update plan**.

### Step G12.3 — Final 1002/1002 verification

- [ ] Count: `grep -c` entries in skip lists → must be 0.
- [ ] Run full suite: all 1002 files PASS.
- [ ] **Commit + update plan**.

---

## G13 — Quality & SOLID Final

> **goa goal**: `G13: quality gate — complexity <15, full green, SOLID, performance`
>
> **Objective**: Enforce cognitive complexity <15 everywhere. Full green suite.
> SOLID architecture. Performance targets met.
>
> **Completion criterion**: `make quality` passes with threshold 15. `go test
> -count=1 ./...` all green. SOLID test passes. Benchmarks meet targets.

### Step G13.1 — Lower the complexity gate

- [ ] Edit `Makefile`: `gocognit -over 90` → `gocognit -over 15`,
      `gocyclo -over 40` → `gocyclo -over 15`.
- [ ] Run `make quality`, capture all offenders.
- [ ] **Commit + update plan**.

### Step G13.2 — Refactor each complexity offender

- [ ] For each function over complexity 15: split into smaller functions,
      extract helpers, use early returns, replace nested conditionals with
      guard clauses.
- [ ] Run tests after each refactor to ensure no behaviour change.
- [ ] **Commit + update plan** after each batch.

### Step G13.2b — SOLID import boundary verification

- [ ] `go test -run TestSOLID_ -count=1 ./...`
- [ ] **Commit + update plan**.

### Step G13.3 — Run staticcheck

- [ ] Fix all staticcheck warnings.
- [ ] **Commit + update plan**.

### Step G13.4 — Final full-green verification

- [ ] Full suite: `go test -count=1 -timeout 120s ./...`
- [ ] Benchmarks: `go test -bench=. -benchtime=1000x ./benchmarks/`
- [ ] **Commit + update plan**.

---

## Progress Tracking

| Phase | Description | Status | PASS/Total | Notes |
|-------|-------------|--------|------------|-------|
| (start) | Baseline | — | 213/865 | 75.4% failure rate |
| G01 | JOIN & Subquery Engine Fix | 🔲 Not started | | Highest impact |
| G02 | Expression & Type Affinity | 🔲 Not started | | |
| G03 | SQL Functions Completion | 🔲 Not started | | |
| G04 | Constraint Enforcement | 🔲 Not started | | |
| G05 | Window Functions | 🔲 Not started | | |
| G06 | ALTER TABLE Completeness | 🔲 Not started | | |
| G07 | Query Planner & ANALYZE | 🔲 Not started | | |
| G08 | ATTACH/DETACH | 🔲 Not started | | |
| G09 | FTS3/4/5 | 🔲 Not started | | Biggest feature gap |
| G10 | Virtual Tables & Remaining | 🔲 Not started | | |
| G11 | Corruption & Edge Cases | 🔲 Not started | | |
| G12 | Remove All Skip-List Entries | 🔲 Not started | | |
| G13 | Quality & SOLID Final | 🔲 Not started | | |

---

## How to Use This Plan (for the execution agent)

1. **Read the master plan** (this file) — focus on the Phase Dependency Chain
   and your assigned phase.
2. **Read the referenced SQLite C source** — it is the behavioural spec.
3. **Implement steps in order** — each step builds on the previous.
4. **Run the verify command after each step**, not just at the end.
5. **MANDATORY: Commit + update this plan after every step.**
   - Commit message: `G<NN>.<step>: <description>`.
   - Update the step's checkbox (`[ ]` → `[x]`).
   - Update the "Live Metrics" table with the new PASS count.
   - Note any findings or deviations.
6. **Run `make quality` + SOLID tests** before declaring the phase complete.
7. **Update the Progress Tracking table** when done.
