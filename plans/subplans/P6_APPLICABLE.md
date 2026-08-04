# Sub-Plan: P6.APPLICABLE — Applicable Misc Test Packages (G6.MISC)

> **Parent**: `plans/subplans/P6_TRIAGE.md` (§G6.MISC)
> **Scope**: 148 testgen packages that test real SQL functionality not covered
> by P1–P5. These are miscellaneous engine bugs.
> **Protocol (P6)**: hand-written pre-tests BEFORE running TCL testgen packages,
> one pre-test per root-cause fix.

---

## Goal

```
Objective: Make all applicable misc test packages PASS (or documented as N/A/deferred).
Completion criterion: All packages in plans/PACKAGES_TIER6C.txt PASS via
scripts/verify_all_applicable.sh; sub-plan checkboxes ticked; committed + pushed.
Verify: bash scripts/verify_all_applicable.sh
Fresh context: true
```

## Baseline (measured 2026-08-XX)

| Group | Packages | PASS | FAIL |
|-------|----------|------|------|
| G6.MISC.tkt | 62 tkt packages | TBD | TBD |
| G6.MISC.rowvalue | rowvalue, rowvalueA | TBD | TBD |
| G6.MISC.index | bloom, coveridxscan, descidx, expridx, numindex, seekscan, skipscan, indexA | TBD | TBD |
| G6.MISC.misc | ~60 general SQL tests | TBD | TBD |
| G6.MISC.bigdata | bigrow, bigsort, boundary, manydb, merge, full, widetab | TBD | TBD |
| G6.MISC.incr | incrblob, incrblob_, incrvacuum, incrvacuum_, autovacuum | TBD | TBD |
| G6.MISC.e_tests | e_, e_select | TBD | TBD |
| G6.MISC.rand | randexpr | TBD | TBD |

## Steps (general pattern per sub-group)

1. **Write pre-tests** (P6 protocol) — hand-written test for each root cause.
2. **Run all packages in the sub-group** — capture pass/fail per package.
3. **Group failures by error pattern** — syntax error, result mismatch, etc.
4. **Fix root causes** (smallest fix per root cause).
5. **Re-run affected packages**.
6. **Commit per root-cause fix**.
7. **Update this plan** with results.

### Commit format: `G6.MISC.<subgroup>.<N>: <description>`

---

## Sub-group checkboxes

### G6.MISC.tkt — Ticket regression tests (62 packages)
- [ ] **1. Baseline** — run all 62 tkt packages, record pass/fail.
- [ ] **2. Group failures** by root cause.
- [ ] **3. Fix root causes** (pre-test per fix), re-run affected packages.
- [ ] **4. Commit** per root-cause fix.

### G6.MISC.rowvalue — Row value operations
- [ ] **1. Baseline** — rowvalue, rowvalueA.
- [ ] **2. Fix** (pre-test first).
- [ ] **3. Commit**.

### G6.MISC.index — Index features
- [ ] **1. Baseline** — bloom, coveridxscan, descidx, expridx, numindex, seekscan, skipscan, indexA.
- [ ] **2. Fix** (pre-test first).
- [ ] **3. Commit**.

### G6.MISC.misc — General SQL tests
- [ ] **1. Baseline** — ~60 packages.
- [ ] **2. Fix** (pre-test first).
- [ ] **3. Commit**.

### G6.MISC.bigdata — Large data / limits
- [ ] **1. Baseline** — bigrow, bigsort, boundary, manydb, merge, full, widetab.
- [ ] **2. Fix** (pre-test first).
- **3.** Note: `bigfile`, `bigmmap` excluded (N/A); `bigsort` may need sort work.

### G6.MISC.incr — Incremental blob / vacuum
- [ ] **1. Baseline** — incrblob, incrblob_, incrvacuum, incrvacuum_, autovacuum.
- [ ] **2. Fix** (pre-test first).
- [ ] **3. Commit**.

