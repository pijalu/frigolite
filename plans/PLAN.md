# Frigolite — Master Plan: Complete SQLite Feature Parity

> **Status**: ACTIVE — 215/869 file-level PASS (24.7%), 6881 sub-test PASS.
> **Goal**: **ALL 1002 harness test files green** (1002/1002), with SOLID
> architecture, good performance, and confirmed feature completeness.
> **Execution**: Sequential — one phase = one goa goal. Each phase is
> self-contained: an agent with no prior context can implement it from this
> document plus the referenced SQLite C source.
>
> **CRITICAL DISCOVERY**: 411 of 1002 test files reference tables that are
> never created in their JSON test data. The converter
> (`tools/convert_compat_json.py`) loses setup SQL because it doesn't handle
> the `db eval { SQL }` TCL construct — used by 39% of TCL test files.
> **Phase 0 (converter fix) is the highest-impact work and must be done first.**

---

## Verified Ground Truth (measured 2025-01-25)

### Test Results
| Metric | Count |
|--------|-------|
| Total test files | 1002 |
| Skipped (unsupported) | 134 |
| Skipped (slow) | 3 |
| Active test files | 869 |
| **File-level PASS** | **215 (24.7%)** |
| **File-level FAIL** | **654 (75.3%)** |
| Sub-test PASS | 6881 |
| Sub-test FAIL | 10727 |

### Top Error Patterns (ranked by impact)
| Rank | Pattern | Count | Root Cause |
|------|---------|-------|------------|
| 1 | "no such table" | 2644 | **Converter loses `db eval` setup SQL** (411 files affected) |
| 2 | result mismatch | ~4000+ | Various engine bugs |
| 3 | unknown function | 310 | Missing SQL functions (JSON, etc.) |
| 4 | parse/syntax error | 300 | Parser gaps |
| 5 | UNIQUE constraint | 228 | Constraint handling bugs |
| 6 | malformed database | 108 | Format/corruption bugs |
| 7 | attached DB encoding | 47 | ATTACH bug |
| 8 | btree errors | 63 | B-tree bugs |

### Completed Work
| Phase | Description | Status | Impact |
|-------|-------------|--------|--------|
| G01 | JOIN & Subquery Engine Fix | ✅ Done | +121 sub-tests, all 7 join patterns fixed |
| — | sortTestsBySection (harness) | ✅ Done | Fixes reversed section ordering |

**G01 fixes applied** (all verified with isolated tests):
- autoIndex equi-join `*ColumnValue` pointer key bug (broke ALL simple equi-joins)
- `collectUsingColumns` incorrectly filtering join-key columns from SELECT *
- RIGHT JOIN and FULL JOIN implementation
- NATURAL JOIN auto-generating USING conditions
- Correlated subquery column resolution (scalar subqueries + EXISTS)
- `currentScanTable` tracking for qualified column resolution

---

## Development Principles (Binding)

1. **1002/1002 is the goal** — the "unsupported" skip list is a tracking
   mechanism, never an acceptable end state.
2. **Test surface is sacred** — never weaken an assertion or delete a test.
3. **Go stdlib first** — no CGO, no external Go modules.
4. **SQLite is the oracle** — `/usr/bin/sqlite3` MAY be used at test-generation
   time only. SQLite C source (`/Users/muaddib/dev/sqlite/src/`) is the spec.
5. **SOLID design** — new subsystems get their own `internal/` package.
6. **Complexity gate** — target <15 per function. Gate stays at 90 until G13.
7. **Regression prevention** — after each step: `go build ./...` + verify.
8. **Commit after every step** — see "Per-Step Protocol" below.

### Per-Step Protocol (MANDATORY)

After completing each numbered step:

1. **Run the verify command** for that step.
2. **Run `go build ./...`** — must compile.
3. **Commit** with message: `G<NN>.<step>: <description>`.
4. **Update this plan** — mark step done (`[x]`), update Live Metrics, note findings.

---

