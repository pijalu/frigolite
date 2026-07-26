# PLAN-P6-FTS.md — Full-Text Search Implementation

## Scope
Implement FTS3/4/5 virtual table modules for full-text search functionality.

## Current State
FTS3/4/5 are registered as `NoopModule` in `internal/vtab/vtab.go`:
```go
r.Register("fts3", &NoopModule{ModuleName: "fts3"})
r.Register("fts4", &NoopModule{ModuleName: "fts4"})
r.Register("fts5", &NoopModule{ModuleName: "fts5"})
```

The NoopModule always returns empty results and allows operations without errors. This means:
- `CREATE VIRTUAL TABLE t1 USING fts3(content)` succeeds
- `INSERT INTO t1 VALUES('text')` succeeds (no-op)
- `SELECT * FROM t1` returns empty set

## SQLite FTS Architecture
FTS3/4/5 are full-text search engines that:
1. Store text content in a specialized b-tree structure (segmented by content)
2. Support full-text queries: MATCH, NEAR, phrase queries, prefix queries
3. Support ranking (FTS4 has built-in rank, FTS5 has bm25)
4. Support content= tables for external content storage
5. Support tokenizers: simple, porter, unicode61, icu, etc.

### Minimal FTS Implementation
For test passage, we need:
1. Virtual table creation via `CREATE VIRTUAL TABLE ... USING fts3(content)`
2. INSERT into FTS table
3. SELECT from FTS table with rowid
4. MATCH operator in WHERE clause
5. Basic tokenization (split on whitespace and punctuation)
6. FTS4 aux functions: `snippet()`, `offsets()`, `matchinfo()`

## Implementation Steps

### Step 1: FTS Virtual Table Module
Create `internal/fts/fts3.go` with:
1. `FTS3Module` struct implementing `vtab.Module` interface
2. `FTS3Table` struct holding:
   - Column definitions (from CREATE TABLE)
   - Content table (segmented storage)
   - Tokenizer
3. `FTS3Cursor` struct implementing cursor interface

### Step 2: FTS Storage
FTS stores text in segments/documents:
1. Each row has a rowid (document ID)
2. Each text column is tokenized into terms
3. A term index maps term → {document ID, column, position}
4. Use a content b-tree (in-memory or storage) for the term index

### Step 3: Tokenizer Implementation
Simple tokenizer:
1. Split text on whitespace and punctuation
2. Lowercase each token
3. Handle unicode folding (basic ASCII case folding at minimum)
4. Support for prefix queries (trailing `*`)

### Step 4: MATCH Operator
1. Parse MATCH expression in WHERE clause
2. Query the term index for matching documents
3. Return matching rowids
4. Support basic query syntax:
   - Single term: `col MATCH 'term'`
   - Phrase: `col MATCH '"exact phrase"'`
   - Prefix: `col MATCH 'prefix*'`
   - Column prefix: `col MATCH 'prefix*'`

### Step 5: FTS4 Enhancements
1. Support content= option for external content
2. Support compress= and uncompress= options
3. Support matchinfo= option (for FTS4 matchinfo function)
4. Implement snippet(), offsets() aux functions

### Step 6: FTS5 Compatibility
1. FTS5 uses a different API (CREATE VIRTUAL TABLE ... USING fts5)
2. FTS5 has different syntax for MATCH
3. FTS5 supports bm25 ranking
4. FTS5 supports column filters
5. FTS5 supports "rank" as a special column

## Verification
```bash
# Basic FTS tests
go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -E "PASS|FAIL"
# FTS3/4/5 compat tests
go test -v -run "TestSQLite_.*fts3|TestSQLite_.*fts4|TestSQLite_.*fts5" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
# All FTS tests pass
go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files
- `internal/fts/fts3.go` — NEW: FTS3 module implementation
- `internal/fts/tokenizer.go` — NEW: text tokenization
- `internal/fts/storage.go` — NEW: FTS storage layer
- `internal/vtab/vtab.go` — register FTS modules
- `internal/exec/engine.go` — MATCH operator support
- `internal/sql/parser.go` — MATCH operator parsing (if not already)
- `internal/sql/lexer.go` — MATCH token (if not already)