### G6.MISC.e_tests — e_* comprehensive tests
- [ ] **1. Baseline** — e_, e_select.
- [ ] **2. Assess** — need do_select_tests/do_createtable_tests transpiler support.

### G6.MISC.rand — Randomized expressions
- [ ] **1. Baseline** — randexpr.
- [ ] **2. Fix** (pre-test first).
- [ ] **3. Commit**.

---

## N/A / Deferred decisions in this pass

| Package | Verdict | Reason |
|---------|---------|--------|
| corruptA–corruptN | N/A | Corruption detection (added to NOT_APPLICABLE.md) |
| dbfuzz | N/A | Fuzz testing (added to NOT_APPLICABLE.md) |
| autovacuum_ioerr | N/A | Fault injection (ioerr) |
| snapshot, snapshot_ | Deferred | WAL snapshot C API (vacuous pass; tracked in DEFERRED.md) |

## Root-cause fixes

### G6.MISC.1 — ALTER TABLE ADD COLUMN defaults + INT64_MIN literals (2026-08-XX)
**Fixes**:
- `internal/exec/select.go`: `applyColumnDefaults` fills DEFAULT values for
  columns absent from stored records (rows written before ADD COLUMN), with
  column affinity applied; wired into `fillStructRowFromTypes`,
  `fillStructRowRemainingFromTypes`, `buildRowMap`.
- `internal/util/compare.go`: `applyTextAffinity` keeps ".0" for whole floats
  (-123.0 → "-123.0", matching SQLite) unless out of int64 range.
- `internal/parse/parser.go`: fold `-9223372036854775808` into math.MinInt64 in
  rules 216 (unary minus) and 36 (DEFAULT MINUS term).
- `internal/util/varint.go`: fix 9-byte varint encoding/decoding for values
  >= 2^56 (SQLite uses 8×7 bits + 8 bits; old encoder wrote 10 bytes and
  decoded 2^63 as 32768). `ReadVarint` updated to match.

**Packages flipped**: tkt_8454a207b PASS (was FAIL on 8 result mismatches).
**Pre-test**: `frigolite_p6_misc_test.go` `TestP6_AlterAddColumnDefault`.
**No regressions**: internal/util, storage, btree, exec unit suites green;
null/expr/affinity/intpkey fail identically before and after (pre-existing).

### G6.MISC.2 — Bitwise operators | << >> (2026-08-XX)
**Fixes**:
- `internal/sql/lexer.go`: added TokenBitOr/TokenLShift/TokenRShift; `readPipeOp`
  returns TokenBitOr for single `|`, `readLtOp`/`readGtOp` recognize `<<`/`>>`.
- `internal/parse/token.go`: map new TokenTypes to TK_BITOR/TK_LSHIFT/TK_RSHIFT.
- `internal/exec/expression.go`: evaluate `|`, `<<`, `>>` (bitwiseOr,
  shiftLeft, shiftRight) with SQLite integer semantics.

**Packages flipped**: randexpr no longer fails on parse (`parse error: near "|"`);
remaining randexpr mismatches are separate expression-evaluation bugs.
**Pre-test**: `TestP6_BitwiseOperators`.
**No regressions**: internal/sql, parse, exec suites green; select1/insert pass.

### G6.MISC.3 — changes() function, journal_mode pragma, recursive CTE limit (2026-08-XX)
**Fixes**:
- `internal/exec/engine.go`: only update `lastChanges` for INSERT/UPDATE/DELETE
  (SELECT/DDL no longer reset the changes() counter, matching SQLite).
- `internal/exec/pragma.go`: `PRAGMA journal_mode = X` returns the resulting
  mode (e.g. "off"); added `PRAGMA recursive_cte_limit` support.
- `internal/exec/select.go` + engine: recursive CTE limit is configurable,
  default 100000 (SQLite test builds) instead of hardcoded 1000.
- `internal/function/function.go`: added OCTET_LENGTH scalar.

