# PLAN-P8 — FTS3/4/5: Full-Text Search

> **Prerequisite**: P0 (test infrastructure). P2 helps (MATCH operator parsing).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/`
>   - FTS3 core: `ext/fts3/fts3.c` (6 219 lines)
>   - FTS3 tokenizer: `ext/fts3/fts3_tokenizer.c` (516 lines)
>   - FTS3 Porter stemmer: `ext/fts3/fts3_porter.c` (662 lines)
>   - FTS3 unicode61: `ext/fts3/fts3_unicode.c` (397 lines)
>   - FTS3 expression parser: `ext/fts3/fts3_expr.c` (1 317 lines)
>   - FTS3 snippet/highlight: `ext/fts3/fts3_snippet.c` (1 777 lines)
>   - FTS5 core: `ext/fts5/` (27 336 lines total)
>   - FTS test files: `test/fts3*.test`, `test/fts5*.test`
> **Goal**: Implement FTS3/4/5 virtual table modules for full-text search.

## Scope

~59 failing compat test functions + any harness tests that use FTS (currently
filtered out by the converter, but P0 removes the filter).

This is the **largest single phase**. SQLite's FTS implementation is ~47 000
lines of C across FTS3 and FTS5.

## Strategy: FTS3 First, Then FTS5

FTS3/4 share the same codebase (`ext/fts3/`). FTS5 is a complete rewrite
(`ext/fts5/`). Most tests use FTS3 syntax. Implement FTS3/4 first, then add
FTS5 compatibility.

**Pragmatic scope:** Not every FTS3 edge case is tested. Focus on:
1. Creating FTS tables.
2. Inserting documents.
3. MATCH queries (single term, phrase, AND/OR/NOT, prefix).
4. Basic aux functions (snippet, offsets).
5. unicode61 tokenizer (most common).

## New Package: `internal/fts/`

```
internal/fts/
├── fts3.go           # FTS3/4 module: Create, Connect, BestIndex, Open cursor
├── fts3insert.go     # INSERT/DELETE/UPDATE handling
├── fts3query.go      # MATCH query expression parser
├── fts3cursor.go     # Cursor: Filter, Next, Column, Eof
├── tokenizer.go      # Tokenizer interface + simple/unicode61/porter
├── porter.go         # Porter stemmer
├── storage.go        # In-memory inverted index
├── snippet.go        # snippet(), offsets(), highlight()
├── fts5.go           # FTS5 module (subset)
├── fts3_test.go      # Unit tests
└── doc.go            # Package documentation
```

**SOLID layer:** Add `internal/fts` to `internalLayers` in
`frigolite_solid_test.go` (layer number higher than `vtab`).

## SQLite FTS3 Architecture

### Shadow tables

When you `CREATE VIRTUAL TABLE ft USING fts3(content)`, SQLite creates:
- `%_content` — stores the original text (docid, content columns)
- `%_segments` — B-tree segments of the inverted index
- `%_segdir` — segment directory (which segments exist)
- `%_stat` — statistics (number of rows, etc.)

For frigolite's in-memory implementation, we can simplify:
- Store content directly in the virtual table's Go struct.
- Store the inverted index in memory (no shadow tables needed).

### MATCH query syntax

The FTS MATCH expression parser (`ext/fts3/fts3_expr.c`):
```
query    := expr
expr     := phrase (OR phrase)*
phrase   := term (term | AND term | NOT term | NEAR term)*
term     := column_prefix? (string | prefix_string)
prefix   := string '*'
column   := string ':'
NEAR     := 'NEAR' '(' phrase ',' phrase [',' distance] ')'
```

### Tokenizers

| Tokenizer | Behavior |
|-----------|----------|
| `simple` | Split on non-alphanumeric, lowercase |
| `porter` | Simple + Porter stemmer |
| `unicode61` | Unicode-aware: letters/digits are tokens, case-folded |

## Implementation Steps

### Step 1: Create the FTS package skeleton

**File:** `internal/fts/fts3.go` (NEW).

```go
package fts

// FTS3Module implements vtab.Module for FTS3/4.
type FTS3Module struct {
    Name string // "fts3", "fts4", "fts5"
}

func (m *FTS3Module) Create(db interface{}, args []string) (vtab.VirtualTable, error) {
    // Parse column definitions from args
    // Parse tokenizer option
    // Create the FTS table
}

func (m *FTS3Module) Connect(db interface{}, args []string) (vtab.VirtualTable, error) {
    // Same as Create for in-memory implementation
}
```

**Register in vtab:**
```go
// internal/vtab/vtab.go
func init() {
    r := DefaultRegistry
    r.Register("fts3", fts.NewFTS3Module("fts3"))
    r.Register("fts4", fts.NewFTS3Module("fts4"))
    r.Register("fts5", fts.NewFTS5Module())
}
```

### Step 2: Implement tokenizers

**File:** `internal/fts/tokenizer.go`, `internal/fts/porter.go`.

```go
type Token struct {
    Term     string
    Position int
}

