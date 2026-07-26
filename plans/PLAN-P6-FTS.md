# PLAN-P6-FTS.md — Full-Text Search Implementation

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
│   ├── Content table (actual text storage, %_content)
│   ├── Segments table (%_segments, %_segdir)  
│   ├── Term index (docid → term positions)
│   └── Tokenizers: simple, porter, unicode61, icu
├── FTS4: Enhanced FTS
│   ├── All FTS3 features +
│   ├── content= option (external content)
│   ├── compress=/uncompress= options
│   ├── matchinfo= option
│   ├── Aux functions: snippet(), offsets(), matchinfo()
│   └── Rank function (built-in ranking)
└── FTS5: Newer API (different module)
    ├── Different module name ("fts5" instead of "fts3")
    ├── Different syntax: `fts5_table MATCH 'query'`
    ├── bm25 ranking function
    ├── rank column (special column for ranking)
    └── Column filters (col1:col2 syntax)
```

## Implementation Architecture (Go)

### New Package: `internal/fts/`

```
internal/fts/
├── fts3.go          # FTS3 module + table + cursor
├── fts5.go          # FTS5 module + table + cursor (extends fts3 concepts)
├── tokenizer.go     # Tokenizers: simple, unicode61, porter stemmer
├── storage.go       # Term index storage (in-memory + content table)
└── fts3_test.go     # Unit tests
```

### Design Decisions

1. **Storage**: Use Go maps + slices for in-memory term index, and a real content table for persistence
2. **Tokenizers**: Implement simple (whitespace/punctuation split + lowercase), unicode61 (unicode-aware), porter (simple stemming via Go library or custom implementation)
3. **MATCH**: Parse FTS query syntax, evaluate against term index, return matching rowids
4. **FTS content tables**: Use the existing btree/pager infrastructure for %_content tables

## Implementation Steps

### Step 1: FTS Module Framework (~2 days)
**Files:** `internal/fts/fts3.go` (new), `internal/vtab/vtab.go` (update registration)

**Create FTS3Module implementing vtab.Module:**
```go
type FTS3Module struct{}

func (m *FTS3Module) Create(args []string) (vtab.VirtualTable, error)
func (m *FTS3Module) Connect(args []string) (vtab.VirtualTable, error)

type FTS3Table struct {
    name       string
    cols       []FTS3Column // content columns + docid
    tokenizer  Tokenizer
    content    *FTS3Content  // content storage
    segments   *FTS3Segments // segment storage
}

type FTS3Cursor struct {
    table    *FTS3Table
    rows     []FTS3Row   // result set
    position int         // current position in result set
}
```

**Steps:**
1. Create `internal/fts/` package
2. Define `FTS3Column`, `FTS3Content`, `FTS3Segments` types
3. Implement `Module.Create()` — parse CREATE VIRTUAL TABLE args
4. Implement `Module.Connect()` — same as Create for simple case
5. Implement `VirtualTable.BestIndex()` — cost estimation
6. Implement `VirtualTable.Open()` — create cursor
7. Register FTS3 in vtab.go: `r.Register("fts3", &fts.FTS3Module{})`
8. **Verify:** `go test -run "TestSQLiteSuite/fts3"` — should no longer panic

### Step 2: Tokenizer (~1 day)
**Files:** `internal/fts/tokenizer.go` (new)

**Tokenizer interface:**
```go
type Tokenizer interface {
    Tokenize(text string) []Token
}

type Token struct {
    Term     string // normalized term (lowercased)
    Position int    // position in document (0-based)
    Start    int    // byte offset in original text
    End      int    // byte offset end
}
```

**Simple tokenizer:**
- Split on whitespace and punctuation
- Lowercase each token
- Filter tokens < 1 character

**Unicode61 tokenizer:**
- Use Go's `unicode` package for character classification
- Default separators: whitespace + punctuation from Unicode categories
- Case folding via `strings.ToLower` (works for ASCII; unicode folding via `unicode.SimpleFold`)

**Porter stemmer:**
- Implement or adapt the Porter stemmer algorithm in Go
- Or use a simplified stemming approach for test passage

**Verify:** Create unit tests for tokenization.

### Step 3: Term Index Storage (~1 day)
**Files:** `internal/fts/storage.go` (new)

**In-memory term index:**
```go
type FTS3Content struct {
    rows    map[int64]FTSRow  // docid → row data
    nextDocID int64
}

type FTSRow struct {
    DocID int64
    Values []string  // column values
}

type FTS3Segments struct {
    // term → list of (docid, column, position)
    index map[string][]Posting
}

