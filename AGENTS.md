# Frigolite — Agent Guide

## Project Overview

Frigolite is a **pure Go** reimplementation of the SQLite database engine.
It reads/writes standard SQLite `.db` files with no CGO, no cgo, no external
dependencies.

## Planning (authoritative)

- **`PORTPLAN.md`** (repo root) — the authoritative implementation plan:
  phases G0…G8, task index, definition of done, commit cadence.
- **`portplan/DESIGN.md`** — the engineering design/analysis behind the plan:
  per-area gap analysis, data structures, algorithms, SQLite source pointers.
- **`portplan/tasks/TASK_G*.md`** — per-phase task files (each contains small Goa
  goals with their own `verifyCommand` and todo lists).
- **`plan/GUIDELINES.md`** — agent guidelines (problem-resolution protocol, triage, oracle use,
  commit cadence, SOLID rules).
- **`tools/status`** — progress-reporting tool (per-feature-family pass/fail/
  skipped); consult before/after every goal.
- **`archive/`** — superseded/historical plans (not the source of truth).

## Working Guidelines

> **NO TRY/FAIL LOOP (mandatory)**: Do not fall into a try/fail loop - when facing issue: Take a step back - consider sqlite approach by checking it's source (@../sqlite/) - write a dedicated fix plan then execute.

> **NO SIMPLIFY (mandatory)**: Do not simplify an issue - the fix must be complete and have the same fundamental approach as sqlite.

> **LESSONS LEARNED (mandatory)**: All key discoveries, validated knowledge, and
> successful approaches must be recorded in `.agents/lessons_learned.md` during
> every fixing/debugging session. Review and summarize that file at the start of
> each session (it is the low-context knowledge base); remove stale/obsolete
> points and consolidate content instead of letting it grow unbounded.

## Architecture

```
frigolite/
├── frigolite.go              # Public API: Open/Close/Exec/Query
├── frigolite_test.go          # Integration tests
├── frigolite_*_test.go        # Feature-specific tests
├── frigolite_harness_test.go  # JSON-driven SQLite compat test harness (1002 files)
│
├── internal/
│   ├── auth/      # SQL authorizer framework
│   ├── util/      # Varint, CRC32, value comparison
│   ├── storage/   # SQLite file format (pages, cells, records, header)
│   ├── pager/     # Page cache, file I/O, in-memory store
│   ├── btree/     # B+Tree with cursor (insert, delete, seek)
│   ├── sql/       # Lexer + AST node types
│   ├── parse/     # LALR(1) parser (go-lemon, from SQLite parse.y)
│   ├── exec/            # Execution engine coordinator (connection state; delegates to sub-executors)
│   ├── execddl/         # DDL execution (CREATE/DROP/ALTER/ATTACH + dependency analysis)
│   ├── execdml/         # DML execution (INSERT/UPDATE/DELETE + OR/RETURNING/rowid/triggers)
│   ├── execquery/       # SELECT execution (join, aggregate, validate, scan, planner, core)
│   ├── execconstraint/  # FOREIGN KEY constraint enforcement (ON actions, deferred FK checks)
│   ├── exectrigger/     # Trigger execution state (depth, NEW/OLD rows, trigger caches)
│   ├── schema/    # sqlite_schema table management
│   ├── function/  # Scalar + aggregate SQL functions (60+ functions)
│   ├── fts/       # Full-text search tokenizer and ranking
│   ├── rename/    # ALTER TABLE RENAME dependency management
│   ├── value/     # SQL value comparison and type system
│   └── vtab/      # Virtual table module system (generate_series, etc.)
│
├── cmd/frigolite/ # Interactive CLI shell (separate module)
├── benchmarks/    # Performance benchmarks
└── build/         # CLI binary output
```

## Key Conventions

- **Plan before change (MANDATORY)** — a detailed plan must be done for *all*
  changes before any edit: what, why, exact files/functions, ordered steps,
  verification. See `plan/GUIDELINES.md §1b`. Plan first, then edit.
- **SOLID design** — each package has one responsibility
- **No CGO** — pure Go only
- **No sqlite3 CLI** — fully standalone
- **SQLite file format** — compatible with standard `.db` files
- **Test coverage** — 1,002 JSON-driven harness tests (converted from SQLite TCL suite) + hand-written unit tests

## Important Implementation Notes

### SQL Dialect
Frigolite supports a useful subset of SQLite SQL:
- DDL: `CREATE TABLE` (with IF NOT EXISTS), `CREATE INDEX`, `CREATE VIEW`, `CREATE TRIGGER`, `CREATE VIRTUAL TABLE`, `DROP TABLE/VIEW/TRIGGER/INDEX`
- DML: `INSERT` (VALUES, SELECT, columns), `UPDATE` (with WHERE, expressions), `DELETE` (with WHERE)
- Queries: `SELECT` with `WHERE`, `LIKE`, `ORDER BY`, `LIMIT`/`OFFSET`, `DISTINCT`, `UNION`, subqueries, `JOIN`, `GROUP BY`, `HAVING`
- Expressions: arithmetic (+, -, *, /), comparison (=, <, >, <=, >=, <>, !=), logical (AND, OR, NOT), `BETWEEN`, `IN`, `LIKE`, `GLOB`, `IS NULL`, `IS NOT NULL`, `CAST`, `CASE`, `EXISTS`
- Functions: 60+ scalar and aggregate functions including `UPPER`, `LOWER`, `LENGTH`, `SUBSTR`, `TRIM`, `IFNULL`, `COALESCE`, `ABS`, `ROUND`, `TYPEOF`, `REPLACE`, `INSTR`, `HEX`, `PRINTF`, `COUNT`, `SUM`, `AVG`, `MIN`, `MAX`, `TOTAL`, `GROUP_CONCAT`, `COMPRESS`, `UNCOMPRESS`, `CRC32`
- PRAGMA: 25+ pragmas (table_info, page_size, journal_mode, etc.)
- EXPLAIN / EXPLAIN QUERY PLAN
- Virtual tables: `generate_series` via module system
- VIEW / TRIGGER (stored and expanded/fired)

