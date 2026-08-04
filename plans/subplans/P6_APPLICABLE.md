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

### G6.MISC.4 — (next root cause)

## Verify

```bash
bash scripts/verify_all_applicable.sh
```

Requires: `plans/PACKAGES_TIER6C.txt` (space-separated, one per line or space-separated),
`scripts/verify_all_applicable.sh` (per-package `go test -tags testgen` with timeout,
then SOLID check).
