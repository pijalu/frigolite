# Frigolite — Master Plan

> **Status**: P0 in progress.
> **Current**: 268/1011 harness file PASS (26.5%), 13,360 sub-test PASS.
> **Goal**: All 1002 test files green, tcl2go pipeline as sole test approach.
>
> **Strategy**: Go TCL interpreter (`tools/tclconvert/tcl/`) executes TCL
> test files and captures ALL setup SQL. `tcl2go` generator (`tools/tcl2go/`)
> produces standalone Go `_test.go` files. Tests run via `go test ./testgen/...`.
> Old JSON harness (`frigolite_harness_test.go` + `testdata/*.json`) deprecated.
>
> **Reference**: SQLite C source at `/Users/muaddib/dev/sqlite/src/` is the spec.
> TCL tests at `ori/sqlite/test/*.test`.

---

## Pipeline

```
ori/sqlite/test/foo.test ──┐
                           │  tools/tcl2go/     testgen/foo/
                           ├─→ (TCL Interp  ──→ foo_test.go
                           │   + gen.go)        bar_test.go
ori/sqlite/test/bar.test ──┘                   util/
```

TCL interpreter (`tools/tclconvert/tcl/`) handles `db eval`, `db onecolumn`, loops,
variables, expressions. `tcl2go` generates one `_test.go` per TCL file, grouped by
package prefix (e.g. `select1.test` → package `select1`). Tests run in parallel
with Go compiler validation. No JSON parsing, no harness overhead.

---

## Current Baseline

| Metric | Harness (deprecated) | tcl2go (target) |
|--------|----------------------|-----------------|
| File-level PASS | 268/1011 (26.5%) | ≥300 |
| Sub-test PASS | 13,360 | TBD |
| Sub-test FAIL | 44,114 | TBD |
| Generated packages | — | 1 (select1) |

**Top failure patterns** (harness, ranked):
1. Result mismatch (~4000+) — engine bugs (expression eval, affinity, comparison)
2. "no such table" (~2000+) — P0: TCL interpreter missing setup SQL
3. Unknown function (~300) — missing JSON, date/time, misc functions
4. Parse/syntax error (~300) — parser gaps (FILTER, OVER, CTE, window specs)
5. UNIQUE constraint (~200) — constraint enforcement bugs

---

## Phase 0 — TCL to Go Test Pipeline (CURRENT)

**Goal**: all 1002 TCL files generate working Go tests. File PASS ≥300 via tcl2go.
JSON harness deprecated. **Completion**: ≥500 files have CREATE TABLE where missing,
file PASS ≥300/869.

### Status

| Component | File | Status |
|-----------|------|--------|
| TCL tokenizer | `tools/tclconvert/tcl/parser.go` (~100 lines) | ✅ Done |
| TCL interpreter | `tools/tclconvert/tcl/interp.go` (~800 lines) | ✅ Done (uncommitted fixes) |
| Expression evaluator | `tools/tclconvert/tcl/expr.go` (~400 lines) | ✅ Done |
| List helpers | `tools/tclconvert/tcl/list.go` (~80 lines) | ✅ Done |
| tcl2go entry point | `tools/tcl2go/main.go` (~160 lines) | ✅ Done |
| tcl2go generator | `tools/tcl2go/gen.go` (~320 lines) | ✅ Done |
| Python converter | `tools/convert_compat_json.py` | ❌ Deleted |
| JSON harness | `frigolite_harness_test.go` (~630 lines) | 🔶 To deprecate |

### Tasks

**Task 0.1 — Commit TCL interpreter changes** `[ ]`
- [ ] Review modified `tools/tclconvert/main.go` (output message improvement)
- [ ] Review modified `tools/tclconvert/tcl/interp.go` (added `join` command handler)
- [ ] Verify: `go build ./tools/tcl2go/...`
- [ ] **Commit** (`P0.1: fix tclconvert output, add join command to TCL interp`)

**Task 0.2 — Run tcl2go across all input files** `[ ]`
- [ ] Run `go run ./tools/tcl2go/` — processes all 1002 `.test` files
- [ ] Count generated files: `ls testgen/*/*_test.go | wc -l` — expect ≥500
- [ ] Verify no TCL interpreter panics or timeouts for any file
- [ ] **Commit** (`P0.2: generate all 1002 test files via tcl2go`)