type Tokenizer interface {
    Tokenize(text string) []Token
}

// SimpleTokenizer: split on non-alphanumeric, lowercase
type SimpleTokenizer struct{}

func (t *SimpleTokenizer) Tokenize(text string) []Token {
    var tokens []Token
    fields := strings.FieldsFunc(text, func(r rune) bool {
        return !unicode.IsLetter(r) && !unicode.IsDigit(r)
    })
    for i, f := range fields {
        tokens = append(tokens, Token{
            Term:     strings.ToLower(f),
            Position: i,
        })
    }
    return tokens
}
```

**Unicode61 tokenizer:** Use `unicode.Is(unicode.Letter, r)` for better Unicode
support. Case-fold with `strings.ToLower()`.

**Porter stemmer:** Implement the classic Porter algorithm. Reference:
`ext/fts3/fts3_porter.c`. This is ~200 lines of Go.

### Step 3: Implement the inverted index storage

**File:** `internal/fts/storage.go`.

```go
type Posting struct {
    DocID    int64
    Column   int
    Position int
}

type InvertedIndex struct {
    index   map[string][]Posting  // term → postings
    docs    map[int64]*Document   // docid → document content
    nextID  int64
}

type Document struct {
    DocID   int64
    Columns [][]string  // column → tokens
}

func (idx *InvertedIndex) Insert(docid int64, columns []string) {
    // Tokenize each column, add postings
    for colNum, text := range columns {
        tokens := tokenizer.Tokenize(text)
        for _, tok := range tokens {
            idx.index[tok.Term] = append(idx.index[tok.Term], Posting{
                DocID:    docid,
                Column:   colNum,
                Position: tok.Position,
            })
        }
    }
}

func (idx *InvertedIndex) Query(term string) []int64 {
    // Return docids that contain the term
    postings := idx.index[term]
    // Deduplicate and return sorted docids
}
```

### Step 4: Implement FTS3 virtual table

**File:** `internal/fts/fts3.go`.

```go
type FTS3Table struct {
    name       string
    columns    []string
    tokenizer  Tokenizer
    index      *InvertedIndex
    contentless bool // content="" option
}

// Implement vtab.VirtualTable interface:
func (t *FTS3Table) BestIndex(info *vtab.IndexInfo) *vtab.IndexInfo {
    // Check for MATCH constraint
    // Return the constraint usage
}

func (t *FTS3Table) Open() (vtab.Cursor, error) {
    return &FTS3Cursor{table: t}, nil
}

func (t *FTS3Table) Insert(values []interface{}) (int64, error) {
    // Tokenize and add to index
    docid := t.index.nextID
    t.index.nextID++
    texts := make([]string, len(values))
    for i, v := range values {
        texts[i] = fmt.Sprintf("%v", v)
    }
    t.index.Insert(docid, texts)
    return docid, nil
}

func (t *FTS3Table) Delete(rowid int64) error {
    t.index.Delete(rowid)
    return nil
}
```

### Step 5: Implement MATCH query parser

**File:** `internal/fts/fts3query.go`.

```go
// Parse MATCH expression: "term1 term2" OR "phrase" OR term* OR col:term
type QueryNode interface {
    Match(docid int64, idx *InvertedIndex) bool
}

type TermNode struct{ Term string }
type PhraseNode struct{ Terms []string }
type AndNode struct{ Left, Right QueryNode }
type OrNode struct{ Left, Right QueryNode }
type NotNode struct{ Inner QueryNode }
type PrefixNode struct{ Prefix string }
type ColumnNode struct{ Column int; Inner QueryNode }

func ParseQuery(query string) (QueryNode, error) {
    // Recursive descent parser for FTS MATCH syntax
    // Reference: ext/fts3/fts3_expr.c
}
```

**MATCH syntax to handle:**
- `hello` → single term
- `"hello world"` → phrase (adjacent terms)
- `hello*` → prefix term
- `hello AND world` → AND
- `hello OR world` → OR
- `hello NOT world` → NOT
- `title:hello` → column-restricted
- `NEAR(hello, world)` → proximity search

### Step 6: Implement FTS3 cursor and MATCH execution

**File:** `internal/fts/fts3cursor.go`.

```go
type FTS3Cursor struct {
    table    *FTS3Table
    docids   []int64  // matching docids
    position int
}

func (c *FTS3Cursor) Filter(constraints []vtab.Constraint) error {
    // Find MATCH constraint
    // Parse the query
    // Execute against the index
    // Store matching docids
}

func (c *FTS3Cursor) Next() bool {
    c.position++
    return c.position < len(c.docids)
}