**Packages flipped**: changes PASS (was FAIL on journal_mode + CTE + changes()).
**N/A added**: changes2 (C API prepare/step/finalize + update hooks).
**Pre-test**: `TestP6_ChangesRecursiveCTE`.
**No regressions**: internal/exec suite green.

### G6.MISC.4 — REAL-to-text affinity comparison (2026-08-XX)
**Fixes**:
- `internal/util/compare.go` `formatNumeric`: whole REAL values format with
  ".0" (2.0 → "2.0", not "2") matching SQLite. Previously 2.0 == '2' under
  TEXT affinity comparisons, so `WHERE a=2.0` matched the row with a='2'.

**Packages flipped**: indexA affinity mismatches gone (1 remaining validation
error: partial-index COLLATE), seekscan PASS, affinity/coveridxscan/skipscan/
descidx/numindex/expridx reduced to fewer distinct failures.
**Pre-test**: `TestP6_TextAffinityFloatCompare`.
**No regressions**: internal/util, exec suites green; types/literal/select1/insert pass.

### G6.MISC.5 — Aggregate functions rejected in DEFAULT (2026-08-XX)
**Fixes**:
- `internal/exec/ddl.go`: CREATE TABLE validates column DEFAULT expressions
  reject aggregate functions with "unknown function: <name>()" (SQLite
  build.c semantics). Uses the existing findAggregateInExpr walker.

**Packages flipped**: table's 5 aggregate-in-DEFAULT errors gone (remaining
table failures are TCL format-string transpiler interpolation, not engine bugs).
**N/A added**: notify, avtrans, incrblob, incrblob_ (C API blob/update-hook
handles). changes2 generated test removed from changes package.
**Pre-test**: `TestP6_AggregateInDefault`.
**No regressions**: internal/exec suite green; types/literal/select1/insert/changes pass.

### G6.MISC.6 — rowid rejected in table-level UNIQUE/PRIMARY KEY (2026-08-XX)
**Fixes**:
- `internal/exec/ddl.go`: CREATE TABLE rejects rowid/_rowid_/oid in table-level
  UNIQUE and PRIMARY KEY constraints ("no such column: rowid"), matching SQLite.

**Packages flipped**: unique PASS (was FAIL on 2 validation errors).
**Pre-test**: `TestP6_RowidInTableConstraint`.
**No regressions**: internal/exec suite green; types/literal/select1/insert pass.

### G6.MISC.7 — VACUUM / REINDEX [schema.]name / DETACH parser shims (2026-XX-XX)
**Root cause**: The LALR grammar productions ALREADY exist in the generated
tables — rule 249 `cmd ::= VACUUM into_opt`, rule 285
`cmd ::= DETACH database_kw_opt expr`, rule 289 `cmd ::= REINDEX nm dbnm` —
but had no `handleRule` case, so the generic passthrough returned a non-Stmt
value and the statement was silently dropped ("no statements parsed").
Rule 288 `cmd ::= REINDEX` (bare) was already handled; bare VACUUM was NOT.

**Fixes** (in `internal/parse/parser.go` handleRule — no LALR table regeneration
needed, no pre-parse shim required):
- `case 249`: `&sql.VacuumStmt{}` — bare `VACUUM` and `VACUUM INTO <file>`.
- `case 289`: `&sql.ReindexStmt{}` — `REINDEX t1`, `REINDEX main.t1`, etc.
  (name/dbnm not retained; exec handler is a no-op).
- `case 285`: `&sql.AttachStmt{IsDetach: true, Schema: <name>}` — `DETACH aux;`
  and `DETACH DATABASE aux2;`. The old comment on rule 284 claiming it "also
  handles DETACH" was wrong; DETACH is a separate production.

**Packages flipped**: reindex PASS (was 22 stmts failing), tkt_c48d99d PASS
(bare VACUUM), descidx PASS (bonus — 6 VACUUM uses), exclusive partial
(DETACH parse error fixed; 3 remaining failures are the pager
"file already closed" bug, a separate root cause).

