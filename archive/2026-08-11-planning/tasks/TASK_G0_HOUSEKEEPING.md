# TASK G0 — Housekeeping, Status Tooling, and Foundation

> **Phase**: G0 (FOUNDATION — must complete first)
> **Goal IDs**: G0.HOUSEKEEPING, G0.STATUS, G0.UNSKIP-SLOW, G0.FIX-4-FAILS
> **Read first**: `PORTPLAN.md` §0 (principles), **`portplan/DESIGN.md` §L (status
> tool design)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started

---

## Objective

Restore a correct baseline before any feature work: archive the outdated planning
files, build the progress-reporting tool, un-skip the 140 packages that were
skipped merely for being "slow", and fix the 4 known engine-bug FAIL packages.
When G0 is done, the testgen tree runs green for all *currently-active* packages
plus the 140 un-skipped slow packages, and `tools/status` can report the state.

This task is split into **four independent Goa goals** (run sequentially; they
touch disjoint concerns).

---

## Goal G0.HOUSEKEEPING — Archive outdated files (mostly DONE at plan time)

**Objective**: Move all superseded planning artifacts into `archive/` so the
plan surface is unambiguous. Keep history; lose the confusion.

> **Already done when this plan was authored** (the deliverable cleaned the tree):
> `PORTPLAN.md` → `archive/PORTPLAN_legacy.md`; `HANDOVER.md` →
> `archive/HANDOVER_legacy.md`; all of `plans/*` → `archive/plans/` (incl. the
> pre-existing `archive`/`archive-old` subdirs and `subplans/`/`tasks/`/`tdd/`);
> the legacy `portplan/TASK_G*.md` + `G6_TRIAGE_STATUS.md` →
> `archive/portplan-legacy/`. `portplan/` now holds only `GUIDELINES.md` +
> `tasks/`. The new `PORTPLAN.md` is the authoritative plan.

**Remaining scope**:
- Update `AGENTS.md` to point to `PORTPLAN.md` as the authoritative plan and note
  the archive layout (add an "Archived plans" pointer to `archive/`).
- Keep `portplan/GUIDELINES.md` (already updated to the "implement, don't defer"
  + stdlib + status-tool directives — verify it reads consistently).
- Commit the archival as `G0.HOUSEKEEPING: archive outdated plan files`; push.

**Verify command**:
```bash
test ! -f HANDOVER.md && ! -d plans && \
test -f archive/PORTPLAN_legacy.md && test -f PORTPLAN.md && \
test -f portplan/GUIDELINES.md && test -d portplan/tasks && \
test -d archive/plans && git status --short && \
go build ./... && make quality
```

**Todos**:
1. Confirm the archive layout matches (files above); fix any stragglers.
2. Update `AGENTS.md` plan pointer to `PORTPLAN.md` + note the archive.
3. Confirm `portplan/GUIDELINES.md` is internally consistent with `PORTPLAN.md`.
4. Commit `G0.HOUSEKEEPING: archive outdated plan files`; push.

---

## Goal G0.STATUS — Build the progress-reporting tool

**Objective**: A `tools/status` program that runs (or reads) the testgen tree and
prints a clear report: progress **per feature family**, each package's state
(pass / fail / skipped), and the skip-reason bucket. This is the single source of
truth for "how green are we?".

**Design** (pure Go stdlib — `os/exec`, `path/filepath`, `sort`, `fmt`):
- Read `tools/tcl2go/gen.go`'s `skipTestFiles`/`skipTests` maps (parse via
  `go/parser` or a focused regex) → known skip set + reason.
- Read `PORTPLAN.md` family table (or a small `tools/status/families.tsv`)
  → package→family mapping (CRUD, JOIN, SCHEMA, FUNCTIONS, CTE/WINDOW, FTS,
  VTAB, JSON, RTREE, C-API, PLANNER, WAL/CONCURRENCY, etc.).
- For each `testgen/<pkg>`: run `go test -tags testgen -count=1 ./testgen/<pkg>/`
  (parallelizable with a worker pool; default concurrency 8) → PASS/FAIL.
  Add a `--skip-run` mode that reports only the *static* pass/fail/skip counts
  from a cached results file (`tools/status/last_run.json`) so CI/quick checks
  don't re-run everything.
- Emit:
  - Per-family summary: `FAMILY  total  pass  fail  skipped  pct`.
  - Per-package detail (one line each): `pkg  family  state  [skip-reason]`.
  - A machine-readable `tools/status/last_run.json` for trend tracking.
- Output modes: `text` (default, terminal table), `markdown` (updates
  `PORTPLAN.md` §2 or a `STATUS.md`), `json`.

**Verify command**:
```bash
cd tools/status && go build -o /dev/null . && \
./status --skip-run --format text 2>&1 | head -5 && \
./status --skip-run --format markdown > /dev/null
```
(Then a real run: `./status --concurrency 8` — used by every later goal.)

