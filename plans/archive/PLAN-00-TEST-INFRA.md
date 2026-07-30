# PLAN-P0 — Test Infrastructure Overhaul

> **⚠️ DEPRECATED APPROACH**: This plan describes the old test harness infrastructure based on JSON test data (`testdata/*.json`) and the Python converter (`convert_compat_json.py`). The project now uses the **tcl2go** pipeline (Go TCL interpreter → Go test files in `testgen/`). See [`PLAN.md`](./PLAN.md) for the current strategy.
>
> **Prerequisite**: None. This is the first phase — it reveals the true failure surface.
> **SQLite reference**: N/A (this is test-tooling work).
> **Goal**: Make the test harness and compat-test helpers *actually verify* results instead of silently swallowing errors.

## Why This Is First

Two helper bugs currently mask an unknown number of real failures:

### Bug 1: `checkQueryResult` silently swallows errors

File: `frigolite_test.go`, lines 27–49.

```go
func checkQueryResult(t *testing.T, res *Result, expected string) {
    t.Helper()
    if res.Error != nil {
        return  // ← ANY query error is silently treated as PASS
    }
    ...
}
```

**6 657** `_ = db.Query(...)` calls in `frigolite_sqlite_compat_test.go` never check
results at all. But even the 4 007 `checkQueryResult` calls that DO check are broken:
if the query errors (parse failure, no-such-table, type mismatch), the helper returns
immediately without comparing — a false PASS.

**Fix:** `checkQueryResult` must surface errors. But because the compat tests were
generated from TCL with lossy conversion, some queries legitimately cannot run (they
use features frigolite does not support yet). Blindly surfacing every error will
create thousands of new FAILs from tests that depend on later phases (P2–P9).

**Strategy:** Introduce an **expected-error allowlist** keyed by feature. The helper
gets a new parameter (or a companion function `checkQueryResultOrKnown`) that accepts
a *known-unsupported* set. As each later phase lands, entries are removed from the
allowlist until it is empty.

### Bug 2: NULL representation mismatch

- `flattenResult` (harness, line 220): NULL → `""` (empty string).
- `cleanExpected` (harness, line 282): `{}` → `"NULL"`.
- `parseTCLList` (compat helper, line 110): `{}` → `"NULL"`.

So the harness compares `""` (got) against `"NULL"` (want) and always FAILs on NULL
columns. Example: `affinity2/501` — `SELECT * FROM t0 WHERE ...` returns `-1 NULL`
but frigolite flattens NULL to `""`, giving `-1 ` which mismatches.

**Fix:** Standardise: NULL is represented as `"NULL"` everywhere (matching SQLite
CLI `.nullvalue NULL` and TCL `{}` semantics). Update `flattenResult` in the harness.

### Bug 3: `normalizeSQL` heuristic comparison is too loose

The harness `normalizeSQL` function strips trailing `)` characters, collapses
spaces around operators, and rewrites `TABLE (` to `TABLE(`. This can make
non-equivalent SQL match. It also fails on expected values that are not SQL (e.g.
EXPLAIN QUERY PLAN output, numeric results).

