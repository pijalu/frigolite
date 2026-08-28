# G02 — Type System & Affinity

> **Prerequisite**: G01 (engine decomposed; `internal/value/` package exists).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/expr.c`, `vdbe.c`, `vdbeapi.c`.
> **Goal**: Implement correct SQLite type affinity, NULL semantics, and blob comparison. Fix all affinity-related test failures.

---

## Context

SQLite uses a **dynamic type system** with **column affinity**. This is fundamentally
different from most SQL databases. The key rules:

1. **Storage classes**: NULL, INTEGER, REAL, TEXT, BLOB — every value has exactly one.
2. **Column affinity**: TEXT, NUMERIC, INTEGER, REAL, BLOB(NONE) — determined by column
   type declaration. Affinity controls how values are coerced when stored or compared.
3. **Comparison rules**: Values are compared by type. TEXT affinity columns compare as text;
   NUMERIC/INTEGER/REAL columns coerce before comparison. NULL is special (not equal to
   anything, including itself).

Frigolite's current type handling is ad-hoc — coercion happens inconsistently across the
engine, causing affinity-sensitive tests to fail.

---

## Current State

After G01, `internal/value/` package has the type model. The remaining issues:

### Affinity bugs
- `affinity2.test`: type coercion in expressions with mixed affinity columns
- `affinity3.test`: column affinity in JOIN conditions
- `cast.test`: CAST expressions don't always apply correct affinity
- `tkt3457.test`, `tkt3733.test`: specific affinity edge cases

### NULL handling bugs
- NULL comparison: `NULL = NULL` → NULL (not true), `NULL IS NULL` → 1
- NULL in arithmetic: `NULL + 1` → NULL
- NULL in aggregates: `SUM`, `AVG`, `COUNT` handle NULL differently

### Blob comparison bugs
- `x'4142' = x'4142'` should be true (binary comparison)
- `x'4142' < x'4143'` should be true (byte-by-byte comparison)
- TEXT vs BLOB comparison: BLOB has higher type precedence than TEXT

---

## SQLite Reference

### Type affinity rules (`vdbe.c:ApplyAffinity`)
```
Column type declaration → Affinity:
  "INT"                    → INTEGER
  "CHAR", "CLOB", "TEXT"   → TEXT
  "BLOB", or no type       → BLOB (NONE)
  "REAL", "FLOA", "DOUB"   → REAL
  everything else          → NUMERIC

Affinity coercion on insert/compare:
  TEXT affinity:   INTEGER/REAL → TEXT if lossless
  NUMERIC affinity: TEXT → INTEGER or REAL if possible
  INTEGER affinity: REAL → INTEGER if lossless (e.g., 1.0 → 1)
  REAL affinity:   INTEGER → REAL
  BLOB affinity:   no coercion
```

### Comparison precedence (`vdbe.c:sqlite3MemCompare`)
```
1. If both are NULL: equal
2. NULL < everything else
3. Both numbers: numeric comparison
4. TEXT vs TEXT: string comparison (with collation)
5. BLOB vs BLOB: byte comparison
6. TEXT vs BLOB: TEXT < BLOB (type precedence)
```

---

## Implementation Steps

### Step 1: Verify `internal/value/` affinity implementation

After G01, `internal/value/` should have `ColumnAffinity(name) Affinity` and
`ApplyAffinity(val, aff) interface{}`. Verify these are complete:

```go
// internal/value/affinity.go

// Affinity represents a SQLite column affinity.
type Affinity int
const (
    AffinityBlob Affinity = iota
    AffinityText
    AffinityNumeric
    AffinityInteger
    AffinityReal
)

// ColumnAffinity determines affinity from a column type declaration string.
// Reference: SQLite vdbe.c:sqlite3TableColumnAffinity / build.c
func ColumnAffinity(typeName string) Affinity { ... }

// ApplyAffinity coerces a value according to affinity rules.
// Reference: SQLite vdbe.c:ApplyAffinity
func ApplyAffinity(val interface{}, aff Affinity) interface{} { ... }
```

**SQLite source**: `/Users/muaddib/dev/sqlite/src/insert.c` function
`sqlite3TableColumnAffinity()` and `/Users/muaddib/dev/sqlite/src/vdbe.c` function
`applyAffinity()` / `sqlite3VdbeMemApplyAffinity()`.

**Verify**:
```bash
go test -v -count=1 ./internal/value/... -run Affinity
```
**Expected outcome**: `internal/value` affinity tests pass — ColumnAffinity correctly maps
type names to affinities (INT→Integer, TEXT→Text, BLOB→Blob, etc.).

### Step 2: Fix value comparison ordering

**File**: `internal/value/compare.go`

Ensure `Compare(a, b interface{}) int` follows SQLite's comparison precedence exactly:

```go
func Compare(a, b interface{}) int {
    aNull := a == nil
    bNull := b == nil
    if aNull && bNull { return 0 }
    if aNull { return -1 }
    if bNull { return 1 }

    // Unwrap column affinity wrappers
    a = unwrap(a)
    b = unwrap(b)

    aNum, aIsNum := asNumber(a)
    bNum, bIsNum := asNumber(b)
    if aIsNum && bIsNum {
        return cmpFloat(aNum, bNum)
    }

    aText, aIsText := asText(a)
    bText, bIsText := asText(b)
    if aIsText && bIsText {
        return strings.Compare(aText, bText) // with collation if applicable
    }

    aBlob, aIsBlob := asBlob(a)
    bBlob, bIsBlob := asBlob(b)
    if aIsBlob && bIsBlob {
        return bytes.Compare(aBlob, bBlob)
    }

    // Type precedence: NULL < numbers < text < blob
    return typePrecedence(a) - typePrecedence(b)
}
```

**SQLite source**: `/Users/muaddib/dev/sqlite/src/vdbe.c` function
`sqlite3MemCompare()` (approximately line 1850+).

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/(affinity2|affinity3|cast)" . 2>&1 | grep -c "FAIL"
```
**Expected outcome**: FAIL count decreases for affinity2, affinity3, and cast tests. Type
precedence ordering NULL<number<text<blob is correct.