**Task 0.3 — Fix common generator patterns** `[ ]`
- [ ] Run `go test ./testgen/... -count=1`, capture all failures to a file
- [ ] Categorize by pattern: result mismatch, missing SQL, syntax in SQL, etc.
- [ ] Fix `gen.go` for top-3 failure patterns:
  - [ ] Multi-statement SQL splitting (`splitSQL` edge cases)
  - [ ] Expected value formatting (braced vs quoted, TCL list flattening)
  - [ ] C API test detection (skip files with `sqlite3_prepare`, `do_malloc_test`, etc.)
- [ ] Fix TCL interpreter for missed SQL patterns:
  - [ ] `db transaction { ... }` blocks
  - [ ] Multi-connection tests (`sqlite3 db2 test.db`)
  - [ ] `db eval { SQL } SCRIPT` (per-row callback form — capture SQL, skip script)
- [ ] Iterate until `go test ./testgen/... -count=1` produces ≥300 file PASS
- [ ] **Commit** (`P0.3: fix tcl2go generator patterns, fix TCL interp gaps`)

**Task 0.4 — Phase out JSON harness** `[ ]`
- [ ] Verify tcl2go covers all 1002 files with no regressions from harness baseline
- [ ] Move `frigolite_harness_test.go` to `plans/archive/`
- [ ] Archive `testdata/*.json` to `testdata/archive/`
- [ ] Update `Makefile` build targets (remove JSON harness dependencies)
- [ ] Remove old hand-written compat tests (`frigolite_agg_test.go`, etc.)
- [ ] Verify: `go test ./testgen/... -count=1` runs all 1002 files
- [ ] **Commit** (`P0.4: phase out JSON harness, tcl2go is sole test approach`)

**Task 0.5 — Set new baseline** `[ ]`
- [ ] Record file PASS from `go test ./testgen/... -count=1`
- [ ] Record sub-test PASS/FAIL counts
- [ ] Update this document's metrics
- [ ] **Commit** (`P0.5: set new tcl2go baseline`)

---

## Phase 1 — Fix Engine Bugs

Goal: fix the highest-count failures exposed by generated tests. Each task follows
**measure → fix → verify → commit**. SQLite reference: `src/expr.c`, `src/resolve.c`,
`src/vdbeapi.c`, `src/vdbemem.c`.

### Task 1.1 — Type affinity, NULL handling, comparison `[ ]`

**Files:** `internal/exec/expression.go`, `internal/exec/engine.go`,
`internal/util/compare.go`, `internal/value/affinity.go`.

**SQLite reference:** `src/vdbemem.c` (`sqlite3VdbeMemApplyAffinity`), `src/vdbe.c`
(opcodes `Ne`, `Eq`, `Lt` with `SQLITE_NULLEQ` flag).

- [ ] **Fix blob arithmetic negation**: `SELECT -x'ce'` → `0`. In unary minus handler,
      if operand is `[]byte`, convert to float64 (empty/non-numeric → 0), return negated.
- [ ] **Fix REAL vs large-INTEGER comparison**: `3175546974276630385 < 3175546974276630385.0`
      → `1` (true). Convert int64 to float64 before comparing (matches SQLite `double`).
      **File:** `internal/util/compare.go`.
- [ ] **Fix affinity on INSERT**: Create `applyAffinity(value, affinity)` in new
      `internal/util/affinity.go`. Apply column affinity when inserting/updating:
      TEXT → `fmt.Sprintf`, INTEGER → try int64 conversion, REAL → float64,
      NUMERIC → try int64 then float64 then text, BLOB/NONE → no conversion.
- [ ] **Fix `typeof()` after affinity**: Must report storage type, not input type.
      `nil`→`"null"`, `int64`→`"integer"`, `float64`→`"real"`, `string`→`"text"`,
      `[]byte`→`"blob"`.
- [ ] **Fix NULL propagation**: `NULL + 1`→NULL, `NULL = 1`→NULL (not 0),
      `IS NULL`→1, `IS NOT NULL`→0. For arithmetic → nil. For comparison → nil.
      For `AND`/`OR` → three-valued logic. WHERE treats NULL as false.
