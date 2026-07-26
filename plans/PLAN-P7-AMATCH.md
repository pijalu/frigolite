# PLAN-P7-AMATCH.md — Approximate Match Virtual Table

## Scope
Implement the "approximate_match" (amatch) virtual table extension for spelling correction / fuzzy match functionality.

## Current Failures (3)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| amatch1 | 3 | No amatch virtual table implementation |

## SQLite amatch Extension
The amatch extension (from `ext/misc/amatch.c`) implements a virtual table that performs approximate matching against a dictionary using the Levenshtein distance algorithm.

### Usage
```sql
CREATE VIRTUAL TABLE dict USING approximate_match(
    vocabulary_table='dictionary',
    column='word',
    maximum_distance=10
);
SELECT word, distance FROM dict WHERE word MATCH 'exmple' AND distance <= 3;
```

### How it works
1. Takes a vocabulary table/column as input
2. For each query (via MATCH), computes Levenshtein distance to all vocabulary entries
3. Returns entries within the specified maximum distance
4. Optimized with V-shaped trie for efficient prefix-based filtering

## Implementation Approach

### Step 1: Create amatch module
Create `internal/vtab/amatch/amatch.go`:
```go
type ApproximateMatchModule struct {
    // configuration from CREATE VIRTUAL TABLE
    vocabularyTable string
    vocabularyColumn string
    maxDistance int
}
```

### Step 2: Implement Levenshtein Distance
Compute edit distance between two strings (standard Levenshtein):
- Insertion cost: 1
- Deletion cost: 1
- Substitution cost: 1

### Step 3: Implement the virtual table interface
Using the virtual table module system from `internal/vtab/vtab.go`:
1. Implement `Module` interface (Create, Connect, Disconnect, Destroy, Open, Close, BestIndex, Filter, Next, Eof, Column, Rowid)
2. Implement `Cursor` interface

### Step 4: Implement Filter with MATCH
1. Parse the MATCH argument to get the query string
2. Iterate over all entries in the vocabulary table
3. Compute Levenshtein distance for each entry
4. Return entries within max_distance

### Step 5: Optimization
For efficiency, implement:
1. V-shaped trie filtering (skip words that are too short/long to be within edit distance)
2. Limit to top-N closest matches

## Verification
```bash
go test -v -run "TestSQLiteSuite/amatch1" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
go test -v -run "TestSQLiteSuite/amatch1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files
- `internal/vtab/amatch/amatch.go` — NEW: amatch virtual table module
- `internal/vtab/vtab.go` — register amatch module
- `internal/exec/engine.go` — MATCH operator handling for virtual tables
