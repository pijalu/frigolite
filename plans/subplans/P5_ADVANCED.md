# Sub-Plan: P5 — Advanced SQL (8 sub-goals)

> **Prerequisite**: P1–P4 complete.
> **Packages**: 90 (largest tier)
> **⚠️ MEMORY WARNING**: FTS merge suites (fts4merge, fts4merge2, fts3*) cause OOM.
> NEVER run these in bulk. Profile and fix the memory leak first (G5.0).

---

## G5.0 — Memory Leak Investigation (PREREQUISITE for FTS)

### Goal
```
Objective: Identify and fix the memory leak that causes OOM when running FTS
merge test suites. Enable safe bulk test execution.
Completion criterion: go test -tags testgen ./testgen/fts3aux/ -count=1 -timeout 120s exits 0 (no OOM); RSS stays bounded.
Verify: go test -tags testgen ./testgen/fts3aux/ ./testgen/fts3expr/ -count=1 -timeout 120s
Fresh context: true
```

### Steps
1. **Profile the leak** — run a single FTS package with pprof.
   ```bash
   go test -tags testgen ./testgen/fts3aux/ -count=1 -memprofile=/tmp/mem.prof
   go tool pprof /tmp/mem.prof
   ```
   Commit: `G5.0.1: profile FTS memory leak with pprof`
2. **Fix the leak** — likely candidates: statement cache, pager/page-cache growth,
   btree cursor retention.
   Commit: `G5.0.2: fix memory leak in <component>`
3. **Verify bounded RSS** across multiple FTS packages.
   Commit: `G5.0.3: verify bounded RSS for FTS suites`

---

## G5.CTE — Common Table Expressions (WITH)

### Goal
```
Objective: WITH (non-recursive), WITH RECURSIVE, MATERIALIZED/NOT MATERIALIZED,
multiple CTEs, CTE in subquery.
Completion criterion: testgen with, withM PASS.
Verify: go test -tags testgen ./testgen/with/ ./testgen/withM/ -count=1 && go test -run TestP5CTE -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p5_cte_test.go`
- Simple CTE: `WITH x AS (SELECT 1) SELECT * FROM x`
- Multiple CTEs: `WITH x AS (...), y AS (...) SELECT ... FROM x JOIN y`
- Recursive: `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<10) SELECT * FROM c`
- MATERIALIZED / NOT MATERIALIZED hints
- CTE column aliases: `WITH c(a,b) AS (SELECT 1,2) SELECT * FROM c`
- CTE in subquery context

### Steps
1. **Write pre-test**. Commit: `G5.CTE.1: add CTE pre-test`
2. **Fix recursive CTE** — termination condition, UNION ALL vs UNION.
   Commit: `G5.CTE.2: fix recursive CTE evaluation`
3. **Fix CTE grammar** — rules 309–310 (wqlist), 318, 410 (wqitem with MATERIALIZED).
   Commit: `G5.CTE.3: implement CTE MATERIALIZED grammar`
4. **Run TCL tests**. Commit: `G5.CTE.N: CTE TCL tests green`

---

## G5.WINDOW — Window Functions

### Goal
```
Objective: OVER(), PARTITION BY, ORDER BY in OVER, window frames (ROWS/RANGE/
GROUPS, BETWEEN), FILTER clause, named windows (WINDOW clause), aggregate and
ranking window functions.
Completion criterion: testgen window–windowE, windowerr, windowfault, windowpushd PASS.
Verify: go test -tags testgen ./testgen/window/ ./testgen/windowA/ ./testgen/windowB/ ./testgen/windowC/ ./testgen/windowD/ ./testgen/windowE/ -count=1 && go test -run TestP5Window -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p5_window_test.go`
- ROW_NUMBER() OVER (ORDER BY col)
- RANK() / DENSE_RANK() OVER (PARTITION BY a ORDER BY b)
- LAG(col) / LEAD(col) / LAG(col, offset, default)
- FIRST_VALUE / LAST_VALUE / NTH_VALUE
- SUM(col) OVER (PARTITION BY a ORDER BY b)
- COUNT(*) OVER () — whole-table aggregate per row
- Frame: ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING
- Frame: RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
- FILTER (WHERE condition) — aggregate with filter
- Named window: `WINDOW w AS (PARTITION BY a)`

### Steps
1. **Write pre-test**. Commit: `G5.WINDOW.1: add window function pre-test`
2. **Implement window grammar** — rules 168–172, 266–267, 270, 390–394, 411 (15+ rules).
   This is the LARGEST grammar gap (see GRAMMAR_COMPLETENESS.md §3a).
   Commit: `G5.WINDOW.2: implement window function grammar (frame, filter, over)`
3. **Implement ranking functions** — ROW_NUMBER, RANK, DENSE_RANK, NTILE.
   Commit: `G5.WINDOW.3: implement ranking window functions`
4. **Implement window frame evaluation** — ROWS/RANGE/GROUPS framing.
   Commit: `G5.WINDOW.4: implement window frame evaluation`
5. **Implement LAG/LEAD/FIRST_VALUE/LAST_VALUE** — offset window functions.
   Commit: `G5.WINDOW.5: implement offset/value window functions`
6. **Run TCL tests**. Commit: `G5.WINDOW.N: window function TCL tests green`

---

## G5.PRAGMA — Pragmas

