# Frigolite — Pure Go SQLite Implementation

## Vision

A pure Go reimplementation of the SQLite database engine at
`github.com/pijalu/frigolite`.  Compatible with the SQLite file format
but with a clean, idiomatic Go design following SOLID principles.

## References

- SQLite file format: https://sqlite.org/fileformat2.html
- SQLite architecture: https://sqlite.org/arch.html
- VDBE opcodes: https://sqlite.org/opcode.html
- Source: `ori/sqlite/`

---

## Package Map (SOLID decomposition)

```
frigolite/
├── go.mod                          # module github.com/pijalu/frigolite
├── frigolite.go                    # Public API: Open(), Close(), Exec(), Query()
├── frigolite_test.go               # Integration tests
│
├── internal/
│   ├── pager/
│   │   ├── pager.go                # Pager: page read/write, cache, locking
│   │   ├── pager_test.go
│   │   └── wal.go                  # Write-Ahead Log (future)
│   │
│   ├── storage/                    # SQLite file format
│   │   ├── header.go               # Database header (page 1)
│   │   ├── cell.go                 # Cell format (table + index)
│   │   ├── cell_test.go
│   │   ├── varint.go               # Variable-length integer encoding
│   │   ├── varint_test.go
│   │   ├── record.go               # Record format (serial type + values)
│   │   ├── record_test.go
│   │   └── page.go                 # Page types (interior/leaf table/index)
│   │
│   ├── btree/
│   │   ├── btree.go                # B-tree: cursor, search, insert, delete
│   │   ├── btree_test.go
│   │   └── cursor.go              # B-tree cursor
│   │
│   ├── sql/
│   │   ├── lexer.go               # SQL tokenizer
│   │   ├── lexer_test.go
│   │   ├── parser.go              # Recursive-descent SQL parser
│   │   ├── parser_test.go
│   │   ├── ast.go                 # AST node types
│   │   └── keywords.go            # Keyword table
│   │
│   ├── exec/
│   │   ├── engine.go              # Query execution engine
│   │   ├── engine_test.go
│   │   ├── select.go              # SELECT execution
│   │   ├── insert.go              # INSERT execution
│   │   ├── update.go              # UPDATE execution
│   │   ├── delete.go              # DELETE execution
│   │   ├── create.go              # CREATE TABLE/INDEX execution
│   │   ├── expr.go                # Expression evaluator
│   │   ├── expr_test.go
│   │   └── filter.go              # Result set filtering
│   │
│   ├── schema/
│   │   ├── schema.go              # sqlite_schema table management
│   │   ├── schema_test.go
│   │   └── types.go               # Column type system
│   │
│   ├── function/
│   │   ├── registry.go            # Function registry
│   │   ├── aggregate.go           # Aggregate functions (COUNT, SUM, AVG, MIN, MAX)
│   │   ├── scalar.go              # Scalar functions (UPPER, LOWER, LENGTH, etc.)
│   │   ├── math.go                # Math functions (ABS, ROUND, etc.)
│   │   └── date.go                # Date/time functions
│   │
│   ├── transaction/
│   │   ├── transaction.go         # Transaction manager (BEGIN, COMMIT, ROLLBACK)
│   │   └── journal.go             # Rollback journal
│   │
│   └── util/
│       ├── crc.go                 # CRC32 calculation
│       ├── crc_test.go
│       └── compare.go             # Collation / value comparison
│
├── cmd/
│   ├── frigolite/
│   │   └── main.go                # Interactive CLI shell
│   └── demo/
│       ├── basic/main.go          # Basic CRUD demo
│       ├── bulk/main.go           # Bulk insert performance demo
│       └── query/main.go          # Query demonstration
│
├── _examples/
│   ├── hello.go                   # Hello world example
│   └── migration.go               # Schema migration example
│
└── benchmarks/
    ├── bench_test.go              # Go benchmarks
    └── results/                   # Benchmark results directory
```

## Phased Micro-Step Plan

### Phase 0: Project foundation
1. Initialize Go module (`go.mod`)
2. Create directory structure
3. Implement utility functions (varint, CRC)
4. Write tests for utilities

