# Frigolite — Master Plan: Feature-Complete SQLite Compatibility

> **Status**: ACTIVE — post-G03 (performance optimizations applied).
> **Target**: Frigolite is **feature-complete**, matching SQLite behaviour for
> all harness tests; **performant** (suite completes <60s, benchmarks within
> target); and **SOLID** (cognitive complexity <15 after feature freeze).
> **Execution**: Sequential, one phase = one goal, autonomous simple-execution
> model. Each phase is self-contained — an agent with no prior context must be
> able to implement it from this document plus the referenced source files.

---

## Current State Assessment (verified)

This section records the verified ground truth as of the rewrite. Every phase
below starts from here.

### Architecture
- **13 internal packages**, correctly layered (SOLID import-boundary test passes).
- Layer order: `util`/`value`/`auth`(0) → `storage`(1) → `pager`(2) →
  `btree`(3) → `sql`/`rename`(4) → `schema`/`function`/`vtab`/`fts`(5) →
  `exec`(6).
- **`internal/exec/engine.go` = 9,472 lines** in a single file. This is the
  dominant SOLID violation. It contains ALL execution logic: SELECT, INSERT,
  UPDATE, DELETE, all DDL, all PRAGMA handling, joins, subqueries, aggregates,
  CTE, etc.

### Performance (benchmarks, M4 Pro, benchtime=1000x)
| Benchmark | Current | Target | SQLite ref | Status |
|-----------|---------|--------|------------|--------|
| BenchmarkInsert | ~2,100-2,900 ns/op | < 2,000 ns/op | ~500 ns/op | Close but not met |
| BenchmarkSelect (1000 rows) | ~88,000-98,000 ns/op | < 100,000 ns/op | ~20,000 ns/op | **MET** |
| BenchmarkSelectWhere | ~117,000-120,000 ns/op | < 50,000 ns/op | ~12,000 ns/op | Not met (needs streaming) |
| Full test suite | **~37s** | < 60s | — | **MET** |

### Stability (post-G02/G03 fixes)
1. **All panics fixed** — subquery filter path (`rowPassesWhere` nil check) and
   pager page-in-range errors resolved in G02.
2. **Multi-level B-tree** — was limited to ~48K rows (interior page capacity).
   Now supports millions of rows via proper recursive split propagation and path-stack
   cursor traversal. This was a critical correctness bug discovered during G03
   performance work (the benchmark crashed at >48K inserts).
3. **No panics, no timeouts** — the full test suite runs to completion in ~37s
   with parallel test execution.
4. **No "out of range" errors** in btree/pager paths.

### Test Infrastructure
- `frigolite_harness_test.go` — the **test oracle**: reads 1,002 JSON files
  from `testdata/`, each converted from an SQLite TCL test.
- **Skip lists** in the harness:
  - `slowTestFiles`: 3 files (joinD, emptytable, indexexpr1) — too slow.
  - `unsupportedTestFiles`: ~121 files — features not yet implemented.
- Net: ~878 test files actively run (but crash before finishing).
- **DELETED**: `frigolite_sqlite_compat_test.go` (1,088 tests, 3 MB) —
  superseded by the JSON harness. Removed in this rewrite.

### Committed-File Review (G07 commit `d3827ec`)
Found and **removed**:
- `cannot-read` (0-byte junk — test artifact)
- `file:test.db2?vfs=tvfs2` (0-byte junk — ATTACH test artifact)
- `file:test2.db?8_3_names=1` (0-byte junk — filename test artifact)
- `.gitignore` updated to prevent recurrence (`cannot-read`, `file:*`).

### Features Already Implemented
- **Parser**: DDL (CREATE/DROP TABLE/INDEX/VIEW/TRIGGER/VIRTUAL TABLE),
  DML (INSERT/UPDATE/DELETE with conflict clauses), SELECT (WHERE, JOIN,
  GROUP BY, ORDER BY, LIMIT/OFFSET, DISTINCT, UNION, subqueries, CASE, CAST,
  EXISTS, IN, BETWEEN, LIKE, GLOB), EXPLAIN/EQP, PRAGMA, ATTACH/DETACH (parse),
  SAVEPOINT, VACUUM, REINDEX, ANALYZE (parse).
- **CTE**: `WITH` and `WITH RECURSIVE` (`execSelectCTE`, `execRecursiveCTE`).
- **Window**: `WindowDef` AST node exists (PARTITION BY); execution is partial.
- **RETURNING**: AST fields exist; execution partial.
- **Functions**: 90+ scalar/aggregate (incl. JSON_ARRAY, JSON_OBJECT, JSONB,
  trig, COMPRESS/UNCOMPRESS, CRC32, PRINTF, etc.).
