# PLAN-P7-AMATCH.md — Approximate Match Virtual Table (Updated 2026-07-27)

## Scope
Implement the "approximate_match" (amatch) virtual table extension for fuzzy string matching.

## Current Failures (2 in amatch1)

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| amatch1 | 2 | No amatch virtual table implementation |

## Current State
amatch is not registered in the vtab registry — tests fail with "no such table: t1aux".

## Implementation Steps (Ordered)

### Step 1: Create amatch package
**File:** `internal/vtab/amatch/amatch.go` (new)

```go
package amatch

type ApproximateMatchModule struct{}

type amatchVTab struct {
    vocabularyTable  string
    vocabularyColumn string
    maxDistance      int
    engine interface{} // reference to access table data
}

type amatchCursor struct {
    vtab     *amatchVTab
    query    string
    results  []amatchResult
    position int
}

type amatchResult struct {
    term     string
    distance int
}
```

### Step 2: Implement Levenshtein Distance
**File:** `internal/vtab/amatch/amatch.go`

Standard Levenshtein with O(n×m) time, O(min(n,m)) space.

### Step 3: Implement Module interface
**File:** `internal/vtab/amatch/amatch.go`

- `Create(args []string) (vtab.VirtualTable, error)` — parse CREATE VIRTUAL TABLE args
- `Connect(args []string) (vtab.VirtualTable, error)` — same as Create
- `BestIndex(input []byte) ([]byte, error)` — cost estimation
- `Open() (vtab.Cursor, error)` — create cursor
- `Disconnect()` / `Destroy()` — cleanup

### Step 4: Implement Cursor interface
**File:** `internal/vtab/amatch/amatch.go`

- `Next() bool` — advance to next result
- `Column(idx int) (interface{}, error)` — return term (0) or distance (1)
- `Close() error` — cleanup
- `Rowid() (int64, error)` — return rowid

### Step 5: Implement Filter
**File:** `internal/vtab/amatch/amatch.go`

1. Receive MATCH query string via BestIndex/Filter constraints
2. Read all entries from vocabulary table
3. For each entry, compute Levenshtein distance
4. Filter by max_distance
5. Sort by distance
6. Store results in cursor

### Step 6: Register in vtab registry
**File:** `internal/vtab/vtab.go`

Add: `r.Register("approximate_match", &amatch.ApproximateMatchModule{})`

### Step 7: Engine integration
**File:** `internal/exec/engine.go`

The amatch module needs to read from a real table. Add a `TableScanner` callback mechanism that can read rows from any table by name. Pass to the vtab module during Connect/Create.

## Completion Check

```bash
go test -v -run "TestSQLiteSuite/amatch1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files

| File | Role |
|------|------|
| `internal/vtab/amatch/amatch.go` | NEW: module + cursor + levenshtein |
| `internal/vtab/vtab.go` | Register amatch module |
| `internal/exec/engine.go` | Pass table scanning capability to vtab |

## Goal Integration

```json
{
  "objective": "Implement approximate_match virtual table: Levenshtein distance, Module/Cursor interfaces, vocabulary table reading, result sorting by distance, engine integration",
  "completionCriterion": "amatch1 suite passes with zero FAIL",
  "verifyCommand": "go test -v -run \"TestSQLiteSuite/amatch1\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq"
}
```