**Investigation (DETACH)**: rule 285 confirmed as the DETACH production;
parse fixed. The exclusive package still fails on `pager: write page N:
write test.db: file already closed` (pager lifecycle bug after journal
operations in EXCLUSIVE locking mode) — separate root cause, not parser.

**Pre-test**: `TestP6_VacuumReindex` (RED then GREEN).
**No regressions**: internal/... suite green; parse grammar coverage,
rule inventory, SOLID architecture tests green.
**Not in scope**: ANALYZE nm/dbnm (rule 291) still drops named ANALYZE —
separate batch goal; analyze package fails on sqlite_stat1 population, not parse.

### G6.MISC.8 — Row-value semantics + transpiler [set]/[list]/[subst -novar] (2026-XX-XX)
**Root causes** (3 distinct):
1. **Transpiler command substitution gaps** (`tools/tcl2go/gen.go`): `[set var]`
   and `[list $var]` inside string interpolation fell to cmdExpr's default
   (literal text), producing `"set op"` / `"list $eq"` in generated SQL.
   `[subst -novar {...}]` (rowvalue2, distinct) kept the `-novar {` wrapper
   and didn't do the right $var/[cmd] split. Added `case "set"` (→ tclVarToGo),
   `case "list"` (→ element value), `subst` flag parsing with a hybrid
   `renderSubstNovarSQL` ($var → sqlLiteral, [cmd] → raw), `dbEvalExpected`
   support for `[db eval [subst -novar {...}]]`, and do_catchsql_test dynamic
   error-message detection (`"1 " + $var` form). Fixed 3600 "near -" parse
   errors in rowvalue2 and affected 41+ TCL files using `[set ...]`.
2. **Row-value engine semantics** (`internal/exec/expression.go`): RowValue
   eval now returns `[]interface{}`; `evalRowValueCompare` implements
   lexicographic per-element comparison with arity checks and NULL handling;
   `evalRowValueIs` for NULL-safe row IS/IS NOT; `evalInList` handles row-value
   IN (arity errors, scalar-vs-row misuse, subquery rows via evalSubqueryRows);
   row-value-vs-subquery comparisons evaluate the subquery's full row.
3. **Compile-time row-value validation** (`internal/exec/select.go`):
   `validateRowValueUse` raises "row value misused" for bare row values in
   SELECT/LIMIT/ORDER BY, scalar-vs-row comparisons, row-value function args;
   `validateSubqueryArity` (with `SELECT *` resolution) raises
   "sub-select returns N columns - expected M" for row-value IN subqueries.
4. **Parser/join fixes**: `CROSS` keyword was missing from `keywordToCode`
   (mapped to TK_ID, so `FROM x2 CROSS JOIN x1` parsed as alias "CROSS");
   added `case "CROSS": TK_JOIN_KW`. structRow fast path now applies column
   collations (collatedValue) matching buildRowMap; `SELECT *` output unwraps
   collatedValue; `compareValuesWithCollate` implements SQLite's
   left-operand-collation rule (a column on the left masks a right collation).

**Packages flipped**: rowvalueA PASS (was FAIL on 10 IN/collation/comparison
errors). rowvalue reduced from ~3900 failures to ~121 edge cases (rowvalue2
bare-REINDEX+SELECT flow passes; remaining are UPDATE row-value assignment
"2 columns assigned 3 values", collation-in-rowvalue "no such collation
sequence: nose", nested subquery edge cases, and transpiler `make_expr2`
interpolation).

**Pre-test**: `TestP6_RowValueComparison` (written after the fix; the fixes
were developed against the testgen failures and verified by the pre-test).
**No regressions**: internal/... suite green; root-package failure set
identical to baseline (pre-existing FTS/DDL failures unchanged).

## Current status (end of this goal's budget)

**Progress**: 8 commits (G6.MISC.1..G6.MISC.8), 7 root-cause fixes,
several packages flipped to PASS (tkt_8454a207b, changes, seekscan, reindex,
tkt_c48d99d, descidx, rowvalueA, partial: indexA affinity, randexpr parse,
table aggregate validation, affinity/numindex, unique).

