# Frigolite

**Pure Go SQL database engine — SQLite-compatible file format**

Frigolite is a from-scratch reimplementation of the SQLite database engine
in pure Go. It reads and writes standard `.db` files and supports a useful
subset of SQL. Zero external dependencies, zero CGO, zero sqlite3 CLI needed.

## Features

- **Pure Go** — no CGO, no external dependencies, no sqlite3 CLI
- **SQLite file format** — creates and reads standard `.db` files
- **Full SQL subset**: `CREATE TABLE/INDEX/VIEW/TRIGGER`, `INSERT`, `SELECT`
  (with `WHERE`, `LIKE`, `ORDER BY`, `LIMIT`/`OFFSET`, `DISTINCT`, `UNION`,
  subqueries, `JOIN`), `UPDATE`, `DELETE`, `CASE`, `CAST`, `EXISTS`,
  `BETWEEN`, `IN`, `GLOB`
- **60+ SQL functions**: `UPPER`, `LOWER`, `LENGTH`, `SUBSTR`, `TRIM`, `IFNULL`,
  `COALESCE`, `ABS`, `ROUND`, `TYPEOF`, `REPLACE`, `INSTR`, `HEX`, `PRINTF`,
  `COUNT`, `SUM`, `AVG`, `MIN`, `MAX`, `TOTAL`, `GROUP_CONCAT`,
  `COMPRESS`, `UNCOMPRESS`, `CRC32`
- **Virtual tables**: `generate_series` via module system
- **25+ PRAGMAs**: `table_info`, `page_size`, `journal_mode`, etc.
- **EXPLAIN / EXPLAIN QUERY PLAN**
- **In-memory and file-based** databases (`:memory:` or file path)
- **B-tree storage** with cursor-based access
- **842 tests** (64 hand-written + 778 auto-converted from SQLite suite)
- **Command-line shell** included

## Quick Start

### Native API

```go
package main

import (
    "fmt"
    "log"

    "github.com/pijalu/frigolite"
)

func main() {
    db, err := frigolite.Open(":memory:")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    // Create a table
    db.Exec("CREATE TABLE users (id INTEGER, name TEXT, age INTEGER)")

    // Insert data
    db.Exec("INSERT INTO users VALUES (1, 'Alice', 30)")
    db.Exec("INSERT INTO users VALUES (2, 'Bob', 25)")

    // Query
    res := db.Query("SELECT * FROM users WHERE age > 20")
    if res.Error != nil {
        log.Fatal(res.Error)
    }

    for _, row := range res.Rows {
        fmt.Printf("id=%v name=%v age=%v\n", row[0], row[1], row[2])
    }
}
```

### database/sql Driver

Frigolite implements the standard `database/sql` interface, letting you use
typed scans, placeholders, prepared statements, and connection pooling:

```go
package main

import (
    "database/sql"
    "fmt"
    "log"

    _ "github.com/pijalu/frigolite/frigodb"
)

func main() {
    db, err := sql.Open("frigolite", ":memory:")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    // Create a table
    db.Exec("CREATE TABLE users (id INTEGER, name TEXT, age INTEGER)")

    // Insert with placeholders
    db.Exec("INSERT INTO users VALUES (?, ?, ?)", 1, "Alice", 30)

    // Query and scan into typed variables
    var id int
    var name string
    var age int
    row := db.QueryRow("SELECT * FROM users WHERE id = ?", 1)
    if err := row.Scan(&id, &name, &age); err != nil {
        log.Fatal(err)
    }
    fmt.Printf("id=%d name=%s age=%d\n", id, name, age)
}
```

See `_examples/native/` and `_examples/driver/` for complete examples.

## CLI Shell

```bash
# Build the CLI
make build-cli

# Run
./build/frigolite mydb.db
```

Or directly:

```bash
go run github.com/pijalu/frigolite/cmd/frigolite mydb.db
```

## Tests

```bash
# Run all tests
make test

# Run with coverage
make test-cover

# Run benchmarks
make bench
```

**842 test functions** — all passing, zero external dependencies.

## Architecture