- **FTS**: tokenizer, inverted index, MATCH query parser (skeleton).
- **vtab**: `generate_series` module.
- **rename**: extracted token-level rename package.
- **ATTACH**: `findTable` searches attached DBs (partial dispatch).

---

## Phase Dependency Chain

```
G01  Cleanup & Hygiene          ← quick; removes junk + dead tests
 │
G02  Engine Stabilization       ← fix panics; suite must complete
 │
G03  Performance                ← suite <60s; benchmarks meet targets
 │
G04  SOLID Decomposition        ← split engine.go; behaviour-preserving
 │
 ├── G05  Parser Completeness   ← WINDOW exec, FILTER, RETURNING
 │        │
 │        └── G06  ALTER TABLE  ← token-level rename (needs full parser)
 │
 ├── G07  Query Planner         ← ANALYZE, cost-based, EQP, auto-index
 │
 ├── G08  ATTACH/DETACH         ← multi-database dispatch
 │
 ├── G09  FTS3/4/5             ← shadow tables, segment merge
 │
 └── G10  Feature Completeness  ← remove skip-list entries one-by-one
      │
      G11  Quality & Final      ← complexity <15, full green
```

**Sequential order for a single agent stream:**
**G01 → G02 → G03 → G04 → G05 → G06 → G07 → G08 → G09 → G10 → G11**

G05/G06, G07, G08, G09 are independent after G04 but run sequentially to avoid
merge conflicts in the shared `engine.go`/exec package.

---

## Development Principles (Binding)

1. **Test surface is sacred** — never weaken an assertion or delete a test to
   pass. Setup/teardown MAY change; functional scope MAY NOT.
2. **Go stdlib first** — no CGO, no external Go modules, no `sqlite3` at
   runtime.
3. **SQLite is the oracle** — `/usr/bin/sqlite3` (v3.51.0) MAY be used at
   *test-generation time* to capture expected output; never at *test-run time*.
4. **SOLID design** — new subsystems get their own `internal/` package; update
   `internalLayers` in `frigolite_solid_test.go` when adding packages.
5. **Complexity gate** — target cognitive complexity **<15** per function. This
   is **deferred until after feature freeze** (G11). During G01–G10 the existing
   gate (90) stays so feature work is not blocked.
6. **Regression prevention** — after each phase: `go build ./...` +
   `go test -run TestSOLID_ ./...` + the phase's verify command.

---

## Phase Detail

Each phase below is self-contained: objective, current problem, step-by-step
instructions, files to touch, SQLite reference, and a verify command.

---

### G01 — Cleanup & Repository Hygiene

**Objective**: Remove dead tests and junk files; commit a clean baseline.

**Status**: ✅ DONE in this rewrite (staged, pending commit).

**Steps**:
1. `git rm -f frigolite_sqlite_compat_test.go` — deprecated compat tests.
2. `git rm` the 3 junk files: `cannot-read`, `file:test.db2?vfs=tvfs2`,
   `file:test2.db?8_3_names=1`.
3. Add `cannot-read` and `file:*` to `.gitignore`.
4. Update `AGENTS.md` test count and remove compat-test references.
5. Commit: `chore: remove deprecated compat tests and junk files`.

**Verify**:
```bash
go build ./... && echo "build OK"
go test -run TestSOLID_ -count=1 ./... && echo "SOLID OK"
git status --porcelain   # should be clean after commit
```

---

### G02 — Engine Stabilization (fix crashes)

**Objective**: The test suite (`TestSQLiteSuite`) must run to completion
without panicking, even if some sub-tests fail. No timeouts.

**Current problem**: Two crash classes prevent the suite from completing:

**Bug A — nil-pointer panic in subquery WHERE**:
`execSelectFromSubquery` (engine.go:~3379) calls `filterSubqueryRows`
(line ~3531), which calls `rowPassesWhere(where, rowMap, nil)` (line ~3538).
But `rowPassesWhere` (line ~5145) calls `e.evalBool(where, row)` where `row`
is typed `Row` but receives a `RowMap`. The `evalBool` path then dereferences
the wrong type → nil pointer panic.

**Fix A**: Make the WHERE evaluation for subquery materialisation use the same
row/column-resolution mechanism as normal table scans. Two options:
- **Option 1 (preferred)**: Convert `filterSubqueryRows` to build a synthetic
  `Row` (or pass `colDefs`) so `evalBool` resolves column references the same
  way it does for base-table scans. The function already has `colDefs`
  available in the caller (engine.go:~3370 builds `rowMap[colDefs[j].Name]`).