### Implemented Extensions
- **FTS3/4** — `internal/fts/` (tokenizers simple/unicode61, inverted index, MATCH, FTS3/4 modules). fts5 is NOT implemented (`NoopModule`, zero testgen packages) — queued goal `P6.FTS5` (see PORTPLAN §4)
- **Virtual tables** — `internal/vtab/` module system (`generate_series`, `fts3`/`fts4`; `fts5`/`dbstat`/`dbdata` are `NoopModule` stubs — queued goals `P6.FTS5`/`P6.DBSTAT`/`P6.DBDATA`)
- **EXPLAIN / EXPLAIN QUERY PLAN** — `internal/execquery/explain.go`

### Not Yet Implemented (planned — see PORTPLAN phases)
- WAL mode / shared-memory / concurrency (G7)
- JSON, RTree, session/RBU (G6/G7)
- Window functions (parsed; execution in G4)
- CTE `WITH` (parsed; execution in G4)
- C API functions (sqlite3_prepare, sqlite3_step, etc.) — N/A (pure Go, no C)
- zipfile extension (G6)

### Test Architecture
- `frigolite_harness_test.go` is the SQLite compatibility harness
- It reads JSON test cases from `testdata/*.json` (1,002 files, converted from SQLite TCL tests)
- Each JSON file contains named test cases with SQL steps and expected results
- Tests are run via `FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$"`
- Slow/unsupported tests are listed in `slowTestFiles`/`unsupportedTestFiles` maps in the harness

### Generating Tests via tcl2go (TCL Transpiler)
```bash
# The tcl2go tool converts SQLite TCL test files into standalone Go test files.
# It is a TRANSPILER — it parses TCL commands and generates Go code directly.
# No TCL execution happens at generation time. All control flow (foreach, for,
# while, if) becomes Go control flow running at test runtime.
go run ./tools/tcl2go/            # generate all test files in testgen/ (~0.5s)
# Generated tests are opt-in via the testgen build tag, so 'go test ./...'
# (and the SOLID verify command) builds only hand-written, non-generated code.
go test -tags testgen ./testgen/... -count=1    # run all generated tests
```

### Diagnosing a failing generated test

A failing testgen assertion may be an engine bug or a transpiler bug. Determine
which before editing anything:

1. **Write a pure Go test** that drives the engine directly (via `frigolite.Open`
   / `Exec` / `Query`) and exercises the actual feature the failing assertion
   covers — not the transpiled wrapper. Run it.
2. If the pure Go test **fails**, the engine is wrong: fix the engine (then
   re-run both the pure Go test and the full verify command).
3. Only if the pure Go test **passes** while the transpiled testgen test fails
   is the transpiler (`tools/tcl2go/`) the suspect — investigate it then.
4. SQLite itself is ground truth. To validate expected behavior/error messages,
   use the `sqlite3` CLI or a throwaway `go-sqlite3` scratch program (e.g.
   under `/tmp`, never as a project dependency) and mirror its output.

This rule keeps transpiler hunts from hiding real engine bugs: most failing
assertions in this project are engine gaps, not transpiler defects.

### Pure-Go supersession (policy, 2026-05)

TCL-transpiler fidelity work has a poor time-to-value ratio. From now on:

1. When a testgen package fails and a **pure-Go port of its essential
   assertions** (driving `frigolite.Open`/`Exec`/`Query` directly, including
   any fixture UDFs such as swarmvtab's `missing=`/`openclose=`) **passes**
   against the engine, the TCL testgen package is **superseded**: list it in
   `unsupportedTestFiles`/`skipTestFiles` with a pointer to the native test
   (`frigolite_<name>_test.go`) and move on. Do NOT keep iterating on tcl2go
   for that package.
2. The native port must cover the same *engine-visible contract* (error
   texts, row sets, module semantics) — validated against the `sqlite3`
   oracle where practical — not necessarily every TCL scaffolding detail
   (e.g. `sqlite_open_file_count`, `::dbcache` TCL-variable mirrors, which
   observe the harness, not the engine).
3. Only invest in tcl2go changes when a *class* of packages is blocked by
   one transpiler gap, or when no native port can express the behavior
   (rare — e.g. per-row `db eval` callbacks are already supported).

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

This runs the strict quality gate on staged non-test Go files
(`tools/quality_gate.sh`: gocognit ≤15, gocyclo ≤12, staticcheck,
file-size hard 1000 / soft 500), `go test`, and SOLID architecture checks
automatically. Commits that break quality or tests are rejected early.

Coverage is checked in CI only (not in the hook) because it's slow
and depends on the full package set.