func (c *FTS3Cursor) Column(idx int) (interface{}, error) {
    docid := c.docids[c.position]
    doc := c.table.index.docs[docid]
    if idx < len(doc.Columns) {
        return strings.Join(doc.Columns[idx], " "), nil
    }
    return nil, nil
}

func (c *FTS3Cursor) Rowid() int64 {
    return c.docids[c.position]
}
```

### Step 7: Implement aux functions

**File:** `internal/fts/snippet.go`.

Functions to implement:
- `snippet(ft, col, start, end, ellipsis, max_tokens)` — return a text snippet
  around matches.
- `offsets(ft)` — return byte offsets of matches.
- `highlight(ft, col, start, end)` — wrap matches with markers.
- `matchinfo(ft)` — return binary match information.

**Register as SQL functions** in `internal/function/`:
```go
function.Register("snippet", snippetFunc)
function.Register("offsets", offsetsFunc)
function.Register("highlight", highlightFunc)
```

### Step 8: Add MATCH operator to parser and executor

**File:** `internal/sql/parser.go`, `internal/exec/engine.go`.

**Parser:**
- `MATCH` is a binary operator (like LIKE, GLOB).
- Left side: column reference or expression.
- Right side: MATCH query string.

**Executor:**
- When evaluating `column MATCH query`:
  - If the column is from an FTS virtual table:
    - Parse the query.
    - Execute against the FTS index.
    - Return whether the current row matches.

**Note:** The P0 fix removes the converter's filtering of `MATCH`.

### Step 9: Implement FTS5

**File:** `internal/fts/fts5.go`.

FTS5 differences from FTS3:
- Module name: `fts5`.
- Column syntax: `col : term` (space around colon).
- bm25() ranking function instead of rank().
- `rank` virtual column.
- Different table naming for shadow tables.
- `highlight()` and `snippet()` with different argument order.

**Pragmatic approach:** Reuse the FTS3 implementation with FTS5-compatible
syntax adjustments. Most tests work with either.

### Step 10: Handle FTS table options

Common FTS options from test files:
- `content=""` — contentless FTS table (no original text stored).
- `content=table` — external content table.
- `tokenize=simple` / `tokenize=unicode61` / `tokenize=porter`.
- `prefix='2 4'` — prefix indexing.

## Files Modified

| File | Change |
|------|--------|
| `internal/fts/*.go` (NEW package) | Full FTS3/4/5 implementation |
| `internal/vtab/vtab.go` | Register FTS modules |
| `internal/sql/parser.go` | MATCH operator |
| `internal/exec/engine.go` | MATCH evaluation for FTS tables |
| `internal/function/` | Register snippet, offsets, highlight, matchinfo |
| `frigolite_solid_test.go` | Add `internal/fts` to `internalLayers` |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# FTS3 tests
go test -v -count=1 -run '^TestSQLite_.*fts' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq

# FTS3 harness tests
go test -v -count=1 -run '^TestSQLiteSuite/fts3' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq

# Quality
make quality
go test -run TestSOLID_ ./...
```

## Scope Warning

FTS3/4/5 is ~47 000 lines of C in SQLite. A full reimplementation is months of
work. This plan focuses on the **most commonly tested subset**:

1. Basic CREATE, INSERT, SELECT with MATCH.
2. Simple and unicode61 tokenizers.
3. AND/OR/NOT/phrase/prefix queries.
4. snippet() and offsets().

**NOT included in initial scope:**
- Porter stemmer edge cases.
- FTS3 corruption tests (fts3corrupt*).
- FTS3 fault injection tests (fts3fault*).
- FTS3 merge/optimization tests.
- ICU tokenizer.
- External content tables.
- Detail=none / detail=column options.
- prefix= indexing.

These can be added incrementally after the core works.

## Reference: SQLite FTS3 Source Map

| File | Lines | Purpose |
|------|-------|---------|
| `ext/fts3/fts3.c` | 6 219 | Core FTS3 module, cursor, virtual table |
| `ext/fts3/fts3_expr.c` | 1 317 | MATCH expression parser |
| `ext/fts3/fts3_tokenizer.c` | 516 | Tokenizer interface |
| `ext/fts3/fts3_porter.c` | 662 | Porter stemmer |
| `ext/fts3/fts3_unicode.c` | 397 | Unicode61 tokenizer |
| `ext/fts3/fts3_unicode2.c` | — | Unicode tables |
| `ext/fts3/fts3_snippet.c` | 1 777 | snippet(), offsets() |
| `ext/fts3/fts3_write.c` | ~5 000 | Segment merge, write operations |
| `ext/fts3/fts3_hash.c` | — | Hash table (internal) |
| `ext/fts3/fts3_aux.c` | — | Auxiliary functions |
| `ext/fts3/fts3_term.c` | — | fts4aux virtual table |
| **Total FTS3** | **~20 000** | |
| `ext/fts5/` | **~27 000** | FTS5 (separate codebase) |
