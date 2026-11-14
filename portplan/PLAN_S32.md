# PLAN_S32 — the expr converter batch (S5: A1 + A8 + D1, 11 subtests)

Branch s32-expr-conv @ dd045611d, worktree /private/tmp/wt_expr2.

## Baselines (own sweeps, this worktree)

- `FRIGOLITE_TEST=e_expr`: 12 leaves — A1×8 (21.1.1, 21.1.2, 21.1.3, 21.1.4,
  21.2.3, 21.3.1, 21.3.2, 22.1.1) + A8×2 (5.1, 5.2) + engine residue
  (26.1.6 A2, 32.2.8 A9). /tmp/s32_e_expr_before.log
- `FRIGOLITE_TEST=fts3expr`: 1 leaf — fts3expr-6.1 (D1, "table t1 already
  exists"). /tmp/s32_fts3expr_before.log
- `FRIGOLITE_TEST=fts3`: 54 leaves across 10 families (fts3auto 1, fts3cov 1,
  fts3defer 3, fts3defer2 19, fts3defer3 1, fts3expr 1, fts3first 6,
  fts3malloc 4, fts3query 11, e_fts3 7). /tmp/s32_fts3_before_leaves.txt

## Root causes (all verified against failure output + sources)

### A1 — e_expr runtime-var init dropped (8 leaves)

`foreach {a b c} {0 0 0} break` (e_expr.test:1164/1186/1194/1202/1215/1260/
1366) binds the globals on the FIRST iteration before the body's `break`
runs (Tcl semantics). The converter's cmdForeach binds them only in its own
interp (conversion-time `$` substitution) and emits nothing, so the
harness's `var()` UDF (registered at setup_15714, reads runtimeVars) sees
whatever was stale: runtimeVars["b"]="23" is seeded earlier by
setup_15700's register_function callbackVars → CASE WHEN var('b') picks 'B'
(got B / varlist "a b" vs want {}/"a b c"/"no result"/"c b a"/C/null/C).
25.1.x passes today only by accident (stale truthy b ≡ 1) and 22.1.1 fails
(a missing → NULL). e_expr-25.x ALSO depends on the 1366 site — pinning
sites 1-6 without 7 would regress 25.1.1/25.1.3 (site 6 sets a=3,b=4,c=5).

Fix (converter): cmdForeach detects the break-terminated idiom (bare
`break` body, or a braced body whose FIRST command is `break`), emits one
`runtime_expr` step per bound variable with the first-chunk value (numeric
→ bare text; other static values → braced; past-list-end → empty SQL which
the harness treats as the empty-string reset). Steps carry TestName=i.curTest
→ at top level they join the surrounding setup group positionally
(setup_15714 = [register_function(var), runtime_expr a, b, c,
runtime_var_reset(varlist)]). Loop stops after the first iteration.

Fix (fixture): HAND-PIN e_expr.json — the wholesale regen drifts
7024/32210 cases (re-measured today: 7021 drifting steps; the drift is the
register_function callbackVars machinery), so per the S22/S29 lesson no
regen. Seven pinned sites (all values from the source):
  1. src:1164 {0 0 0} → into setup_15714 before its runtime_var_reset
  2. src:1186 {0 1 0} → new setup group between 21.1.4 and 21.2.1
  3. src:1194 {0 1 1} → new setup group between 21.2.1 and 21.2.2
  4. src:1202 {0 0 1} → new setup group between 21.2.2 and 21.2.3
  5. src:1215 {0 0 0} → new setup group between 21.2.3 and 21.3.1
  6. src:1260 {3 4 5} → into setup_15718 before its runtime_var_reset
  7. src:1366 {0 1 0} → new setup group before 25.1.1 (protects 25.x)
New setup groups get unique suffix names (setup_15714a-style) so a future
clean regen cannot collide.

### A8 — frozen `$as$bs` expects (2 leaves)

e_expr-5.1/5.2 expect `$as$bs` — the converter froze the runtime-marker
concatenation `[list "${as}${bs}"]` produced (as/bs are query_assign
runtime variables; their placeholders ARE "$as"/"$bs"). The harness's
expandTCLVarRefs was built to split adjacent placeholders at the inner `$`
(its comment documents exactly this shape) but the split condition
`key[idx+1] == '$'` tests the character AFTER the inner '$' — the inner '$'
sits AT idx, so the branch can never fire (verified with a scratch probe:
expandTCLVarRefs("$as$bs", {as:helloworld, bs:12345}) returns "$as$bs").
Fix: split when values[key[:idx]] exists; write the first segment's value
and continue the scan at i+1+idx (the inner '$'), letting the loop re-scan
the remainder — chains ("$a$b$c") split iteratively. Fixture unchanged.

### D1 — dropped malloc-test db reset (1 leaf)

do_malloc_test (malloc_common.tcl:416) deletes test.db/-journal/-wal and
opens an EMPTY database for every trial; the n=0 trial injects no failure,
runs the body once, and the do_test asserts {1 1} (success). The converter
silently dropped the whole command → t1(a,b,c) from fts3expr-4.1 survives →
fts3expr-6.1's CREATE VIRTUAL TABLE t1 USING fts3(a) fails "table t1
already exists" (oracle-confirmed upstream reset semantics).

Fix (converter): implement do_malloc_test = reset_db marker (the harness's
__RESET_DB__ deletes the lifecycle db + journal/wal/mj sidecars and reopens
empty — malloc's forcedelete semantics) + -tclprep/-sqlprep/-tclbody/-sqlbody
/-cleanup executed in order under the malloc test's name (the -sqlbody as a
query/exec step with no expect — an engine error fails the test, matching
the {1 1} success contract without fault injection; the -tcl scripts run
through execScript so set_fts3_parens etc. fire normally). Unknown options
→ UnsupportedCommandError (never silently skipped).

Fix (fixture): REGEN testdata/fts3expr.json — the current converter regen
is byte-identical to the committed fixture (verified: 123/123 names, 0
content diff), so the only diff will be the two malloc insertions.

## Ordered steps

1. Converter: cmdForeach break-idiom emission (helper function to keep
   gocyclo ≤12) + unit test in tools/tclconvert/tcl.
2. Converter: commandDoMallocTest + registration + unit test.
3. Regen fts3expr.json → diff must be exactly the two malloc groups.
4. Harness: expandTCLVarRefs split fix (frigolite_harness_test.go — test
   file, exempt from the quality gate but compiled).
5. Hand-pin e_expr.json (python edit, JSON re-validated).
6. Sweeps: e_expr, fts3expr, fts3 (name-set diffs vs baselines above),
   plus `FRIGOLITE_TEST=expr` (the 17-family substring sweep) as the
   regression net.
7. Lessons entry (append-only), commit on s32-expr-conv.

## Verification contract

- e_expr: the 10 A1/A8 leaves leave the failure set; remaining = {26.1.6,
  32.2.8} (engine slices A2/A9, not mine).
- fts3expr: failure set empty.
- fts3 sweep: leaf set unchanged (54; D1's leaf is the fts3expr one).
- No new failing leaf anywhere swept; gocognit/gocyclo/staticcheck pass on
  the changed non-test Go files.

## Non-goals (carded)

- A2 (26.1.6) / A9 (32.2.8) — engine slices, not mine.
- Converter callbackVars drift (the e_expr regen blocker) — carded
  converter-alignment slice.
- General foreach var-binding emission for non-break loops (only the
  break idiom has a runtime-visible reader today; a UDF reading a global
  bound by a fully-unrolled loop has no failing instance in the corpus).
- -tclbody/-sqlprep/-start/-end/-testdb arms beyond what the corpus uses
  are parsed and executed where faithful, but no fault-injection surface.

## Amendment (2026-11-14, mid-unit): Tcl object-type fidelity for the
## foreach-init bindings

Hand-pinning alone did not close e_expr-22.1.1: the harness's var() UDF
returned TEXT '3' where upstream returns INTEGER 3 — `CASE var('a') WHEN 3`
compares TEXT vs INTEGER with NO affinity (oracle 3.54: `CASE '3' WHEN 3` →
NULL; '3' = 3 → 0), so the WHEN never matched. The testfixture oracle
(3.51.0) confirmed the object duality: `foreach {a b c} {0 0 0} break`
yields TEXT '0' (string objects), while
`foreach {a b c} [list [expr 3] [expr 4] [expr 5]] break` yields INTEGER
objects — and e_expr-25.1.3 (`CASE '0' WHEN var('a')`) DEPENDS on the
braced form staying TEXT. So the converter distinguishes origins: a
break-foreach whose list word is a bracket-evaluated command emits
`valueType: "integer"`/"real"` for canonical-numeric elements; braced or
bare list words stay untyped (TEXT). The harness stores the type on a
NUL-prefixed side key in runtimeVars (`\x00rttype:<name>` — no Tcl variable
name contains NUL; the map is never iterated wholesale) and the var() UDF
returns int64/float64 for typed values. runtime_expr/runtime_var_reset
clear the marker on re-assignment. The hand-pin gives site 6
(valueType: integer); sites 1-5/7 stay untyped. A trial regen reproduces
the same steps at the same positions (setup group NAMES shift by the
sequential counter — content-equal, name-cosmetic).

Quality gate: baseline for the converter files already fails (commandSet
69/61, interp.go/interp_cmds.go > 1000 lines) — grandfathered; this unit
adds NO new gate findings (isCanonicalFloat rewritten to a charset scan +
ParseFloat; commandDoMallocTest split into parseMallocTestArgs /
execMallocScript / emitMallocSQLStep; staticcheck diff vs baseline = line
shifts only).
