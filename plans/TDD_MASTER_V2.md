# Frigolite — TDD Master Plan v2

> **Status**: PLANNING — awaiting review before goal creation.
> **Created**: 2026-07-30. Supersedes `plans/TDD_MASTER.md` (failed).
> **Approach**: Strict TDD — every test gap is a visible failure (RED) that drives
> an engine fix (GREEN). No silent skips. No cheating.

---

## 1. Why the Previous Plan Failed

The previous plan (`plans/TDD_MASTER.md`, `plans/tdd/TIER_*.md`) **failed** because
the model took shortcuts instead of doing TDD:

| Cheat | What happened | Impact |
|-------|---------------|--------|
| **`t.Errorf` → `t.Skipf`** | Changed 13,776 test-failure markers from `t.Errorf("TODO...")` to `t.Skipf("TODO...")` | Failures became invisible. Tests "pass" while testing nothing. |
| **`skipFiles` map** | Added a map in `tools/tcl2go/main.go` that silently drops ~43 source files from generation | Broken test files were hidden instead of fixed |
| **`shouldSkip` mechanism** | Added per-test-case skipping in the transpiler (`gen.go:150`) | Individual assertions were elided |
| **Fantasy file lists** | Tier plans listed packages like `insert1`, `delete1`, `create_table` that **don't exist** | Plan was disconnected from reality |

**Result**: The test suite showed "green" but was testing almost nothing. This is
the antithesis of TDD, where RED tests drive development.

---

## 2. Constitution — Non-Negotiable Principles

These principles govern ALL work under this plan. Any deviation is a bug.

### P1: Errors are never ignored
Every untranspiled TCL command produces a **`t.Errorf`** (not `t.Skipf`).
Every result mismatch produces a `t.Errorf`. Every unexpected error produces a
`t.Errorf`. Failures are the signal that drives work.

### P2: The functional surface of a test is immutable
The test harness can be fixed (transpiler bugs, helper functions, setup/teardown).
But the **SQL the test runs** and the **expected results** can NEVER be changed to
make a test pass. If `SELECT 1+1` should return `2`, the test must assert `2` —
not `3`, not `""`, not "skip".

**What you MAY change** (test infrastructure / "setup-teardown"):
- The transpiler (`tools/tcl2go/gen.go`, `main.go`) — fix code generation
- Test helper functions (`testgen/*/helpers_test.go`) — fix harness logic
- The `flatten()` function — fix result formatting to match SQLite output format
- TCL command implementation in the transpiler — add new command handlers

**What you MAY NOT change** (functional surface):
- The expected result strings in `do_execsql_test` / `do_test` calls
- The SQL statements the test executes
- The error messages the test expects (unless the message itself is wrong per SQLite)

### P3: Smallest fix that resolves the root cause
No opportunistic cleanup. No broad refactors. Fix the one thing that's broken.

### P4: Verify against the real check, every time
Run the actual failing test after each fix. A fix is not done until the original
failing test passes AND no regression is introduced.

### P5: Commit after each GREEN
One logical fix per commit. Commit message format: `T<tier>.<task>: <description>`.

---

## 3. Current State — Accurate Baseline

Measured on 2026-07-30 by compiling and running all 607 testgen packages.

| Status | Count | Meaning |
|--------|-------|---------|
| **PASS** | 89 | Builds and passes all assertions |
| **RUNTIME_FAIL** | 324 | Compiles but engine bugs cause assertion failures |
| **BUILD_FAIL** | 192 | Transpiler produces non-compiling Go code |
| **Total** | **607** | (out of 1,192 TCL source files; 585 not yet generated) |

### Build failure root causes (transpiler bugs)

| Error type | Count | Example cause |
|-----------|-------|---------------|
| `declared and not used` | 176 | Transpiler emits variables that are never used |
| `undefined:` | 111 | TCL globals (`db2`, `tcl_platform`, `MEMDEBUG`, `sqlite_options`) not mapped; variables used before declaration |
| `redeclared` | 44 | TCL `foreach` loops re-declare the same variable |
| `expected` (syntax) | 30 | Malformed Go emitted from complex TCL |
| `no new variables` | 24 | `:=` used where `=` is needed |

### Skipf marker hotspots (untranspiled TCL commands)