type Posting struct {
    DocID    int64
    Column   int
    Position int
}
```

**Operations:**
- `Insert(docID int64, values []string)` — tokenize each column, build postings
- `Delete(docID int64)` — remove from index
- `Update(docID int64, values []string)` — delete + insert
- `Query(term string) []Posting` — lookup in index
- `QueryPhrase(terms []string) []Posting` — phrase query (positions must match)

**Verify:** `go test -run "TestSQLiteSuite/fts3"` — should show INSERT success

### Step 4: MATCH Operator (~1 day)
**Files:** `internal/fts/fts3.go`, `internal/sql/lexer.go`, `internal/sql/parser.go`

**Check if MATCH token exists in lexer:**
- `MATCH` should be a keyword token
- If not, add `TokenMatch` to lexer

**Parser:**
- `MATCH` should be parsed as a binary operator: `expr MATCH expr`
- In WHERE clause: `t1 MATCH 'query'`

**Executor:**
- In engine.go, when encountering MATCH operator:
  1. If left operand is a virtual table column, check if it's an FTS table
  2. Call FTS3Table.Match(query) → returns matching docids
  3. Use docids to filter rows

**FTS query syntax:**
- Single term: `'term'` → exact match
- Prefix: `'term*'` → prefix match
- Phrase: `'"exact phrase"'` → phrase match
- Column prefix: `'col : term'` → match in specific column
- NEAR: `'term1 NEAR term2'` → proximity match
- AND/OR/NOT: `'term1 AND term2'` → boolean operators

**Verify:** `go test -run "TestSQLiteSuite/fts3expr"`

### Step 5: FTS3 Full Implementation (~2 days)
**Files:** `internal/fts/fts3.go`, `internal/fts/storage.go`

**Implement cursor operations:**
- `Filter(constraints, order)` — execute query, populate cursor rows
- `Next() bool` — advance to next row
- `Column(idx int) (interface{}, error)` — return column value
- `Eof() bool` — check if done
- `Rowid() (int64, error)` — return current rowid

**Content table management:**
- Create `%_content` table in storage for persistence
- On INSERT: tokenize, store in content table, update term index
- On DELETE: remove from content table, update term index
- On SELECT: read from content table

**Verify:** `go test -run "TestSQLiteSuite/fts3"` — SELECT should return rows

### Step 6: FTS4 Features (~2 days)
**Files:** `internal/fts/fts4.go` (new, or extend fts3.go)

**FTS4-specific:**
- `content=` option: use external table for content, not internal content table
- `compress=` / `uncompress=` : custom compression (store as-is for now)
- `matchinfo=` : store match information for ranking
- `prefix=` : prefix indexing

**Aux functions:**
- `snippet(FTS_table, start_marker, end_marker, ellipsis, column)` — extract snippet
- `offsets(FTS_table)` — return term offsets
- `matchinfo(FTS_table)` — return match information

**Implementation approach for aux functions:**
- These are SQL functions that take an FTS table as argument
- The function implementation accesses the FTS table's match info
- Store match info in cursor state during Filter()
- On aux function call, retrieve from cursor state

**Verify:** `go test -run "TestSQLiteSuite/fts4"`

### Step 7: FTS5 Compatibility (~2 days)
**Files:** `internal/fts/fts5.go` (new)

**FTS5 differences from FTS3/4:**
- Different module name: `fts5` (not `fts3`/`fts4`)
- `CREATE VIRTUAL TABLE t USING fts5(content)` — same syntax but different parser
- MATCH query syntax differs: `t MATCH 'query'` (table-level, not column-level)
- `rank` is a special hidden column that returns bm25 score
- Built-in bm25 ranking function
- Column filters in queries: `col1:col2 query`
- No content table (content stored differently in FTS5)

**Implementation:**
- Create FTS5Module that shares FTS3 storage but has different query interface
- Implement bm25 ranking (standard text retrieval ranking function)
- Support `rank` column
- Support column filters

**Verify:** `go test -run "TestSQLiteSuite/fts5"`

### Step 8: FTS Aux Tables (~1 day)
**Files:** `internal/fts/fts4aux.go`, `internal/fts/fts3tok.go`

**FTS4 aux virtual tables:**
- `fts4aux(table)` — auxiliary table with term statistics
- `fts3tokenize(tokenizer, text)` — show tokenization results

**Verify:** `go test -run "TestSQLiteSuite/fts4aux\|fts3tok"`

### Step 9: Edge Cases and Robustness (~1 day)
**Files:** Various

- Handle FTS3 corrupt database tests
- Handle FTS3 fault/injection tests  
- Handle FTS3 with UNION ALL queries
- Handle FTS3 sort order tests
- Handle FTS3 prefix queries
- Handle FTS3 deferred tokenization
- Handle FTS3 drop module tests

**Verify:** `go test -run "TestSQLiteSuite/fts3fault\|fts3corrupt\|fts3sort\|fts3prefix\|fts3drop\|fts3defer"`

## Verification

```bash
# After each step:
go test -v -run "TestSQLiteSuite/fts3" . 2>&1 | grep -E "PASS|FAIL" | head -5
go test -v -run "TestSQLiteSuite/fts4" . 2>&1 | grep -E "PASS|FAIL" | head -5
go test -v -run "TestSQLiteSuite/fts5" . 2>&1 | grep -E "PASS|FAIL" | head -5

# Full FTS suite:
go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -E "PASS|FAIL" 
```

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite && go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## SOLID Design Notes

1. **Single Responsibility**: Each file/type has one job (tokenizer, storage, module, cursor)
2. **Open/Closed**: New tokenizers can be added by implementing the Tokenizer interface
3. **Interface Segregation**: Tokenizer interface is minimal (one method)
4. **Dependency Inversion**: FTS module depends on vtab.Module interface, not concrete types

## Go Standard Library Usage

| Feature | Go stdlib |
|---------|-----------|
| Text case folding | `strings.ToLower()`, `unicode.SimpleFold()` |
| Unicode character classification | `unicode.IsLetter()`, `unicode.IsDigit()` |
| String splitting | `strings.Fields()`, `strings.FieldsFunc()` |
| Sorting | `sort.Slice()` |
| Map storage | `map[string][]Posting` |
| Compression (compress=) | `compress/gzip` or `compress/flate` |
| CRC | `hash/crc32` (already used in storage package) |

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
| NoopModule stub | `/Users/muaddib/dev/frigolite/internal/vtab/vtab.go` (lines 148-185) |