- [ ] **Fix COLLATE clause**: Ensure `ORDER BY col COLLATE nocase` applies correct
      collation. Implement `NOCASE`, `BINARY`, `RTRIM` collation functions.
- [ ] **Verify**: `go test ./testgen/e_* ./testgen/expr* ./testgen/affinity* -count=1`.
      Result mismatch errors drop by ≥50%.
- [ ] **Commit** (`P1.1: fix type affinity, NULL handling, comparison, COLLATE`)

### Task 1.2 — Fix missing setup SQL (no-such-table) `[ ]`

**Files:** `tools/tclconvert/tcl/interp.go` (TCL interpreter command handlers).

- [ ] Run failure baseline: `FRIGOLITE_TEST= go test -run "^TestSQLiteSuite$" . 2>&1 |
      grep "no such table" | sort | uniq -c | sort -rn`
- [ ] For each TCL pattern producing errors:
  - [ ] `db eval { SQL } SCRIPT` — capture SQL, ignore per-row callback script
  - [ ] `db onecolumn { SQL }` — capture as query step
  - [ ] `db transaction { ... }` — execute body commands, capture their SQL
  - [ ] Multi-connection: `sqlite3 db2 test.db; db2 eval { SQL }` — note as multi-conn
  - [ ] `for`/`foreach` loops: execute loop body, unroll INSERT patterns
- [ ] Regenerate: `go run ./tools/tcl2go/`
- [ ] Verify: no-such-table errors drop by ≥80%
- [ ] **Commit** (`P1.2: fix TCL interp for missed SQL patterns`)

### Task 1.3 — Implement missing SQL functions `[ ]`

**Files:** `internal/function/` (existing and new files).

**SQLite reference:** `src/func.c`, `ext/misc/json.c`, `src/date.c`.

- [ ] **Audit**: Extract all "unknown function" errors across all generated tests.
      Build complete list: function name, file it's used in, count.
- [ ] **JSON functions** (new `internal/function/json.go`):
  - [ ] `json_valid(X)` — returns 1 if X is valid JSON, 0 otherwise
  - [ ] `json_type(X)` — returns the type of the root value
  - [ ] `json_type(X,P)` — returns the type at path P
  - [ ] `json_extract(X,P1,P2,...)` — extracts values at paths
  - [ ] `json_remove(X,P1,P2,...)` — removes values at paths
  - [ ] `json_quote(X)` — quotes a value as JSON
  - [ ] `json_array_length(X)` — returns array length
  - [ ] `json_array_length(X,P)` — returns array length at path
  - [ ] `json_array_insert(X,P,V)` — inserts into array at path
  - [ ] `json_patch(X,P)` — applies JSON patch
  - [ ] `json_each(X)` — table-valued function
  - [ ] `json_tree(X)` — table-valued function
  - [ ] Reference: `/Users/muaddib/dev/sqlite/ext/misc/json.c`
  - [ ] Verify: `go test ./testgen/json* -count=1` — zero unknown function errors
- [ ] **Date/time functions** (fix `internal/function/datetime.go`):
  - [ ] Fix `strftime(format, timestring, mod...)`
  - [ ] Fix `date(timestring, mod...)`
  - [ ] Fix `time(timestring, mod...)`
  - [ ] Fix `datetime(timestring, mod...)`
  - [ ] Fix `julianday(timestring, mod...)`
  - [ ] Handle modifiers: `+N days`, `start of month`, `localtime`, `utc`
  - [ ] Reference: `/Users/muaddib/dev/sqlite/src/date.c`
  - [ ] Verify: `go test ./testgen/date* ./testgen/strftime* -count=1`
- [ ] **Other missing functions:**
  - [ ] `changes()` — number of rows changed by last statement
  - [ ] `total_changes()` — total rows changed since connection opened
  - [ ] `octet_length(X)` — byte length of string
  - [ ] `intreal(X)` — convert integer to real
  - [ ] `sqlite_compileoption_used(X)` — compile option check
  - [ ] Verify: `go test ./testgen/misc*.test -count=1` — zero unknown function errors
- [ ] **Verify**: zero "unknown function" errors across all generated tests.
- [ ] **Commit** (`P1.3: implement missing SQL functions — JSON, date, misc`)

### Task 1.4 — Fix parse/syntax errors `[ ]`

**Files:** `internal/sql/lexer.go`, `internal/sql/parser.go`, `internal/sql/ast.go`.

