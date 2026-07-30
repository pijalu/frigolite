# PLAN-P1 — Type System, Affinity & NULL Handling

> **Prerequisite**: P0 (test infrastructure must be fixed so affinity tests are verified).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/`
>   - `sqlite3VdbeMem` / affinity application: `src/vdbemem.c`
>   - Type affinity rules: `src/build.c` (function `sqlite3AffinityType`)
>   - Comparison: `src/vdbemem.c` (function `sqlite3MemCompare`)
>   - NULL semantics: `src/where.c`, `src/vdbe.c`
> **Goal**: Correct column affinity, NULL propagation, and blob/text comparison.

## Scope

~7 known failures:
- `affinity2`: 5 (NULL representation, blob negation, REAL comparison)
- `atomic2`: 2 (transaction/atomicity edge cases)

After P0 surfaces hidden errors, more affinity-related failures may appear in the
compat tests. This phase fixes all type-system bugs.

## Current State

Frigolite stores values as `interface{}`. Column affinity is partially implemented:
- `INTEGER` affinity coerces text that looks like an integer.
- `REAL` affinity coerces to float64.
- `TEXT` and `BLOB` affinity are partially handled.

**Key gaps:**
1. NULL is stored as Go `nil` but comparison semantics differ from SQLite.
2. Blob negation (`-x'ce'`) returns wrong type (should be `0` integer).
3. REAL-vs-INTEGER comparison can produce wrong ordering for large integers.
4. Column affinity is not applied to DEFAULT values or computed expressions.
5. The `typeof()` function may report wrong types after affinity coercion.

## SQLite Affinity Rules (Canonical Reference)

From SQLite docs and `src/build.c`:

| Declared type contains | Affinity | Rule |
|------------------------|----------|------|
| "INT" | INTEGER | Text that looks like an integer → integer. Otherwise text. |
| "CHAR", "CLOB", "TEXT" | TEXT | All values → text. |
| "BLOB" or no type | BLOB (NONE) | No conversion. |
| "REAL", "FLOA", "DOUB" | REAL | Integer → real. Text that looks real → real. |
| (anything else) | NUMERIC | Text → integer or real if possible, else text. |

**NUMERIC affinity details** (`src/build.c:sqlite3AffinityType`):
- If text is a valid integer → INTEGER.
- If text is a valid float → REAL.
- If text has leading zeros after conversion → TEXT retains original form, but
  comparison uses numeric value.

**Key comparison rule** (`src/vdbemem.c:sqlite3MemCompare`):
1. If one side is NULL → result is NULL (for `=`) or determined by `IS`/`IS NOT`.
2. If both are INTEGER or both REAL → numeric comparison.
3. If one is INTEGER/REAL and the other TEXT/NUMERIC → NUMERIC comparison
   (text is converted to number).
4. If both are TEXT → text comparison (by collation).
5. If one or both are BLOB → BLOB comparison (memcmp).
6. Affinity is applied BEFORE comparison based on the expression's declared affinity.

## Implementation Steps

### Step 1: Fix blob arithmetic negation

**Problem:** `SELECT -x'ce'` should return `0` (integer), but frigolite may error
or return a wrong type.

**SQLite behavior** (`src/expr.c`): Negating a BLOB converts it to numeric (0 for
non-numeric blobs, or the numeric value if the blob is a valid number). The result
type is always numeric (integer or real).

**File:** `internal/exec/engine.go` — unary minus evaluation.

**Fix:**
1. In the unary minus handler, if the operand is `[]byte` (blob):
   - Attempt to convert to float64. If the blob is empty or non-numeric → 0.
   - Return `-value` as int64 (if integral) or float64.
2. Same logic applies to `+x'ce'` and `+-+x'ce'` (chained unary operators).

**Verify:**
```bash
cd /Users/muaddib/dev/frigolite
# These should match SQLite output
echo "SELECT quote(-x'ce')" | sqlite3  # Expected: 0
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/501' . 2>&1 | grep -c FAIL
```

### Step 2: Fix REAL vs large-INTEGER comparison

**Problem:** `SELECT 3175546974276630385 < c0` where `c0` is `3175546974276630385.0`
(REAL) should return `1` (true), because REAL can't represent the exact integer and
the REAL value is slightly larger.

**SQLite behavior**: When comparing INTEGER to REAL, SQLite converts the integer to
REAL. Since `3175546974276630385` exceeds `2^53` (float64 precision), the REAL
representation is `3175546974276630400.0`, which IS larger than the integer.

**File:** `internal/exec/engine.go` — comparison evaluation; `internal/util/compare.go`.

**Fix:**
1. In the comparison function, when comparing `int64` vs `float64`:
   - Convert `int64` to `float64` and compare.
   - This matches SQLite's behavior (which uses `double` for the comparison).
2. Do NOT special-case large integers — `float64(int64Value)` is the correct
   conversion.

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/500' . 2>&1 | grep -c FAIL
```

