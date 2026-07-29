# G01 — Engine Decomposition (SOLID Refactor)

> **Prerequisite**: G00 (test infrastructure must be reliable to detect regressions).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/` — architecture of how SQLite separates concerns.
> **Goal**: Decompose the 9 348-line `internal/exec/engine.go` monolith into focused, testable units. **Behavior-preserving** — no functional changes, zero new test failures.

---

## Context

`internal/exec/engine.go` is 9 348 lines with 309 functions. It handles:
- Expression evaluation (`evalBinaryOpValues`, `evalAdd`, `evalConcat`, ...)
- Type coercion (`toFloat`, `toBool`, `boolToInt`, `negateValue`, ...)
- LIKE/GLOB/REGEXP matching (`likeMatch`, `likeValues`, `globValues`, `regexpValues`, ...)
- SQL text generation from AST (`insertStmtToString`, `selectStmtToString`, `rebuildCreateTableSQL`, ...)
- Row set operations (`dedupeRows`, `intersectRows`, `exceptRows`, `hasConflictAt`, ...)
- Column reference collection (`collectColumnRefs`, `collectExprRefs`, `findAggregateInExpr`, ...)
- Query planning (`simplePlan`, `estimateSelectivity`, `computeBetweenSelectivity`, ...)
- DDL execution (CREATE/DROP TABLE/INDEX/VIEW/TRIGGER)
- DML execution (INSERT/UPDATE/DELETE)
- ALTER TABLE (`internal/exec/rename.go`, 362 lines)

This violates Single Responsibility and makes every change a regression risk.

---

## Current State

```
internal/exec/
├── engine.go    9 348 lines, 309 functions ← TARGET: decompose
├── rename.go      362 lines  (ALTER TABLE)
└── exec_test.go    46 lines  (minimal)
```

The `Engine` struct (line 37–62) holds 20+ fields:
```go
type Engine struct {
    authorizer     auth.Authorizer
    databases      map[string]*DatabaseContext
    mainDB         *DatabaseContext
    pager          *pager.Pager
    schema         *schema.Manager
    funcs          *function.Registry
    vtabs          *vtab.Registry
    lastRowID      int64
    colCache       map[string][]sql.ColumnDef
    stmtCache      map[string][]sql.Stmt
    tableRootPages map[string]uint32
    nextRowIDCache map[uint32]int64
    triggerDepth   int
    triggerNewRow  map[string]interface{}
    triggerOldRow  map[string]interface{}
    inTransaction  bool
    ddlBuffer      []func()
    outerRow       map[string]interface{}
    outerRows      []map[string]interface{}
    resolvingViews map[string]bool
    legacyAlterTable bool
    encoding       string
}
```

### SOLID test layer assignments (`frigolite_solid_test.go`)
```go
var internalLayers = map[string]int{
    ".../internal/util":     0,
    ".../internal/auth":     0,
    ".../internal/storage":  1,
    ".../internal/pager":    2,
    ".../internal/btree":    3,
    ".../internal/sql":      4,
    ".../internal/schema":   5,
    ".../internal/function": 5,
    ".../internal/vtab":     5,
    ".../internal/exec":     6,
}
```

---

## Decomposition Plan

**Strategy**: Split `engine.go` into focused files within `internal/exec/` (same package,
no layer changes). Extract two new packages for cohesive, reusable logic. This is a
behavior-preserving refactor — the test suite (from G00) must show zero new failures.

### Target file structure

```
internal/exec/
├── engine.go          ~600 lines  — Engine struct, NewEngine, Exec dispatcher, context management
├── context.go         ~150 lines  — DatabaseContext, multi-DB helpers
├── eval.go            ~800 lines  — Expression evaluation (evalBinaryOpValues, evalAdd, evalConcat, ...)
├── types.go           ~300 lines  — Type coercion (toFloat, toBool, negateValue, boolToInt, ...)
├── like.go            ~250 lines  — LIKE/GLOB/REGEXP matching
├── select.go         ~1500 lines  — SELECT execution (incl. JOIN, subquery, union)
├── insert.go          ~500 lines  — INSERT execution
├── update.go          ~400 lines  — UPDATE execution
├── delete.go          ~300 lines  — DELETE execution
├── ddl.go             ~600 lines  — CREATE/DROP TABLE/INDEX/VIEW/TRIGGER
├── alter.go           ~400 lines  — ALTER TABLE (merge rename.go)
├── rows.go            ~300 lines  — Row set operations (dedupe, intersect, except)
├── aggregate.go       ~400 lines  — Aggregate function evaluation
├── trigger.go         ~300 lines  — Trigger firing and evaluation
├── planner.go         ~400 lines  — Query planning, selectivity estimation
├── sqlgen.go          ~800 lines  — AST → SQL text generation (xxxToString functions)
├── pragma.go          ~400 lines  — PRAGMA handling
├── auth.go            ~100 lines  — Authorization checks
├── explain.go         ~200 lines  — EXPLAIN / EXPLAIN QUERY PLAN
├── value.go           ~200 lines  — Value formatting (formatColumnValue, extractColumnName, ...)
└── *_test.go                       — Unit tests for each extracted module
```

### New packages (cross-package extraction)

Two cohesive units deserve their own packages:

1. **`internal/value/`** (layer 1) — SQLite value model:
   - Type affinity constants and rules
   - Value comparison with collation
   - Value coercion (integer ↔ real ↔ text ↔ blob)
   - NULL semantics
   - Functions: `Compare`, `Affinity`, `Coerce`, `IsNull`, `Format`

2. **`internal/sqlgen/`** (layer 5, depends on `sql`) — AST → SQL text:
   - All `xxxToString` functions
   - `rebuildCreateTableSQL`, `buildIndexSQL`, `buildTriggerSQL`
   - Used by ALTER TABLE and schema introspection

---

## Implementation Steps

### Step 1: Establish baseline failure count

Before touching anything, record the current pass/skip/fail counts from G00's harness:
```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run "^TestSQLiteSuite$" -timeout 120s . 2>&1 | \
    tee /tmp/g01_baseline.txt
