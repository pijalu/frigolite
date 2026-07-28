# Frigolite — Master Plan: All Tests Green

> **Status**: PLANNING — detailed plan for sub-agent sequential execution.
> **Goal**: Zero FAIL across all three test systems (harness JSON, compat Go, hand-written).
> **Constraint**: Functional scope preserved. Setup/teardown may change. C API tests use Go recreation.

## What This Plan Fixes (The Root Complexities)

The previous plan treated every failure as an engine bug. Investigation reveals **five
structural problems** that must be addressed first — fixing them reorders and shrinks
the remaining work:

| # | Complexity | Impact | Phase |
|---|-----------|--------|-------|
| C1 | **Test helpers silently swallow errors.** `checkQueryResult` returns immediately on `res.Error != nil` — 6 657 compat-test queries are never verified. `flattenResult` emits `""` for NULL while `cleanExpected` emits `NULL`. | Every compat test with an erroring query reports false PASS. | P0 |
| C2 | **The TCL→test converters are lossy and aggressive.** They deduplicate SQL (losing `foreach`-loop cases), filter out `MATCH`, `FILTER(`, `USING(`, `json_`, `RAISE`, `randomblob`, etc., and skip all 123 files that mention any `sqlite3_*` C-API symbol. | Tests for real features never exist; C-API tests are entirely absent. | P0 + P7 |
| C3 | **ALTER TABLE RENAME must use token-level processing**, not AST rewriting. SQLite re-parses trigger/view bodies (which contain window functions, CTEs, FILTER) and walks the parse tree to find/replace name tokens. Frigolite's current string-regex approach fails on any unsupported syntax in trigger bodies. | ~99 ALTER TABLE failures; many need window-function parsing to even create the trigger. | P2 → P3 |
| C4 | **EXPLAIN QUERY PLAN requires a real cost-based planner.** `analyze7` expects `SEARCH t1 USING INDEX t1b (b=?)` but frigolite always reports `SCAN t1`. No ANALYZE statistics are consumed. | ~55 ANALYZE failures, ~15 auto-index failures. | P4 |
| C5 | **ATTACH needs a multi-database connection.** `aux.t1`, `main.t4`, `temp.sqlite_master` — frigolite has schema-prefix parsing but no attached-database dispatch. | ~14 ATTACH failures. | P6 |

**Bottom line:** the true failure count is **unknown** until P0 lands, because the
error-swallowing helper hides an unknown number of broken queries behind false PASSes.
P0 must run first to reveal the real surface.

## Phase Dependency Chain

```
P0  Test Infrastructure          ← reveals true failure surface; MUST be first
 │
 ├── P1  Type System & Affinity   ← small, foundational, unblocks many tests
 │
 ├── P2  SQL Parser Completeness  ← WINDOW, CTE, FILTER, RETURNING; unblocks P3
 │       │
 │       └── P3  ALTER TABLE      ← token-level rename; needs full parser
 │
 ├── P4  Query Planner & ANALYZE  ← index selection, EXPLAIN QUERY PLAN
 │       │
 │       └── P5  Auto-Index & JOIN ← automatic indexes, NULL padding
 │
 ├── P6  ATTACH Database          ← multi-database; independent architecture
 │
 ├── P7  C API Go Layer           ← Stmt/Step/Column/Bind; unblocks 123 test files
 │
 ├── P8  FTS3/4/5                 ← full-text search; largest single effort
 │       │
 │       └── P9  amatch & vtab    ← builds on vtab framework hardened by P8
 │
 └── P10 Quality & Final          ← gocognit, SOLID, full green verification
```

**P1, P2, P6, P7, P8** can start in parallel after P0 (they touch different subsystems).
The arrows show hard dependencies. The recommended **sequential** order for a single
sub-agent stream is:

**P0 → P1 → P2 → P3 → P4 → P5 → P6 → P7 → P8 → P9 → P10**

## Sub-Plan Files

| Phase | File | Topic | Estimated FAIL impact |
|-------|------|-------|----------------------|
| P0 | `PLAN-00-TEST-INFRA.md` | Test infrastructure overhaul | Reveals hidden failures; fixes ~15 infra-only FAILs |
| P1 | `PLAN-01-AFFINITY.md` | Type affinity, NULL, blob comparison | ~7 (affinity2, atomic2) |
| P2 | `PLAN-02-PARSER.md` | Window functions, CTE, FILTER, RETURNING | Enabler for P3; fixes parse-only FAILs |
| P3 | `PLAN-03-ALTER.md` | ALTER TABLE token-level rename | ~99 (altertab3, alterlegacy, altercons2, altertab2) |
| P4 | `PLAN-04-PLANNER.md` | Query planner, ANALYZE, EXPLAIN QUERY PLAN | ~55 (analyze*) |
| P5 | `PLAN-05-AUTOINDEX.md` | Auto-index, JOIN NULL handling | ~15 (autoindex*) |
| P6 | `PLAN-06-ATTACH.md` | Multi-database ATTACH/DETACH | ~14 (attach3) |
| P7 | `PLAN-07-CAPI.md` | Go recreation of SQLite C API | Unblocks 123 test files |
| P8 | `PLAN-08-FTS.md` | FTS3/4/5 full-text search | ~59+ |
| P9 | `PLAN-09-VTAB.md` | amatch virtual table | ~3 (amatch1) |
| P10 | `PLAN-10-QUALITY.md` | gocognit, SOLID, final verification | Quality gates |

