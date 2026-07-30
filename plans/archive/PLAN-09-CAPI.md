# G09 — C-API Test Pattern Migration

> **⚠️ DEPRECATED APPROACH**: This plan describes modifications to the old Python converter (`convert_compat_json.py`) which has been deleted. The test data now uses the **tcl2go** pipeline (Go TCL interpreter → Go test files). See [`PLAN.md`](./PLAN.md) for the current strategy.
>
> **Prerequisite**: G00 (test infrastructure with oracle).
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/tclsqlite.c` (TCL test wrapper),
> `/Users/muaddib/dev/sqlite/src/main.c` (function `sqlite3_prepare()` and friends),
> `/Users/muaddib/dev/sqlite/src/legacy.c` (function `sqlite3_exec()`).
> **Goal**: Migrate the 123 C-API test files to Go paradigms. Extract SQL behavior tests from C-API test patterns.

---

## Context

The converter (`tools/convert_compat_json.py`) currently **skips 123 TCL test files**
because they contain any `sqlite3_*` C-API symbol (see `C_API_RE` regex at line 8, and the
skip logic in `main()` at line 421). This is overly aggressive — many of these files contain
valuable SQL behavior tests that happen to use the C API as their execution vehicle.

### What C-API tests look like

```tcl
# Typical C-API test pattern (Phase A — tests C API internals)
do_test test_name {
  set STMT [sqlite3_prepare db "SELECT ?" -1 TAIL]
  sqlite3_step $STMT
  sqlite3_column_text $STMT 0
} {result}

