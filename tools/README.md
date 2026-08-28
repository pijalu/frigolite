# Benchmark & Comparison Tools

This directory contains tools for benchmarking and comparing Frigolite against SQLite.

## Tools

### `benchmark_tests.py`

Categorizes test files by execution speed against SQLite (via `sqlite3` CLI).

```
python3 tools/benchmark_tests.py                  # benchmark all test files
python3 tools/benchmark_tests.py --slow-only       # show only slow tests
python3 tools/benchmark_tests.py --compare FILE    # compare specific file with Frigolite
python3 tools/benchmark_tests.py --export-csv FILE # export results as CSV
```

### `compare-benchmark/` (Go)

A standalone Go module that benchmarks test files against both Frigolite and
go-sqlite3 (CGo-based SQLite driver). Provides a detailed comparison table.

Requires CGo (`go-sqlite3` uses CGo).

```
cd tools/compare-benchmark
go build -o /tmp/bench .

# Run on first 10 test files
/tmp/bench -dir ../../testdata -limit 10

# Compare specific file
/tmp/bench -dir ../../testdata -file where4

# Export results as CSV
/tmp/bench -dir ../../testdata -limit 50 -csv results.csv
```

## How to Add New Tests to the Skip List

If a test file is too slow to run in the default test suite, add it to the
`slowTestFiles` map in `frigolite_harness_test.go`:

```go
var slowTestFiles = map[string]string{
    "joinD":      "large multi-table joins are slow without index optimization (P4)",
    "emptytable": "large table scans with many rows are slow without index optimization",
    "indexexpr1": "large table scans with many rows are slow without index optimization",
    "where4":     "slow test: complex UNIQUE constraint parsing and LEFT JOIN queries",
}
```

To run slow tests, set `FRIGOLITE_RUN_SLOW=1`:
```
FRIGOLITE_RUN_SLOW=1 go test -run TestSQLiteSuite/where4 .
```