## Live Metrics

| Phase | Step | Description | File PASS | Sub-test PASS | Delta | Notes |
|-------|------|-------------|-----------|---------------|-------|-------|
| (start) | — | Baseline | 213/869 | 6760 | — | — |
| G01 | done | JOIN & subquery fixes | 215/869 | 6881 | +2 files, +121 sub | 0 regressions |

---

## Phase Dependency Chain

```
P0  Converter Fix              ← HIGHEST IMPACT: fixes 411 files' missing setup
 │
P1  JOIN & Subquery            ← DONE (G01)
 │
P2  Expression & Affinity      ← type coercion, comparison, COLLATE
 │
P3  SQL Functions              ← JSON, date/time, missing scalars
 │
P4  Constraint Enforcement     ← UNIQUE, NOT NULL, CHECK, FK
 │
P5  Window Functions           ← ROW_NUMBER, RANK, etc.
 │
P6  ALTER TABLE                ← RENAME, ADD/DROP COLUMN
 │
P7  Query Planner & ANALYZE    ← cost-based, EQP, auto-index
 │
P8  ATTACH / DETACH            ← multi-database dispatch
 │
P9  FTS3/4/5                   ← full-text search
 │
P10 Virtual Tables             ← vtab modules, bestindex
 │
P11 Corruption & Edge Cases    ← corrupt*, tkt*, bigfile*
 │
P12 Remove All Skip-Lists      ← zero skips
 │
P13 Quality & SOLID Final      ← complexity <15, full green, perf
```

---

## P0 — Converter Fix (HIGHEST IMPACT)

> **goa goal**: `P0: Fix test data converter to extract db eval, db onecolumn, and preserve TCL execution order`
>
> **Objective**: Fix `tools/convert_compat_json.py` so that ALL setup SQL from
> TCL test files is captured in the JSON test data. This is the root cause of
> 411 test files having "no such table" errors. After fixing, regenerate all
> 1002 JSON files and re-measure.
>
> **Completion criterion**: At least 500/1002 test files have CREATE TABLE
> statements that were previously missing. File-level PASS ≥ 300/869.
>
> **Files**: `tools/convert_compat_json.py`, `testdata/*.json` (regenerated)
>
> **SQLite source**: `ori/sqlite/test/*.test` (input TCL files)

### Problem Analysis

The converter currently handles:
- `do_execsql_test NAME { SQL } { EXPECTED }` ✅
- `do_catchsql_test NAME { SQL } { EXPECTED }` ✅
- `do_test NAME { ... execsql { SQL } ... } { EXPECTED }` ✅
- `do_test NAME { ... catchsql { SQL } ... } { EXPECTED }` ✅
- Standalone `execsql { SQL }` ✅

The converter MISSES:
- `db eval { SQL }` — used by 39% of TCL test files ❌
- `db onecolumn { SQL }` — used for scalar queries ❌
- `db eval { SQL }` inside `do_test` blocks ❌
- TCL `for`/`foreach` loops with `db eval` inside (data generation) ❌
- `db transaction { ... }` blocks ❌
- Multi-connection tests (`sqlite3 db2 test.db`) ❌

### Step P0.1 — Add `db eval` extraction to the converter

- [ ] In `extract_tests()`, add a Phase 2b: scan for `db eval { SQL }` patterns
      inside `do_test` blocks, similar to the existing execsql extraction.
- [ ] Also add standalone `db eval { SQL }` extraction (Phase 3b).
- [ ] Handle `db eval { SQL }` where SQL contains TCL variables (`$var`) —
      substitute with placeholder values or skip if unresolvable.
- [ ] **Verify**: Run converter on `analyze8.test`, check that CREATE TABLE
      appears in the JSON output.
      ```bash
      python3 tools/convert_compat_json.py
      python3 -c "import json; d=json.load(open('testdata/analyze8.json')); print([s.get('sql','')[:40] for tc in d['tests'] for s in tc['steps']])"
      ```