**SQLite reference:** `src/tokenize.c`, `src/parse.y`.

- [ ] Collect all parse/syntax errors: `grep -r "syntax error\|parse error" testgen/`
- [ ] Fix top-10 parser gaps:
  - [ ] `FILTER (WHERE ...)` clause on aggregates (AST: `FunctionCall.Filter Expr`)
  - [ ] `OVER (PARTITION BY ... ORDER BY ...)` clause (AST: `OverClause` struct)
  - [ ] Window frame: `ROWS BETWEEN ... PRECEDING AND ... FOLLOWING` (`WindowFrame`)
  - [ ] `RETURNING` clause on INSERT/UPDATE/DELETE
  - [ ] `WITH` clause (CTE — `WITH name AS (SELECT ...)`)
  - [ ] `WINDOW win AS (...)` clause in SELECT
  - [ ] `NULLS FIRST` / `NULLS LAST` in ORDER BY
  - [ ] `GENERATED ALWAYS AS (expr) STORED` column syntax
  - [ ] `ON CONFLICT (col) DO UPDATE SET ...` upsert syntax
  - [ ] `TABLE` keyword in `SELECT * FROM TABLE(func(args))` (table-valued functions)
- [ ] Verify: parse/syntax errors drop by ≥80%
- [ ] **Commit** (`P1.4: fix parser gaps — FILTER, OVER, CTE, window specs`)

### Task 1.5 — Fix constraint enforcement `[ ]`

**Files:** `internal/exec/engine.go` — INSERT/UPDATE execution.

**SQLite reference:** `src/insert.c`, `src/vdbe.c` (constraint check opcodes).

- [ ] **UNIQUE constraint error messages**: Match SQLite exactly —
      `"UNIQUE constraint failed: table.column"` not just `"UNIQUE constraint failed"`.
- [ ] **Multi-column UNIQUE**: `CREATE TABLE t(a,b,UNIQUE(a,b))` — enforce pairs.
      NULL handling: `NULL != NULL` in UNIQUE (two rows with NULL in a are allowed).
- [ ] **NOT NULL constraint**: Error on INSERT of NULL to `col NOT NULL`.
      Error message: `"NOT NULL constraint failed: table.column"`.
- [ ] **CHECK constraint**: `CREATE TABLE t(a CHECK(a > 0))` — error on violation.
      Error message: `"CHECK constraint failed: table"`.
- [ ] **FOREIGN KEY**: `REFERENCES parent(col)` — enforce referential integrity.
      Default action: RESTRICT. Support ON DELETE/UPDATE CASCADE|SET NULL|SET DEFAULT.
- [ ] **PRIMARY KEY**: Synthesize UNIQUE index for PK columns. Auto-generate rowid.
- [ ] Verify: `go test ./testgen/conflict* ./testgen/unique* ./testgen/notnull*
      ./testgen/check* ./testgen/fkey* -count=1` — all pass.
- [ ] **Commit** (`P1.5: fix constraint enforcement — UNIQUE, NOT NULL, CHECK, FK`)

---

## Phase 2 — Full Feature Coverage

Goal: all 1002 test files green. Zero skip-list entries. Each task: remove area from
skip list, fix/tests, verify.

### Task 2.1 — Window functions `[ ]`

**Files:** `internal/exec/window.go` (new), `internal/sql/parser.go` (OVER clause),
`internal/function/` (window function implementations).

**SQLite reference:** `src/window.c`.

- [ ] Remove `window*` from `unsupportedTestFiles` in harness
- [ ] Run tests: `FRIGOLITE_TEST=window go test -run "^TestSQLiteSuite$" .`
      — capture baseline failures
- [ ] **Window pipeline** (partition/sort/frame/evaluate):
  - [ ] Partition input rows by `PARTITION BY` expressions
  - [ ] Sort each partition by `ORDER BY` expressions
  - [ ] Apply frame specification (`ROWS`/`RANGE`/`GROUPS` between)
  - [ ] Evaluate window function for each row in frame
