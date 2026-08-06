# TASK G5.VTAB — Virtual tables (generate_series, carray, basic modules)

> **Phase**: G5 (advanced SQL).
> **Goal**: G5.VTAB.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.CREATE; G1.SELECT.
> **Current state: PARTIAL** — generate_series + echo module exist.

## Objective
Virtual tables work for the applicable vtab tests: `CREATE VIRTUAL TABLE ...
USING <module>(args)`, the built-in `generate_series`, the module/bestindex
contract (frigolite uses a simpler planner — correctness over the full bestindex
negotiation), `sqlite_master` entries for vtabs, and DROP of a vtab. Echo/series
modules suffice for most tests; `carray`/`intarray`/`tabfunc` are table-functions
(triage — may be N/A if they're pure C-extension).

## Scope — testgen packages
`vtab`, `vtab_`, `vtabA`–`vtabL`, `generate_series` (if present), `bestindex`–
`bestindexG`, `vtabdistinct`, `vtabdrop`, `vtabrhs`. (Many bestindex tests probe
the planner negotiation — triage; keep correctness, N-A the pure-bestindex-output.)

## Pre-test file
`frigolite_p5_vtab_test.go` — `TestP5Vtab_*`. Cases vs oracle:
- `CREATE VIRTUAL TABLE t USING generate_series(1,10)`; SELECT from it.
- Module args parsing; column metadata.
- DROP VIRTUAL TABLE.
- Echo module proxies underlying table (existing).
- Series with start/stop/step; NULL/empty.

## SQLite source references
- `src/vtab.c`, `src/vtabaux.c` — module interface.
- `ext/misc/series.c` — generate_series.
- `internal/vtab/` — frigolite module system.

## Steps
- [ ] **G5.VTAB.1** Baseline vtab packages; record results. Commit:
      `G5.VTAB.1: vtab baseline`.
- [ ] **G5.VTAB.2** Pre-test suite. Commit: `G5.VTAB.2: vtab pre-test suite`.
- [ ] **G5.VTAB.3** generate_series correctness (start/stop/step, empty, negative
      step). Commit: `G5.VTAB.3: generate_series`.
- [ ] **G5.VTAB.4** CREATE/DROP VIRTUAL TABLE + module arg parsing + metadata.
      Commit: `G5.VTAB.4: vtab lifecycle`.
- [ ] **G5.VTAB.5** Triage bestindex packages — keep correctness; N-A the pure
      bestindex-output tests with evidence. Commit: `G5.VTAB.5: bestindex triage`.
- [ ] **G5.VTAB.6** vtab family green (applicable subset). Commit:
      `G5.VTAB.6: vtab TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/vtab/ ./testgen/vtab_/ ./testgen/vtabA/ ./testgen/vtabB/ ./testgen/vtabC/ 2>&1 | tail -5 && \
go test -run 'TestP5Vtab' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Virtual tables work for applicable tests: CREATE/DROP VIRTUAL TABLE USING module, generate_series (start/stop/step/empty/negative), module arg parsing + metadata, echo module. bestindex negotiation kept for correctness, pure-bestindex-output tests triaged to N-A. generate_series + echo module exist. See portplan/TASK_G5_VTAB.md." \
  completionCriterion "applicable vtab packages PASS and TestP5Vtab pre-tests PASS; bestindex-output-only tests documented N-A." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/vtab/ ./testgen/vtab_/ ./testgen/vtabA/ ./testgen/vtabB/ ./testgen/vtabC/ && go test -run TestP5Vtab -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G5.VTAB. generate_series + echo module exist (internal/vtab/). bestindex negotiation is simplified —
correctness bar, not full planner output.
Decisions: pure bestindex-output tests → N-A with evidence (document in NOT_APPLICABLE.md).
Next: baseline, pre-tests, generate_series correctness, vtab lifecycle, bestindex triage.
Risks: many vtab tests probe the C module ABI — triage honestly; don't fake bestindex output.
Carried limits: verifyCommand above.
```