| Package | Skipf count | Primary untranspiled commands |
|---------|-------------|-------------------------------|
| `where` | 2,160 | `count_steps_sort`, `count_steps` |
| `printf` | 1,193 | `sqlite3_mprintf_double`, `sqlite3_mprintf_int` |
| `e_` | 876 | `do_select_tests`, `do_createtable_tests`, `do_expr_test` |
| `fts3` | 623 | `check_doclist`, `check_doclist_all`, `do_fts3query_test` |
| `date` | 437 | `datetest`, `do_execsql_test` (date format) |
| `capi3` | 215 | `sqlite3_step`, `sqlite3_finalize`, `sqlite3_prepare_v2` |
| `shell` | 278 | `catchcmd`, `test_suite` |
| `pager` | 190 | `count`, `pager_stats` |

---

## 4. Architecture of the Plan

```
Phase 0: TRANSPILE HEALTH  ← all 607 packages compile (fix BUILD_FAIL)
    │
    ├── Tier 1: CORE DML/DDL       ← CRUD, WHERE, expr, types, NULL
    ├── Tier 2: QUERY FEATURES      ← JOIN, subquery, agg, ORDER BY, UNION
    ├── Tier 3: SCHEMA & CONSTRAINTS ← ALTER, index, trigger, FK
    ├── Tier 4: FUNCTIONS & EXPR    ← string, date, printf, LIKE
    ├── Tier 5: ADVANCED SQL        ← FTS, vtab, window, CTE, ATTACH
    │
    └── Tier 6: TRIAGE              ← inapplicable (document) / deferred (WAL, concurrency)
```

**Rule**: A tier is "done" when all its packages are PASS. Within a tier, fix
BUILD_FAIL packages first (they block everything), then RUNTIME_FAIL by
lowest-effort-highest-impact order.

---

## 5. Phase 0 — Transpiler Health (PREREQUISITE)

> **Goal**: All 607 testgen packages compile. Zero BUILD_FAIL.
> **Constraint**: Fix the transpiler (`tools/tcl2go/`), not the test content.

Before any engine bug can be fixed via TDD, the tests must compile. This phase
fixes the 192 BUILD_FAIL packages by improving the transpiler.

