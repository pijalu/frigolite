# Generated testgen baseline

Command: `go test -tags testgen ./testgen/... -count=1`

Current baseline: command exceeds the 300-second verification window before completion.

Categorized baseline evidence (latest completed status report):

- Total: 1,192 packages/files; 616 pass, 185 fail, 391 skipped (51.7% pass).
- Passing coverage: RTREE, SESSION, and selected CRUD/CTE/FTS/ORDER/SCHEMA/VTAB/WAL cases.
- Known failures: remaining aggregate, C-API, CRUD, CTE/WINDOW, expression, FTS, function, join, order, schema, VTAB, and WAL compatibility gaps.
- Expected skips: JSON and unsupported C-API/concurrency/WAL cases; WAL/journal-mode cases are skipped because WAL mode is not implemented.
- Generated-suite build/runtime failures are baseline compatibility gaps, not production gate failures.

Root-cause mapping:

- Aggregate, expression, function, join, order, and CTE/WINDOW failures: unsupported or incomplete SQL execution semantics in those feature areas.
- C-API, concurrency, and WAL/journal failures: SQLite C callback, shared-concurrency, and WAL subsystems are outside the pure-Go production scope or not implemented.
- CRUD, schema, and VTAB failures: compatibility differences in DDL/DML edge cases and virtual-table behavior.
- FTS failures: remaining tokenizer/index/query compatibility differences.
- JSON failures: JSON extension is not implemented and therefore skipped.

Production gates: `staticcheck ./...`, `go test ./...`, `go vet ./...`, `go test -run 'TestSOLID_' ./...`, and `go test -race ./...` pass.
