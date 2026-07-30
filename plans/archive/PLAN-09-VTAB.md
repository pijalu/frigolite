# PLAN-P9 — Virtual Tables: amatch & Extensions

> **Prerequisite**: P8 (vtab framework hardened by FTS implementation).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/`
>   - amatch: `ext/misc/amatch.c`
>   - Virtual table interface: `src/vtab.c`, `src/vtab.h`
>   - Virtual table tests: `test/amatch1.test`
> **Goal**: Implement the `amatch` (approximate match) virtual table module
> and fix remaining virtual table issues.

## Scope

~3 failures in `amatch1`.

## What is amatch?

The `amatch` (approximate match) module is a virtual table that finds words in
a dictionary that are close to a query word (within a edit distance). It's used
for fuzzy string matching.

**Usage:**
```sql
CREATE VIRTUAL TABLE temp.vword USING amatch(
    vocab='/path/to/dict',
    vocabulary_table='t1', word_column='word'
);

SELECT word, distance FROM vword
WHERE word MATCH 'korporation' AND distance < 300;
```

## Implementation Steps

### Step 1: Read the amatch source and test

**Reference files:**
- `ext/misc/amatch.c` — the C implementation.
- `test/amatch1.test` — the test file.

**Understand the test requirements:**
```bash
cat /Users/muaddib/dev/frigolite/ori/sqlite/test/amatch1.test
```

### Step 2: Implement the amatch module

**File:** `internal/vtab/amatch/amatch.go` (NEW).

**Design:**
```go
package amatch

type AmatchModule struct{}

type AmatchTable struct {
    vocab       []string  // dictionary words
    wordColumn  string
    // ...
}

type AmatchCursor struct {
    table    *AmatchTable
    matches  []AmatchMatch
    position int
}

type AmatchMatch struct {
    Word     string
    Distance int
}
```

**Edit distance algorithm:** Levenshtein distance between the query word and
each dictionary word. Filter by maximum distance.

### Step 3: Register amatch module

**File:** `internal/vtab/vtab.go`.

```go
func init() {
    r := DefaultRegistry
    r.Register("amatch", &amatch.AmatchModule{})
}
```

### Step 4: Verify against amatch1 test

```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run '^TestSQLiteSuite/amatch1/' . 2>&1
```

## Files Modified

| File | Change |
|------|--------|
| `internal/vtab/amatch/amatch.go` (NEW) | amatch virtual table module |
| `internal/vtab/vtab.go` | Register amatch |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run '^TestSQLiteSuite/amatch1/' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
make quality
go test -run TestSOLID_ ./...
```