### Step 3: Fix affinity application on INSERT

**Problem:** When inserting values, frigolite may not apply column affinity correctly.
For example, inserting `'03'` into a REAL column should store `3.0`, not `'03'`.

**SQLite behavior**: Affinity is applied at INSERT time. The value is converted
according to the column's declared affinity before being stored.

**File:** `internal/exec/engine.go` — INSERT execution; column affinity lookup.

**Fix:**
1. Create a function `applyAffinity(value interface{}, affinity string) interface{}`
   in `internal/util/affinity.go` (new file).
2. On INSERT/UPDATE, look up the target column's affinity and apply it.
3. Affinity rules (from the table above):
   - TEXT: `fmt.Sprintf("%v", value)` (but only if not already text).
   - INTEGER: try to convert text to int64; if lossless, store int64.
   - REAL: convert to float64.
   - NUMERIC: try int64 first, then float64, else keep text.
   - BLOB/NONE: no conversion.

**Reference**: `/Users/muaddib/dev/sqlite/src/vdbemem.c` — function
`sqlite3VdbeMemApplyAffinity`.

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLite_affinity2$' . 2>&1 | grep -c FAIL
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/' . 2>&1 | grep -c FAIL
```

### Step 4: Fix `typeof()` after affinity

**Problem:** `typeof()` should report the STORAGE type, not the input type. After
affinity coercion, `typeof(xi)` for an INTEGER column with input `'2'` should return
`'integer'`, not `'text'`.

**File:** `internal/exec/engine.go` — `typeof()` function implementation.

**Fix:** `typeof()` should inspect the actual stored value type:
- `nil` → `"null"`
- `int64` → `"integer"`
- `float64` → `"real"`
- `string` → `"text"`
- `[]byte` → `"blob"`

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLite_affinity2$' . 2>&1 | grep "typeof"
# Should show no mismatches
```

### Step 5: Fix NULL propagation in expressions

**Problem:** NULL should propagate through arithmetic and comparison:
- `NULL + 1` → NULL
- `NULL = 1` → NULL (not 0)
- `NULL IS NULL` → 1
- `NULL IS NOT NULL` → 0

**File:** `internal/exec/engine.go` — expression evaluation.

**Fix:**
1. In binary expression evaluation, if either operand is nil (NULL):
   - For arithmetic (`+`, `-`, `*`, `/`): return nil.
   - For comparison (`=`, `<`, `>`, etc.): return nil (not false).
   - For `IS`/`IS NOT`: compare directly (`nil IS nil` → 1, `nil IS 1` → 0).
   - For `AND`/`OR`: use three-valued logic.
2. In WHERE clause filtering, NULL is treated as false (row excluded).

**Reference**: `/Users/muaddib/dev/sqlite/src/vdbe.c` — opcodes `Ne`, `Eq`, `Lt`, etc.
handle NULL via the `SQLITE_NULLEQ` flag.

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/' . 2>&1 | grep -c FAIL
# Should be 0 after Steps 1–5
```

### Step 6: Fix `atomic2` failures

**Problem:** Transaction atomicity edge cases.

**Investigate:** Run the failing tests and examine the specific assertions:
```bash
go test -v -count=1 -run '^TestSQLiteSuite/atomic2/' . 2>&1
```

**Likely issues:**
- BEGIN/COMMIT/ROLLBACK not properly isolating changes.
- Error during transaction should roll back all changes.
- Nested transactions (savepoints) not handled.

**File:** `internal/exec/engine.go` — transaction handling.

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/atomic2/' . 2>&1 | grep -c FAIL
```

## Files Modified

| File | Change |
|------|--------|
| `internal/util/affinity.go` (NEW) | `applyAffinity()`, affinity type classification |
| `internal/util/compare.go` | Fix int64/float64 comparison |
| `internal/exec/engine.go` | Apply affinity on INSERT/UPDATE; fix typeof; NULL propagation; blob arithmetic |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -count=1 -run '^TestSQLiteSuite/atomic2/' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -count=1 -run '^TestSQLite_affinity2$' . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
make quality
go test -run TestSOLID_ ./...
```