```
frigolite/
├── frigolite.go              # Public API: Open/Close/Exec/Query
├── frigolite_test.go          # Integration tests
├── frigolite_solid_test.go    # SOLID architecture verification tests
├── frigolite_*_test.go        # Feature-specific tests (50+ tests)
├── frigolite_sqlite_compat_test.go  # 778 auto-generated SQLite compat tests
│
├── internal/
│   ├── util/      # Varint, CRC32, value comparison
│   ├── storage/   # SQLite file format (pages, cells, records, header)
│   ├── pager/     # Page cache, file I/O, in-memory store
│   ├── btree/     # B+Tree with cursor (insert, delete, seek)
│   ├── sql/       # Lexer + recursive-descent parser → AST
│   ├── exec/      # Query execution engine
│   ├── schema/    # sqlite_schema table management
│   ├── function/  # Scalar + aggregate SQL functions (60+ functions)
│   └── vtab/      # Virtual table module system (generate_series, etc.)
│
├── frigodb/                  # database/sql driver wrapper
├── _examples/
│   ├── native/               # Native API example
│   └── driver/               # database/sql driver example
├── cmd/frigolite/            # Interactive CLI shell (separate module)
├── benchmarks/               # Performance benchmarks
└── build/                    # CLI binary output
```

## SQL Support

### Statements
| Statement | Status |
|-----------|--------|
| `CREATE TABLE` | ✅ Full (columns, types, IF NOT EXISTS) |
| `CREATE INDEX` | ✅ |
| `CREATE VIEW` | ✅ (stored, expanded on SELECT) |
| `CREATE TRIGGER` | ✅ (stored, fired on INSERT/UPDATE/DELETE) |
| `CREATE VIRTUAL TABLE` | ✅ (module system with generate_series) |
| `DROP TABLE / VIEW / TRIGGER / INDEX` | ✅ |
| `ALTER TABLE` | ✅ |
| `SELECT` | ✅ Full (WHERE, JOIN, subqueries, UNION, ORDER BY, LIMIT) |
| `INSERT` | ✅ (VALUES, SELECT, explicit columns) |
| `UPDATE` | ✅ (with WHERE, expressions) |
| `DELETE` | ✅ (with WHERE) |
| `BEGIN / COMMIT / ROLLBACK` | ✅ |
| `PRAGMA` | ✅ 25+ pragmas |
| `EXPLAIN / EXPLAIN QUERY PLAN` | ✅ |

### Expressions
| Expression | Status |
|------------|--------|
| Arithmetic (+, -, *, /) | ✅ |
| Comparison (=, <, >, <=, >=, <>, !=) | ✅ |
| Logical (AND, OR, NOT) | ✅ |
| `BETWEEN` | ✅ |
| `IN` / `NOT IN` | ✅ |
| `LIKE` / `GLOB` | ✅ |
| `IS NULL` / `IS NOT NULL` | ✅ |
| `CAST` | ✅ |
| `CASE` (WHEN, expr) | ✅ |
| `EXISTS` / `NOT EXISTS` | ✅ |
| Subqueries (scalar, IN) | ✅ |

### Functions
| Category | Functions |
|----------|-----------|
| **Aggregate** | COUNT, SUM, AVG, MIN, MAX, TOTAL, GROUP_CONCAT |
| **String** | UPPER, LOWER, LENGTH, SUBSTR, TRIM, LTRIM, RTRIM, REPLACE, INSTR, HEX, QUOTE, UNICODE, CHAR, PRINTF |
| **Numeric** | ABS, ROUND, RANDOM |
| **Conditional** | IFNULL, COALESCE, NULLIF |
| **Type** | TYPEOF |
| **Pattern** | GLOB, LIKE |
| **Compression** | COMPRESS, UNCOMPRESS, CRC32 |

## License

GNU General Public License v3.0 — see [LICENSE](LICENSE).

## Design Principles

- **Single Responsibility**: Each package has one concern
- **Interface Segregation**: Small, focused interfaces
- **Dependency Inversion**: High-level packages depend on abstractions
- **Go Idioms**: `io.ReaderAt`/`io.WriterAt`, `sync.RWMutex`, error wrapping
- **Test Coverage**: 842 tests, all green