### Goal
```
Objective: All applicable pragmas — table_info, foreign_keys, journal_mode,
page_size, encoding, etc. Table-valued pragmas (pragma_table_info).
Completion criterion: testgen pragma, pragmafault PASS.
Verify: go test -tags testgen ./testgen/pragma/ ./testgen/pragmafault/ -count=1 && go test -run TestP5Pragma -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p5_pragma_test.go`
- PRAGMA table_info(t1)
- PRAGMA foreign_keys = ON/OFF
- PRAGMA journal_mode = DELETE/WAL/TRUNCATE
- PRAGMA page_size; PRAGMA page_size = 4096
- PRAGMA encoding
- PRAGMA user_version; PRAGMA user_version = 1
- PRAGMA index_list(t1)
- PRAGMA index_info(idx1)
- PRAGMA compile_options
- PRAGMA foreign_key_list(t1)
- Table-valued: SELECT * FROM pragma_table_info('t1')

### Steps
1. **Write pre-test**. Commit: `G5.PRAGMA.1: add pragma pre-test`
2. **Fix pragma grammar** — rules 285–301 (PRAGMA variants).
   Commit: `G5.PRAGMA.2: implement remaining PRAGMA grammar rules`
3. **Implement table-valued pragmas** — pragma_*() as table functions in FROM.
   Commit: `G5.PRAGMA.3: implement table-valued pragma functions`
4. **Run TCL tests**. Commit: `G5.PRAGMA.N: pragma TCL tests green`

---

## G5.FTS — Full-Text Search

### Goal
```
Objective: FTS3/4/5 basic — CREATE VIRTUAL TABLE USING fts3/fts4/fts5, MATCH
operator, ranking, snippet, highlight.
Completion criterion: fts3aux, fts3expr, fts3matchinfo PASS (start simple).
⚠️ Depends on G5.0 (memory leak fix).
Verify: go test -tags testgen ./testgen/fts3aux/ ./testgen/fts3expr/ -count=1 -timeout 120s
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p5_fts_test.go` — CREATE fts table, INSERT, MATCH query.
   Commit: `G5.FTS.1: add FTS pre-test`
2. **Fix FTS grammar** — MATCH operator, CREATE VIRTUAL TABLE USING fts*.
   Commit: `G5.FTS.2: implement FTS grammar (MATCH, virtual table)`
3. **Implement FTS tokenizer + index** — basic tokenizer, inverted index.
   Commit: `G5.FTS.3: implement FTS tokenizer and inverted index`
4. **Implement MATCH query execution** — boolean, phrase, prefix queries.
   Commit: `G5.FTS.4: implement MATCH query execution`
5. **Implement ranking/snippet** — bm25, snippet(), highlight().
   Commit: `G5.FTS.5: implement FTS ranking and snippet functions`
6. **Run TCL tests** (package-by-package, with timeout). Commit: `G5.FTS.N: FTS TCL tests green`

---

## G5.VTAB — Virtual Tables

### Goal
```
Objective: Virtual table module system — generate_series, sqlite_dbdata, csv,
and the xBestIndex/xFilter/xColumn interface.
Completion criterion: vtab–vtabL, bestindexA–G, generate_series functional.
Verify: go test -tags testgen ./testgen/vtab/ ./testgen/vtabA/ ./testgen/vtabB/ -count=1 && go test -run TestP5Vtab -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p5_vtab_test.go` — generate_series, CREATE VIRTUAL TABLE.
   Commit: `G5.VTAB.1: add virtual table pre-test`
2. **Fix vtab grammar** — rules 302, 406–408 (CREATE VIRTUAL TABLE args).
   Commit: `G5.VTAB.2: implement virtual table grammar`
3. **Fix generate_series** — ensure it works as a table-valued function.
   Commit: `G5.VTAB.3: fix generate_series virtual table`
4. **Run TCL tests**. Commit: `G5.VTAB.N: virtual table TCL tests green`

---

## G5.ATTACH — ATTACH / DETACH Database

### Goal
```
Objective: ATTACH DATABASE ... AS name, DETACH, schema-qualified table access
(main.t1, aux.t1), multi-database joins.
Completion criterion: testgen attach PASS.
Verify: go test -tags testgen ./testgen/attach/ -count=1 && go test -run TestP5Attach -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p5_attach_test.go`.
   Commit: `G5.ATTACH.1: add ATTACH pre-test`
2. **Implement ATTACH runtime** — multi-database support (schema list).
   SQLite ref: `src/attach.c`.
   Commit: `G5.ATTACH.2: implement ATTACH DATABASE multi-db support`
3. **Run TCL tests**. Commit: `G5.ATTACH.N: ATTACH TCL tests green`

---

## G5.JSON — JSON1 Extension

### Goal
```
Objective: JSON functions — json(), json_extract(), json_array(), json_object(),
json_type(), json_valid(), json_each(), -> and ->> operators.
Completion criterion: testgen json, jsonb PASS (if feasible).
Verify: go test -tags testgen ./testgen/json/ ./testgen/jsonb/ -count=1 && go test -run TestP5Json -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p5_json_test.go`.
   Commit: `G5.JSON.1: add JSON pre-test`
2. **Implement JSON parser** — basic JSON value model.
   Commit: `G5.JSON.2: implement JSON value parser`
3. **Implement json functions** — json_extract, json_array, json_object, etc.
   Commit: `G5.JSON.3: implement JSON1 extension functions`
4. **Run TCL tests**. Commit: `G5.JSON.N: JSON TCL tests green`