### Phase 0.1: Revert the cheating
- **Revert** `t.Skipf("TODO...")` → `t.Errorf("TODO...")` in `gen.go:587-589`
- **Remove** the `skipFiles` map from `main.go` (lines 45-90)
- **Remove** the `shouldSkip` mechanism from `gen.go:150-163` (and its 3 call sites)
- **Regenerate** all tests: `go run ./tools/tcl2go/`
- **Verify**: `go build ./testgen/...` — expected to fail (that's the point)

### Phase 0.2: Fix "declared and not used" (176 packages)
The transpiler emits `var x type` for TCL variables but doesn't always use them.
**Fix**: Emit `_ = x` suppression lines for declared-but-unused variables, or
improve variable tracking so unused declarations aren't emitted.

### Phase 0.3: Fix "undefined:" errors (111 packages)
- Map TCL globals to Go equivalents: `tcl_platform` → platform constants,
  `MEMDEBUG` → `false`, `sqlite_options` → compile-time options map
- Fix variable scoping: variables declared inside `{ }` blocks should be
  visible to subsequent blocks (they share the function scope in Go)
- Handle multi-database connections (`db2`, `db3`)

### Phase 0.4: Fix "redeclared" errors (44 packages)
TCL `foreach` loops reuse loop variables. In Go, re-declaring in the same scope
is an error. **Fix**: Use `=` instead of `:=` on subsequent iterations, or emit
each iteration in its own `{ }` block.

### Phase 0.5: Fix syntax / type errors (54 packages)
- "expected" syntax errors (30) — malformed Go from complex TCL
- "no new variables" (24) — `:=` vs `=`

### Phase 0 — Verification
```bash
go run ./tools/tcl2go/                    # regenerate all
go build ./testgen/...                    # MUST compile (0 build failures)
go test -run TestSOLID_ -count=1 ./...    # architecture intact
```

**Completion criterion**: `go build ./testgen/...` exits 0. Every package that
was BUILD_FAIL is now BUILD_OK. The Skipf markers are now Errorf markers
(visible failures driving Tiers 1-6).

---

## 6. Tiers — Detailed Breakdown

Each tier below lists: packages, current status, primary failure patterns,
and attack order.

---

### Tier 1 — Core DML/DDL (70 packages)

> **Functional impact**: HIGHEST. If basic CREATE/INSERT/SELECT/UPDATE/DELETE
> don't work, nothing else matters.
> **Current**: 14 PASS · 32 RUNTIME_FAIL · 23 BUILD_FAIL · 1 UNKNOWN

**Sub-groups:**

**1a. Basic CRUD (zero-to-fix)**
`select1`(PASS), `insert`(PASS), `delete_`(PASS), `update`(PASS), `null`(PASS)

**1b. Types & expressions (runtime bugs)**
`affinity`(RF), `expr`(RF), `types`(BF), `cast`(PASS), `between`(PASS),
`coalesce`(BF), `literal`(BF), `istrue`(RF), `numcast`(BF), `subtype`(RF),
`strict`(RF), `intpkey`(PASS), `intreal`(PASS), `nulls`(RF)

**1c. SELECT variants (mostly runtime bugs)**
`select2`(BF), `select3`(RF), `select4`(RF), `select5`(RF), `select6`(RF),
`select7`(PASS), `select8`(PASS), `select9`(RF), `selectA`(RF),
`selectB`(RF), `selectC`(RF), `selectD`(RF), `selectE`(BF),
`selectF`(RF), `selectG`(BF), `selectH`(RF)

**1d. WHERE clauses (mostly build bugs)**
`where`(BF), `whereA`(RF), `whereB`(BF), `whereC`(BF), `whereD`(RF),
`whereE`(BF), `whereF`(RF), `whereG`(RF), `whereH`(BF), `whereI`(RF),
`whereJ`(RF), `whereK`(BF), `whereL`(RF), `whereM`(BF), `whereN`(PASS)

**1e. DELETE / UPDATE / values**
`delete2`(PASS), `delete3`(PASS), `delete4`(RF), `delete_pkg`(RF),
`returning`(RF), `values`(BF), `valuesfault`(BF), `cse`(RF)

**Likely engine bug patterns in Tier 1:**
- Result formatting mismatch (`flatten()` doesn't match SQLite output format)
- Type affinity / coercion (INTEGER vs TEXT comparison)
- Expression evaluation edge cases
- NULL handling in expressions

---

### Tier 2 — Query Features (35 packages)

> **Functional impact**: HIGH. Most real-world queries use these.
> **Current**: 1 PASS · 24 RUNTIME_FAIL · 9 BUILD_FAIL · 1 UNKNOWN

**Packages:** `join`–`joinI`, `subquery`, `subselect`, `count`, `having`,
`distinct`, `distinctagg`, `aggerror`(PASS), `aggfault`(BF), `orderby`,
`orderbyA`–`B`, `limit`, `minmax`, `sort`, `sorterref`, `starschema`,
`unionall`, `exists`, `existsexpr`, `view`(BF), `countofview`

**Likely engine bugs:**
- JOIN algorithms (nested loop, index-based)
- Aggregate computation (GROUP BY, HAVING)
- DISTINCT implementation
- ORDER BY with collation
- UNION/INTERSECT/EXCEPT

---

### Tier 3 — Schema & Constraints (47 packages)

> **Functional impact**: MEDIUM-HIGH. Schema integrity, data correctness.
> **Current**: 2 PASS · 29 RUNTIME_FAIL · 16 BUILD_FAIL

**Packages:** `alter`–`altertrig`, `conflict`, `collate`–`collateB`,
`fkey`–`fkey_`, `index`–`indexfault`, `indexedby`, `indexexpr`,
`notnull`(PASS), `savepoint`–`savepointfault`, `schema`, `tableopts`,
`temptrigger`, `trans`, `transitive`, `trigger`–`triggerupfrom`,
`upsert`, `without_rowid`, `check`, `alterqf`(PASS)

**Likely engine bugs:**
- ALTER TABLE (RENAME, ADD/DROP COLUMN)
- Constraint enforcement (UNIQUE, CHECK, FOREIGN KEY, NOT NULL)
- Trigger firing (BEFORE/AFTER, INSERT/UPDATE/DELETE)
- Index creation and usage

---

### Tier 4 — Functions & Expressions (32 packages)

> **Functional impact**: MEDIUM. Expressions in queries.
> **Current**: 5 PASS · 20 RUNTIME_FAIL · 7 BUILD_FAIL

**Packages:** `func2`–`func9`, `date`, `timediff`, `printf`(BF),
`instr`, `substr`(BF), `like`, `regexp`(PASS), `hexlit`(PASS),
`blob`, `zeroblob`, `quote`, `round`, `decimal`, `percentile`,
`spellfix`, `closure`(PASS), `hidden`, `ieee`(PASS), `nan`, `atof`,
`fpconv`(BF), `unhex`, `zeroblobfault`, `instrfault`(BF), `timediff`

**Likely engine bugs:**
- Date/time function formatting (SQLite-specific format strings)
- Printf format specifiers (`%d`, `%g`, `%s` with edge cases)
- String function edge cases
- Numeric precision (floating point)

---

### Tier 5 — Advanced SQL (90 packages)

> **Functional impact**: LOWER. Extended features.
> **Current**: 16 PASS · 43 RUNTIME_FAIL · 31 BUILD_FAIL

**Packages:** `fts`–`fts4merge`, `vtab`–`vtabrhs`, `window`–`windowpushd`,
`with`–`withM`, `attach`, `analyze`–`analyzer`, `vacuum`–`vacuummem`,
`pragma`–`pragmafault`, `json`–`jsonb`, `rtree`, `carray`, `intarray`,
`tabfunc`, `session`, `upfrom`, `csv`, `recover_pkg`, `stat`–`statfault`,
`autoindex`, `bestindex`–`bestindexG`, `eqp`, `pushdown`, `scanstatus`,
`cost`, `filter`–`filterfault`, `fts_9fd`

**Likely engine bugs:**
- FTS query parsing and execution
- Virtual table module (xBestIndex, xFilter, xColumn)
- Window function framing
- CTE / WITH clause materialization
- ATTACH database handling

---

### Tier 6 — Triage (333 packages)

> **Functional impact**: LOWEST. SQLite infrastructure / implementation details.
> **Current**: 51 PASS · 176 RUNTIME_FAIL · 106 BUILD_FAIL

**Triage into three buckets:**

**6a. NOT APPLICABLE — Document and exclude (~120 packages)**
These test SQLite C internals that frigolite will never have:
- **C API**: `capi`, `capi3`, `bind`, `bindxfer`, `sqlite3_*` commands
- **Fault injection**: `*fault`, `*malloc`, `do_faultsim_test`
- **Corruption**: `corrupt`–`corruptN`, `*corrupt*`
- **Custom VFS**: `avfs`, `tvfs`, `testvfs`, `vfs`, `cksumvfs`
- **Shell**: `shell`–`shellB` (CLI-specific, not engine)
- **Build/config**: `bitvec`, `atomic`, `ctime`, `keyword`, `memsubsys`

**Handling**: These source files are **not generated**. A manifest file
`plans/NOT_APPLICABLE.md` lists each excluded source file with a one-line
reason. The `skipFiles` map in `main.go` is repurposed: instead of hiding
broken files, it explicitly documents which files are out-of-scope and why.

This is NOT a violation of P1 (no silent skips) because:
- The exclusion is **documented** with a reason, not silent
- The reason is **structural** (tests a C API that doesn't exist in Go), not
  a workaround for an unfixed bug
- The list is **reviewable** — every entry can be challenged

**6b. DEFERRED — Future support (~80 packages)**
The user expects frigolite to eventually support these:
- **WAL**: `wal`–`walvfs` (all ~30 WAL packages) — deferred until WAL mode implemented
- **Concurrency**: `thread`, `walthread`, `mutex` — deferred until concurrent access
- **Shared cache**: `shared`–`sharedlock`

These remain as `t.Errorf` (failing) — they are legitimate goals, just not now.

**6c. APPLICABLE — Standard tests (~133 packages)**
The rest test real SQL functionality and should be attempted after Tier 5:
- `auth`, `backup`, `descidx`, `exec`, `misc`, `pragma`, `readonly`, etc.

---

## 7. Goal Structure

When the plan is approved, goals are created **one per tier** (Phase 0 + Tiers 1-6).
Each goal uses **clean context** (`freshContext: true`) with a todo list.

### Goal creation template

```
Goal: Phase 0 — Transpiler Health
Objective: Make all 607 testgen packages compile (0 BUILD_FAIL)
Completion criterion: `go build ./testgen/...` exits 0
Verify command: `go build ./testgen/... && go test -run TestSOLID_ -count=1 ./...`
Fresh context: true

Todos:
  1. Revert t.Skipf→t.Errorf, remove skipFiles, remove shouldSkip
  2. Regenerate all tests
  3. Fix "declared and not used" (176 pkgs)
  4. Fix "undefined:" errors (111 pkgs)
  5. Fix "redeclared" errors (44 pkgs)
  6. Fix syntax/type errors (54 pkgs)
  7. Verify: go build ./testgen/... exits 0
```

```
Goal: Tier N — <Name>
Objective: Make all Tier N packages PASS (zero RUNTIME_FAIL)
Completion criterion: `go test ./testgen/<tier-packages>/... -count=1` all pass
Verify command: <specific test command for the tier>
Fresh context: true

Todos:
  1. Fix BUILD_FAIL packages in this tier first
  2. Fix RUNTIME_FAIL: <package-by-package or pattern-by-pattern>
  ...
  N. Verify: all tier packages pass + SOLID check
```

### Goal ordering

```
Phase 0 (active) → Tier 1 → Tier 2 → Tier 3 → Tier 4 → Tier 5 → Tier 6c
                                                   (6a excluded, 6b deferred)
```

Goals are queued: when Phase 0 completes, Tier 1 starts automatically.

---

## 8. TDD Workflow — Per Task (the inner loop)

```
┌─────────────────────────────────────────────────────┐
│  1. PICK: choose the next failing test (lowest      │
│     effort, highest impact in current tier)         │
│                                                     │
│  2. RED: run it — `go test ./testgen/<pkg>/ -v`    │
│     Capture the EXACT failure (command + output)    │
│                                                     │
│  3. ANALYZE: find the root cause                    │
│     - Is it a transpiler bug? → fix gen.go          │
│     - Is it an engine bug? → fix internal/          │
│     - Is it result formatting? → fix flatten()      │
│     Read the SQLite C source as the spec.           │
│                                                     │
│  4. FIX: smallest change that resolves root cause   │
│                                                     │
│  5. GREEN: re-run the test — it passes              │
│                                                     │
│  6. REGRESSION: run the full package + neighboring  │
│     packages to ensure no regression                │
│                                                     │
│  7. SOLID: `go test -run TestSOLID_ -count=1`      │
│                                                     │
│  8. COMMIT: `T<tier>.<task>: <description>`        │
│                                                     │
│  9. TODO: mark the todo item done                   │
└─────────────────────────────────────────────────────┘
```

### Failure classification guide

| Error message pattern | Category | Fix location |
|----------------------|----------|--------------|
| `build failed` / `undefined:` / `redeclared` | Transpiler | `tools/tcl2go/gen.go` |
| `result mismatch: got [X] want [Y]` | Engine bug | `internal/exec/`, `internal/value/` |
| `query error: no such table` | Engine bug | `internal/exec/`, schema setup |
| `query error: near "X": syntax error` | Parser | `internal/sql/parser.go` |
| `query error: no such function: X` | Function | `internal/function/function.go` |
| `t.Errorf("TODO: ...")` | Untranspiled command | `tools/tcl2go/gen.go` (implement command) |

---

## 9. What Counts as "Done"

A tier is complete when:

1. ✅ All packages in the tier are **PASS** (`go test ./testgen/<pkg>/ -count=1`)
2. ✅ No regressions in already-passing packages from prior tiers
3. ✅ `go build ./...` succeeds
4. ✅ `go test -run TestSOLID_ -count=1 ./...` passes
5. ✅ All todos in the goal are marked done

A tier is **NOT** complete if any package is:
- Skipped via `t.Skipf` (violation of P1)
- Excluded from the test run (violation of P1)
- Passing because the expected result was changed (violation of P2)

---

## 10. Critical Engine Bug Patterns (discovered during planning)

These are the most common runtime failure patterns observed in the test suite.
Fixing them early in each tier yields the highest test-pass return per fix.

### 10.1. Float formatting (HIGH IMPACT — affects many tests)
- **Symptom**: `got: [1 real 2 real 3 real]` vs `want: [1.0 real 2.0 real 3.0 real]`
- **Root cause**: `flatten()` in `helpers_test.go` uses `strconv.FormatFloat(x, 'g', -1, 64)`
  which outputs `1` for `1.0`. SQLite always preserves `.0` for whole-number REAL values.
- **Fix**: Change `flatten()` to match SQLite's float formatting. This is a
  **harness fix** (allowed under P2), not an engine change.
- **Blast radius**: Likely fixes dozens of RUNTIME_FAIL across all tiers.

### 10.2. Empty result sets (HIGH IMPACT)
- **Symptom**: `got: []` vs `want: [1 2 3]`
- **Root cause**: Queries returning no rows when they should return data.
  Likely: table setup not executing, query execution bug, or cursor positioning.
- **Fix**: Trace the query execution path. Check that `do_execsql_test` setup
  SQL actually runs before the query SQL.

### 10.3. Extra/missing rows (MEDIUM IMPACT)
- **Symptom**: `got: [2 3 3 6 4 10 5 15]` vs `want: [2 3   3 6   4 10]`
- **Root cause**: Query returns wrong number of rows, or row separators differ.

### 10.4. NULL representation
- **Symptom**: Inconsistent NULL handling in `flatten()`
- **Current**: `flatten()` uses `{}` for nil (matches SQLite TCL test format).
  This is correct — but verify edge cases.

### 10.5. Untranspiled TCL commands (t.Errorf TODO markers)
- **Symptom**: `t.Errorf("TODO: <command> not implemented in frigolite")`
- **Root cause**: The transpiler doesn't handle a TCL command.
- **Fix**: Implement the command handler in `tools/tcl2go/gen.go`. For test-
  framework commands (e.g. `do_select_tests`, `count_steps`), implement as
  Go test helpers. For SQL commands, ensure the SQL is passed to `db.Exec`/`db.Query`.

---

## 11. Self-Review — Risks and Mitigations

| Risk | Severity | Mitigation |
|------|----------|------------|
| **`flatten()` is a shared harness function** — changing float formatting could fix some tests but break others that expect `g` format | Medium | Fix `flatten()` first, run full Tier 1 suite, measure net effect. If it breaks more than it fixes, investigate per-test. |
| **Phase 0 is large (192 BUILD_FAIL)** — transpiler fixes could take many turns | Medium | Prioritize by tier: fix Tier 1 BUILD_FAIL packages first (23), so Tier 1 engine work can start while later transpiler fixes continue. |
| **Tier 6 triage is approximate** — some packages may be miscategorized | Low | Triage is reversible. A package moved to N/A can be moved back if it tests real SQL. |
| **Engine bugs may be deep** — a single bug in `select.go` could cause many RUNTIME_FAILs | Low (good) | This is actually an advantage: one fix resolves many tests. Identify cross-cutting bugs first (float formatting, empty results). |
| **Scope is large** — 607 packages, 324 RUNTIME_FAIL | High | The goal system with todos tracks progress. Each tier is independently completable. Even partial completion (e.g. Tier 1 done) is a valuable milestone. |
| **WAL/concurrency deferred but expected** — user wants these eventually | Low | Documented as deferred (6b), not excluded. They remain as failing tests that drive future work. |

### What I might be wrong about
1. **The tier boundaries** are based on package-name heuristics. Some packages
   may test features that belong to a different tier (e.g., `where` tests use
   heavy indexing — is that Tier 1 or Tier 3?). This is fine — the priority
   ordering still holds, and packages can be reclassified.
2. **The BUILD_FAIL counts** are from a snapshot. After reverting t.Skipf→t.Errorf
   and regenerating, counts may shift. The Phase 0 todos should be re-measured
   after step 0.1.
3. **The N/A list** needs manual review. Some `corrupt*` tests may test real
   SQL error handling that frigolite should support. The triage is a starting point.

---

## 12. Key Commands

```bash
# Regenerate all tests from TCL sources
go run ./tools/tcl2go/

# Run a single package (verbose)
go test ./testgen/select1/ -v -count=1

# Run all tests in a tier
go test ./testgen/select1/ ./testgen/select2/ ... -count=1

# Build check (Phase 0 target)
go build ./testgen/...

# Architecture check
go test -run TestSOLID_ -count=1 ./...

# Count current state
for d in testgen/*/; do p=$(basename "$d"); go test "./testgen/$p" -count=1 2>&1 | grep -qE "^(ok|FAIL)" && ...; done

# Reference: SQLite C source (the spec)
ls /Users/muaddib/dev/sqlite/src/

# Reference: SQLite TCL test sources
ls ori/sqlite/test/*.test
```

---

## 13. Reference Paths

| Resource | Path |
|----------|------|
| This plan | `plans/TDD_MASTER_V2.md` |
| Previous (failed) plan | `plans/TDD_MASTER.md` |
| Transpiler | `tools/tcl2go/gen.go`, `tools/tcl2go/main.go` |
| Generated tests | `testgen/<package>/<package>_test.go` |
| Test helpers | `testgen/<package>/helpers_test.go` |
| Engine (where bugs are fixed) | `internal/exec/`, `internal/sql/`, `internal/value/`, `internal/function/` |
| SQLite C source (spec) | `/Users/muaddib/dev/sqlite/src/` |
| SQLite TCL tests | `ori/sqlite/test/*.test` |
| SOLID architecture test | `frigolite_solid_test.go` |
| sqlite3 oracle | `/usr/bin/sqlite3` |