**Todos**:
1. Scaffold `tools/status/` (main.go, families mapping, gen.go skip-parser).
2. Implement the `go test` runner with a worker pool + timeout + cached-results
   load/store (`last_run.json`).
3. Implement the family classification (write `families.tsv` from
   `PORTPLAN.md` task index + the skip-reason buckets in §1).
4. Implement text/markdown/json renderers.
5. Write a pre-test `tools/status/status_test.go` that validates the family
   classification and that every `testgen/` package maps to a family.
6. Commit `G0.STATUS: progress-reporting tool (tools/status)`; push.

---

## Goal G0.UNSKIP-SLOW — Un-skip the 140 "slow" packages

**Objective**: Remove the ~140 whole-file `skipTestFiles` entries whose reason is
"slow deep-engine applicable package DEFERRED". These are **real tests** skipped
only for a verify-time budget. They must run and (mostly) pass; any that fail
become work items for G1–G2, *not* re-skips.

**Scope**: In `tools/tcl2go/gen.go`, delete every `skipTestFiles` entry whose
reason contains "slow deep-engine applicable package DEFERRED" (the ~140 listed
in `archive/plans/DEFERRED.md §G6.TRIAGE slow-package`). Then regenerate.

**Verify command**:
```bash
# All un-skipped slow packages must at least COMPILE and run (failures are
# triaged into G1/G2; the verify is "no compile error / no panic"):
go run ./tools/tcl2go/ && go build -tags testgen ./testgen/... && \
go test -tags testgen -count=1 -timeout 60s ./testgen/view/ ./testgen/index/ \
  ./testgen/select1/ ./testgen/select2/ ./testgen/count/ ./testgen/sort/ \
  ./testgen/savepoint/ ./testgen/attach/ ./testgen/orderby1/ 2>&1 | \
  grep -cE '^(ok|FAIL|---)' | grep -q '[0-9]'
```
Then run `tools/status` to record the new baseline of pass/fail per family —
this becomes the work backlog for G1–G2.

**Todos**:
1. Enumerate the ~140 slow-skip entries; confirm each reason text.
2. Remove them from `skipTestFiles` in `gen.go` (with an evidence comment that
   they were un-skipped under PORTPLAN "implement don't defer").
3. Regenerate testgen (`go run ./tools/tcl2go/`); review `git diff --stat`.
4. Build all testgen (`go build -tags testgen ./testgen/...`) — fix any compile
   breakage by fixing the *engine/transpiler*, not by re-skipping.
5. Run `tools/status` → save the new pass/fail baseline as the G1–G2 backlog.
6. Commit `G0.UNSKIP-SLOW: un-skip 140 perf-only DEFERRED packages`; push.

---

## Goal G0.FIX-4-FAILS — Fix the 4 known engine-bug FAIL packages

**Objective**: The 4 currently-failing applicable packages from
`archive/portplan-legacy/G6_TRIAGE_STATUS.md §4` — `check`, `fkey`, `subquery`,
`rowvalue` — each has a pure-Go repro and is a genuine engine gap. Fix them.

**Per-package work** (each a todo; triage via pure-Go test + oracle first):
- **check** — CHECK constraint with `BETWEEN` + unary-plus on TEXT/BLOB operands.
  SQLite `src/insert.c` `checkConstraint` + affinity. Repro in §4 of the archived
  status. Expected: all 5 rows pass.
- **fkey** — self-referential FK with `INSERT OR REPLACE`. SQLite `src/fkey.c`
  + `src/insert.c` conflict resolution. Expected: row accepted.
- **subquery** — subquery-valued `LIMIT` ignored. SQLite `src/select.c` applies
  `LIMIT` from a subquery expression. Expected: 3 rows.
- **rowvalue** — add the 6 `skipTests` entries (5 are documented N-A planner
  row-order / EXPLAIN detail; verify each against oracle, only skip with evidence;
  the 1 collation case may be a real bug — fix if so).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/check/ ./testgen/fkey/ ./testgen/subquery/ ./testgen/rowvalue/ && \
go test -run 'TestP0' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `check`: write `frigolite_p0_check_test.go` pure-Go repro; oracle; fix engine.
2. `fkey`: write `frigolite_p0_fkey_test.go`; fix FK/REPLACE resolution.
3. `subquery`: write `frigolite_p0_subquery_test.go`; fix subquery-valued LIMIT.
4. `rowvalue`: triage the 6 residual tests; fix the collation case if engine;
   add evidence-backed `skipTests` for genuine planner-order/EXPLAIN cases.
5. Run verify; commit `G0.FIX-4-FAILS: check/fkey/subquery/rowvalue`; push.

---

## Definition of Done (this task)
- All four goals' verify commands pass; `archive/` populated; `tools/status`
  runs and reports a per-family baseline; the 140 slow packages run (failures
  are in the G1–G2 backlog); the 4 known FAIL packages are green.
- `PORTPLAN.md` §5 status row → 🟢.
