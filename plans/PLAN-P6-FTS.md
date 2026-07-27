# PLAN-P6-FTS.md — Full-Text Search Implementation (Updated 2026-07-27)

## Scope
Implement FTS3/4/5 virtual table modules for full-text search. Largest effort phase (~284 tests).

## Current State
FTS3/4/5 registered as `NoopModule` in `internal/vtab/vtab.go`:
```go
r.Register("fts3", &NoopModule{ModuleName: "fts3"})
r.Register("fts4", &NoopModule{ModuleName: "fts4"})
r.Register("fts5", &NoopModule{ModuleName: "fts5"})
```

## SQLite FTS Architecture

```
FTS Virtual Table
├── FTS3: Basic full-text search
│   ├── Content table (%_content)
│   ├── Segments table (%_segments, %_segdir)
│   ├── Term index (docid → term positions)
│   └── Tokenizers: simple, porter, unicode61
├── FTS4: Enhanced FTS
│   ├── All FTS3 features +
│   ├── content=, compress=/uncompress= options
│   ├── Aux functions: snippet(), offsets(), matchinfo()
│   └── Rank function
└── FTS5: Newer API
    ├── Different module name ("fts5")
    ├── Different syntax: `fts5_table MATCH 'query'`
    ├── bm25 ranking function
    ├── rank column
    └── Column filters
```

## New Package: `internal/fts/`

```
internal/fts/
├── fts3.go          # FTS3 module + table + cursor
├── fts5.go          # FTS5 module + table + cursor
├── tokenizer.go     # Tokenizers: simple, unicode61, porter stemmer
├── storage.go       # Term index storage (in-memory + content table)
└── fts3_test.go     # Unit tests
```

## Implementation Steps (Ordered)

### Step 1: FTS Module Framework
**Files:** `internal/fts/fts3.go` (new), `internal/vtab/vtab.go` (update registration)

**Create FTS3Module implementing vtab.Module:**
```go
type FTS3Module struct{}
type FTS3Table struct {
    name      string
    cols      []FTS3Column
    tokenizer Tokenizer
    content   *FTS3Content
    segments  *FTS3Segments
}
type FTS3Cursor struct {
    table    *FTS3Table
    rows     []FTS3Row
    position int
}
```

1. Create `internal/fts/` package
2. Define types and interfaces
3. Implement `Module.Create()` and `Module.Connect()`
4. Implement `VirtualTable.BestIndex()` and `VirtualTable.Open()`
5. Register FTS3/4/5 in vtab.go: replace NoopModule with real modules
6. **Verify:** Tests no longer panic

### Step 2: Tokenizer
**Files:** `internal/fts/tokenizer.go` (new)

```go
type Tokenizer interface {
    Tokenize(text string) []Token
}
type Token struct {
    Term     string // normalized (lowercased)
    Position int    // 0-based position in document
    Start    int    // byte offset in original text
    End      int    // byte offset end
}
```

**Simple tokenizer:** Split on whitespace/punctuation, lowercase each token
**Unicode61 tokenizer:** Use Go `unicode` package for character classification, case folding via `strings.ToLower`
**Porter stemmer:** Implement or adapt Porter stemmer algorithm

### Step 3: Term Index Storage
**Files:** `internal/fts/storage.go` (new)

**In-memory term index:**
```go
type FTS3Content struct {
    rows      map[int64]FTSRow // docid → row data
    nextDocID int64
}
type FTS3Segments struct {
    index map[string][]Posting // term → list of (docid, column, position)
}
type Posting struct {
    DocID    int64
    Column   int
    Position int
}
```

**Operations:** Insert, Delete, Update, Query, QueryPhrase

### Step 4: MATCH Operator
**Files:** `internal/fts/fts3.go`, `internal/sql/lexer.go`, `internal/sql/parser.go`

**Check if MATCH token exists in lexer** — add `TokenMatch` if needed
**Parser:** `MATCH` as binary operator
**Executor:** When encountering MATCH operator:
1. If left operand is an FTS virtual table column, call FTS3Table.Match(query)
2. Return matching docids

**FTS query syntax:** Single term, prefix (`term*`), phrase (`"exact phrase"`), column prefix, NEAR, AND/OR/NOT

### Step 5: FTS3 Full Implementation
**Files:** `internal/fts/fts3.go`, `internal/fts/storage.go`

