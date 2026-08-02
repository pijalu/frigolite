# Sub-Plan: G0 — Grammar Completion

> **Goal ID**: G0
> **Prerequisite**: None. Runs first.
> **Sub-plans**: This plan has 3 phases (G0.1, G0.2, G0.3), each a separate goal.

---

## Goal Definition

```
Objective: Complete the LALR grammar handler so every reachable grammar rule
produces a real AST node — no multi-symbol passthrough-to-nil.
Completion criterion: TestGrammarCoverage passes with 0 multi-symbol passthrough
failures; the 10 passthrough rules have real handlers; all previously-passing
testgen packages remain green.
Verify command: go test ./internal/parse/ -count=1 && go test -tags testgen ./testgen/select1/ ./testgen/insert/ ./testgen/update/ ./testgen/delete_/ ./testgen/null/ ./testgen/cast/ -count=1
Fresh context: true
```

## Handover Note (for fresh context)

```
State: internal/parse/parser.go handleRule implements 261 of 412 rules.
TestRuleInventory (run: FRIGOLITE_INVENTORY=1 go test ./internal/parse/ -run
TestRuleInventory) shows 282 fired, 130 unfired, 10 passthrough (fired but nil).
The 10 passthrough rules are the priority — see GRAMMAR_COMPLETENESS.md §2.
Rule numbers reference the LALR tables (RuleInfoLhs/RuleInfoNRhs in sql_tables.go).
Decisions: single-symbol passthroughs (rules 2,14,348,351) are acceptable (type
aliases). Multi-symbol passthroughs MUST get handlers. The grammar coverage test
already exists (grammar_coverage_test.go) — extend its corpus, don't rewrite it.
Next steps: fix rule 277 (trigger_decl) first — highest impact. Then 133, 231,
302. Then extend corpus to exercise unfired rules. NEVER run full testgen suite
(FTS merge suites OOM). Run per-package only.
Risks: Some "unfired" rules may be unreachable in practice (dead grammar).
Verify each is reachable before implementing. The RD parser (internal/sql) may
already handle some constructs — check if the LALR path is even used for that
feature before adding handlers.
Carried limits: verify command above; completion criterion above.
```

---

## G0.1 — Fix the 10 Passthrough Rules

### Step 1: Rule 277 — trigger_decl (3 symbols)
- **Problem**: `CREATE TRIGGER ... WHEN ...` loses trigger time/event/when structure.
- **Root cause**: No handler for rule 277 (trigger_decl with temp/TRIGGER/ifnotexists/nm/dbnm/trigger_time/trigger_event/ON/fullname/foreach_clause/when_clause).
- **SQLite ref**: `src/parse.y:1743` (trigger_decl production), `src/build.c:sqlite3BeginTrigger`.
- **Fix**: Add `case 277:` in handleRule that builds `sql.CreateTriggerStmt` from RHS symbols.
- **Verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1 -v`
- **Verify TCL**: `go test -tags testgen ./testgen/trigger/ -count=1 -timeout 60s`
- **Commit**: `G0.1.1: implement trigger_decl grammar handler (rule 277)`
- [x] Done

### Step 2: Rule 133 — NOT INDEXED (2 symbols)
- **Problem**: `SELECT * FROM t1 NOT INDEXED` loses the NOT INDEXED hint.
- **Root cause**: No handler for the seltablist NOT INDEXED production.
- **SQLite ref**: `src/parse.y:913` (indexed_by ::= NOT INDEXED).
- **Fix**: Add handler that marks the table ref as NOT INDEXED.
- **Verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1`
- **Verify TCL**: `go test -tags testgen ./testgen/whereE/ ./testgen/whereH/ -count=1`
- **Commit**: `G0.1.2: implement NOT INDEXED grammar handler (rule 133)`
- [x] Done

