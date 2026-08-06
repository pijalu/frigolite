# TASK G5.ANALYZE — ANALYZE, statistics, autoindex, query planning

> **Phase**: G5 (advanced SQL).
> **Goal**: G5.ANALYZE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G3.INDEX; G5.EXPLAIN (plans).
> **Current state: UNKNOWN** — needs baseline.

## Objective
`ANALYZE` + statistics-based query *correctness* match SQLite. Frigolite does
not need a cost-based optimizer for *results*, but: (a) `ANALYZE` must populate
`sqlite_stat1` (and the testgen tests that read it must see the right shape),
(b) `autoindex` (automatic indexes for `WHERE x IN (...)` / joins) must not
change *results* — only speed, (c) `REINDEX`, (d) index-assisted WHERE must
produce identical results to a full scan. Pure *plan-choice* tests (which index
SQLite picks, cost estimates) are out of scope / N-A — only result correctness.

## Scope — testgen packages
`analyze`, `analyzeC`, `analyzeD`, `analyzeE`, `analyzeF`, `analyzeG`,
`analyzer`, `autoindex`, `eqp` (overlap G5.EXPLAIN), `cost`, `filter`,
`pushdown`, `scanstatus`. (Many are planner-cost tests → triage to N-A the
cost-only ones.)

## Pre-test file
`frigolite_p5_analyze_test.go` — `TestP5Analyze_*`. Cases vs oracle:
- ANALYZE; `SELECT * FROM sqlite_stat1` shape.
- REINDEX.
- Query result identical whether or not an autoindex is used (correctness).
- autoindex does not corrupt results on `WHERE x IN (subquery)` / joins.

## SQLite source references
- `src/analyze.c` — `sqlite3Analyze`, stat1 population.
- `src/where*.c` — autoindex, planner (result-equivalence only).

## Steps
- [ ] **G5.ANALYZE.1** Baseline analyze + autoindex packages; record results.
      Commit: `G5.ANALYZ.1: analyze baseline`.
- [ ] **G5.ANALYZE.2** Pre-test suite (correctness focus). Commit:
      `G5.ANALYZE.2: analyze pre-test suite`.
- [ ] **G5.ANALYZE.3** ANALYZE populates sqlite_stat1 correctly. Commit:
      `G5.ANALYZE.3: ANALYZE + stat1`.
- [ ] **G5.ANALYZE.4** autoindex result-equivalence (with/without → same rows).
      Commit: `G5.ANALYZE.4: autoindex correctness`.
- [ ] **G5.ANALYZE.5** Triage cost/scanstatus/pushdown → N-A the pure-cost tests.
      Commit: `G5.ANALYZE.5: planner-cost N-A`.
- [ ] **G5.ANALYZE.6** applicable analyze subset green. Commit:
      `G5.ANALYZE.6: analyze TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/analyze/ ./testgen/analyzeC/ ./testgen/autoindex/ && \
go test -run 'TestP5Analyze' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "ANALYZE/stat1 correctness + autoindex/index-assisted result-equivalence (results identical with/without index; speed/planner-choice not required). REINDEX. Pure cost/plan-choice tests documented N-A. See portplan/TASK_G5_ANALYZE.md." \
  completionCriterion "testgen analyze, analyzeC, autoindex PASS; cost-only planner tests documented N-A; TestP5Analyze pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/analyze/ ./testgen/analyzeC/ ./testgen/autoindex/ && go test -run TestP5Analyze -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G5.ANALYZE. Baseline TBD. ANALYZE populates sqlite_stat1 (src/analyze.c). autoindex is result-equivalence
only — speed/plan-choice is out of scope (N-A cost tests).
Decisions: correctness bar = same rows with/without index; never let an index change results.
Next: baseline, pre-tests, ANALYZE/stat1, autoindex correctness, triage cost tests.
Risks: autoindex bugs that change results are subtle — always cross-check against full-scan output.
Carried limits: verifyCommand above.
```