- [ ] **Commit + update plan**.

### Step P0.2 — Add `db onecolumn` extraction

- [ ] Add extraction for `db onecolumn { SQL }` (scalar query returning single value).
- [ ] **Verify**: Run converter on a file that uses `db onecolumn`.
- [ ] **Commit + update plan**.

### Step P0.3 — Handle TCL variable substitution in `db eval`

- [ ] Many `db eval` statements contain TCL variables like `$a`, `$b`, `$i`.
      These are used inside TCL `for` loops for data generation.
      Example: `db eval {INSERT INTO t1 VALUES($a,$b,$c,$i)}`
- [ ] For simple variable substitution (constants), replace with values.
- [ ] For loop-generated data, extract the INSERT pattern and note it as
      "requires TCL execution" — skip the step but keep the CREATE TABLE.
- [ ] **Verify**: `testdata/select1.json` has CREATE TABLE for tables it queries.
- [ ] **Commit + update plan**.

### Step P0.4 — Regenerate all 1002 JSON files and re-measure

- [ ] Run `python3 tools/convert_compat_json.py` to regenerate all test data.
- [ ] Run the full harness suite:
      ```bash
      go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v . 2>&1 | \
        grep -E "^    --- (PASS|FAIL): TestSQLiteSuite/[^/]+$" | \
        awk '/PASS/{p++} /FAIL/{f++} END{printf "PASS=%d FAIL=%d\n",p,f}'
      ```
- [ ] Record the new PASS count. Target: **≥300/869**.
- [ ] Check for regressions (files that passed before but fail now).
- [ ] **Commit + update plan** with new measurements.

### Step P0.5 — Fix any converter regressions

- [ ] If the regenerated test data causes regressions, investigate and fix the
      converter or add per-file overrides.
- [ ] **Verify**: No regressions from the P0 baseline (215 files).
- [ ] **Commit + update plan**.

---

## P1 — JOIN & Subquery Engine Fix ✅ DONE

> **Status**: Complete. All 7 join/subquery patterns fixed.
> See "Completed Work" section above and git log for details.

---

## P2 — Expression & Type Affinity

> **goa goal**: `P2: Fix expression evaluation, type affinity, comparison semantics, COLLATE`
>
> **Objective**: Fix expression bugs causing "result mismatch" in e_* and expr*
> tests. Correct type affinity, NULL handling, comparison ordering.
>
> **Completion criterion**: All e_* test files pass. ≥400/869 file-level PASS.
>
> **SQLite reference**: `src/expr.c`, `src/resolve.c`, `src/vdbeapi.c`.

### Step P2.1 — Baseline e_* and expr* failures
- [ ] Run `FRIGOLITE_TEST=e_` and `FRIGOLITE_TEST=expr`.
- [ ] Categorize failures. Document top patterns.
- [ ] **Commit + update plan**.

### Step P2.2 — Fix type affinity in expressions
- [ ] Review `applyAffinity` in `internal/exec/expression.go`.
- [ ] Fix affinity application for TEXT/NUMERIC/INTEGER/REAL/BLOB columns.
- [ ] **Verify**: `FRIGOLITE_TEST=affinity`.
- [ ] **Commit + update plan**.

### Step P2.3 — Fix comparison semantics
- [ ] NULL handling: `NULL = NULL` → NULL, `NULL IS NULL` → 1.
- [ ] Numeric/text comparison ordering per SQLite rules.
- [ ] **Verify**: `FRIGOLITE_TEST=expr`.
- [ ] **Commit + update plan**.

### Step P2.4 — Fix COLLATE clause
- [ ] **Verify**: `FRIGOLITE_TEST=collate`.
- [ ] **Commit + update plan**.

### Step P2.5 — Full suite measurement
- [ ] Target: **≥400/869 PASS**.
- [ ] **Commit + update plan**.

---

## P3 — SQL Functions Completion