**Cursor operations:**
- `Filter(constraints, order)` — execute query, populate cursor rows
- `Next() bool` — advance to next row
- `Column(idx int) (interface{}, error)` — return column value
- `Eof() bool` — check if done
- `Rowid() (int64, error)` — return current rowid

**Content table management:** Create `%_content` table, handle INSERT/DELETE/SELECT

### Step 6: FTS4 Features
**Files:** `internal/fts/fts4.go` (new, or extend fts3.go)

**FTS4-specific:**
- `content=` option: external content table
- `compress=` / `uncompress=` : compression
- `matchinfo=` : match information for ranking
- `prefix=` : prefix indexing

**Aux functions:** `snippet()`, `offsets()`, `matchinfo()`

### Step 7: FTS5 Compatibility
**Files:** `internal/fts/fts5.go` (new)

**FTS5 differences:** Different module name, different MATCH query interface, `rank` column, bm25 ranking, column filters

### Step 8: FTS Aux Tables
**Files:** `internal/fts/fts4aux.go`, `internal/fts/fts3tok.go`

**FTS4 aux virtual tables:** `fts4aux(table)`, `fts3tokenize(tokenizer, text)`

### Step 9: Edge Cases and Robustness
- FTS3 corrupt database tests
- FTS3 fault/injection tests
- FTS3 with UNION ALL queries
- FTS3 sort order tests
- FTS3 prefix queries
- FTS3 deferred tokenization
- FTS3 drop module tests

## Completion Check

```bash
go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Verification After Each Step

```bash
go test -v -run "TestSQLiteSuite/fts3" . 2>&1 | grep -E "PASS|FAIL" | head -5
go test -v -run "TestSQLiteSuite/fts4" . 2>&1 | grep -E "PASS|FAIL" | head -5
go test -v -run "TestSQLiteSuite/fts5" . 2>&1 | grep -E "PASS|FAIL" | head -5
```

## SOLID Design Notes

1. **Single Responsibility**: Each file/type has one job (tokenizer, storage, module, cursor)
2. **Open/Closed**: New tokenizers by implementing Tokenizer interface
3. **Interface Segregation**: Tokenizer interface is minimal (one method)
4. **Dependency Inversion**: FTS module depends on vtab.Module interface

## Go Standard Library Usage

| Feature | Go stdlib |
|---------|-----------|
| Text case folding | `strings.ToLower()`, `unicode.SimpleFold()` |
| Unicode classification | `unicode.IsLetter()`, `unicode.IsDigit()` |
| String splitting | `strings.Fields()`, `strings.FieldsFunc()` |
| Sorting | `sort.Slice()` |
| Map storage | `map[string][]Posting` |
| Compression (compress=) | `compress/gzip` or `compress/flate` |
| CRC | `hash/crc32` |

## Reference Files

| Resource | Location |
|----------|----------|
| SQLite FTS3 C source | `/Users/muaddib/dev/sqlite/src/fts3.c` |
| SQLite FTS3 tokenizer | `/Users/muaddib/dev/sqlite/src/fts3_tokenizer.c` |
| SQLite FTS3 porter | `/Users/muaddib/dev/sqlite/src/fts3_porter.c` |
| SQLite FTS3 unicode61 | `/Users/muaddib/dev/sqlite/src/fts3_unicode.c` |
| SQLite FTS5 source | `/Users/muaddib/dev/sqlite/src/fts5.c` |
| FTS test files | `/Users/muaddib/dev/sqlite/test/fts3*.test` |
| FTS JSON test data | `/Users/muaddib/dev/frigolite/testdata/fts*.json` |
| vtab interface | `/Users/muaddib/dev/frigolite/internal/vtab/vtab.go` |

## Goal Integration

```json
{
  "objective": "Implement FTS3/4/5 full-text search virtual table modules: tokenizer, term index storage, MATCH operator, FTS3/4/5 module interfaces, content table management, aux functions, and edge cases",
  "completionCriterion": "All FTS suites pass with zero FAIL across fts3, fts4, fts5, fts4aux, fts3tok, fts3corrupt, fts3fault, fts3sort, fts3prefix, fts3defer, fts3drop",
  "verifyCommand": "go test -v -run \"TestSQLite_.*fts\" . 2>&1 | grep -c \"FAIL\" | xargs test 0 -eq"
}
```