**Fix:** Make normalisation opt-in — only apply it when the expected value starts
with `CREATE` or `SELECT` (i.e. it's clearly SQL text). For all other values, do
exact comparison after NULL normalisation.

## Implementation Steps

### Step 1: Fix NULL representation in `flattenResult`

**File:** `frigolite_harness_test.go` — function `flattenResult` (line ~216).

**Change:**
```go
// Before:
if val == nil {
    parts = append(parts, "")
}
// After:
if val == nil {
    parts = append(parts, "NULL")
}
```

**Verify:**
```bash
cd /Users/muaddib/dev/frigolite
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/' . 2>&1 | grep -c "FAIL"
# Should drop from 4 to ≤2 (some affinity2 failures are real type bugs, fixed in P1)
```

### Step 2: Tighten `normalizeSQL` — only for SQL text

**File:** `frigolite_harness_test.go` — inside the `query` case (line ~141).

**Change:** Wrap the `normalizeSQL` fallback in a guard:
```go
// Only normalise when both sides look like SQL/DDL text.
isSQLLike := func(s string) bool {
    su := strings.ToUpper(strings.TrimSpace(s))
    return strings.HasPrefix(su, "CREATE ") || strings.HasPrefix(su, "SELECT ") ||
           strings.HasPrefix(su, "INSERT ") || strings.HasPrefix(su, "ALTER ") ||
           strings.HasPrefix(su, "WITH ") || strings.HasPrefix(su, "TRIGGER ")
}
if isSQLLike(got) && isSQLLike(want) {
    if normalizeSQL(got) != normalizeSQL(want) {
        t.Errorf(...)
    }
} else {
    t.Errorf(...)  // exact comparison
}
```

**Verify:**
```bash
go test -v -count=1 -run '^TestSQLiteSuite/altertab3/2.1' . 2>&1 | head -10
```

### Step 3: Introduce known-unsupported error allowlist for compat tests

**File:** `frigolite_test.go` — new function alongside `checkQueryResult`.

**Design:**
```go
// knownUnsupportedPatterns lists (regex → reason) for queries that are
// expected to fail until a later phase implements the feature.
// As phases P2–P9 land, entries are removed. The goal is an empty list.
var knownUnsupportedPatterns = []struct {
    pattern *regexp.Regexp
    reason  string
}{
    // P2: window functions
    {regexp.MustCompile(`(?i)\b(OVER|WINDOW|FILTER)\b`), "window functions (P2)"},
    // P2: CTE
    {regexp.MustCompile(`(?i)\bWITH\s+\w+.*AS\s*\(`), "CTE (P2)"},
    // P8: FTS
    {regexp.MustCompile(`(?i)\b(fts\d|MATCH)\b`), "FTS (P8)"},
    // ... add more as discovered
}

func isKnownUnsupported(sql string) bool {
    for _, p := range knownUnsupportedPatterns {
        if p.pattern.MatchString(sql) {
            return true
        }
    }
    return false
}
```

**Then update `checkQueryResult`:**
```go
func checkQueryResult(t *testing.T, res *Result, expected string) {
    t.Helper()
    if res.Error != nil {
        // If the query uses a known-unsupported feature, skip silently.
        // This will be tightened as features are implemented.
        // NOTE: expected is compared in the call site; we need the SQL.
        // For compat tests, we accept the error as "known" if isKnownUnsupported.
        return  // Keep returning for now — the SQL is not available here.
    }
    // ... existing comparison logic
}
```

**Important:** The compat tests call `checkQueryResult(t, db.Query(sql), expected)`.
The SQL is not passed to `checkQueryResult`. To make the allowlist work, either:
- (a) Change the function signature to `checkQueryResult(t, sql, res, expected)`, OR
- (b) Store the last SQL on the `Result` struct.

**Recommended:** Option (b) — add `SQL string` to `Result` and set it in `Query`.
This is a one-line change in `frigolite.go` and requires no test-file regeneration.

```go
// frigolite.go — in Query():
return &Result{
    Columns: allColumns,
    Rows:    allRows,
    SQL:     sqlStr,  // ← add this
}
```

Then:
```go
func checkQueryResult(t *testing.T, res *Result, expected string) {
    t.Helper()
    if res.Error != nil {
        if res.SQL != "" && isKnownUnsupported(res.SQL) {
            return // expected failure — feature not yet implemented
        }
        t.Errorf("query error: %v\n  sql: %s", res.Error, res.SQL)
        return
    }
    // ... existing comparison
}
```

**Verify:**
```bash
# Count how many compat tests fail after surfacing errors:
go test -v -count=1 -run '^TestSQLite_affinity2$' . 2>&1 | grep -c "FAIL"
```

### Step 4: Fix the conversion pipeline — add `sqlite3` oracle mode

**Files:** `tools/convert_compat_test.py`, `tools/convert_compat_json.py`.

**Goal:** Instead of trusting the lossy TCL extraction for expected results, run
each extracted SQL against real `sqlite3` and capture the actual output as the
expected value. This eliminates false expectations from conversion errors.

**New tool:** `tools/oracle_generate.py`

```python
#!/usr/bin/env python3
"""
For each SQL extracted from a TCL test, run it against real sqlite3
and capture the output. Store as expected result.
Usage: python3 tools/oracle_generate.py --testdir <dir> --outdir <dir>
"""
```

**Process:**
1. Extract SQL pairs (existing logic — keep the TCL parsing).
2. For each SQL pair, create a fresh in-memory sqlite3 DB.
3. Execute all statements; capture the final query result.
4. Store the real output as the expected value.
5. If sqlite3 itself errors (TCL-specific constructs), skip the test.

**Key rules:**
- Each test gets its own `:memory:` DB (matching `setupDB`).
- `reset_db` / `__RESET_DB__` creates a fresh DB.
- Multi-statement SQL is split on `;` and executed in order.
- Output format matches frigolite's `flattenResult`: space-separated values,
  NULL → `"NULL"`, one row per line (or space-joined — match the harness).

**Why this works:** We're targeting SQLite3 compatibility. Real sqlite3 IS the
correct expected output. Using it at generation time (not runtime) is allowed.

**Regenerate (old approach, DEPRECATED):**
```bash
cd /Users/muaddib/dev/frigolite
# OLD: python3 tools/oracle_generate.py
# OLD: python3 tools/convert_compat_json.py
# NEW (tcl2go - Go TCL interpreter → Go test files):
go run ./tools/tcl2go/
```

### Step 5: Remove over-aggressive SQL filtering from converters

**Files:** `tools/convert_compat_test.py`, `tools/convert_compat_json.py`.

**Current problem:** The `has_unsupported_features` / `UNSUPPORTED_FEATURES` regex
filters out SQL containing:
- `MATCH` — needed for FTS (P8)
- `FILTER(` — needed for window functions (P2)
- `USING(` — incorrectly catches `JOIN ... USING(col)` (a basic JOIN feature!)
- `json_` — JSON functions
- `RAISE` — trigger RAISE
- `RETURNING` — RETURNING clause
- `randomblob` / `zeroblob` — blob functions

**Fix:** Move ALL feature filtering into `knownUnsupportedPatterns` (Step 3).
The converters should NOT filter SQL — they should extract it and let the test
runner decide whether to skip it. This ensures:
1. Tests exist for every feature (even if currently skipped).
2. As features are implemented, tests automatically activate.
3. No converter regeneration needed when a feature lands — just remove the
   allowlist entry.

**Specific changes:**
- In `convert_compat_test.py`: remove `UNSUPPORTED` regex and the
  `has_unsupported_features` calls. Keep TCL-specific filtering (`$var`, `{}`).
- In `convert_compat_json.py`: same — remove `UNSUPPORTED_FEATURES` and
  `has_unsupported_features` calls. Keep TCL-specific filtering.

### Step 6: Fix TCL `foreach` loop handling (partial)

**Problem:** TCL `foreach` loops generate many test cases:
```tcl
foreach {tn sql res} {
    1 {SELECT ...} {expected1}
    2 {SELECT ...} {expected2}
} {
    do_execsql_test $tn $sql $res
}
```

The converter sees `do_execsql_test $tn $sql $res` with variable names (`$tn`, `$sql`)
and skips them. The actual test data is in the `foreach` list.

**Fix:** Add a `foreach`-list parser to both converters:
1. Detect `foreach {var1 var2 ...} { ... } { body }` patterns.
2. Parse the list as alternating values.
3. Expand each iteration into a concrete `do_execsql_test` call with actual values.
4. This unrolls the loop at conversion time.

**Note:** This is complex TCL parsing. For the first pass, handle only the common
`foreach {tn sql result} { ... }` pattern (3-variable foreach with inline data).
More complex patterns (nested foreach, computed values) can be added later.

**Priority:** Medium — unrolls tests for features like ALTER, analyze, etc.
that use foreach to test many SQL variants. Not blocking for the initial P0.

## Files Modified

| File | Change |
|------|--------|
| `frigolite.go` | Add `SQL string` field to `Result`; set it in `Query()` |
| `frigolite_test.go` | Fix `checkQueryResult` to surface errors; add `knownUnsupportedPatterns` |
| `frigolite_harness_test.go` | Fix `flattenResult` NULL → `"NULL"`; tighten `normalizeSQL` |
| `tools/convert_compat_test.py` | Remove feature filtering; add oracle integration |
| `tools/convert_compat_json.py` | Remove feature filtering; add oracle integration |
| `tools/oracle_generate.py` | NEW — sqlite3 oracle for expected results |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. NULL fix verified
go test -v -count=1 -run '^TestSQLiteSuite/affinity2/501' . 2>&1 | grep -c "FAIL"
# Should be 0 (or 1 if the underlying type bug in affinity2 still exists — that's P1)

# 2. Error surfacing works (compat test with known-bad query now reports)
go test -v -count=1 -run '^TestSQLite_affinity2$' . 2>&1 | grep -q "query error"
# Should find at least one "query error" line

# 3. Quality gates still pass
make quality
go test -run TestSOLID_ ./...

# 4. Regenerated test data loads
go test -count=1 -run '^TestSQLiteSuite/altertab3' . 2>&1 | grep -c "FAIL"
```

## What P0 Does NOT Do

- P0 does not fix engine bugs. It reveals them.
- P0 does not implement window functions, FTS, ATTACH, etc. It ensures tests for
  those features EXIST and are properly verified when the features land.
- P0 may temporarily INCREASE the FAIL count (by surfacing hidden errors). This is
  expected — each surfaced error is tracked and resolved in P1–P9.