> **goa goal**: `P3: Implement all missing SQL functions — JSON, date/time, scalar functions`
>
> **Objective**: Zero "unknown function" errors. 310 errors across ~20 functions.
>
> **Completion criterion**: Zero "unknown function" in harness. ≥450/869 PASS.
>
> **SQLite reference**: `src/func.c`, `ext/misc/json.c`, `src/date.c`.

### Step P3.1 — Audit missing functions
- [ ] Extract all "unknown function" errors from the full suite.
- [ ] Build complete list: function name, type, count.
- [ ] **Commit + update plan**.

### Step P3.2 — Implement JSON functions
- [ ] Create `internal/function/json.go`.
- [ ] Implement: `json_valid`, `json_type`, `json_extract`, `json_remove`,
      `json_quote`, `json_array_length`, `json_array_insert`, `json_patch`,
      `json_each` (table-valued), `json_tree` (table-valued).
- [ ] Reference: `/Users/muaddib/dev/sqlite/ext/misc/json.c`.
- [ ] **Verify**: `FRIGOLITE_TEST=json`.
- [ ] **Commit + update plan**.

### Step P3.3 — Fix date/time functions
- [ ] Fix "unrecognized date/time" errors.
- [ ] Review against `src/date.c`.
- [ ] **Verify**: `FRIGOLITE_TEST=date`, `FRIGOLITE_TEST=strftime`.
- [ ] **Commit + update plan**.

### Step P3.4 — Implement remaining missing functions
- [ ] `changes()`, `total_changes()`, `octet_length()`, `intreal()`,
      `sqlite_compileoption_used()`, and others found in P3.1.
- [ ] **Verify**: Full suite — zero "unknown function" errors.
- [ ] **Commit + update plan**.

### Step P3.5 — Full suite measurement
- [ ] Target: **≥450/869 PASS**.
- [ ] **Commit + update plan**.

---

## P4 — Constraint Enforcement

> **goa goal**: `P4: Fix UNIQUE, NOT NULL, CHECK, FOREIGN KEY constraint enforcement`
>
> **Objective**: Correct constraint handling. 228 UNIQUE, 8 CHECK, 6 NOT NULL errors.
>
> **Completion criterion**: All constraint tests pass. ≥500/869 PASS.
>
> **SQLite reference**: `src/insert.c`, `src/update.c`, `src/fkey.c`.

### Step P4.1 — Audit constraint failures
- [ ] Categorize all constraint errors.
- [ ] **Commit + update plan**.

### Step P4.2 — Fix UNIQUE constraint enforcement
- [ ] Correct error messages, multi-column UNIQUE, NULL handling.
- [ ] **Verify**: `FRIGOLITE_TEST=conflict`, `FRIGOLITE_TEST=unique`.
- [ ] **Commit + update plan**.

### Step P4.3 — Fix NOT NULL and CHECK
- [ ] **Verify**: `FRIGOLITE_TEST=notnull`, `FRIGOLITE_TEST=check`.
- [ ] **Commit + update plan**.

### Step P4.4 — Fix FOREIGN KEY
- [ ] **Verify**: `FRIGOLITE_TEST=fkey`.
- [ ] **Commit + update plan**.

### Step P4.5 — Full suite measurement
- [ ] Target: **≥500/869 PASS**.
- [ ] **Commit + update plan**.

---

## P5 — Window Functions

> **goa goal**: `P5: Implement window functions — ROW_NUMBER, RANK, LAG, LEAD, aggregates, FILTER`
>
> **Objective**: Full window function support. Remove 17 window tests from skip list.
>
> **Completion criterion**: All window* tests pass. ≥520/869 PASS.
>
> **SQLite reference**: `src/window.c`.

### Step P5.1 — Remove window tests from skip list
- [ ] Remove all `window*` entries from `unsupportedTestFiles`.
- [ ] Run tests, capture failures.
- [ ] **Commit + update plan**.

### Step P5.2 — Implement window pipeline
- [ ] Create `internal/exec/window.go`.
- [ ] Implement partition/sort/frame/evaluate pipeline.
- [ ] **Commit + update plan**.

