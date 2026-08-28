# Frigolite — Master Plan

> **Status**: P0 in progress.
> **Current**: 268/1011 harness file PASS (26.5%), 13,360 sub-test PASS.
> **Goal**: All 1002 test files green, tcl2go pipeline as sole test approach.
>
> **Strategy**: TCL-to-Go transpiler (`tools/tcl2go/`) converts TCL test files to
> standalone Go `_test.go` files by parsing TCL commands and emitting Go code
> directly. No TCL execution happens at generation time — all control flow
> (`foreach`, `for`, `while`, `if`) becomes native Go control flow running at
> test runtime. Tests run via `go test ./testgen/...`. Generation of all 1002+
> files completes in ~0.5s.
>
> **Reference**: SQLite C source at `/Users/muaddib/dev/sqlite/src/` is the spec.
> TCL tests at `ori/sqlite/test/*.test`.

---

## 🔨 Approach

**Work step by step.** Every task is sequential. Do not start the next task until
the current one is committed and verified. No parallel work.

**Think before acting.** When a test fails:
1. Reproduce — run the specific test, capture exact output
2. Investigate — trace expected vs actual, read the SQLite source
3. Understand — one hypothesis at a time
4. Fix — smallest change that resolves the root cause
5. Verify — original failing test must pass, no regressions

**Basic before complex.** Complex queries are useless if basic queries don't work:
1. Single-row INSERT + simple SELECT
2. WHERE filters with comparison operators
3. Expressions (arithmetic, functions, NULL handling)
4. JOINs (inner → outer → complex)
5. Aggregates and GROUP BY
6. Subqueries and CTEs
7. Advanced features (window functions, FTS, etc.)

**One failure pattern at a time.** Group by error pattern, pick the most
fundamental, fix it, re-run, measure, repeat.

---

## Pipeline

```
ori/sqlite/test/foo.test ──┐
                           │  tools/tcl2go/          testgen/foo/
                           ├─→ (TCL Transpiler ──→  foo_test.go
                           │    gen.go)               bar_test.go
ori/sqlite/test/bar.test ──┘                        util/
```
The transpiler parses TCL commands (not executes them) and emits Go source.
Generation completes in ~0.5s for all 1002+ files.

---

## Current Baseline

| Metric | Harness (deprecated) | tcl2go (target) |
|--------|----------------------|-----------------|
| File-level PASS | 268/1011 (26.5%) | ≥300 |
| Sub-test PASS | 13,360 | TBD |
| Sub-test FAIL | 44,114 | TBD |
| Generated packages | — | 1 (select1) |

**Top failure patterns** (harness, ranked):
1. Result mismatch (~4000+) — engine bugs (expression eval, affinity, comparison)
2. "no such table" (~2000+) — P0: TCL interpreter missing setup SQL
3. Unknown function (~300) — missing JSON, date/time, misc functions
4. Parse/syntax error (~300) — parser gaps (FILTER, OVER, CTE, window specs)
5. UNIQUE constraint (~200) — constraint enforcement bugs

---

## Phase 0 — TCL to Go Test Pipeline (CURRENT)

**Goal**: all 1002 TCL files generate working Go tests. File PASS ≥300 via tcl2go.

| # | Task | Links | Status |
|---|------|-------|--------|
| 0.1 | Refactor tcl2go to TCL transpiler | [details](tasks/TASK_0_1.md) | ✅ |
| 0.2 | Run tcl2go across all input files | [details](tasks/TASK_0_2.md) | ✅ |
| 0.3 | Fix generated test patterns | [details](tasks/TASK_0_3.md) | 🔲 |
| 0.4 | Phase out JSON harness | [details](tasks/TASK_0_4.md) | 🔲 |
| 0.5 | Set new baseline | [details](tasks/TASK_0_5.md) | 🔲 |

---

## Phase 1 — Fix Engine Bugs

**Goal**: fix the highest-count failures exposed by generated tests.
Start with basics (affinity, NULL, comparison) before moving to complex areas.