- **Option 2**: Add a dedicated `evalBoolRowMap(where, rowMap)` that resolves
  `ColumnRef` lookups against the map. Simpler but duplicates logic.

**Bug B — pager "page out of range"**:
During certain INSERT sequences (multi-row insert, large records, or
WITHOUT ROWID tables), the btree page-split allocates a page number beyond the
pager's known page count. Symptom: `pager: page 261 out of range (max 20)`.

**Fix B**: Trace the split path in `internal/btree/btree.go`. The issue is in
page allocation during `splitLeaf`/`splitInterior`. The new page number must be
`pager.PageCount()` (append), and the pager's page count must be incremented
atomically. Check:
- `btree.go` `splitNode` / `splitPage` — verify the new page number comes from
  `pager.AllocatePage()` not from a stale cached count.
- `pager.go` `AllocatePage()` — verify it increments the file header's page
  count and grows the cache map.

**SQLite reference**:
- Bug A: `/Users/muaddib/dev/sqlite/src/where.c` — SQLite evaluates WHERE on
  materialised subquery results using the same `WhereInfo` machinery as base
  tables; there is no separate "RowMap" code path.
- Bug B: `/Users/muaddib/dev/sqlite/src/btree.c` functions
  `allocateBtreePage()` and `balance()` — page allocation during splits.

**Files to modify**:
- `internal/exec/engine.go` — `filterSubqueryRows`, `rowPassesWhere`,
  `execSelectFromSubquery` (lines ~3370–3545, ~5145).
- `internal/btree/btree.go` — split/allocation path.
- `internal/pager/pager.go` — `AllocatePage` if needed.

**Steps**:
1. Reproduce Bug A: find a test case in `testdata/` that triggers
   `execSelectFromSubquery` with a WHERE clause (search for `SELECT * FROM
   (SELECT ...) WHERE`). Create a minimal hand-written test that reproduces the
   panic (test-first / RED).
2. Fix `filterSubqueryRows` to pass column definitions so `evalBool` can
   resolve references. Run the minimal test → GREEN.
3. Reproduce Bug B: search `testdata/` for the INSERT pattern that triggers
   "page out of range" (the error appears in the sample log). Create a minimal
   test that inserts enough rows to trigger a page split.
4. Fix the page-allocation path. Run the test → GREEN.
5. Run the full harness with a timeout: it must complete (even with failures).

**Verify**:
```bash
# Suite completes without panic (failures are OK in this phase)
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s . 2>&1 | tee /tmp/g02.log
! grep -q "panic:" /tmp/g02.log
! grep -q "out of range" /tmp/g02.log
grep -q "^FAIL\|^ok" /tmp/g02.log   # reaches a terminal state
```

---

### G03 — Performance

**Objective**: Full test suite completes in <60s. Benchmarks meet targets.

**Results (verified M4 Pro, benchtime=1000x)**:
| Metric | Before | After | Target | Status |
|--------|--------|-------|--------|--------|
| TestSQLiteSuite | >45s (timeout/crash) | **~37s** | <60s | **MET** |
| BenchmarkInsert | 3,582 ns/op | ~2,100-2,900 ns/op | <2,000 | Close but not met |
| BenchmarkSelect | 142,225 ns/op | ~88,000-98,000 ns/op | <100K | **MET** |
| BenchmarkSelectWhere | 175,413 ns/op | ~117,000-120,000 ns/op | <50K | Not met |

**What was done** (in execution order):

**1. Profiling and bottleneck identification:**
- Profiled with CPU and memory profiles (pprof). Found dominant cost was
  allocation pressure from per-row struct allocations, string decoding, and
  output row building.
- Identified GC/scheduling overhead consuming ~70% of CPU time.

**2. Statement caching in public API** (`frigolite.go`, `internal/exec/engine.go`):
- Changed `DB.Exec` and `DB.Query` to use `Engine.Prepare` (existing stmtCache).
- Added cache size limit (1000 entries) to prevent unbounded growth.

**3. EncodeRecord allocation elimination** (`internal/storage/storage.go`):
- Replaced per-value byte slice allocations with `encodeValueSize` +
  `encodeValueInto` that compute sizes first, then write directly into a single
  output buffer.
- Result: one allocation per record instead of N+1 allocations.

**4. Insert path optimization** (`internal/exec/engine.go`):
- Applied affinity in-place on the values slice instead of allocating a
  separate `affValues` slice.
- Added trigger-existence cache (`hasTriggersCache`) to avoid schema lookups
  on every INSERT for tables without triggers (cached per table, invalidated
  when triggers are created/dropped).
- Only builds the trigger RowMap when triggers actually exist for the table.