### Step P5.3 — Implement built-in window functions
- [ ] ROW_NUMBER, RANK, DENSE_RANK, LAG, LEAD, FIRST_VALUE, LAST_VALUE, NTH_VALUE.
- [ ] **Commit + update plan**.

### Step P5.4 — Implement aggregates-as-windows
- [ ] SUM, AVG, COUNT, MIN, MAX over windows.
- [ ] **Commit + update plan**.

### Step P5.5 — Full suite measurement
- [ ] Target: **≥520/869 PASS**.
- [ ] **Commit + update plan**.

---

## P6 — ALTER TABLE Completeness

> **goa goal**: `P6: ALTER TABLE — RENAME correctness, ADD/DROP COLUMN, token-level rename`
>
> **Objective**: All alter* tests pass.
>
> **Completion criterion**: All alter* tests pass. ≥540/869 PASS.
>
> **SQLite reference**: `src/alter.c`.

### Step P6.1 — Remove alter tests from skip list
- [ ] Remove `altercorrupt`, `altertab2`, `altertab3` from skip list.
- [ ] **Commit + update plan**.

### Step P6.2 — Fix token-level rename
- [ ] Use `internal/rename` package for robust identifier replacement.
- [ ] **Verify**: `FRIGOLITE_TEST=altertab`.
- [ ] **Commit + update plan**.

### Step P6.3 — Fix ADD/DROP COLUMN
- [ ] **Verify**: `FRIGOLITE_TEST=altercol`.
- [ ] **Commit + update plan**.

### Step P6.4 — Full suite measurement
- [ ] Target: **≥540/869 PASS**.
- [ ] **Commit + update plan**.

---

## P7 — Query Planner & ANALYZE

> **goa goal**: `P7: Query planner — ANALYZE, cost-based index selection, EQP, auto-index`
>
> **Objective**: Index selection uses statistics. EQP reports correct plans.
>
> **Completion criterion**: All analyze*, eqp* tests pass. ≥580/869 PASS.
>
> **SQLite reference**: `src/analyze.c`, `src/where.c`.

### Step P7.1 — Implement ANALYZE
- [ ] Create `sqlite_stat1` table, store statistics.
- [ ] **Commit + update plan**.

### Step P7.2 — Cost-based index selection
- [ ] Use stats to choose full-scan vs index lookup.
- [ ] **Commit + update plan**.

### Step P7.3 — EXPLAIN QUERY PLAN
- [ ] Output `SEARCH ... USING INDEX` / `SCAN` correctly.
- [ ] **Commit + update plan**.

### Step P7.4 — Auto-index for joins
- [ ] **Commit + update plan**.

### Step P7.5 — Full suite measurement
- [ ] Target: **≥580/869 PASS**.
- [ ] **Commit + update plan**.

---

## P8 — ATTACH / DETACH

> **goa goal**: `P8: ATTACH/DETACH — multi-database dispatch, file-backed databases`
>
> **Objective**: All attach* tests pass (currently 93/181 pass).
>
> **Completion criterion**: All attach* tests pass. ≥620/869 PASS.
>
> **SQLite reference**: `src/attach.c`.

### Step P8.1 — Fix encoding check false positive
- [ ] 47 false "encoding mismatch" errors.
- [ ] **Commit + update plan**.

### Step P8.2 — Fix multi-database schema resolution
- [ ] **Commit + update plan**.

### Step P8.3 — Full suite measurement
- [ ] Target: **≥620/869 PASS**.
- [ ] **Commit + update plan**.

---

## P9 — FTS3/4/5

> **goa goal**: `P9: FTS3/4/5 — shadow tables, segment store, MATCH query, BM25 ranking`
>
> **Objective**: Full-text search. ~76 test files (largest single feature gap).
>
> **Completion criterion**: All fts* tests pass. ≥700/869 PASS.
>
> **SQLite reference**: `ext/fts3/`, `ext/fts5/`.

