# Frigolite — Master Plan

> **Status**: P0 in progress.
> **Current**: 268/1011 harness file PASS (26.5%), 13,360 sub-test PASS.
> **Goal**: All 1002 test files green, tcl2go pipeline as sole test approach.
>
> **Strategy**: A Go-based TCL interpreter (`tools/tclconvert/tcl/`) executes TCL
> test files and captures SQL. The `tcl2go` generator (`tools/tcl2go/`) produces
> standalone Go `_test.go` files. Tests run via `go test ./testgen/...`.
>
> The old JSON harness (`frigolite_harness_test.go` + `testdata/*.json`) and Python
> converter (`tools/convert_compat_json.py`) are deprecated.

---

## Quick Start

```bash
go run ./tools/tcl2go/                # Generate test files from TCL source
go test ./testgen/... -count=1        # Run generated tests
go test ./testgen/select1/... -v      # Run a single package
```

---

## Pipeline

```
ori/sqlite/test/foo.test ──┐
                           │  tools/tcl2go/     testgen/foo/
                           ├─→ (TCL Interp  ──→ foo_test.go
                           │   + gen.go)        bar_test.go
ori/sqlite/test/bar.test ──┘                   util/
                                                  util_test.go
```

- TCL interpreter (`tools/tclconvert/tcl/`) handles `db eval`, `db onecolumn`,
  loops, variables, expressions — captures ALL setup SQL
- `tcl2go` generates one `_test.go` per TCL file, grouped by package prefix
- Generated tests run independently, in parallel, with Go compiler validation

---

## Current Baseline

| Metric | Harness (deprecated) | tcl2go (target) |
|--------|---------------------|------------------|
| File-level PASS | 268/1011 (26.5%) | TBD after full generate |
| Sub-test PASS | 13,360 | TBD |
| Sub-test FAIL | 44,114 | TBD |
| Generated packages | — | 1 (select1) |

**Top failure patterns** (harness, ranked by count):
1. Result mismatch (~4000+) — engine bugs
2. "no such table" (~2000+) — P0 fix in progress (missing setup SQL)
3. Unknown function (~300) — missing SQL functions
4. Parse/syntax error (~300) — parser gaps
5. UNIQUE constraint (~200) — constraint handling

---

## Phase 0 — TCL to Go Test Pipeline (CURRENT)

**Goal**: All 1002 TCL files generate working Go tests. File-level PASS ≥300 via
tcl2go. JSON harness deprecated.

**Completion criterion**: At least 500/1002 test files have CREATE TABLE
statements that were previously missing. File-level PASS ≥300/869.

**Files**: `tools/tclconvert/tcl/` (interpreter), `tools/tcl2go/` (generator),
`testgen/` (output), `frigolite_harness_test.go` (to deprecate).

### Status

| Component | File | Lines | Status |
|-----------|------|-------|--------|
| TCL tokenizer | `tools/tclconvert/tcl/parser.go` | ~100 | ✅ Done |
| TCL interpreter | `tools/tclconvert/tcl/interp.go` | ~800 | ✅ Done (uncommitted fixes) |
| Expression evaluator | `tools/tclconvert/tcl/expr.go` | ~400 | ✅ Done |
| List helpers | `tools/tclconvert/tcl/list.go` | ~80 | ✅ Done |
| tcl2go entry point | `tools/tcl2go/main.go` | ~160 | ✅ Done |
| tcl2go generator | `tools/tcl2go/gen.go` | ~320 | ✅ Done |
| Python converter | `tools/convert_compat_json.py` | — | ❌ Deleted |
| JSON harness | `frigolite_harness_test.go` | ~630 | 🔶 To deprecate |

### Steps

#### P0.6 — Fix uncommitted TCL interpreter changes and commit
- [ ] Review and commit the modified `tools/tclconvert/main.go` and `tools/tclconvert/tcl/interp.go`
- [ ] **Verify**: `go build ./tools/tcl2go/...`
- [ ] **Commit**.

