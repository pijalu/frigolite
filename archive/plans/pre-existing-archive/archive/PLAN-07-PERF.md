# G07 — Performance Optimization

> **Prerequisite**: G01 (engine decomposed), G04 (query planner with index selection).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/btree.c`, `pager.c`, `vdbe.c`.
> **Goal: Achieve within 5× of SQLite performance for common workloads. The full test suite must complete in <60s.**

---

## Context

**Current performance baseline** (Apple M4 Pro):
```
BenchmarkInsert-12       50    13 429 ns/op     ~74 000 inserts/s
BenchmarkSelect-12       50   687 427 ns/op     ~1 450 full-table-scan/s (1000 rows)
BenchmarkSelectWhere-12  50   459 358 ns/op     ~2 180 filtered-scan/s (1000 rows, ~500 match)
```

For comparison, SQLite on the same hardware:
```
BenchmarkInsert              ~2 000 000 inserts/s     (27× faster)
BenchmarkSelect              ~50 000 scans/s           (34× faster)
BenchmarkSelectWhere         ~80 000 filtered-scans/s  (37× faster)
```

**The full test suite times out at 60s** because of O(N×M) nested loop joins and full
table scans where indexes exist.

---

## Performance Analysis — Root Causes

### R1: O(N×M) nested loop JOINs (fixed by G04)
After G04, index-based joins reduce JOIN from O(N×M) to O(N × log M). This is the
single biggest win.

### R2: Pager cache misses (every page access goes through map lookup)
`internal/pager/pager.go` caches pages in a `map[uint32][]byte`. Every cell access requires
a map lookup, bounds check, and byte copy. SQLite uses a page cache with direct array
indexing.

### R3: Record decoding overhead
`internal/storage/storage.go` decodes records from their byte representation on every cell
access. SQLite caches decoded records. The varint decoding is also not optimized.

### R4: Expression evaluation allocates excessively
`eval.go` creates intermediate values for every expression node. Each comparison allocates
new `interface{}` values. String concatenation creates new strings for each operation.

### R5: Row materialization
Every row is materialized as `[]interface{}` even when only a few columns are needed. SQLite
uses a virtual machine that processes columns lazily.

---

## Implementation Steps

### Step 1: Profile and set targets

**Before any optimization**, profile to confirm the root causes:

```bash
cd /Users/muaddib/dev/frigolite

# CPU profile of SELECT benchmark
go test -bench=BenchmarkSelect -benchtime=1000x -cpuprofile=/tmp/cpu.prof ./benchmarks/
go tool pprof -top -cum /tmp/cpu.prof | head -20

# CPU profile of INSERT benchmark
go test -bench=BenchmarkInsert -benchtime=10000x -cpuprofile=/tmp/cpu_insert.prof ./benchmarks/
go tool pprof -top -cum /tmp/cpu_insert.prof | head -20
```

**Record the baseline numbers** (these are the targets to beat):
```
Target:
  BenchmarkInsert       < 2 000 ns/op    (500 000 inserts/s)
  BenchmarkSelect       < 100 000 ns/op  (10 000 scans/s)
  BenchmarkSelectWhere  < 50 000 ns/op   (20 000 filtered-scans/s)
```

### Step 2: Optimize the pager cache

**File**: `internal/pager/pager.go`

Current: `map[uint32][]byte` with copy-on-write semantics.
Optimization:
- Use a **page array** indexed by page number (for small databases)
- Eliminate unnecessary copies — pages are only copied on write, not on read
- Pre-allocate page buffers from a pool

```go
type Pager struct {
    file      *os.File
    pageSize  int
    pages     map[uint32]*pageEntry  // cache
    dirty     []uint32               // dirty page list (for flush)
    pageCount uint32
}

type pageEntry struct {
    data  []byte    // the page data (pooled)
    dirty bool
}
```

**Key insight**: Avoid copying page data on read. Return a pointer to the cached page.
Copy only when writing (copy-on-write for transaction isolation).

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/pcache.c` — SQLite's page cache uses
a hash table with LRU eviction. For frigolite's in-memory mode, a simple map is fine, but
the data copy must be eliminated. Key functions: `pcacheFetch()` (page retrieval),
`pcacheManage()` (eviction), `sqlite3PcacheOpen()` (initialization).

**Verify**:
```bash
go test -bench=BenchmarkSelect -benchtime=1000x ./benchmarks/
```

### Step 3: Optimize record decoding

**File**: `internal/storage/storage.go`

Current: `DecodeRecord` creates a new `[]interface{}` for every record.
Optimization:
- Lazy decoding — decode only the columns that are accessed
- Avoid `interface{}` boxing for integers — use typed accessors
- Cache the record header length (skip re-parsing on repeated access)

```go
// Fast path for single-column integer lookup (common for WHERE id = ?)
func DecodeRecordColumn(data []byte, colIdx int) (interface{}, error) {
    // Parse header, skip to column colIdx, decode just that column
}
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/vdbe.c` function `sqlite3VdbeMemFromBtree()`
— SQLite decodes only the columns it needs from each B-tree cell.

**Verify**:
```bash
go test -bench=BenchmarkSelectWhere -benchtime=1000x ./benchmarks/
```

### Step 4: Optimize varint decoding

**File**: `internal/util/varint.go`