- [ ] **Built-in window functions:**
  - [ ] `ROW_NUMBER()` — sequential row number within partition
  - [ ] `RANK()` — row number with gaps for ties
  - [ ] `DENSE_RANK()` — row number without gaps for ties
  - [ ] `LAG(expr, offset, default)` — previous row value
  - [ ] `LEAD(expr, offset, default)` — next row value
  - [ ] `FIRST_VALUE(expr)` — first value in window frame
  - [ ] `LAST_VALUE(expr)` — last value in window frame
  - [ ] `NTH_VALUE(expr, N)` — Nth value in window frame
- [ ] **Aggregate functions over windows:**
  - [ ] `SUM(expr) OVER (...)`, `AVG(expr) OVER (...)`, `COUNT(*) OVER (...)`
  - [ ] `MIN(expr) OVER (...)`, `MAX(expr) OVER (...)`, `TOTAL(expr) OVER (...)`
- [ ] Verify: `FRIGOLITE_TEST=window go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** (`P2.1: implement window functions — ROW_NUMBER, RANK, frame`)

### Task 2.2 — ALTER TABLE `[ ]`

**Files:** `internal/rename/` (existing), `internal/exec/engine.go` (ALTER execution).

**SQLite reference:** `src/alter.c`.

- [ ] Remove `altercorrupt`, `altertab2`, `altertab3` from skip list
- [ ] Run tests: `FRIGOLITE_TEST=alter go test -run "^TestSQLiteSuite$" .`
- [ ] **Fix token-level RENAME**: use `internal/rename` package for robust identifier
      replacement in stored SQL (trigger bodies, view definitions, index SQL).
- [ ] **Fix ADD COLUMN**: support `ALTER TABLE t ADD COLUMN col type DEFAULT expr`.
      - Default must be constant expression. NOT NULL requires default.
      - Foreign key references allowed on ADD COLUMN.
- [ ] **Fix DROP COLUMN**: `ALTER TABLE t DROP COLUMN col`.
      - Reject if column is referenced by triggers, views, or FKs.
      - Rewrite stored CREATE TABLE SQL to omit the column.
- [ ] Verify: `FRIGOLITE_TEST=alter go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** (`P2.2: fix ALTER TABLE — RENAME, ADD COLUMN, DROP COLUMN`)

### Task 2.3 — ATTACH / DETACH `[ ]`

**Files:** `internal/exec/engine.go` (multi-database dispatch), `internal/pager/`,
`internal/schema/` (per-database schema).

**SQLite reference:** `src/attach.c`.

- [ ] **Multi-database engine**: change `Engine` from single `schema`+`pager` to
      `[]Database` where each Database has a `Name`, `*schema.Manager`, `*pager.Pager`.
      Index 0 = main, 1 = temp, 2+ = attached.
- [ ] **Fix encoding check false positives**: 47 false "encoding mismatch" errors.
      When attaching `:memory:`, use same encoding as main database (UTF-8).
- [ ] **Implement ATTACH**: Parse `ATTACH 'file' AS name`. Validate name (not main/temp).
      Open file/create in-memory. Load schema. Add to databases[].
- [ ] **Implement DETACH**: Find by name. Reject main/temp. Close pager. Remove.
- [ ] **Schema-prefix dispatch**: Update ALL table/view/trigger/index lookup functions
      to support `schema.name` prefix. Functions: findTable, findView, findTrigger,
      findIndex, execSelect, execInsert, execUpdate, execDelete, PRAGMA handlers.
- [ ] **sqlite_master per database**: `SELECT * FROM aux.sqlite_master` returns tables
      from the `aux` database. Without prefix, search `main` first.
- [ ] **Cross-database queries**: `SELECT * FROM main.t1, aux.t2 WHERE ...` — resolve
      each table to its database, execute JOIN across pagers.
