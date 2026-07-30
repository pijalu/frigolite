# Task 1.1 — Type affinity, NULL handling, comparison

> **Phase**: 1 — Fix Engine Bugs
> **Status**: 🔲 Not started
> **Files**: `internal/exec/expression.go`, `internal/exec/engine.go`, `internal/util/compare.go`, `internal/value/affinity.go`
> **SQLite ref**: `src/vdbemem.c` (sqlite3VdbeMemApplyAffinity), `src/vdbe.c` (Ne/Eq/Lt opcodes)
> **Estimated**: 2-3 sessions

## Description

Fix type affinity on INSERT/UPDATE, NULL propagation in expressions, REAL vs
INTEGER comparison semantics, and COLLATE clause handling. These are the most
common source of result mismatch errors (~4000+ occurrences).

## Steps

- [ ] Run failure baseline: `go test ./testgen/e_* ./testgen/expr* ./testgen/affinity* -count=1`
- [ ] **Fix blob arithmetic negation**: `SELECT -x'ce'` → `0`. In unary minus handler,
      if operand is `[]byte`, convert to float64 (empty/non-numeric → 0), return negated.
- [ ] **Fix REAL vs large-INTEGER comparison**: `3175546974276630385 < 3175546974276630385.0`
      → `1` (true). Convert int64 to float64 before comparing (matches SQLite `double`).
      File: `internal/util/compare.go`.
- [ ] **Fix affinity on INSERT**: Create `applyAffinity(value, affinity)` in new
      `internal/util/affinity.go`. Apply column affinity on INSERT/UPDATE:
      TEXT → `fmt.Sprintf`, INTEGER → try int64 conversion, REAL → float64,
      NUMERIC → try int64 then float64 then text, BLOB/NONE → no conversion.
- [ ] **Fix `typeof()` after affinity**: Must report storage type, not input type.
      `nil`→`"null"`, `int64`→`"integer"`, `float64`→`"real"`, `string`→`"text"`, `[]byte`→`"blob"`.
- [ ] **Fix NULL propagation**: `NULL + 1`→NULL, `NULL = 1`→NULL (not 0),
      `IS NULL`→1, `IS NOT NULL`→0. Arithmetic → nil. Comparison → nil.
      `AND`/`OR` → three-valued logic. WHERE treats NULL as false.
- [ ] **Fix COLLATE clause**: `ORDER BY col COLLATE nocase` — implement NOCASE, BINARY, RTRIM.
- [ ] Verify: result mismatch errors drop by ≥50%.
- [ ] **Commit** with message: `P1.1: fix type affinity, NULL handling, comparison, COLLATE`

## Verification

```bash
go test ./testgen/e_* ./testgen/expr* ./testgen/affinity* -count=1
# Measure: result mismatch count before vs after
```

## Session notes

- Started:
- Completed:
- Fixes applied:
- Result mismatch count before:
- Result mismatch count after:
- Blockers:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
