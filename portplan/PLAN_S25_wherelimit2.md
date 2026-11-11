# S25 plan — wherelimit2 family closure (4 subtests)

STATUS (2026-11-12): DONE. wherelimit2 4 -> 0; wherelimit sweep 5 -> 0;
where sweep 86 -> 81 (fixed = exactly the wherelimit2 five); zero
regressions in every name-set diff. go vet + go build clean.

Branch `s25-limit` (feature/perf @ 965d510db). Goal: wherelimit2 failures
5.4, 5.5, 6.1, 6.2 → 0, zero new failures anywhere.

## Failure set (reproduced in this worktree)

- wherelimit2-5.4: runtime variable mismatch got [""] want [a a b b c c]
- wherelimit2-5.5: runtime variable mismatch got [""] want [ax a bx b cx c dx d ex a]
- wherelimit2-6.1: query error near "ORDER" — WITH t2 AS MATERIALIZED (VALUES(5)) DELETE FROM t2 ORDER BY rank()OVER() LIMIT 2;
- wherelimit2-6.2: SELECT * FROM t2 got [1 2 3 5 8 13] want [3 5 8 13] (cascade of 6.1 — the delete never ran)

## Diagnosis (probe-verified; pure-Go probe driving Open/Exec/Query)

### 5.4/5.5 — ENGINE CORRECT, harness gap

The TCL source (`wherelimit2.test` 5.3-5.5) registers a variadic UDF
(`proc log {args} { lappend ::log {*}$args }; db func log log`) and the
INSTEAD OF trigger bodies `SELECT log(old."a b", old."c d")` fire it per
row. The committed fixture's `setup_0` already carries
`register_function(functionName=log)`; the harness registers it through
the general Tcl-proc fallback `newHarnessTclFunction`
(frigolite_tcl_function_test.go). The probe proves the ENGINE fires the
trigger and calls the UDF with exactly the expected arguments
(`a a b b c c` / `ax a bx b cx c dx d ex a`).

Gap: `harnessTclFunction.Call` evaluates the proc in its own
`tcl.Interp`; the proc's `lappend ::log ...` writes the INTERP's global
which is never synced back into the harness `runtimeVars` map that the
`runtime_var` step reads → got [""].

Sibling precedents: the rtgeom recorder's `syncVars`
(frigolite_harness_test.go:139) and the unlock-notify eval_script sync
(frigolite_harness_test.go:2169) both copy callback-written interp
globals into runtimeVars. `NotifyScriptVarTargets`
(tools/tclconvert/tcl/interp_stmt.go:28) lists the vars a script body
writes (set/lappend/append/incr heads) and is already exported for
exactly this purpose.

Sub-requirement: the stale fixture lacks the `runtime_var_reset log`
steps (`set ::log {}` in the source). Without them 5.5 would append onto
5.4's list. The fixture must be regenerated
(`go run ./tools/tclconvert -testdir /Users/muaddib/dev/sqlite/test
-outdir /tmp/x25 -only wherelimit2`): diff vs committed = only
`__RESET_DB__` steps null→[] (harmless), `setup_0` gains
`callbackProcs` + newer converter callbackVars keys (converter drift,
harmless), 5.4/5.5 gain `runtime_var_reset log`. All other 31 cases
byte-identical; name-set equal.

### 6.1 — parser gap: the UDL strip skips WITH-prefixed statements with materialization hints

DELETE/UPDATE ... ORDER BY/LIMIT is not in the LALR tables; the
pre-parse rewrite `rewriteStmtOrderLimit` (internal/parse/parser_extras.go)
strips the trailing clause and re-attaches it post-parse. The WITH
prefix is handled by `isTopLevelStmt`, which walks the depth-0 keywords
before the DML keyword and allows ONLY {WITH, AS, RECURSIVE, ","}. A
CTE header with a materialization hint — `WITH t2 AS MATERIALIZED
(VALUES(5)) DELETE ...` — trips on `MATERIALIZED` and the statement is
left alone → LALR rejects at ORDER.

Fix: complete the CTE-header vocabulary with `MATERIALIZED` and `NOT`
(Tcl source grep: no other file combines MATERIALIZED with DELETE/
UPDATE; the where sweep confirms no regression).

Semantics after parse (oracle /usr/bin/sqlite3 3.54, tagged): the full
6.0→6.1→6.2 sequence returns `3 5 8 13` — the CTE does NOT shadow the
DELETE target; rows 1,2 are deleted from the REAL table. The probe
confirms frigolite's delete executor already handles
`ORDER BY rank() OVER () LIMIT 2` correctly on a plain DELETE (collect
rows → sort → limit; the window expr evaluates per row), so no
execution-side change is expected.

6.2 is a cascade: once 6.1 parses, the delete runs and 6.2's SELECT
matches.

## Changes

1. `internal/parse/parser_extras.go` — `isTopLevelStmt`: allow
   `MATERIALIZED` / `NOT` (the full CTE-header keyword vocabulary) as
   depth-0 keywords before the DML keyword; comment updated.
2. `frigolite_tcl_function_test.go` — `harnessTclFunction`: collect the
   var targets the proc body / callback procs write
   (`tcl.NotifyScriptVarTargets` over FunctionBody + CallbackProcs
   bodies); per Call: push each target present in runtimeVars into the
   interp (mirror the shared-global semantics — also carries
   runtime_var_reset into the interp), invoke, pull each target back
   into runtimeVars.
3. `testdata/wherelimit2.json` — regenerated with tclconvert (adds the
   runtime_var_reset steps; converter drift on setup_0/__RESET_DB__).

## Verification

- `FRIGOLITE_TEST=wherelimit2 go test -run "^TestSQLiteSuite$" -count=1 -timeout 10m .` → 0 failures (ok, 0.293s).
- Baselines BEFORE any edit: `FRIGOLITE_TEST=wherelimit` (5) and
  `FRIGOLITE_TEST=where` (86) failure name-sets (/tmp/s25_*_base_names.txt).
  After (python set diffs both directions): wherelimit 5 → 0; where 86 → 81;
  fixed = exactly the wherelimit2 five; regressions = [] in both sweeps.
- `go vet . ./internal/parse/` + `go build ./...` clean (tmp_probe25 removed).
- Commit on s25-limit (pre-commit hook runs quality gates: gocognit ≤15,
  gocyclo ≤12, no unused imports/commented-out code).
- S25 record appended at the END of .agents/lessons_learned.md
  (append-only; TODO.md untouched).