- [ ] Verify: `FRIGOLITE_TEST=attach go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** (`P2.3: implement ATTACH/DETACH — multi-database dispatch`)

### Task 2.4 — FTS3/4/5 `[ ]`

**Files:** `internal/fts/` (existing), `internal/vtab/` (virtual table interface).

**SQLite reference:** `/Users/muaddib/dev/sqlite/ext/fts3/` + `ext/fts5/`.

- [ ] Remove `fts*` from `unsupportedTestFiles`
- [ ] Run: `FRIGOLITE_TEST=fts go test -run "^TestSQLiteSuite$" .`
- [ ] **Tokenizers**: `simple` (split on whitespace + punctuation), `porter` (stemmer),
      `unicode61` (unicode-aware, remove diacritics), `ascii` (ASCII only).
      Reference: `ext/fts3/fts3_tokenizer1.c`.
- [ ] **Inverted index storage**: content table + segment b-tree for term→docid→position.
      Segment merge (automatically when segments accumulate).
- [ ] **FTS virtual table**: implement `vtab.VirtualTable` interface: `Open`, `BestIndex`,
      `Filter`, `Next`, `Eof`, `Column`, `Rowid`, `Close`, `Update`, `BeginTransaction`,
      `CommitTransaction`, `RollbackTransaction`, `FindFunction`, `Rename`.
- [ ] **MATCH query parser**: parse `col MATCH 'expr'` — support bare phrases,
      `"quoted phrase"`, `col:value`, `+term`, `-term`, `AND`, `OR`, `NEAR`.
      Reference: `ext/fts3/fts3_expr.c`.
- [ ] **FTS3 cursor execution**: look up matching docids in index, iterate results,
      rank by BM25 (matchinfo + offsets for snippet/bm25 aux functions).
- [ ] **Auxiliary functions**: `snippet()`, `offsets()`, `matchinfo()`, `bm25()`,
      `highlight()`. Reference: `ext/fts3/fts3_aux.c`.
- [ ] **FTS5**: distinct from FTS3/4 — different MATCH syntax, different ranking,
      `rank` column, `detail` option. Implement after FTS3/4 baseline works.
      Reference: `ext/fts5/`.
- [ ] Verify: `FRIGOLITE_TEST=fts go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** (`P2.4: implement FTS3/4/5 — full-text search`)

### Task 2.5 — Virtual tables `[ ]`

**Files:** `internal/vtab/`, `internal/exec/engine.go` (vtab dispatch).

**SQLite reference:** `src/vtab.c`.

- [ ] Remove `vtab*`, `bestindex*` from skip list
- [ ] **Fix xBestIndex**: implement cost-based plan selection for vtab queries.
      Virtual table reports cost per plan; planner selects cheapest.
      Handle constraints (`col = ?`, `col IN (...)`, `col > ?`, etc.) and order-by.
- [ ] **WITHOUT ROWID tables**: `CREATE TABLE t(a, b, PRIMARY KEY(a)) WITHOUT ROWID`.
      Use the PK as the row key in b-tree (no separate rowid column).
      Clustered index: data stored in PK order. References from FKs on rowid tables.
- [ ] **Missing vtab modules**: `dbstat` (page-level DB stats), `pragma_*` tables,
      `generate_series` (improve existing), `json_each`/`json_tree` (eponymous).
- [ ] Verify: `FRIGOLITE_TEST=vtab go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** (`P2.5: fix virtual tables — xBestIndex, WITHOUT ROWID, modules`)

### Task 2.6 — Query planner & ANALYZE `[ ]`

**Files:** `internal/exec/plan.go` (new), `internal/exec/engine.go` (index selection).

**SQLite reference:** `src/analyze.c`, `src/where.c`.

- [ ] Remove `analyze*`, `eqp*` from skip list
- [ ] **Implement ANALYZE**: `ANALYZE` / `ANALYZE table` / `ANALYZE schema.table`.
      Create `sqlite_stat1` table storing `(tbl, idx, stat)` where `stat` is
      `"N K1 K2 ..."` format (N = rows, K1 = distinct prefix[1], etc.).
      Populate by scanning indexes, counting distinct prefix lengths.
- [ ] **Cost-based index selection**: Use `sqlite_stat1` to estimate row count for
      each query plan, select cheapest. Fallback to full scan when no stats.
      Estimate: `estRows = ceil(N / Ki)` for equality, `N/3` for range, etc.
- [ ] **EXPLAIN QUERY PLAN**: output `SEARCH table USING INDEX idx` / `SCAN table`.
      Format: `id parent detail` hierarchy (like SQLite's tab-indented output).
- [ ] **Auto-index for joins**: When joining two tables without usable index,
      automatically build a transient index on the join key.
      Reference: `src/where.c` — `sqlite3WhereBegin`.
- [ ] Verify: `FRIGOLITE_TEST=analyze go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** (`P2.6: implement query planner — ANALYZE, cost-based, EQP, auto-index`)