**5. SELECT * fast path + structRow reuse** (`internal/exec/engine.go`):
- Replaced per-row `&structRow{}` allocation with a single reusable structRow.
- Added SELECT * fast path: copies decoded values directly to output rows,
  skipping the `row.Get` map lookups and `UnwrapColumnValue` calls.
- Pre-allocates larger output capacity (1024 vs 256).

**6. DecodeRecordValuesFiltered stack allocation** (`internal/storage/storage.go`):
- Replaced dynamic `var serialTypes []uint64` with stack-allocated
  `var stackSerialTypes [16]uint64`, avoiding per-row heap allocation for
  records with up to 16 columns.

**7. Fast comparison path for WHERE** (`internal/exec/engine.go`):
- Added `fastEvalComparison` in `rowPassesWhere` that handles simple
  `ColumnRef OP Literal` (and `Literal OP ColumnRef`) comparisons directly,
  avoiding the full `evalExpr` → `evalBinaryOp` → `evalBinaryOpValues` chain.
- Added direct int64 fast path in `compareValuesWithCollate` for the common
  case of integer column vs integer literal.
- Both integer and non-integer fast paths available.

**8. Lexer allocation reduction** (`internal/sql/lexer.go`):
- `readIdent`: replaced `[]byte` buffer + `string(buf)` with direct string
  slice (`t.input[identStart:t.pos]`), avoiding one allocation per identifier.
- `readNumber`: fast path for simple integers uses direct string slice,
  avoiding byte buffer allocation. Complex numbers (hex, float, exponent,
  underscore separators) fall through to the original path.
- `readString`: fast path for strings without escaped quotes uses direct
  string slice. Strings with `''` escape sequences use the original buffer path.
- `simpleSingleCharToken`: replaced `string(ch)` allocation with pre-defined
  constant string literals.

**9. Parallel test execution** (`frigolite_harness_test.go`):
- Added `t.Parallel()` to per-file sub-tests. Each test has its own in-memory
  DB so concurrent execution is safe.
- Reduced suite wall-clock from ~60s to ~37s (~38% improvement).

**10. Multi-level B-tree (critical correctness fix)** (`internal/btree/btree.go`):
- The btree was limited to 2 levels (interior root → leaf children). Interior
  pages had no split logic, so the tree could not grow beyond ~48K rows.
- Rewrote insert path with proper recursive split propagation:
  - `InsertCell` delegates to `insertPage(rootPage, cell)` which returns
    `(splitKey, newSibling, error)`. If splitKey > 0, a new root is created.
  - `insertLeafPage`: direct insert or leaf split with propagation.
  - `insertInteriorPage`: routes to child, handles child split, splits itself
    if full.
- Rewrote cursor traversal to support multi-level trees:
  - Added `path []cursorPathEntry` stack to the cursor.
  - `descendToFirstLeaf` / `descendToFirstLeafFromCurrent` push entries as
    they descend through interior pages.
  - `navigateToNextChild` walks up the path stack to find the next sibling,
    then descends.
- Removed the old `insertIntoInterior`, `insertIntoInteriorChild`,
  `splitInteriorAndRetry`, `splitInteriorAndRetryWithCell`, `retryInsertFromRoot`
  dead code.
- Fixed `CellPointer` offset calculation in cursor navigation for interior pages.
- Verified: 1M rows inserted, scanned, and counted correctly.

**Steps not completed** (deferred to later phases for the remaining gap):
- **Index-based JOIN** (Step 1 in original plan): not implemented — deferred
  to G07 (Query Planner).
- **Pager read-copy elimination** (Step 2): the pager already returned pointer
  to cached page (no copy). Confirmed no change needed.
- **Lazy record decoding** (Step 3): `DecodeRecordValuesFiltered` exists but
  lazy decode was attempted and found net-negative for SelectWhere
  (re-parsing header outweighed savings). Disabled for now.

**Remaining gap analysis**:
- **Insert <2000**: would require parameterized prepared statements to avoid
  per-call SQL parsing (which dominates the ~2100-2900 ns/op).
- **SelectWhere <50K**: would require streaming result sets (instead of
  materializing all rows into a `[][]interface{}` slice). The per-row
  allocation of output slices (~117K out of ~175K baseline = 33% reduction)
  cannot be eliminated without changing the result API.

**Verify**:
```bash
go test -bench=. -benchtime=1000x ./benchmarks/ 2>&1 | tee /tmp/g03_bench.log
go test -run "^TestSQLiteSuite$" -count=1 -timeout 90s . 2>&1 | tee /tmp/g03.log
```

### G04 — SOLID Decomposition (engine.go)

