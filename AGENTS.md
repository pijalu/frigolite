# Frigolite — Agent Guide

## Project Overview

Frigolite is a **pure Go** reimplementation of the SQLite database engine.
It reads/writes standard SQLite `.db` files with no CGO, no cgo, no external
dependencies.

## Architecture

```
frigolite/
├── frigolite.go              # Public API: Open/Close/Exec/Query
├── frigolite_test.go          # Integration tests
├── frigolite_*_test.go        # Feature-specific tests
├── frigolite_sqlite_compat_test.go  # 778 auto-generated SQLite compat tests
│
├── internal/
│   ├── util/      # Varint, CRC32, value comparison
│   ├── storage/   # SQLite file format (pages, cells, records, header)
│   ├── pager/     # Page cache, file I/O, in-memory store
│   ├── btree/     # B+Tree with cursor (insert, delete, seek)
│   ├── sql/       # Lexer + recursive-descent parser → AST
│   ├── exec/      # Query execution engine (SELECT, INSERT, UPDATE, DELETE, DDL)
│   ├── schema/    # sqlite_schema table management
│   ├── function/  # Scalar + aggregate SQL functions (60+ functions)
│   └── vtab/      # Virtual table module system (generate_series, etc.)
│
├── cmd/frigolite/ # Interactive CLI shell (separate module)
├── benchmarks/    # Performance benchmarks
└── build/         # CLI binary output
```

## Key Conventions

- **SOLID design** — each package has one responsibility
- **No CGO** — pure Go only
- **No sqlite3 CLI** — fully standalone
- **SQLite file format** — compatible with standard `.db` files
- **Test coverage** — 842 tests (64 hand-written + 778 auto-converted from SQLite suite)

## Important Implementation Notes

### SQL Dialect
Frigolite supports a useful subset of SQLite SQL:
- DDL: `CREATE TABLE` (with IF NOT EXISTS), `CREATE INDEX`, `CREATE VIEW`, `CREATE TRIGGER`, `CREATE VIRTUAL TABLE`, `DROP TABLE/VIEW/TRIGGER/INDEX`
- DML: `INSERT` (VALUES, SELECT, columns), `UPDATE` (with WHERE, expressions), `DELETE` (with WHERE)
- Queries: `SELECT` with `WHERE`, `LIKE`, `ORDER BY`, `LIMIT`/`OFFSET`, `DISTINCT`, `UNION`, subqueries, `JOIN`, `GROUP BY` (parsed), `HAVING` (parsed)
- Expressions: arithmetic (+, -, *, /), comparison (=, <, >, <=, >=, <>, !=), logical (AND, OR, NOT), `BETWEEN`, `IN`, `LIKE`, `GLOB`, `IS NULL`, `IS NOT NULL`, `CAST`, `CASE`, `EXISTS`
- Functions: 60+ scalar and aggregate functions including `UPPER`, `LOWER`, `LENGTH`, `SUBSTR`, `TRIM`, `IFNULL`, `COALESCE`, `ABS`, `ROUND`, `TYPEOF`, `REPLACE`, `INSTR`, `HEX`, `PRINTF`, `COUNT`, `SUM`, `AVG`, `MIN`, `MAX`, `TOTAL`, `GROUP_CONCAT`, `COMPRESS`, `UNCOMPRESS`, `CRC32`
- PRAGMA: 25+ pragmas (table_info, page_size, journal_mode, etc.)
- EXPLAIN / EXPLAIN QUERY PLAN
- Virtual tables: `generate_series` via module system
- VIEW / TRIGGER (stored and expanded/fired)

### Not Supported
- C API functions (sqlite3_prepare, sqlite3_step, etc.) — pure Go, no C
- FTS3/4/5, RTree, JSON, session, RBU, zipfile extensions
- WAL mode (rollback journal only)
- Window functions, CTE (WITH)

### Test Architecture
- `frigolite_sqlite_compat_test.go` is auto-generated from SQLite TCL test files
- Each `.test` file becomes one `TestSQLite_*` Go function
- SQL statements are extracted and executed through the frigolite engine
- Results are logged (exact result matching requires manual validation)
- To regenerate: `python3 /tmp/convert_final3.py`

### Generating the Compat Test File
```bash
# The converter lives in /tmp/convert_final3.py
# It reads from ori/sqlite/test/ (reference copy of upstream SQLite tests)
# And generates frigolite_sqlite_compat_test.go
python3 /tmp/convert_final3.py
```

## Source Cleanup Guidelines

- No unused imports
- No `_ = ` patterns (use `var _ =` or remove)
- No commented-out code blocks
- All exported symbols have GoDoc comments
- Tests are self-contained (no external file deps)

## SOLID / MUST Test

A `frigolite_solid_test.go` enforces architecture rules automatically in CI:

| Principle | Test | What it checks |
|-----------|------|----------------|
| **S**ingle Responsibility | `TestSOLID_SingleResponsibility` | Each `internal/` package has a focused scope; exported symbol counts are monitored |
| **D**ependency Inversion | `TestSOLID_ImportBoundaries` | High-level packages (`exec`) only import lower-level ones; no upward or circular imports |
| **I**nterface Segregation | Manual review | Interfaces are small and focused (e.g., `io.ReaderAt`, `io.WriterAt`) |
| **O**pen/Closed + **L**iskov | Compile-time checks | `var _ Interface = (*Type)(nil)` patterns verify substitutability |

### Running the SOLID test

```bash
go test -run TestSOLID_ ./...
```

### Adding a new internal package

1. Assign a layer number in `internalLayers` map in `frigolite_solid_test.go`
2. Ensure it only imports from its own layer or lower
3. Run `go test -run TestSOLID_ImportBoundaries` to verify
4. Update `AGENTS.md` architecture diagram if needed

## Pre-Commit Hook

A pre-commit hook runs quality gates and tests before each commit.
Install it once:

```bash
git config core.hooksPath .githooks
```

This runs `make quality`, `go test`, and SOLID architecture checks
automatically. Commits that break quality or tests are rejected early.

Coverage is checked in CI only (not in the hook) because it's slow
and depends on the full package set.