### Task 2.7 — Corruption & edge cases `[ ]`

**Files:** `internal/storage/` (page/cell validation), `internal/btree/` (B-tree integrity).

- [ ] Remove `corrupt*`, `tkt*`, `bigfile*` from skip list
- [ ] **Corruption handling**: match SQLite error messages exactly:
      `"malformed database schema - malformed database encoding (ma)"` etc.
      Validate page headers, cell pointers, b-tree structure on read.
- [ ] **Ticket tests** (~73 `tkt-*` files): each tests a specific bug fix in SQLite.
      Debug each failure by reading `tkt-XXXX.test` and implementing the edge case.
- [ ] **Large data**: `bigfile*`, `bigrow*` — test large BLOBs, large row counts,
      file growth. May need pager optimization for >4KB pages.
- [ ] Verify: remaining tests pass progressively
- [ ] **Commit** (`P2.7: fix corruption handling, edge cases, large data tests`)

---

## Phase 3 — Quality & SOLID

Goal: clean architecture, low complexity, full docs, zero skips.

### Task 3.1 — Complexity gate `[ ]`
- [ ] Set gocognit threshold to 15 in Makefile (currently 90)
- [ ] Set gocyclo threshold to 15 in Makefile (currently 40)
- [ ] For each offender: split function, extract helpers, use guard clauses
- [ ] Verify: `make quality` passes at threshold 15
- [ ] **Commit** (`P3.1: lower complexity gate to 15`)

### Task 3.2 — Static analysis `[ ]`
- [ ] Run `staticcheck ./...` on all packages
- [ ] Fix all warnings (unused code, incorrect error handling, etc.)
- [ ] Verify: `staticcheck ./...` clean, zero warnings
- [ ] **Commit** (`P3.2: staticcheck clean`)

### Task 3.3 — SOLID compliance `[ ]`
- [ ] Verify import boundaries: `go test -run TestSOLID_ImportBoundaries -count=1 ./...`
      — no upward or circular deps between packages
- [ ] Verify single-responsibility: each `internal/` package has focused scope
- [ ] Verify all exported symbols have GoDoc comments
- [ ] **Commit** (`P3.3: SOLID compliance — clean deps, full GoDoc`)

### Task 3.4 — Remove all skip-list entries `[ ]`
- [ ] Remove remaining entries from `unsupportedTestFiles` in `frigolite_harness_test.go`
- [ ] Remove remaining entries from `slowTestFiles`
- [ ] Verify: `go test ./testgen/... -count=1` — 1002/1002 file PASS
- [ ] **Commit** (`P3.4: zero skip-list entries, 1002/1002 green`)

---

## Key Commands

```bash
# tcl2go: generate all tests
go run ./tools/tcl2go/

# tcl2go: run generated tests
go test ./testgen/... -count=1

# tcl2go: single package
go test ./testgen/select1/... -v

# Harness (deprecated, transitional)
FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .

# SOLID architecture check
go test -run TestSOLID_ -count=1 ./...

# Quality gate
make quality
go build ./...
```

## Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source (spec) | `/Users/muaddib/dev/sqlite/src/` |
| SQLite FTS source | `/Users/muaddib/dev/sqlite/ext/fts3/`, `ext/fts5/` |
| SQLite JSON source | `/Users/muaddib/dev/sqlite/ext/misc/json.c` |
| SQLite date source | `/Users/muaddib/dev/sqlite/src/date.c` |
| SQLite TCL tests | `ori/sqlite/test/*.test` |
| TCL interpreter | `tools/tclconvert/tcl/` (parser, interp, expr, list) |
| tcl2go generator | `tools/tcl2go/` (main.go, gen.go) |
| Generated Go tests | `testgen/` |
| JSON harness (deprecated) | `frigolite_harness_test.go` + `testdata/*.json` |
| SOLID tests | `frigolite_solid_test.go` |
| Quality gates | `Makefile` (`make quality`) |
| sqlite3 oracle | `/usr/bin/sqlite3` |

## Protocol

After each task:
1. Run the verify command for that task
2. `go build ./...` — must compile
3. `go test -run TestSOLID_ -count=1 ./...` — architecture check
4. **Commit** with message: `P<phase>.<task>: <description>`
5. Update this plan — mark task `[x]`, update metrics, note findings