### Step 3: Fix NULL semantics in expression evaluation

**File**: `internal/exec/eval.go` (after G01)

Three-valued logic (Kleene):
- `NULL AND x` → NULL if x is true; false if x is false
- `NULL OR x` → NULL if x is false; true if x is true
- `NULL = x` → NULL (for any x)
- `NULL + x` → NULL
- `NULL IS NULL` → 1 (true)
- `NULL IS NOT NULL` → 0 (false)
- `x IS NULL` → 1 if x is NULL, else 0

Verify `kleeneAnd`, `kleeneOr` (from G01 extraction) implement this correctly.

**SQLite source**: `/Users/muaddib/dev/sqlite/src/expr.c` function
`sqlite3ExprIfTrue()` / `sqlite3ExprIfFalse()`.

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/null" .
```
**Expected outcome**: NULL handling tests pass — three-valued logic works correctly
(NULL AND x, NULL OR x, NULL = x all produce correct results).

### Step 4: Fix blob value handling

**File**: `internal/value/compare.go`, `internal/exec/eval.go`

Issues:
- Blob values stored as `[]byte` must be compared byte-by-byte, never as strings
- `typeof(x'41')` → `"blob"`
- `length(x'4142')` → `2` (not 8)
- Blobs in ORDER BY: sorted by byte value
- CAST to BLOB: numeric → big-endian bytes, text → UTF-8 bytes

**SQLite source**: `/Users/muaddib/dev/sqlite/src/func.c` function `typeofFunc()`.

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/(blob|hexlit)" .
```
**Expected outcome**: Blob tests pass — typeof(x'41')=blob, length(x'4142')=2, byte-by-byte
comparison works.

### Step 5: Fix CAST expressions

**File**: `internal/exec/eval.go`

SQLite CAST rules (`CAST(value AS type)`):
- `CAST(x AS INTEGER)`: truncates REAL toward zero; parses TEXT prefix as number; BLOB → error
- `CAST(x AS REAL)`: converts INTEGER to REAL; parses TEXT as number
- `CAST(x AS TEXT)`: number → decimal string; BLOB → hex string interpretation
- `CAST(x AS BLOB)`: number → big-endian bytes; TEXT → UTF-8 bytes
- `CAST(x AS NUMERIC)`: INTEGER or REAL, whichever is lossless

**SQLite source**: `/Users/muaddib/dev/sqlite/src/expr.c` function `sqlite3ExprCast()`.

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/cast" .
```
**Expected outcome**: CAST tests pass — CAST(x AS INTEGER), CAST(x AS REAL), CAST(x AS TEXT),
CAST(x AS BLOB) all produce correct type conversions.

### Step 6: Fix `randomblob` and `zeroblob`

**File**: `internal/function/function.go`

```go
// randomblob(N) returns an N-byte blob filled with random data.
// zeroblob(N) returns an N-byte blob filled with zero bytes.
```

These are currently in the `knownUnsupported` list. Implement them and remove from the list.

**SQLite source**: `/Users/muaddib/dev/sqlite/src/func.c` function `randomBlob()`.

**Verify**:
```bash
go test -count=1 -run "^TestSQLiteSuite/func" .
```
**Expected outcome**: randomblob/zeroblob tests pass — length(zeroblob(10))=10,
typeof=randomblob=n=blob. No SKIP for these functions.

---

## Files Modified

| File | Change |
|------|--------|
| `internal/value/affinity.go` | Verify/complete affinity rules |
| `internal/value/compare.go` | Fix comparison precedence and NULL ordering |
| `internal/exec/eval.go` | Fix NULL three-valued logic, CAST expressions |
| `internal/function/function.go` | Implement `randomblob`, `zeroblob` |
| `frigolite_test.go` | Remove `randomblob`/`zeroblob` from `knownUnsupported` |

---

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. Affinity tests pass
go test -count=1 -run "^TestSQLiteSuite/affinity" .

# 2. CAST tests pass
go test -count=1 -run "^TestSQLiteSuite/cast" .

# 3. NULL handling tests pass
go test -count=1 -run "^TestSQLiteSuite/null" .

# 4. Blob comparison tests pass
go test -count=1 -run "^TestSQLiteSuite/(blob|hexlit)" .

# 5. No new failures elsewhere
make quality
go test -run TestSOLID_ ./...

# 6. randomblob/zeroblob no longer skipped
go test -v -count=1 -run "^TestSQLiteSuite/func" . 2>&1 | grep -c "randomblob.*SKIP"
# Should be 0
```