**Objective**: Split `engine.go` (9,472 lines) into focused files. No
behaviour change. All tests still pass.

**Current problem**: One file holds ALL execution logic. Cognitive complexity
of individual functions is manageable (gate passes at 90), but the file is
unmaintainable and violates Single Responsibility.

**Rules**:
- **Behaviour-preserving** — move code, do not rewrite logic. No new features.
- One file per major responsibility. Target: no file > ~1,500 lines.
- Keep everything in `package exec` (no new package needed yet — extraction to
  sub-packages can happen later if import boundaries require it).
- Run the full test suite after EACH file split to catch regressions.

**Target file structure**:
| New file | Content moved from engine.go | Approx lines |
|----------|------------------------------|--------------|
| `engine.go` | `Engine` struct, `NewEngine`, `Exec` dispatch, shared helpers | ~800 |
| `select.go` | `execSelect`, `execSelectFrom*`, `execJoins`, `finalizeSelectResult`, aggregate handling | ~2,500 |
| `insert.go` | `execInsert`, `execInsertSelect`, `execInsertDefault`, `execInsertOnConflict` | ~1,200 |
| `update.go` | `execUpdate` | ~800 |
| `delete.go` | `execDelete`, FTS delete helpers | ~600 |
| `ddl.go` | `execCreateTable`, `execCreateTableAsSelect`, `execCreateIndex`, `execCreateView`, `execCreateTrigger`, `execCreateVirtualTable`, `execDrop*`, `execAlterTable*`, `execOtherDDL` | ~1,500 |
| `pragma.go` | `execPragma`, all PRAGMA case handling | ~800 |
| `subquery.go` | `execSelectFromSubquery`, `filterSubqueryRows`, `rowPassesWhere` | ~400 |
| `cte.go` | `execSelectCTE`, `execRecursiveCTE` | ~500 |
| `explain.go` | `execExplain`, `execExplainQueryPlan` | ~400 |
| `transaction.go` | `execBegin`, `execCommit`, `execRollback`, `execAttach`, `execDetach`, `execSavepoint`, `execAnalyze`, `execReindex`, `execVacuum` | ~600 |

**Steps**:
1. Create each new file with `package exec` and the relevant `import` block.
2. Move functions one group at a time (start with the most self-contained:
   `pragma.go`, then `transaction.go`, then `explain.go`).
3. After each move: `go build ./...` then run a fast test subset.
4. Move the big ones last: `select.go`, `insert.go`, `ddl.go`.
5. Resolve any duplicate helper functions (move shared helpers to `engine.go`
   or a new `helpers.go`).
6. Final: full test suite + SOLID test.

**SQLite reference**: `src/vdbe.c` is SQLite's giant opcode executor, but the
C is split across `insert.c`, `delete.c`, `update.c`, `select.c`, `trigger.c`,
`pragma.c`, etc. Frigolite mirrors this structure.

**Verify**:
```bash
go build ./... && echo "build OK"
go test -run TestSOLID_ -count=1 ./... && echo "SOLID OK"
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s . 2>&1 | tee /tmp/g04.log
# Same pass/fail count as end of G03 (no regressions)
make quality
```

---

### G05 — Parser Completeness (WINDOW, FILTER, RETURNING)

**Objective**: Window functions, FILTER clause, and RETURNING execute
correctly. `window*` and `with*` harness tests pass.

**Current problem**: `WindowDef` AST node exists but execution is partial.
`FILTER (WHERE ...)` for aggregates is not executed. `RETURNING` AST fields
exist but rows are not returned.

**Step 1 — Window function execution**:
- File: `internal/exec/engine.go` (→ `select.go` after G04).
- Implement the standard window-function pipeline:
  1. Partition rows by PARTITION BY expressions.
  2. Sort each partition by ORDER BY expressions.
  3. Compute window-frame bounds (default: UNBOUNDED PRECEDING to CURRENT ROW
     for ordered; whole partition for unordered).
  4. Evaluate window aggregate over the frame.
- Built-in window functions: `ROW_NUMBER()`, `RANK()`, `DENSE_RANK()`,
  `LAG()`, `LEAD()`, `FIRST_VALUE()`, `LAST_VALUE()`, `NTH_VALUE()`, plus
  aggregates used as window functions (`SUM`, `AVG`, `COUNT`, `MIN`, `MAX`).
- SQLite ref: `src/window.c` — the canonical implementation. Key functions:
  `sqlite3WindowCodeStep()`, `windowAggFinal()`.
- Test data: `testdata/window1.json`–`windowE.json`, `windowerr.json`.