### Step 3: Rule 231 — CASE+expr precedence (2 symbols)
- **Problem**: `SELECT case when 1 then 99 else ? end + ?` — CASE result + arithmetic.
- **Root cause**: The CASE exprlist rule that includes the trailing expr loses structure.
- **SQLite ref**: `src/parse.y:1581` (case_exprlist).
- **Fix**: Ensure case_exprlist reduction properly chains to the parent expr.
- **Verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1`
- **Verify TCL**: `go test -tags testgen ./testgen/expr/ ./testgen/cse/ -count=1`
- **Commit**: `G0.1.3: fix CASE+expr grammar handler (rule 231)`
- [x] Done

### Step 4: Rule 302 — CREATE VIRTUAL TABLE module name (1 symbol)
- **Problem**: `CREATE VIRTUAL TABLE ... USING fts4` loses the module name.
- **Root cause**: create_vtab passthrough drops the USING nm clause.
- **SQLite ref**: `src/parse.y:1932` (create_vtab).
- **Fix**: Build `sql.CreateVirtualTableStmt` with ModuleName from RHS.
- **Verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1`
- **Commit**: `G0.1.4: implement CREATE VIRTUAL TABLE grammar handler (rule 302)`
- [x] Done

### Step 5: Rules 349, 352 — multi-statement chaining (2 symbols each)
- **Problem**: Multi-statement scripts (`BEGIN; INSERT...; COMMIT`) may lose statements.
- **Root cause**: cmdlist/ecmd chaining not properly building statement list.
- **Fix**: Ensure cmdlist reduction accumulates statements into a list.
- **Verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1`
- **Commit**: `G0.1.5: implement multi-statement chaining handlers (rules 349, 352)`
- [x] Done

### Step 6: Rules 2, 14, 348, 351 — single-symbol passthroughs (verify OK)
- **Action**: Verify these are single-symbol type-alias productions (acceptable passthrough).
  If any is multi-symbol, add a handler. If single-symbol, document as acceptable.
- **Commit**: `G0.1.6: audit single-symbol passthrough rules (2,14,348,351) — document`
- [x] Done

---

## G0.2 — Extend Grammar Coverage Corpus

### Step 1: Add window-function statements to corpus
- **Action**: Add OVER/FILTER/PARTITION/WINDOW statements to `grammarCoverageCorpus`
  in `grammar_coverage_test.go`. This will expose unfired window rules.
- **Verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1 -v`
  (expect NEW passthrough failures for window rules)
- **Commit**: `G0.2.1: extend grammar corpus with window-function statements`
- [x] Done

### Step 2: Add CTE / WITH statements
- **Action**: Add `WITH x AS (...)`, `WITH RECURSIVE`, `MATERIALIZED` to corpus.
- **Commit**: `G0.2.2: extend grammar corpus with CTE/WITH statements`
- [x] Done

### Step 3: Add ALTER TABLE statements
- **Action**: Add ADD/DROP/RENAME COLUMN, DROP CONSTRAINT to corpus.
- **Commit**: `G0.2.3: extend grammar corpus with ALTER TABLE statements`
- [x] Done

### Step 4: Add constraint statements
- **Action**: Add FK REFERENCES, CHECK, GENERATED ALWAYS AS to corpus.
- **Commit**: `G0.2.4: extend grammar corpus with constraint statements`
- [x] Done

---

## G0.3 — Implement Remaining Reachable Rules to Full Coverage

### Iterative loop:
For each passthrough failure exposed by the extended corpus:
1. Identify the rule number and grammar production.
2. Add a handler in `handleRule`.
3. Verify `TestGrammarCoverage` improves.
4. Run the relevant testgen package to check runtime impact.
5. Commit.

### Target: 0 multi-symbol passthrough failures in TestGrammarCoverage.

- **Final verify**: `go test ./internal/parse/ -run TestGrammarCoverage -count=1 -v`
- **Commit**: `G0.3.N: implement <rule> handler — grammar coverage at N%'
- [ ] Done

---

## Verification Matrix

After G0 completes:

| Check | Command | Expected |
|-------|---------|----------|
| Grammar coverage | `go test ./internal/parse/ -run TestGrammarCoverage -count=1` | PASS (0 failures) |
| Rule inventory | `FRIGOLITE_INVENTORY=1 go test ./internal/parse/ -run TestRuleInventory` | 0 multi-symbol passthroughs |
| No regression | `go test -tags testgen ./testgen/{select1,insert,update,delete_,null,cast,between,coalesce}/ -count=1` | All PASS |
| SOLID | `go test -run TestSOLID_ ./... -count=1` | PASS |
| Build | `go build ./...` | OK |