# Or using execsql wrapper (Phase B — tests SQL behavior via TCL wrapper)
do_test test_name {
  execsql {
    SELECT * FROM t1 WHERE x = 5
  }
} {expected_result}
```

The first pattern tests the C API (prepare/step/column) — Go doesn't have this API.
The second pattern tests SQL behavior via a TCL wrapper — fully convertible.

### Categorization of C-API tests

| Category | Count (est.) | Strategy | SQLite C source |
|----------|-------------|----------|-----------------|
| **SQL behavior via execsql/catchsql wrapper** | ~60% | Extract SQL, convert normally | `tclsqlite.c:TclSqliteExec` |
| **Error code verification** (SQLITE_CONSTRAINT, etc.) | ~20% | Map to Go error checking | `main.c:sqlite3_errcode` |
| **Prepared statement lifecycle** (prepare/bind/step/finalize) | ~10% | Convert to frigolite API or database/sql driver | `main.c:sqlite3_prepare`, `vdbeapi.c:sqlite3_step` |
| **C-specific** (memory, threading, file handles, malloc) | ~10% | Exclude — not applicable to Go | `malloc.c`, `threads.c` |

---

## Implementation Steps

### Step 1: Enhance the converter to extract SQL from C-API patterns

**File**: `tools/convert_compat_json.py`

Currently, `main()` (line 411) skips any file containing a `sqlite3_*` symbol (via
`C_API_RE.search(content)` at line 421). Instead:

1. **Don't skip the file** — process it like any other test file. Remove the
   `if C_API_RE.search(content): skip_files.add(fname)` guard at line 421.

2. **Extract `execsql {}` and `catchsql {}` patterns** (already handled by Phases 1–8 in
   `extract_tests()` at line 115).

3. **Extract `db eval {}` patterns** (already handled by Phase 6–7 in `extract_tests()`).

4. **Extract SQL from `sqlite3_prepare db "SQL"` patterns** — add new regex extraction in
   `extract_tests()`:

```python
def extract_prepare_sql(content: str) -> list[tuple[int, str, str]]:
    """Extract SQL from sqlite3_prepare calls with inline SQL.
    
    Returns list of (position, step_type, sql) tuples.
    Handles both "quoted" and {braced} SQL arguments.
    """
    entries = []
    # Pattern: sqlite3_prepare <db_handle> "SQL STRING" -1 <tail_var>
    for m in re.finditer(r'sqlite3_prepare\s+\w+\s+"([^"]*)"', content):
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "execsql", sql))
    # Pattern: sqlite3_prepare <db_handle> { SQL }
    for m in re.finditer(r'sqlite3_prepare\s+\w+\s+\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "execsql", sql))
    return entries
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/main.c` function `sqlite3_prepare_v2()`
(the prepared-statement entry point) and `/Users/muaddib/dev/sqlite/src/tclsqlite.c` function
`Sqlite3PrepareCmd()` (the TCL wrapper that parses the `sqlite3_prepare` command).

5. **Mark extracted C-API tests** with `"source": "capi"` so the harness can treat them
   differently if needed. Add a `source` field to the JSON entry in `flush()` (line 122).

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
# Verify the converter runs without errors
python3 tools/convert_compat_json.py 2>&1 | head -5
# Should print "Skipping 0 C API test files" (or far fewer than 123)
# Expected: file count in output increases
```
**Expected outcome**: The "Skipping N C API test files" line shows a much smaller number
than 123. More test data files are generated.

### Step 2: Map SQLite error codes to Go error checking

**File**: `tools/convert_compat_json.py` and `frigolite_harness_test.go`

C-API tests often verify specific error codes:
```tcl
do_test test_name {
  catchsql { INSERT INTO t1 VALUES(1, 2) }
} {1 {constraint failed: UNIQUE constraint}}  # {errorcode errmsg}
```

The `catchsql` pattern is already handled — it produces `expect` starting with `1` (error
expected). The harness already checks for error matching.

For C-API tests that check `sqlite3_errcode` / `sqlite3_errmsg` directly:
- Skip these patterns — they test C API internals
- But keep the SQL execution that led to the error

Add a filter function in `extract_tests()`:

```python
def is_c_api_only(step_sql: str) -> bool:
    """Return True if the step only tests C-API internals (no SQL to run)."""
    return bool(re.match(r'^\s*sqlite3_(errcode|errmsg|finalize|reset|step|'
                         r'column_|bind_|db_status|status|changes|total_changes)',
                         step_sql.strip(), re.IGNORECASE))
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/main.c` function `sqlite3_errcode()` and
`sqlite3_errmsg()` — the C-API error accessors being tested.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
# Verify the harness still compiles
```
**Expected outcome**: `go build ./...` succeeds with no errors.

### Step 3: Convert prepared-statement tests to Go paradigm

**File**: `tools/convert_compat_json.py`

Tests that test `sqlite3_prepare` / `sqlite3_bind` / `sqlite3_step` / `sqlite3_column`:

These test the **prepared statement API**. In Go, the equivalent is:
- `database/sql` driver (`frigodb/driver.go`) — `db.Prepare()`, `stmt.Query()`
- Or the native frigolite API — `db.Query()` with inline parameters

**Strategy**: Extract the SQL from these tests and run them as regular queries. The
parameter binding (`sqlite3_bind_int $STMT 1 42`) becomes inline value substitution.

```python
def extract_prepare_sequence(content: str, start_pos: int) -> tuple[str, list[str]] | None:
    """Extract a sqlite3_prepare + sqlite3_bind_* + sqlite3_step sequence
    as a single query with substituted parameters.
    
    Returns (sql_with_values_substituted, bound_values) or None if not extractable.
    """
    # 1. Find sqlite3_prepare at start_pos — get the SQL template
    # 2. Scan forward for sqlite3_bind_* calls — collect (param_index, value) pairs
    # 3. Substitute ? placeholders with bound values
    # 4. Return the fully-substituted SQL
    pass  # Implementation-specific to TCL parsing
```

**Simpler approach**: For the first pass, just extract the SQL from `sqlite3_prepare` calls
(Step 1's `extract_prepare_sql`) and run it as a regular query. Parameter substitution can be
handled by the oracle (which runs against real sqlite3).

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/vdbeapi.c` function `sqlite3_step()` and
`sqlite3_bind_*()` family — the prepared-statement execution path.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
# Verify a sample C-API test file now produces JSON
python3 -c "
import sys; sys.path.insert(0, 'tools')
from convert_compat_json import extract_tests
with open('ori/sqlite/test/auth2.test', errors='replace') as f:
    tests = extract_tests(f.read())
print(f'Extracted {len(tests)} test steps from auth2.test')
"
```
**Expected outcome**: Test steps are extracted from a previously-skipped C-API file. Output
shows `Extracted N test steps from auth2.test` with N > 0.

### Step 4: Exclude C-specific test patterns

**File**: `tools/convert_compat_json.py`

Add patterns that indicate C-specific testing (not applicable to Go):

```python
C_SPECIFIC_PATTERNS = [
    r'sqlite3_malloc',      # memory allocation testing
    r'sqlite3_free',
    r'sqlite3_memory_used',
    r'sqlite3_threadsafe',  # threading tests
    r'sqlite3_db_mutex',
    r'sqlite3_file_control',
    r'sqlite3_overload_function',
    r'TEST_REALLOC_STRESS',
    r'malloc_test',
    r'fault_inject',
]
```

If a test case ONLY contains C-specific patterns (no SQL execution), skip it. If a test
file mixes SQL and C-specific tests, keep the SQL tests.

Add this check to `extract_tests()`:
```python
def is_c_specific_test(content: str) -> bool:
    """Return True if the test block ONLY tests C-specific patterns."""
    has_sql = bool(re.search(r'(execsql|catchsql|db\s+eval)', content))
    has_c_only = all(re.search(p, content) for p in C_SPECIFIC_PATTERNS)
    return has_c_only and not has_sql
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/malloc.c` (memory testing infrastructure)
and `/Users/muaddib/dev/sqlite/src/threads.c` (threading API).

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go build ./...
# Verify no C-specific tests crash the converter
python3 tools/convert_compat_json.py 2>&1 | grep -i error
```
**Expected outcome**: No errors printed; converter completes successfully.

### Step 5: Use the oracle for C-API test expected values

**File**: `tools/oracle_generate.py`

C-API tests that are converted to SQL tests need expected values. The oracle runs the SQL
against real sqlite3 to capture the expected output — same as any other test.

**SQLite reference**: `/usr/bin/sqlite3` v3.51.0 (the oracle binary itself).

The oracle generator already handles this for all test cases. No special C-API handling
needed — the converted SQL is treated identically.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
python3 tools/oracle_generate.py --test auth2,backup,capidata 2>&1 | head -10
```
**Expected outcome**: Oracle generates expected values for previously-skipped C-API tests
without errors.

### Step 6: Verify converted tests

After conversion:

```bash
cd /Users/muaddib/dev/frigolite
# Count how many new test files are active
ls testdata/*.json | wc -l
# Should be significantly more than 696

# Run a sample of newly converted tests
go test -v -count=1 -run "^TestSQLiteSuite/(auth|backup|capidata)" . 2>&1 | head -20
```

**SQLite reference**: `/Users/muaddib/dev/sqlite/src/tclsqlite.c` function `TESTNAME()` —
the TCL test naming convention that maps to Go test names.

**Verify**:
```bash
cd /Users/muaddib/dev/frigolite
go test -count=1 -run "^TestSQLiteSuite/auth2" . 2>&1 | tail -5
make quality
```
**Expected outcome**: `ls testdata/*.json | wc -l` shows > 696 files (closer to 800+).
Newly converted tests run — many PASS or SKIP for unsupported features, but they exist and
don't crash. `make quality` passes.

---

## Files Modified

| File | Lines (est.) | Change |
|------|------|--------|
| `tools/convert_compat_json.py` | ~450 | Remove blanket C-API skip (line 421); add `extract_prepare_sql()`, `is_c_api_only()`, `is_c_specific_test()`; add `source: "capi"` field |
| `tools/oracle_generate.py` | ~300 | Handle C-API test expected values (no change needed — already generic) |

---

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# 1. More test files are active
ls testdata/*.json | wc -l
# Should be > 696 (closer to 800+)

# 2. C-API test files run (many will SKIP for unsupported features, but they exist)
go test -v -count=1 -run "^TestSQLiteSuite/auth2" . 2>&1 | head -10

# 3. No regressions in existing tests
make quality

# 4. Count of test data files
python3 -c "
import json, os
total = 0
for f in os.listdir('testdata'):
    if f.endswith('.json'):
        with open(f'testdata/{f}') as fh:
            data = json.load(fh)
            total += len(data.get('tests', []))
print(f'Total test cases: {total}')
"
# Expected: Total test cases significantly higher than pre-G09 count

# 5. SOLID architecture unchanged
go test -run TestSOLID_ ./...
```

**Expected final outcome**:
- `ls testdata/*.json | wc -l` → count > 696
- `make quality` → passes (vet, staticcheck, gocognit, gocyclo all OK)
- `go test -run TestSOLID_ ./...` → passes
- Total test case count increases measurably

## Scope Note

Not all 123 C-API test files will produce useful tests. Many test C-specific behavior
(memory management, threading, file I/O) that is irrelevant to a Go implementation. The goal
is to extract the ~60-70% that contain SQL behavior tests, not to force-convert everything.