The varint decoder is called for every record header byte and every integer value. Optimize:
- Use a lookup table for the first byte (determines length)
- Branch prediction-friendly code layout (common case: 1-byte varint)

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/util.c` function
`sqlite3GetVarint()` and `sqlite3PutVarint()` — SQLite's varint implementation with
fast-path optimization for common 1-byte values.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -count=1 ./internal/util/...
```
**Expected outcome**: `go build` succeeds. Varint tests pass. Decode performance improves.

```go
var varintLenTable = [256]int{
    // 0-127: 1 byte, 128-191: 2 bytes, etc.
}

func DecodeVarint(data []byte) (uint64, int) {
    if data[0] < 0x80 {
        return uint64(data[0]), 1  // fast path: 90% of varints
    }
    // ... slow path
}
```

### Step 5: Reduce expression evaluation allocations

**File**: `internal/exec/eval.go`

- Pre-compute constant expressions once (constant folding)
- Reuse evaluation context buffers instead of allocating per-expression
- For integer comparisons (the most common case), avoid `interface{}` boxing

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/expr.c` function
`sqlite3ExprCodeAtCurrentLine()` and `/Users/muaddib/dev/sqlite/src/vdbe.c` opcode
handlers — SQLite folds constant expressions at prepare time and reuses registers.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -bench=BenchmarkSelectWhere -benchtime=1000x ./benchmarks/
```
**Expected outcome**: `go build` succeeds. Expression-heavy benchmarks show reduced
allocations and improved ns/op.

```go
// Fast path for integer comparison
func evalIntCompare(a, b int64) int {
    if a < b { return -1 }
    if a > b { return 1 }
    return 0
}
```

### Step 6: Optimize B-tree cursor

**File**: `internal/btree/btree.go`

Current: cursor navigates the tree on every Next() call.
Optimization:
- Cache the current leaf page and cell index
- Only traverse up to parent when the current leaf is exhausted
- Pre-fetch child pages during descent

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/btree.c` functions
`sqlite3BtreeNext()` (cursor advance), `sqlite3BtreePrevious()`, and `cursorHint()` —
SQLite's cursor caches the current page and cell position.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -bench=BenchmarkSelect -benchtime=1000x ./benchmarks/
```
**Expected outcome**: `go build` succeeds. Select benchmark shows measurable improvement
in ns/op.

### Step 7: Optimize INSERT path

**File**: `internal/exec/insert.go`

Current: each INSERT re-opens the B-tree cursor, encodes the record, and writes.
Optimization:
- Reuse the cursor across multiple inserts (when possible)
- Batch record encoding
- Avoid schema lookups on every INSERT (cache column definitions)

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/insert.c` function
`sqlite3Insert()` — SQLite reuses the cursor and pre-computes column mappings for batch
inserts. Also `vdbe.c:op_insert` opcode handler.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
go test -bench=BenchmarkInsert -benchtime=1000x ./benchmarks/
```
**Expected outcome**: `go build` succeeds. Insert benchmark shows significant improvement
(target: < 2 000 ns/op).

### Step 8: Enable parallel test execution

**File**: `frigolite_harness_test.go`

Ensure tests are safe for parallel execution:
```go
t.Run(base, func(t *testing.T) {
    t.Parallel()  // safe — each test gets its own DB
    // ...
})
```

Each sub-test creates its own in-memory DB, so parallel execution is safe. This won't speed
up individual queries but will speed up the full suite by utilizing multiple cores.

### Step 9: Profile again and iterate

```bash
go test -bench=. -benchtime=1000x -cpuprofile=/tmp/cpu_after.prof ./benchmarks/
go tool pprof -top -cum /tmp/cpu_after.prof | head -20
```

Compare against the profile from Step 1. Focus on the new top hotspot.

---

## Performance Targets

| Benchmark | Current | Target | SQLite (reference) |
|-----------|---------|--------|-------------------|
| BenchmarkInsert | 13 429 ns/op | < 2 000 ns/op | ~500 ns/op |
| BenchmarkSelect (1000 rows) | 687 427 ns/op | < 100 000 ns/op | ~20 000 ns/op |
| BenchmarkSelectWhere (1000 rows) | 459 358 ns/op | < 50 000 ns/op | ~12 000 ns/op |
| Full test suite | >60s (timeout) | < 60s | — |

---

## Files Modified

| File | Change |
|------|--------|
| `internal/pager/pager.go` | Eliminate unnecessary page copies; use pointer-based cache |
| `internal/storage/storage.go` | Lazy column decoding; typed accessors |
| `internal/util/varint.go` | Lookup table for fast varint decode |
| `internal/exec/eval.go` | Constant folding; reduce allocations |
| `internal/exec/insert.go` | Cursor reuse; batch encoding |
| `internal/btree/btree.go` | Cursor optimization (cache leaf page) |
| `frigolite_harness_test.go` | Enable `t.Parallel()` |

---

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. Benchmarks meet targets
go test -bench=. -benchtime=1000x ./benchmarks/
# BenchmarkInsert   < 2 000 ns/op
# BenchmarkSelect   < 100 000 ns/op
# BenchmarkSelectWhere < 50 000 ns/op

# 2. Full test suite completes
time go test -timeout 120s -count=1 -run "^TestSQLiteSuite$" .
# Must complete in < 60s

# 3. No regressions
make quality
go test -run TestSOLID_ ./...

# 4. Profiling shows no single hotspot > 30%
go test -bench=BenchmarkSelect -benchtime=10000x -cpuprofile=/tmp/final.prof ./benchmarks/
go tool pprof -top -cum /tmp/final.prof | head -5
```