| # | Task | Links | Status |
|---|------|-------|--------|
| 1.1 | Type affinity, NULL handling, comparison | [details](tasks/TASK_1_1.md) | 🔲 |
| 1.2 | Fix missing setup SQL (no-such-table) | [details](tasks/TASK_1_2.md) | 🔲 |
| 1.3 | Implement missing SQL functions | [details](tasks/TASK_1_3.md) | 🔲 |
| 1.4 | Fix parse/syntax errors | [details](tasks/TASK_1_4.md) | 🔲 |
| 1.5 | Fix constraint enforcement | [details](tasks/TASK_1_5.md) | 🔲 |

---

## Phase 2 — Full Feature Coverage

**Goal**: all 1002 test files green. Zero skip-list entries.
Each task: remove area from skip list → implement/fix → verify → commit.

| # | Task | Links | Status |
|---|------|-------|--------|
| 2.1 | Window functions | [details](tasks/TASK_2_1.md) | 🔲 |
| 2.2 | ALTER TABLE | [details](tasks/TASK_2_2.md) | 🔲 |
| 2.3 | ATTACH / DETACH | [details](tasks/TASK_2_3.md) | 🔲 |
| 2.4 | FTS3/4/5 (Full-Text Search) | [details](tasks/TASK_2_4.md) | 🔲 |
| 2.5 | Virtual tables | [details](tasks/TASK_2_5.md) | 🔲 |
| 2.6 | Query planner & ANALYZE | [details](tasks/TASK_2_6.md) | 🔲 |
| 2.7 | Corruption & edge cases | [details](tasks/TASK_2_7.md) | 🔲 |

---

## Phase 3 — Quality & SOLID

**Goal**: clean architecture, low complexity, full docs, zero skips.

| # | Task | Links | Status |
|---|------|-------|--------|
| 3.1 | Complexity gate | [details](tasks/TASK_3_1.md) | 🔲 |
| 3.2 | Static analysis | [details](tasks/TASK_3_2.md) | 🔲 |
| 3.3 | SOLID compliance | [details](tasks/TASK_3_3.md) | 🔲 |
| 3.4 | Remove all skip-list entries | [details](tasks/TASK_3_4.md) | 🔲 |

---

## Key Commands

```bash
# tcl2go: generate all tests (transpiler, ~0.5s)
go run ./tools/tcl2go/

# tcl2go: run generated tests
go test ./testgen/... -count=1

# tcl2go: single package
go test ./testgen/select1/... -v

# Harness (deprecated, transitional)
FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .

# Build + quality
go build ./...
go test -run TestSOLID_ -count=1 ./...
make quality
```

## Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source (spec) | `/Users/muaddib/dev/sqlite/src/` |
| SQLite TCL tests | `ori/sqlite/test/*.test` |
| TCL tokenizer | `tools/tclconvert/tcl/parser.go` |
| tcl2go transpiler | `tools/tcl2go/` (main.go, gen.go) |
| Generated Go tests | `testgen/` |
| Task detail files | `plans/tasks/TASK_*.md` (21 files) |
| JSON harness (deprecated) | `frigolite_harness_test.go` + `testdata/*.json` |
| SOLID tests | `frigolite_solid_test.go` |
| Quality gates | `Makefile` (`make quality`) |
| sqlite3 oracle | `/usr/bin/sqlite3` |

## Protocol

**Before fixing anything:**
1. Reproduce the failure — run the exact failing test, capture output
2. Investigate — trace the expected vs actual, read the SQLite source
3. Understand the root cause — one hypothesis at a time
4. Only then fix — smallest change that resolves the root cause

**After each task:**
1. Run the verify command for that task
2. `go build ./...` — must compile
3. `go test -run TestSOLID_ -count=1 ./...` — architecture check
4. **Commit** with message: `P<phase>.<task>: <description>`
5. Update PLAN.md — mark task status, update metrics

**Before stopping (for interrupt/resume):**
1. Commit all work with a clear message
2. Update the task file's Session notes section with current state
3. Update PLAN.md task status to reflect progress
4. Push to remote
