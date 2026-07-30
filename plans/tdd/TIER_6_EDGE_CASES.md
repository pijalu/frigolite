# TDD Tier 6 — Edge Cases & Infrastructure

> **Goal**: All remaining tests either pass or have clear skip messages.
> **Files**: ~400 files, ~4700 TODOs remaining.
> **Prerequisite**: Tiers 1-5 complete.

## Categories

### C API Tests (~2500 TODOs)
Tests that verify the SQLite C API (sqlite3_prepare, sqlite3_step,
sqlite3_finalize, etc.). These are not applicable to frigolite because
frigolite is pure Go and doesn't expose a C API.

**Strategy**: Emit `t.Skip("C API test: %s — not applicable to frigolite")`
for the file as a whole, or emit `t.Skip` for each test block that
exclusively uses C API calls.

Key files: `capi3c.test` (151 TODOs), `capi3.test` (137 TODOs),
`bind.test` (178 TODOs)

### Corruption Tests (~250 TODOs)
Tests that deliberately corrupt the database file and check that SQLite
handles it gracefully. Frigolite may handle corruption differently.

Key files: `corruptA.test`, `corruptB.test`, `fkey_corrupt.test`

### Memory/Fault Tests (~350 TODOs)
Tests that simulate OOM conditions, use specialized memory allocators,
and test fault tolerance. These require fault injection infrastructure.

Key files: `mallocA.test`, `mallocB.test`, `faultsim*.test`

### Concurrency/Locking Tests (~130 TODOs)
Tests with multiple threads, locking modes, and concurrency patterns.
Some may be applicable if frigolite supports concurrent access.

Key files: `lock*.test`, `thread*.test`

### Shell/CLI Tests (~300 TODOs)
Tests for the sqlite3 command-line shell. Not applicable to frigolite
(which has its own CLI in `cmd/frigolite/`).

Key files: `shell*.test`

### File Format/VFS Tests (~400 TODOs)
Tests for SQLite's Virtual File System layer, file format internals,
and low-level I/O. Not applicable to frigolite.

Key files: `vfs*.test`, `mmap*.test`, `wal*.test`

### Extension Tests (~200 TODOs)
Tests for SQLite loadable extensions (FTS, JSON, RTree, etc.).
Some may apply if frigolite has equivalent functionality.

Key files: `fts3*.test`, `json*.test`, `rtree*.test`

### Regression Tests (~200 TODOs)
Individual bug-fix regression tests. These should all pass if the
corresponding features are implemented. Most are in `tkt*.test` files.

## Strategy

For each category, decide:
1. **Implement** — if the test tests frigolite-relevant functionality
2. **Skip** — if the test tests C API, shell, or infrastructure behavior
3. **Set expected failure** — if the test tests behavior that's valid
   but not yet implemented

Each skipped file should have a clear message explaining WHY it's skipped:
```go
t.Skipf("C API test: %s — sqlite3 C API not exposed in frigolite", file)
```

## Verification

```bash
# All remaining tests should either pass or have explicit skip messages
go test ./testgen/... -count=1 2>&1 | grep -E "PASS|FAIL|SKIP|TODO"
# No TODOs should remain in skipped files
go build ./...
go test -run TestSOLID_ -count=1
```