### Step P9.1 — Remove FTS tests from skip list, audit
- [ ] **Commit + update plan**.

### Step P9.2 — Implement shadow table architecture
- [ ] `CREATE VIRTUAL TABLE ft USING fts3(col)` creates shadow tables.
- [ ] **Commit + update plan**.

### Step P9.3 — Implement FTS insert path
- [ ] Tokenize, write postings, update content table.
- [ ] **Commit + update plan**.

### Step P9.4 — Implement MATCH query
- [ ] Parse MATCH expression, look up postings, rank by BM25.
- [ ] **Commit + update plan**.

### Step P9.5 — Implement segment merge
- [ ] **Commit + update plan**.

### Step P9.6 — FTS5-specific features
- [ ] **Commit + update plan**.

### Step P9.7 — Full suite measurement
- [ ] Target: **≥700/869 PASS**.
- [ ] **Commit + update plan**.

---

## P10 — Virtual Tables & Remaining Features

> **goa goal**: `P10: Virtual tables, WITHOUT ROWID, remaining missing features`
>
> **Objective**: Fix vtab*, bestindex*, without* tests.
>
> **Completion criterion**: ≥750/869 PASS.
>
> **SQLite reference**: `src/vtab.c`.

### Step P10.1 — Fix xBestIndex
- [ ] **Commit + update plan**.

### Step P10.2 — Fix WITHOUT ROWID tables
- [ ] **Commit + update plan**.

### Step P10.3 — Implement remaining vtab modules
- [ ] `zipfile`, `dbstat`, `fsdir`, etc.
- [ ] **Commit + update plan**.

### Step P10.4 — Full suite measurement
- [ ] Target: **≥750/869 PASS**.
- [ ] **Commit + update plan**.

---

## P11 — Corruption & Edge Cases

> **goa goal**: `P11: Corruption handling, ticket tests, large data edge cases`
>
> **Objective**: Fix corrupt* (14), tkt* (~73), bigfile*/bigrow* tests.
>
> **Completion criterion**: ≥850/869 PASS.
>
> **SQLite reference**: Ticket numbers referenced in each `tkt*` file.

### Step P11.1 — Fix corruption handling
- [ ] Match SQLite error messages exactly.
- [ ] **Commit + update plan**.

### Step P11.2 — Fix ticket tests
- [ ] Remove tkt* from skip list. Fix each.
- [ ] **Commit + update plan**.

### Step P11.3 — Fix large data tests
- [ ] **Commit + update plan**.

### Step P11.4 — Full suite measurement
- [ ] Target: **≥850/869 PASS**.
- [ ] **Commit + update plan**.

---

## P12 — Remove All Skip-List Entries

> **goa goal**: `P12: Zero skip-list entries — all 1002 test files active and green`
>
> **Objective**: Remove ALL remaining entries from `unsupportedTestFiles` and
> `slowTestFiles`.
>
> **Completion criterion**: 1002/1002 PASS. Zero skip-list entries.

### Step P12.1 — Remove remaining unsupported entries
- [ ] Threads, WAL, misc — remove one category at a time.
- [ ] **Commit + update plan**.

### Step P12.2 — Remove slow test entries
- [ ] `emptytable`, `indexexpr1`, `joinD`.
- [ ] **Commit + update plan**.

### Step P12.3 — Final 1002/1002 verification
- [ ] **Commit + update plan**.

---

## P13 — Quality & SOLID Final

> **goa goal**: `P13: Quality gate — complexity <15, full green, SOLID, performance`
>
> **Objective**: Enforce cognitive complexity <15. Full green. SOLID. Perf targets.
>
> **Completion criterion**: `make quality` passes at threshold 15. `go test
> -count=1 ./...` all green. SOLID passes. Benchmarks meet targets.

### Step P13.1 — Lower complexity gate
- [ ] `Makefile`: `gocognit -over 90` → `-over 15`, `gocyclo -over 40` → `-over 15`.
- [ ] **Commit + update plan**.

