# Task 2.4 — FTS3/4/5 (Full-Text Search)

> **Phase**: 2 — Full Feature Coverage
> **Status**: 🔲 Not started
> **Files**: `internal/fts/` (existing), `internal/vtab/` (virtual table interface)
> **SQLite ref**: `/Users/muaddib/dev/sqlite/ext/fts3/`, `ext/fts5/`
> **Prerequisite**: Phase 1 complete
> **Estimated**: 5-6 sessions (largest single feature)

## Description

Implement full-text search via FTS3/4/5 virtual table modules. Tokenizers,
inverted index storage, MATCH query parsing, BM25 ranking, and auxiliary
functions.

## Steps

- [ ] Remove `fts*` from `unsupportedTestFiles`
- [ ] Run: `FRIGOLITE_TEST=fts go test -run "^TestSQLiteSuite$" .` — capture baseline failures
- [ ] **Tokenizers**: `simple` (split on whitespace + punctuation), `porter` (stemmer),
      `unicode61` (unicode-aware, remove diacritics), `ascii` (ASCII only).
      Reference: `ext/fts3/fts3_tokenizer1.c`.
- [ ] **Inverted index storage**: content table + segment b-tree for term→docid→position.
      Segment merge (automatically when segments accumulate).
- [ ] **FTS virtual table**: implement `vtab.VirtualTable` interface:
      Open, BestIndex, Filter, Next, Eof, Column, Rowid, Close,
      Update, BeginTransaction, CommitTransaction, RollbackTransaction,
      FindFunction, Rename.
- [ ] **MATCH query parser**: parse `col MATCH 'expr'` — support bare phrases,
      `"quoted phrase"`, `col:value`, `+term`, `-term`, `AND`, `OR`, `NEAR`.
      Reference: `ext/fts3/fts3_expr.c`.
- [ ] **FTS3 cursor execution**: look up matching docids in index, iterate results,
      rank by BM25 (matchinfo + offsets for snippet/bm25 aux functions).
- [ ] **Auxiliary functions**: `snippet()`, `offsets()`, `matchinfo()`, `bm25()`,
      `highlight()`. Reference: `ext/fts3/fts3_aux.c`.
- [ ] **FTS5**: different MATCH syntax, different ranking, `rank` column, `detail` option.
      Implement after FTS3/4 baseline works. Reference: `ext/fts5/`.
- [ ] Verify: `FRIGOLITE_TEST=fts go test -run "^TestSQLiteSuite$" .` — all pass
- [ ] **Commit** with message: `P2.4: implement FTS3/4/5 — full-text search`

## Verification

```bash
FRIGOLITE_TEST=fts go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 300s .
```

## Session notes

- Started:
- Completed:
- Tokenizers implemented:
- MATCH parser status:
- Baseline failures:
- Final failures:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