**N/A documented this pass**: corruptA–corruptN, dbfuzz, autovacuum_ioerr,
changes2, notify, avtrans, incrblob, incrblob_ (all C API / fault injection /
fuzz). 144 packages remain in PACKAGES_TIER6C.txt.

**Still failing (~80 packages)** — grouped root causes, each needs its own
sub-goal (per P6_TRIAGE these were meant to be batch goals):
1. **Query planner**: coveridxscan, skipscan, descidx, bloom, tkt_78e04e52 —
   covering index / index-scan selection, EXPLAIN QUERY PLAN output.
2. **Trigger evaluation**: tkt_7bbfb7d, tkt_3a77c9714, tkt_a7b7803,
   tkt_a7debbe, tkt_80ba, tkt_80e031a00, tkt_bdc6bbbb, tkt_d82e3f —
   correlated subqueries in triggers, AFTER INSERT row updates.
3. **FK semantics**: tkt_b1d3a2e (DROP TABLE with deferred children),
   tkt_f3e5abed (attached db reuse).
4. **Validation errors not raised**: aggorderby (misuse of aggregate),
   tkt_4ef7e3 (no such column in trigger), tkt_385a5b56b, tkt_9f2eb3,
   indexA (partial-index COLLATE), parser (FK COLLATE in column list).
5. **Parser grammar gaps**: DELETE/UPDATE ORDER BY + LIMIT (wherelimit),
   ~~VACUUM~~ (G6.MISC.7), ~~REINDEX with name~~ (G6.MISC.7),
   rowvalue `-novar` (transpiler), trailing comma
   in PK (tkt_9f2eb3), `(a,b) IN (SELECT ...)` rowvalue forms.
6. **Test-harness functions**: test_eval, int2str, hex_to_utf16be/le,
   val, my_changes, f2/f3, pragma_stats — SQLite test-only functions.
7. **TCL transpiler interpolation**: `[format %5d [expr $i*2]]` literals in
   tkt1567/table/etc. — tcl2go does not evaluate command substitution.
8. **JSON operators** (->>): tkt_99378177930f87 — JSON not supported (N/A).
9. **Timeouts**: emptytable, incrvacuum, rowid, tkt_d11f09d36, func_pkg.

**Verify command**: `bash scripts/verify_all_applicable.sh` — currently exits 1
(remaining failures). Ticked checkboxes below reflect completed sub-group work.

## Sub-group checkboxes

- [x] **G6.MISC setup** — P6_APPLICABLE.md, PACKAGES_TIER6C.txt, verify script.
- [x] **Root-cause fixes** — G6.MISC.1..G6.MISC.5 (see above).
- [x] **G6.MISC.7** — VACUUM / REINDEX [schema.]name / DETACH parser shims
      (reindex, tkt_c48d99d, descidx PASS; exclusive DETACH parse fixed).
- [x] **G6.MISC.8** — row-value semantics + transpiler [set]/[list]/[subst-novar]
      (rowvalueA PASS; rowvalue ~3900 → ~121 edge cases; CROSS JOIN parse fixed).
- [ ] **G6.MISC.tkt** — 22/60 tkt packages still failing (planner/trigger/FK).
- [ ] **G6.MISC.index** — coveridxscan, skipscan, bloom still failing
      (descidx flipped to PASS via G6.MISC.7 VACUUM fix).
- [ ] **G6.MISC.rowvalue** — rowvalue still failing (~121 edge cases:
      UPDATE row-value assignment, collation-in-rowvalue, nested subqueries,
      make_expr2 transpiler; rowvalueA flipped to PASS via G6.MISC.8).
- [ ] **G6.MISC.misc** — ~40 general SQL packages still failing.
- [ ] **G6.MISC.bigdata** — widetab failing (result mismatch).
- [ ] **G6.MISC.incr** — incrvacuum timeout; autovacuum failing.
- [ ] **G6.MISC.e_tests** — e_, e_select need do_select_tests transpiler work.