**Step 2 — FILTER clause**:
- Syntax: `aggregate_func(...) FILTER (WHERE expr)`.
- File: parser (`internal/sql/parser.go`) — parse `FILTER` after an aggregate
  call in the SELECT list.
- Execution: when evaluating the aggregate, skip rows where the FILTER expr is
  false (applied *before* aggregation, not to the result).
- SQLite ref: `src/func.c` — `FILTER` is applied in the aggregate step.

**Step 3 — RETURNING clause**:
- Syntax: `INSERT/UPDATE/DELETE ... RETURNING col1, col2, *`.
- File: `internal/sql/ast.go` (fields exist), `internal/exec/insert.go` /
  `update.go` / `delete.go` (after G04).
- Execution: after modifying each row, evaluate the RETURNING column list
  against the row and yield it as a result row.
- SQLite ref: `src/upsert.c`, `src/update.c` — `RETURNING` uses an ephemeral
  table to collect rows.

**Verify**:
```bash
FRIGOLITE_TEST=window go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s . 2>&1 | tee /tmp/g05_window.log
FRIGOLITE_TEST=with go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s . 2>&1 | tee /tmp/g05_with.log
go test -run TestSOLID_ -count=1 ./...
```

---

### G06 — ALTER TABLE (token-level rename)

**Objective**: `ALTER TABLE ... RENAME TO` correctly updates all references in
triggers, views, and indexes. `altertab*` harness tests pass.

**Current problem**: String-regex rename fails when trigger/view bodies contain
unsupported syntax (window functions, CTEs, FILTER). Partial work done (P3 in
the old plan) — 5 tests fixed, ~55 remaining.

**Approach**: Use the `internal/rename` package's token-level processing.
SQLite re-parses the trigger/view SQL and replaces identifier *tokens*, not
substrings. This is robust against any syntax in the body.

**Steps**:
1. When `ALTER TABLE old RENAME TO new`:
   - For each trigger/view that references `old`, re-tokenize its stored SQL.
   - Use `rename.FindRenameTokens(sql, "old", "new")` to find identifier tokens
     matching `old` and replace with `new`.
   - Re-store the modified SQL.
2. Validate: `old` must not be a reserved keyword in the new name.
3. Update `sqlite_schema` rows for the table, its indexes, and referencing
   triggers/views.
4. Handle `legacy_alter_table` PRAGMA (string-only rename, no token walk).

**SQLite reference**: `src/alter.c` functions `sqlite3AlterRenameTable()`,
`reloadTableSchema()`, and `renameTokenFind()`. The token walk uses the parser
to find all `Token` objects equal to the old name.

**Test data**: `testdata/altertab.json`, `altertab2.json`, `altertab3.json`,
`alterlegacy.json`, `alterauth.json`, `altercorrupt.json`.

**Verify**:
```bash
FRIGOLITE_TEST=altertab go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s . 2>&1 | tee /tmp/g06.log
FRIGOLITE_TEST=alter go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .
```

---

### G07 — Query Planner & ANALYZE (cost-based)

**Objective**: Index selection uses statistics. `EXPLAIN QUERY PLAN` reports
`SEARCH ... USING INDEX` when appropriate. `analyze*` tests pass.

**Current problem**: The planner always does `SCAN t1` (full table scan). No
ANALYZE statistics are consumed. Auto-indexes for joins are not created.

**Step 1 — ANALYZE**:
- File: `internal/exec/engine.go` (→ a new `internal/stats` concept or
  `exec/analyze.go` after G04).
- `ANALYZE` creates/updates `sqlite_stat1` (and optionally `sqlite_stat4`).
  Store per-index: row count, distinct-value estimate, and histogram.
- SQLite ref: `src/analyze.c`.

**Step 2 — Cost-based index selection**:
- For a WHERE clause `col = ?`, if an index covers `col`, estimate cost as
  `estimatedRows / distinctValues` vs full-scan cost `estimatedRows`.
- Choose the cheaper plan.
- SQLite ref: `src/where.c` `whereLoopBuilder`, `src/wherecode.c`.

**Step 3 — EXPLAIN QUERY PLAN**:
- Output `SEARCH t1 USING INDEX idx (col=?)` when an index is used, `SCAN t1`
  for full scan. Match SQLite's exact output format.
- Test data: `testdata/eqp*.json`, `testdata/analyze*.json`.

**Step 4 — Automatic indexes (auto-index)**:
- For a join with no index on the join column, create a temporary ephemeral
  index. This turns O(N×M) into O(N log M).
- SQLite ref: `src/where.c` `whereLoopAddVirtualOne()` — automatic indexes.