### Phase 1: File Format Layer (storage package)
5. Implement database header parsing
6. Implement page type definitions
7. Implement cell encoding/decoding (table leaf, table interior, index leaf, index interior)
8. Implement record format (serial type codes, value encoding)
9. Write comprehensive tests for all storage types

### Phase 2: Pager
10. Implement pager with page cache
11. Implement page read (from file or memory)
12. Implement page write with dirty tracking
13. Implement locking model (shared, reserved, exclusive)
14. Write pager tests

### Phase 3: B-Tree
15. Implement B-tree cursor (seek, next, prev)
16. Implement key search (binary search within page)
17. Implement cell insertion with page splitting
18. Implement cell deletion with balancing
19. Write B-tree tests

### Phase 4: Schema Layer
20. Implement schema table management
21. Implement column type system (TEXT, INTEGER, REAL, BLOB, NULL)
22. Implement table/index descriptor
23. Write schema tests

### Phase 5: SQL Frontend
24. Implement SQL lexer (tokenizer)
25. Implement SQL parser (recursive descent)
26. Define AST types for supported SQL statements
27. Parse: CREATE TABLE, CREATE INDEX, SELECT, INSERT, UPDATE, DELETE, BEGIN, COMMIT, ROLLBACK
28. Write lexer and parser tests

### Phase 6: Expression Evaluator
29. Implement expression tree evaluation
30. Implement comparison operators (=, <, >, <=, >=, !=, IN, BETWEEN, LIKE, IS NULL)
31. Implement logical operators (AND, OR, NOT)
32. Implement type casting and affinity
33. Write expression tests

### Phase 7: Query Execution
34. Implement SELECT execution (scan, filter, project)
35. Implement INSERT execution
36. Implement UPDATE execution
37. Implement DELETE execution
38. Implement CREATE TABLE execution
39. Implement CREATE INDEX execution
40. Implement transaction support
41. Write execution tests

### Phase 8: SQL Functions
42. Implement function registry
43. Implement aggregate functions (COUNT, SUM, AVG, MIN, MAX)
44. Implement scalar functions (UPPER, LOWER, LENGTH, TRIM, SUBSTR)
45. Implement math functions (ABS, ROUND, RANDOM)
46. Write function tests

### Phase 9: Public API
47. Implement `Open()` / `Close()` / `Exec()` / `Query()` API
48. Implement `Prepare()` / `Step()` / `Finalize()` statement API
49. Implement `Column*()` accessors
50. Write integration tests

### Phase 10: CLI and Demos
51. Implement interactive CLI shell
52. Write basic CRUD demo
53. Write bulk insert demo
54. Write query demo

### Phase 11: Performance & Coverage
55. Write Go benchmarks (insert, select, bulk)
56. Run benchmarks and capture baseline
57. Profile and optimize top bottlenecks
58. Achieve ≥80% test coverage
59. Document performance characteristics

### Phase 12: Polish
60. Add comprehensive error handling
61. Add GoDoc comments to all exported symbols
62. Create README.md with usage examples
63. Verify SQLite file compatibility

## Design Decisions

### SOLID Application
- **S**: Each package has exactly one responsibility
- **O**: Interfaces for pager, storage, functions allow extension
- **L**: All interface implementations are substitutable
- **I**: Small interfaces (Reader, Writer, Seeker, Scanner)
- **D**: High-level packages depend on interfaces, not concrete types

### Go Idioms
- Use `io.ReaderAt`/`io.WriterAt` for file I/O
- Use `context.Context` for cancellable operations
- Use `sync.RWMutex` for concurrency safety
- Use error wrapping with `%w` for error chains
- Use table-driven tests
- Use `testing/synctest` for concurrent tests
- Use `*os.File` for file storage with `mmap` option
- Provide in-memory storage option (`:memory:`)

### SQLite Compatibility
- Read/write SQLite format database files
- Support same serial type codes for records
- Support same page structure (interior/leaf table/index)
- Same varint encoding
- Same database header format
- Implementation can differ internally (no VDBE bytecode required)