### Step P13.2 — Refactor each complexity offender
- [ ] Split functions, extract helpers, guard clauses.
- [ ] Run tests after each refactor.
- [ ] **Commit + update plan**.

### Step P13.3 — Run staticcheck
- [ ] Fix all warnings.
- [ ] **Commit + update plan**.

### Step P13.4 — Final verification
- [ ] Full suite green. Benchmarks meet targets. SOLID passes.
- [ ] **Commit + update plan**.

---

## Progress Tracking

| Phase | Description | Status | File PASS | Sub-test PASS | Notes |
|-------|-------------|--------|-----------|---------------|-------|
| (start) | Baseline | — | 213/869 | 6760 | 75% failure rate |
| G01/P1 | JOIN & Subquery Fix | ✅ Done | 215/869 | 6881 | +121 sub-tests, 0 regressions |
| P0 | Converter Fix | 🔲 Not started | | | HIGHEST IMPACT: fixes 411 files |
| P2 | Expression & Affinity | 🔲 Not started | | | |
| P3 | SQL Functions | 🔲 Not started | | | |
| P4 | Constraint Enforcement | 🔲 Not started | | | |
| P5 | Window Functions | 🔲 Not started | | | |
| P6 | ALTER TABLE | 🔲 Not started | | | |
| P7 | Query Planner & ANALYZE | 🔲 Not started | | | |
| P8 | ATTACH / DETACH | 🔲 Not started | | | |
| P9 | FTS3/4/5 | 🔲 Not started | | | |
| P10 | Virtual Tables | 🔲 Not started | | | |
| P11 | Corruption & Edge Cases | 🔲 Not started | | | |
| P12 | Remove All Skip-Lists | 🔲 Not started | | | |
| P13 | Quality & SOLID Final | 🔲 Not started | | | |

---

## Key Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source | `/Users/muaddib/dev/sqlite/src/` |
| SQLite FTS source | `/Users/muaddib/dev/sqlite/ext/fts3/`, `ext/fts5/` |
| SQLite JSON source | `/Users/muaddib/dev/sqlite/ext/misc/json.c` |
| SQLite TCL tests | `ori/sqlite/test/*.test` |
| Frigolite test data | `testdata/*.json` (1002 files) |
| Frigolite harness | `frigolite_harness_test.go` |
| Converter | `tools/convert_compat_json.py` |
| SOLID tests | `frigolite_solid_test.go` |
| Quality gates | `Makefile` (`make quality`) |
| sqlite3 binary (oracle) | `/usr/bin/sqlite3` (v3.51.0) |
| Internal packages | `internal/` (13 packages) |

---

## Common Verify Commands

```bash
# File-level pass/fail count
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v . 2>&1 | \
  grep -E "^    --- (PASS|FAIL): TestSQLiteSuite/[^/]+$" | \
  awk '/PASS/{p++} /FAIL/{f++} END{printf "PASS=%d FAIL=%d TOTAL=%d\n",p,f,p+f}'

# Sub-test pass/fail count
go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v . 2>&1 | \
  awk '/^        --- PASS/{p++} /^        --- FAIL/{f++} END{printf "Sub-test PASS=%d FAIL=%d\n",p,f}'

# Filtered harness
FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$" -count=1 -v -timeout 60s .

# SOLID architecture
go test -run TestSOLID_ -count=1 ./...

# Quality gate
make quality

# Build
go build ./...
```

---

## How to Use This Plan (for the execution agent)

1. **Read the master plan** (this file) — focus on the Dependency Chain and
   your assigned phase.
2. **Read the referenced SQLite C source** — it is the behavioural spec.
3. **Implement steps in order** — each step builds on the previous.
4. **Run the verify command after each step**.
5. **MANDATORY: Commit + update this plan after every step.**
6. **Run SOLID tests** before declaring a phase complete.
7. **Update the Progress Tracking table** when done.