#### P0.7 — Generate all 1002 test files via tcl2go
- [ ] Run `go run ./tools/tcl2go/`
- [ ] Verify at least 500 files in `testgen/`
- [ ] **Verify**: `ls testgen/*/*_test.go | wc -l` — expect ≥500
- [ ] **Commit**.

#### P0.8 — Fix generator failures and result mismatches
- [ ] Run `go test ./testgen/... -count=1` and capture failures
- [ ] Categorize failure patterns
- [ ] Fix `gen.go` for common patterns
- [ ] Fix TCL interpreter issues causing missing SQL
- [ ] Iterate until generated tests produce meaningful results
- [ ] **Commit**.

#### P0.9 — Phase out JSON harness
- [ ] Once tcl2go covers all 1002 files, deprecate `frigolite_harness_test.go`
- [ ] Archive `testdata/*.json`
- [ ] **Verify**: `go test ./testgen/... -count=1` runs all files
- [ ] **Commit**.

#### P0.10 — Set new baseline
- [ ] Record PASS count from `go test ./testgen/...`
- [ ] Update this document
- [ ] **Commit**.

---

## Phase 1 — Test Fixing

Once all tests run through tcl2go, fix the engine bugs exposed by generated tests.
Priority by impact:

| Priority | Pattern | Approach |
|----------|---------|----------|
| P1.1 | Result mismatch (~4000+) | Fix expression eval, type affinity, comparison |
| P1.2 | "no such table" (remaining) | Ensure TCL interpreter captures all setup SQL |
| P1.3 | Unknown function (~300) | Implement missing SQL functions |
| P1.4 | Syntax errors (~300) | Fix parser gaps |
| P1.5 | Constraint errors (~200) | Fix UNIQUE, NOT NULL, CHECK, FK |

Each sub-phase follows: **measure → fix → verify → commit**.

---

## Phase 2 — Full Coverage

All 1002 test files green. Zero skips.

| Area | Test files | Notes |
|------|-----------|-------|
| Window functions | ~17 files | ROW_NUMBER, RANK, frame clauses |
| ALTER TABLE | ~3 files | RENAME, ADD/DROP COLUMN |
| ATTACH/DETACH | ~10 files | Multi-database dispatch |
| FTS3/4/5 | ~76 files | Full-text search |
| Virtual tables | ~10 files | xBestIndex, WITHOUT ROWID |
| Query planner | ~10 files | ANALYZE, cost-based index |
| Corruption/edge | ~90 files | corrupt*, tkt*, bigfile* |

---

## Phase 3 — Quality & SOLID

- Complexity gate: gocognit <15 per function
- Full SOLID architecture compliance
- No unused imports, no commented code
- All exported symbols documented

---

## Key Commands

```bash
# tcl2go: generate all tests
go run ./tools/tcl2go/

# tcl2go: run generated tests
go test ./testgen/... -count=1

# tcl2go: run specific package
go test ./testgen/select1/... -v

# Harness (deprecated, transitional)
FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .

# Build
go build ./...

# SOLID architecture check
go test -run TestSOLID_ -count=1 ./...

# Quality gate
make quality
```

---

## Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source (spec) | `/Users/muaddib/dev/sqlite/src/` |
| SQLite TCL tests | `ori/sqlite/test/*.test` |
| TCL interpreter (Go) | `tools/tclconvert/tcl/` (parser, interp, expr, list) |
| tcl2go generator | `tools/tcl2go/` (main.go, gen.go) |
| Generated Go tests | `testgen/` |
| JSON harness (deprecated) | `frigolite_harness_test.go` + `testdata/*.json` |
| SOLID tests | `frigolite_solid_test.go` |
| Quality gates | `Makefile` (`make quality`) |
| sqlite3 oracle | `/usr/bin/sqlite3` |

---

## Per-Step Protocol

After each numbered step:
1. Run the verify command for that step
2. Run `go build ./...` — must compile
3. Run SOLID check: `go test -run TestSOLID_ -count=1 ./...`
4. **Commit** with message: `P<phase>.<step>: <description>`
5. Update this plan — mark step done, update metrics, note findings
