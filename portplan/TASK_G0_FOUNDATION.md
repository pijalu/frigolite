# TASK G0 — Foundation: Grammar, Triage Harness, Oracle Helper

> **Phase**: G0 (prerequisite to all feature work).
> **Goals**: G0.GRAMMAR, G0.TRIAGE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.

## Objective
Remove the systemic blockers that pollute every feature task:
1. Ensure the lemon-LALR parser reaches full reachable-rule coverage (no
   "passthrough" multi-symbol rules that silently drop AST nodes).
2. Stand up a reusable **triage harness** so any agent can run the
   pure-Go-test-first protocol without reinventing it.

## Why first
Many testgen failures cascade from grammar rules that reduce to nothing, and
from agents re-deriving oracle behavior ad hoc. Fixing both once unblocks every
downstream task and makes engine-vs-transpiler decisions fast and trustworthy.

## Scope — testgen packages
None directly. This task improves *infrastructure*:
- `internal/parse/` (parser reduce handlers, rule inventory test).
- A shared oracle helper used by all pre-tests.

## SQLite source references
- `/Users/muaddib/dev/sqlite/src/parse.y` — the grammar.
- `/Users/muaddib/dev/sqlite/src/build.c`, `expr.c`, `select.c` — semantic actions.

---

## G0.GRAMMAR — Grammar coverage

### Current signal
`internal/parse/grammar_coverage_test.go` + `rule_inventory_test.go` exist. The
goal is **0 multi-symbol passthrough rules** (rules where `handleRule` returns
the first RHS value instead of building a real node).

### Steps
- [ ] **G0.GRAMMAR.1** Run `go test ./internal/parse/ -run TestGrammarCoverage -count=1 -v`.
  Capture the list of multi-symbol passthrough rules. Record them.
  Commit: `G0.GRAMMAR.1: grammar coverage baseline`.
- [ ] **G0.GRAMMAR.2** Extend `grammarCoverageCorpus` to exercise window, CTE,
  ALTER, constraint, RETURNING, UPSERT, generated-column statements so all
  reachable rules fire. Re-run; expect new failures.
  Commit: `G0.GRAMMAR.2: extend grammar corpus`.
- [ ] **G0.GRAMMAR.3** Implement a real AST node for each multi-symbol
  passthrough rule (rules are number-keyed in `handleRule`). One rule group per
  commit. After each, re-run coverage.
  Commit per group: `G0.GRAMMAR.3.<n>: handle rule <no>`.
- [ ] **G0.GRAMMAR.4** Re-run a CRUD regression to prove no breakage:
  `go test -tags testgen ./testgen/select1/ ./testgen/insert/ ./testgen/update/ ./testgen/delete_/ ./testgen/where/ -count=1`.
  Commit: `G0.GRAMMAR.4: grammar full coverage, CRUD regression green`.

### Verify command
```bash
go test ./internal/parse/ -run TestGrammarCoverage -count=1 && \
go test -tags testgen -count=1 ./testgen/select1/ ./testgen/insert/ ./testgen/update/ ./testgen/delete_/ ./testgen/where/ && \
go build ./...
```

---

## G0.TRIAGE — Reusable triage/oracle harness

### Objective
A tiny, shared test helper that any pre-test can call to:
1. Open a frigolite in-memory DB and run a sequence of SQL statements.
2. Compare result rows against an expected slice, with NULL-token + ordering
   config matching the test's `tcl_nullvalue`.
3. Optionally shell out to `/usr/bin/sqlite3` to derive the expected value.

### Steps
- [ ] **G0.TRIAGE.1** Add a helper (e.g. in a new `frigolite_oracle_test.go` at
  repo root, build-tag-free so pre-tests import it) with:
  - `runSQL(t, db, stmts...)` — exec, fatal on error.
  - `queryRows(t, db, sql) [][]string` — render rows with a configurable NULL token.
  - `oracleRows(t, sql) [][]string` — pipes SQL to `sqlite3 :memory:` and parses
    output (pipe-separated); `t.Skip` if `sqlite3` is absent so CI without it
    still passes.
  Commit: `G0.TRIAGE.1: add oracle/triage test helpers`.
- [ ] **G0.TRIAGE.2** Document usage in `portplan/GUIDELINES.md §11` and show a
  worked example. Commit: `G0.TRIAGE.2: document triage helpers`.

### Verify command
```bash
go test -run 'TestOracleHelper' -count=1 . && go build ./...
```
(Write a minimal `TestOracleHelper_Smoke` proving the helpers compile and run.)

---

## Goal create commands

### G0.GRAMMAR
```
goal create \
  objective "Reach full reachable grammar coverage: 0 multi-symbol passthrough rules in internal/parse. See portplan/TASK_G0_FOUNDATION.md §G0.GRAMMAR." \
  completionCriterion "TestGrammarCoverage passes with 0 multi-symbol passthrough failures; CRUD regression packages still PASS." \
  verifyCommand "go test ./internal/parse/ -run TestGrammarCoverage -count=1 && go test -tags testgen -count=1 ./testgen/select1/ ./testgen/insert/ ./testgen/update/ ./testgen/delete_/ ./testgen/where/ && go build ./..." \
  freshContext true
```

### G0.TRIAGE
```
goal create \
  objective "Add reusable oracle/triage test helpers (runSQL, queryRows, oracleRows) for the pure-Go-test-first protocol. See portplan/TASK_G0_FOUNDATION.md §G0.TRIAGE." \
  completionCriterion "Helpers exist, a smoke test passes, and GUIDELINES.md documents them." \
  verifyCommand "go test -run TestOracleHelper -count=1 . && go build ./..." \
  freshContext true
```

---

## Handover note (copy into goal handover field)

```
State: G0 foundation. Parser is lemon-LALR in internal/parse (rule-numbered
handleRule). Grammar source is SQLite parse.y. Coverage tests: grammar_coverage_test.go,
rule_inventory_test.go. Done: [fill after completion] — list rules handled, helper
APIs added.
Decisions: pre-tests live at repo root as frigolite_p<N>_*_test.go; oracle is
/usr/bin/sqlite3 (t.Skip when absent).
Next: run TestGrammarCoverage, fix each passthrough rule, then CRUD tasks (G1.*).
Risks: extending the corpus may surface deep grammar gaps; fix rule-by-rule, never
silently pass through.
Carried limits: verifyCommand above; completionCriterion above.
```