grep -c "^    --- PASS" /tmp/g01_baseline.txt  # baseline PASS
grep -c "^    --- SKIP" /tmp/g01_baseline.txt  # baseline SKIP
grep -c "^    --- FAIL" /tmp/g01_baseline.txt  # baseline FAIL
```

**This baseline is the regression gate. After each extraction step, the PASS+SKIP+FAIL
counts must be identical (no new failures).**

### Step 2: Extract `internal/value/` package

**New package**: `internal/value/`

Extract from `engine.go` and `internal/util/compare.go`:
- Type affinity constants: `AffinityNone`, `AffinityText`, `AffinityNumeric`, `AffinityInteger`, `AffinityReal`, `AffinityBlob`
- `ColumnAffinity(name string) Affinity` — determines affinity from column type name
- `ApplyAffinity(val interface{}, aff Affinity) interface{}` — applies affinity rules
- `Compare(a, b interface{}) int` — SQLite 3-way comparison (NULL ordering, numeric vs text)
- `CompareWithCollation(a, b interface{}, collation string) int`
- `CoerceToInteger`, `CoerceToReal`, `CoerceToText`, `CoerceToBlob`
- `IsNull(val interface{}) bool`

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/expr.c` (function `sqlite3ExprCollSeq`),
`/Users/muaddib/dev/sqlite/src/vdbe.c` (function `ApplyAffinity`), and
[SQLite type affinity documentation](https://www.sqlite.org/datatype3.html).

**Move existing code**: `internal/util/compare.go` → `internal/value/compare.go`. Update
imports throughout.

Key function signatures to be in the new package:

```go
package value

// Affinity represents a SQLite column affinity.
// Reference: vdbe.c:sqlite3TableColumnAffinity
type Affinity int

const (
    AffinityBlob    Affinity = iota // NONE affinity — no coercion
    AffinityText                    // TEXT — coerce to text
    AffinityNumeric                 // NUMERIC — coerce to integer or real
    AffinityInteger                 // INTEGER — coerce to integer if lossless
    AffinityReal                    // REAL — coerce to real
)

// ColumnAffinity determines affinity from a column type declaration string.
// Reference: insert.c:sqlite3TableColumnAffinity()
func ColumnAffinity(typeName string) Affinity

// ApplyAffinity coerces a value according to affinity rules.
// Reference: vdbe.c:sqlite3VdbeMemApplyAffinity()
func ApplyAffinity(val interface{}, aff Affinity) interface{}

// Compare performs SQLite 3-way comparison with NULL ordering and type precedence.
// Reference: vdbe.c:sqlite3MemCompare()
func Compare(a, b interface{}) int

// IsNull returns true if the value is SQL NULL.
func IsNull(val interface{}) bool
```

**SOLID**: Add to `internalLayers`:
```go
".../internal/value": 1,  // depends only on stdlib
```

**Verify**:
```bash
go build ./...
go test -count=1 ./internal/value/...
go test -v -count=1 -run "^TestSQLiteSuite/affinity" . 2>&1 | diff - <(grep -E "PASS|FAIL|SKIP" /tmp/g01_baseline.txt | grep affinity)
```
**Expected outcome**: `go build ./...` succeeds. `internal/value` tests pass. Affinity test
results are identical to baseline (behavior-preserving — no new PASS/FAIL/SKIP changes).

### Step 3: Extract type coercion helpers (`types.go`)

**File**: `internal/exec/types.go`

Move from `engine.go`:
- `toFloat`, `toBool`, `boolToInt`, `negateValue`, `isInt`, `isTrue`, `isFalse`
- `numericValue`, `numericLitValue`, `evalNumericLit`
- `typesMatchForEquality`, `kleeneAnd`, `kleeneOr`

These become standalone functions (not methods on Engine) where possible. For those that
need Engine state, keep as methods but in the new file.

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/where1$" .`
**Expected outcome**: `go build` succeeds. where1 results unchanged from baseline.

### Step 4: Extract LIKE/GLOB matching (`like.go`)

**File**: `internal/exec/like.go`

Move from `engine.go`:
- `likeMatch`, `likeMatchEscaped`, `likeMatchPercentEscaped`, `likeMatchRecursiveEscaped`
- `likeValues`, `likeValuesWithEscape`
- `globValues`
- `regexpValues`

These are pure functions — no Engine state needed.

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/func.c` (function `likeFunc`).

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/like2$" .`
**Expected outcome**: `go build` succeeds. like2 results unchanged from baseline.

### Step 5: Extract expression evaluation (`eval.go`)

**File**: `internal/exec/eval.go`

Move from `engine.go`:
- `evalBinaryOpValues`, `evalAdd`, `evalArithmeticOp`, `evalConcat`
- `addValues`, `subValues`, `mulValues`, `divValues`, `modValues`
- `concatValues`, `bitwiseAnd`
- `columnValue`, `extractValue`
- `walkExpr` and all `find*` helper functions
- `evalIsDistinctFrom`, `evalIsNotDistinctFrom` (if they exist)

These are methods on Engine (they need access to function registry, current row, etc.).

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/expr" .`
**Expected outcome**: `go build` succeeds. expr tests unchanged from baseline.

### Step 6: Extract row set operations (`rows.go`)

**File**: `internal/exec/rows.go`

Move from `engine.go`:
- `dedupeRows`, `intersectRows`, `exceptRows`
- `hasConflictAt`, `rowKey`, `copyRowMap`
- `buildRowMapFromValues`

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/select.c` (compound select processing).

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/(select1|union)" .`
**Expected outcome**: `go build` succeeds. select1/union results unchanged from baseline.

### Step 7: Extract SQL text generation (`sqlgen.go` and `internal/sqlgen/`)

**File**: `internal/exec/sqlgen.go` (or new package `internal/sqlgen/`)

Move from `engine.go`:
- `insertStmtToString`, `deleteStmtToString`, `updateStmtToString`, `selectStmtToString`
- `rebuildCreateTableSQL`, `buildIndexSQL`, `buildTriggerSQL`
- `stmtToString` and all `*ToString` helpers
- `formatColumnDef`, `formatTableConstraint`, `formatConditions`
- `exprToString`, `caseExprToString`, `funcCallToString`, `inListToString`
- `betweenToString`, `windowDefToString`, `aliasClause`

**Decision**: Keep in `internal/exec/sqlgen.go` for now (they reference Engine types).
If `internal/sqlgen/` is extracted later, it depends on `sql` (layer 4) and `value` (layer 1).

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/alter" .`
**Expected outcome**: `go build` succeeds. alter tests unchanged from baseline.

### Step 8: Extract DML execution (`insert.go`, `update.go`, `delete.go`)

Move from `engine.go`:
- INSERT: `execInsert` and helpers → `insert.go`
- UPDATE: `execUpdate`, `applyUpdate`, `updateChange` type → `update.go`
- DELETE: `execDelete` → `delete.go`

**SQLite reference**:
- INSERT: `/Users/muaddib/dev/sqlite/src/insert.c`
- UPDATE: `/Users/muaddib/dev/sqlite/src/update.c`
- DELETE: `/Users/muaddib/dev/sqlite/src/delete.c`

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/(insert|update|delete)" .`
**Expected outcome**: `go build` succeeds. insert/update/delete results unchanged from baseline.

### Step 9: Extract DDL execution (`ddl.go`)

Move from `engine.go`:
- `execCreateTable`, `execCreateIndex`, `execCreateView`, `execCreateTrigger`
- `execDropTable`, `execDropIndex`, `execDropView`, `execDropTrigger`
- `parseIndexColumns`, `formatColumnDef`

Merge `rename.go` → `alter.go` (ALTER TABLE logic).

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/build.c` (DDL processing).

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/(alter|index|trigger|view)" .`
**Expected outcome**: `go build` succeeds. alter/index/trigger/view results unchanged from baseline.

### Step 10: Extract SELECT execution (`select.go`)

Move from `engine.go`:
- `execSelect` and all SELECT-related helpers
- JOIN handling
- Subquery handling
- UNION/INTERSECT/EXCEPT dispatch
- Aggregate evaluation → `aggregate.go`
- ORDER BY, GROUP BY, LIMIT/OFFSET, DISTINCT

This is the largest extraction (~1 500 lines).

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/select.c` (the entire file is the spec).

**Verify**: `go build ./... && go test -count=1 -run "^TestSQLiteSuite/(select|join|group|order)" .`
**Expected outcome**: `go build` succeeds. select/join/group/order results unchanged from baseline.

### Step 11: Extract remaining concerns

- `pragma.go` — PRAGMA handling
- `auth.go` — Authorization checks (wrap `authorizer`)
- `explain.go` — EXPLAIN / EXPLAIN QUERY PLAN
- `trigger.go` — Trigger firing logic
- `planner.go` — `simplePlan`, `estimateSelectivity`, `computeBetweenSelectivity`
- `context.go` — `DatabaseContext`, multi-DB helpers
- `value.go` — `formatColumnValue`, `extractColumnName`, value formatting

**Verify after each extraction**: `go build ./...`
**Expected outcome**: `go build` succeeds after each extraction step.

### Step 12: Final verification — no new failures

```bash
cd /Users/muaddib/dev/frigolite

# Compare against baseline
go test -v -count=1 -run "^TestSQLiteSuite$" -timeout 120s . 2>&1 | \
    tee /tmp/g01_after.txt

# PASS count must be >= baseline
diff <(grep -E "^    --- (PASS|FAIL|SKIP)" /tmp/g01_baseline.txt) \
     <(grep -E "^    --- (PASS|FAIL|SKIP)" /tmp/g01_after.txt)
# Only acceptable change: FAIL→PASS (fixes) or PASS→SKIP (newly discovered unsupported)
# NO new FAIL lines.
```

### Step 13: Add unit tests for extracted modules

Each extracted file gets a focused unit test file:
- `eval_test.go` — expression evaluation
- `types_test.go` — type coercion
- `like_test.go` — LIKE/GLOB patterns
- `rows_test.go` — set operations
- `value_test.go` (in `internal/value/`) — affinity and comparison

These tests run fast (<1s each) and verify the extracted logic in isolation.

### Step 14: Update SOLID test layers

**File**: `frigolite_solid_test.go`

Add new package layers:
```go
var internalLayers = map[string]int{
    // ... existing ...
    ".../internal/value":  1,  // value model (depends on stdlib only)
}
```

If `internal/sqlgen/` was extracted:
```go
    ".../internal/sqlgen": 5,  // SQL generation (depends on sql, value)
```

---

## Files Modified

| File | Change |
|------|--------|
| `internal/exec/engine.go` | Reduced from 9 348 → ~600 lines (dispatcher only) |
| `internal/exec/*.go` | NEW files for each extracted concern |
| `internal/value/*.go` | NEW package — value model, affinity, comparison |
| `internal/util/compare.go` | Moved to `internal/value/compare.go` |
| `frigolite_solid_test.go` | Add layer assignments for new packages |
| `internal/exec/*_test.go` | NEW unit tests for each module |

---

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. Build succeeds
go build ./...

# 2. No new test failures (compare against baseline)
go test -v -count=1 -run "^TestSQLiteSuite$" -timeout 120s . 2>&1 | \
    grep "^    --- FAIL" | wc -l
# Must be <= baseline FAIL count from Step 1

# 3. Quality gates
make quality

# 4. SOLID architecture
go test -run TestSOLID_ ./...

# 5. Engine.go is now a coordinator (< 1000 lines)
wc -l internal/exec/engine.go
# Should be < 1000

# 6. No file in internal/exec/ exceeds 2000 lines
find internal/exec/ -name "*.go" ! -name "*_test.go" -exec wc -l {} \; | sort -rn | head -5
```

## What G01 Does NOT Do

- G01 does not fix bugs or implement features. It is purely structural.
- G01 does not change any behavior. If a test passed before, it passes after.
- G01 does not optimize performance. Same algorithms, just organized differently.