**Verify**:
```bash
FRIGOLITE_TEST=analyze go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s . 2>&1 | tee /tmp/g07.log
FRIGOLITE_TEST=eqp go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .
FRIGOLITE_TEST=autoindex go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .
```

---

### G08 — ATTACH / DETACH (multi-database)

**Objective**: `ATTACH 'file.db' AS aux` makes `aux.t1` resolve to a table in
the attached database. `attach*` tests pass.

**Current problem**: `findTable` searches attached DBs but there is no real
multi-database connection — each attached DB needs its own pager/btree/schema.

**Steps**:
1. Extend `DB`/`Engine` to hold a map of `*Database` instances keyed by schema
   name (`main`, `temp`, and user-attached names).
2. Each `*Database` has its own pager + btree + schema manager.
3. `ATTACH 'path' AS name`: open a new pager for `path`, create a
   `*Database{name}`, add to the map.
4. `DETACH name`: close the pager, remove from map.
5. Table/view resolution: parse `schema.table` prefix; look up in the named
   database, falling back to `main` then `temp`.
6. Cross-database queries: `SELECT * FROM main.t1 JOIN aux.t2` must work.
7. Transaction semantics: by default only `main` participates in a
   transaction; attached DBs use their own (matches SQLite's behaviour).

**SQLite reference**: `src/attach.c` functions `sqlite3AttachDatabase()`,
`sqlite3DetachDatabase()`. Schema resolution in `src/build.c`
`sqlite3LocateTable()`.

**Test data**: `testdata/attach*.json`.

**Verify**:
```bash
FRIGOLITE_TEST=attach go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s . 2>&1 | tee /tmp/g08.log
```

---

### G09 — FTS3/4/5 (full-text search)

**Objective**: `CREATE VIRTUAL TABLE ft USING fts3/4/5(col)` creates a
searchable full-text index. `MATCH` queries return ranked results.
`fts3*`/`fts5*` tests pass.

**Current problem**: Tokenizer and inverted index exist (`internal/fts/`) but
there is no shadow-table architecture (the persistent segment store), no
segment merge, and limited query support.

**Steps**:
1. **Shadow tables**: an FTS table `ft` creates `<ft>_content`, `<ft>_segdir`,
   `<ft>_segments`, `<ft>_stat` shadow tables. These store the inverted index
   persistently in the same DB file.
2. **Insert path**: tokenize each row's text, write postings to the current
   segment. Update `<ft>_content` with the rowid and column values.
3. **MATCH query**: parse the MATCH expression (AND/OR/NOT/phrase/prefix),
   look up postings in the segments, intersect/union doclists, return matching
   rowids ranked by BM25 (FTS5) or the older FTS3 ranking.
4. **Segment merge**: merge small segments into larger ones (the b-tree-style
   merge in `fts3_write.c`).
5. **FTS5-specific**: enhanced query syntax, column filters, `rank` function.

**SQLite reference**: `ext/fts3/` (FTS3/4 source), `ext/fts5/` (FTS5 source).
Key files: `fts3_write.c` (index write), `fts3.c` (query), `fts5_index.c`.

**Test data**: `testdata/fts3*.json`, `testdata/fts5*.json`. Note: many FTS
tests are currently in `unsupportedTestFiles` — remove entries as features land.

**Verify**:
```bash
FRIGOLITE_TEST=fts3 go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 90s . 2>&1 | tee /tmp/g09.log
FRIGOLITE_TEST=fts5 go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 90s .
```

---

### G10 — Feature Completeness (remove skip-list entries)

**Objective**: Zero entries in `unsupportedTestFiles` and `slowTestFiles`. All
1,002 harness test files run and the failing ones are fixed.

**Current problem**: ~121 files are skipped because they need unimplemented
features. Each skip entry names the missing feature.

**Approach**: Work through `unsupportedTestFiles` in `frigolite_harness_test.go`
grouped by feature category. For each group:
1. Remove the skip entries for that group.
2. Run the tests — capture failures.
3. Implement the missing feature.
4. Re-run until green.
5. Move to the next group.

**Categories** (grouped for efficiency):

| Category | Files (examples) | Missing feature |
|----------|------------------|-----------------|
| WAL | `wal*` (3) | WAL journal mode. Decision: implement rollback-only is acceptable if tests are marked; else basic WAL. |
| Threads | `thread*` (5) | Concurrent access / locking. |
| Large data | `bigfile*`, `bigmmap`, `bigrow*`, `bigsort`, `zeroblob` (7) | Performance/correctness on large data. |
| Tickets | `tkt*` (~55) | Various isolated bugs — fix case-by-case. |
| Misc | `misc2`, `tempdb*`, `temptable*`, `unionall`, `affinity2`, `speed3`, `table*` (~10) | Various. |

**SQLite reference**: Each `tkt*` file references a SQLite ticket number; the
fix is documented in the SQLite commit that closed the ticket. Search
`src/` changelog or the test file header.

**Verify**:
```bash
# Count skip-list entries (must be 0)
grep -cE '^\s+"[a-z]' frigolite_harness_test.go   # adjust after refactor
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s . 2>&1 | tee /tmp/g10.log
! grep -q "FAIL" /tmp/g10.log
```

---

### G11 — Quality & Final Verification

**Objective**: Cognitive complexity <15 everywhere. Full green. SOLID enforced.

**Current problem**: Complexity gate is 90 (deferred during feature work).
`engine.go` and other files contain many complex functions that must be
refactored.

**Steps**:
1. **Lower the gate**: change `make quality` threshold from 90 to 15:
   - Edit `Makefile`: `gocognit -over 90` → `gocognit -over 15`.
   - Edit `Makefile`: `gocyclo -over 40` → `gocyclo -over 15`.
2. **Run the gate** — capture all offenders:
   ```bash
   gocognit -over 15 $(find . -name '*.go' ! -name '*_test.go' -not -path './cmd/*' -not -path './third_party/*')
   ```
3. **Refactor each offender** — split into smaller functions, extract helpers,
   use early returns, replace nested conditionals with guard clauses. Run
   tests after each refactor to ensure no behaviour change.
4. **Run staticcheck** — fix all warnings.
5. **Final full-green verification** (see below).

**Verify**:
```bash
# 1. Complexity gate < 15
make quality

# 2. SOLID architecture
go test -run TestSOLID_ -count=1 ./...

# 3. Full test suite — zero FAIL
go test -count=1 -timeout 120s ./... 2>&1 | tee /tmp/g11_full.log
! grep -q "FAIL" /tmp/g11_full.log

# 4. Benchmarks still meet targets (no perf regression from refactoring)
go test -bench=. -benchtime=1000x ./benchmarks/
```

---

## Key Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source | `/Users/muaddib/dev/sqlite/src/` |
| SQLite FTS3 source | `/Users/muaddib/dev/sqlite/ext/fts3/` |
| SQLite FTS5 source | `/Users/muaddib/dev/sqlite/ext/fts5/` |
| SQLite TCL tests (source) | `ori/sqlite/test/` |
| Frigolite test data (JSON) | `testdata/*.json` (1,002 files) |
| Frigolite harness | `frigolite_harness_test.go` |
| Frigolite test helpers | `frigolite_test.go` |
| Converters | `tools/convert_compat_json.py` |
| sqlite3 binary (oracle) | `/usr/bin/sqlite3` (v3.51.0) |
| SOLID tests | `frigolite_solid_test.go` |
| Quality gates | `Makefile` (`make quality`) |
| Internal packages | `internal/` (13 packages) |
| Exec engine (to decompose) | `internal/exec/engine.go` (9,472 lines) |

---

## Progress Tracking

| Phase | Description | Status | Notes |
|-------|-------------|--------|-------|
| G01 | Cleanup & Hygiene | ✅ Done (staged) | Removed compat tests + 3 junk files |
| G02 | Engine Stabilization | 🔲 Not started | Fix nil-pointer + pager crashes |
| G03 | Performance | 🔲 Not started | Suite <60s, benchmarks target |
| G04 | SOLID Decomposition | 🔲 Not started | Split engine.go into ~11 files |
| G05 | Parser (WINDOW, FILTER, RETURNING) | 🔲 Not started | |
| G06 | ALTER TABLE | 🔲 Not started | Token-level rename |
| G07 | Query Planner & ANALYZE | 🔲 Not started | Cost-based, EQP, auto-index |
| G08 | ATTACH/DETACH | 🔲 Not started | Multi-database |
| G09 | FTS3/4/5 | 🔲 Not started | Shadow tables, segment merge |
| G10 | Feature Completeness | 🔲 Not started | Remove skip-list entries |
| G11 | Quality & Final | 🔲 Not started | Complexity <15, full green |

---

## How to Use This Plan (for the execution agent)

1. **Read the master plan** (this file) — focus on the Dependency Chain and
   your assigned phase.
2. **Read the referenced SQLite C source** — it is the behavioural spec.
3. **Implement steps in order** — each step builds on the previous.
4. **Run the verify command after each step**, not just at the end.
5. **Run `make quality` + SOLID tests** before declaring the phase complete.
6. **Update the Progress Tracking table** when done.
7. **Commit with message** `GNN: <phase description>` to maintain the history.