## Development Principles (Binding)

1. **Test surface is sacred** — never weaken an assertion or delete a test to make it
   pass. Setup/teardown (DB init, cleanup, state reset) MAY change; functional scope
   MAY NOT.
2. **Go stdlib first** — no CGO, no external Go modules, no `sqlite3` CLI at runtime.
3. **SOLID design** — new subsystems get their own `internal/` package; check
   `internalLayers` in `frigolite_solid_test.go` and update it.
4. **SQLite is the oracle** — `sqlite3` v3.51.0 is available on this machine and MAY be
   used **at test-generation time** (never at test-run time) to capture expected output.
5. **Regression prevention** — after each phase: `make quality` + `go test -run TestSOLID_ ./...` + the phase's verify command.
6. **Each sub-plan is self-contained** — a sub-agent with no prior context must be able
   to implement it from the file alone. Every plan includes: context, current state,
   SQLite reference, step-by-step instructions, files to touch, and a verify command.

## Verification Strategy

### Per-phase verification
Each sub-plan ends with a phase-specific `go test -run` command that MUST exit 0.

### Global verification (after P10)
```bash
# 1. Full test suite — zero FAIL
go test -count=1 ./... 2>&1 | tee /tmp/frigolite_full.log
! grep -q "FAIL" /tmp/frigolite_full.log

# 2. Quality gates
make quality

# 3. SOLID architecture
go test -run TestSOLID_ ./...

# 4. Count remaining sub-test FAILs (must be 0)
go test -v -count=1 . 2>&1 | grep -c "^    --- FAIL" | xargs test 0 -eq
```

## Key Reference Paths

| Resource | Path |
|----------|------|
| SQLite C source | `/Users/muaddib/dev/sqlite/src/` |
| SQLite FTS3 source | `/Users/muaddib/dev/sqlite/ext/fts3/` |
| SQLite FTS5 source | `/Users/muaddib/dev/sqlite/ext/fts5/` |
| SQLite TCL tests | `/Users/muaddib/dev/frigolite/ori/sqlite/test/` |
| Frigolite test data (JSON) | `testdata/*.json` (696 files) |
| Frigolite compat tests | `frigolite_sqlite_compat_test.go` (1 088 functions) |
| Frigolite harness | `frigolite_harness_test.go` |
| Frigolite test helpers | `frigolite_test.go` (`setupDB`, `checkQueryResult`, `checkExecOK`) |
| Converters | `tools/convert_compat_test.py`, `tools/convert_compat_json.py` |
| sqlite3 binary (oracle) | `/usr/bin/sqlite3` (v3.51.0) |
| SOLID tests | `frigolite_solid_test.go` |

## How to Use This Plan (For Sub-Agents)

1. **Read the master plan** (this file) for context and dependency ordering.
2. **Read your assigned sub-plan** in full before touching any code.
3. **Read the referenced SQLite C source** sections — they are the behavioural spec.
4. **Implement steps in order** — each step builds on the previous.
5. **Run the verify command** after each step, not just at the end.
6. **Run `make quality` + SOLID tests** before declaring the phase complete.
7. **Update the progress table** below when done.

## Progress Tracking

| Phase | Description | Status | FAIL before | FAIL after | Notes |
|-------|-------------|--------|-------------|------------|-------|
| P0 | Test Infrastructure | 🔲 Not started | Unknown | — | Reveals hidden failures |
| P1 | Type Affinity & NULL | 🔲 Not started | ~7 | — | |
| P2 | Parser (WINDOW, CTE) | 🔲 Not started | Enabler | — | Unblocks P3 |
| P3 | ALTER TABLE | 🟡 In progress | ~99 | ~17 (altertab3) | Token-level rename + trigger name fix |
| P4 | Query Planner & ANALYZE | 🔲 Not started | ~55 | — | |
| P5 | Auto-Index & JOIN | 🔲 Not started | ~15 | — | |
| P6 | ATTACH Database | 🔲 Not started | ~14 | — | Multi-DB architecture |
| P7 | C API Go Layer | 🔲 Not started | Unblocks 123 files | — | |
| P8 | FTS3/4/5 | 🔲 Not started | ~59+ | — | Largest effort |
| P9 | amatch & vtab | 🔲 Not started | ~3 | — | |
| P10 | Quality & Final | 🔲 Not started | — | — | gocognit + full green |

## Archived Plans

The original plan files (PLAN-P1-ALTER.md, PLAN-P2-ANALYZE.md, etc.) are in
`plans/archive/`. They are superseded by the new plans but preserved for reference.
