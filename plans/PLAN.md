# Frigolite — Test Passage Plan

## Current State
- **1088** auto-generated Go test functions (`frigolite_sqlite_compat_test.go`)
- **717** JSON test data files (`testdata/*.json`)
- **395** passing / **286** failing sub-tests in `TestSQLiteSuite` (JSON suite)
- **7** failing manual tests (aggregates + update)
- **Total: 681 tests, 293 failing, 388 passing**

## Goal
Make ALL tests pass by implementing missing SQL features in a SOLID, idiomatic Go approach. No CGO, no external dependencies.

## Guiding Principles
1. **Test surface is sacred** — do not modify tests except for setup/teardown issues
2. **Implement features in order of dependency** — parser → execution → storage
3. **Each category is a self-contained goal** — achieveable and verifiable independently
4. **SOLID design** — single responsibility per package, clean interfaces
5. **Go stdlib first** — use Go standard library when possible (collections, compression, etc.)
6. **Sequential execution** — categories are ordered for optimal context reuse
7. **Verification** — each sub-plan has explicit completion checks (`go test -run <pattern>`)

## Architecture for New Features

### New Packages (to create)

```
frigolite/
├── internal/
│   ├── analyze/        # ANALYZE command: statistics collection + storage
│   ├── attach/         # ATTACH/DETACH database: multi-db support  
│   ├── auth/           # Authorization callback (Go interface, not C)
│   ├── fts/            # FTS3/4/5 implementation
│   ├── autoindex/      # Automatic index creation for joins
│   └── vtab/amatch/    # Approximate match virtual table (extension)
```

## Work Categories (ordered for execution)

### Phase 0: Foundation Fixes
#### PF-0: Aggregate Function Bugs
**7 manual tests + 25 compat sub-tests**  
**Files:** `engine.go` (exec), `function.go`  
**Current:** Aggregates return struct objects instead of scalar values in some paths; ORDER BY in aggregates broken (producing `&{0 84}` style struct literals); AVG produces 0 instead of correct values; GROUP_CONCAT returns wrong format  
**Fix:** Debug the evalAggregateExpr → evalAggFuncCall path to ensure Final() is always called; fix the non-aggregate-column evaluation in aggregate context

### Phase 1: ALTER TABLE
**128+ failing compat sub-tests** (altertab3-41, alterlegacy-33, altertab2-22, altercons2-18, alterdropcol-8, altercons3-4, altercorrupt-3, altermalloc2-2)  
**Files:** `parser.go`, `engine.go`, `schema.go`  
**Current:** RENAME COLUMN returns empty result without actually renaming; DROP COLUMN has incomplete validation; error messages don't match SQLite; schema SQL not updated properly  

### Phase 2: ANALYZE
**57+ failing compat sub-tests** (analyzeC-24, analyzeE-23, analyze7-14, autoanalyze1-11, analyze6-3, analyzeD-2, analyze8-2)  
**Current:** ANALYZE is a complete no-op (returns success without doing anything)  

### Phase 3: ATTACH DATABASE
**20 failing compat sub-tests**  
**Current:** ATTACH is parsed but execution returns empty result with no multi-db support  

### Phase 4: Authorization  
**5 failing compat sub-tests**  
**Current:** No authorization mechanism exists  

### Phase 5: Auto-Index
**15 failing compat sub-tests** (autoindex3-6, autoindex4-5, autoindex2-2)  
**Current:** No automatic index creation during query planning for joins without explicit indexes  

### Phase 6: Virtual Table Extensions (FTS)
**New package: `internal/fts/`**  
**Current:** FTS3/4/5 registered as NoopModule — always works but produces no content  

### Phase 7: Virtual Table Extensions (amatch)
**3 failing compat sub-tests**  
**Current:** amatch needs to be implemented as a proper virtual table module  

### Phase 8: Misc Fixes
**atomic2-2, atof-2, atomic-2**  
Various remaining small issues

---

## Execution Tracking

Each phase is a self-contained goal to be run with the `goal` tool. The completion check verifies all tests in the phase pass.

| Phase | Goal ID | Test Pattern | Est. Failures | Est. Effort | Priority |
|-------|---------|-------------|---------------|-------------|----------|
| PF-0 Aggregate | `pass-agg` | `TestAggregate\|TestSQLiteSuite/agg` | 32 | Small | **HIGH** (foundation) |
| P1 ALTER TABLE | `pass-alter` | `TestSQLiteSuite/alter` | 128 | Large | HIGH |
| P2 ANALYZE | `pass-analyze` | `TestSQLiteSuite/analyze\|autoanalyze` | 57 | Medium | HIGH |
| P3 ATTACH | `pass-attach` | `TestSQLiteSuite/attach` | 20 | Medium | HIGH |
| P4 Auth | `pass-auth` | `TestSQLiteSuite/alterauth` | 5 | Small | MEDIUM |
| P5 Auto-Index | `pass-autoindex` | `TestSQLiteSuite/autoindex` | 15 | Medium | MEDIUM |
| P6 FTS | `pass-fts` | `TestSQLite_.*fts` | ~30 | Large | MEDIUM |
| P7 amatch | `pass-amatch` | `TestSQLiteSuite/amatch` | 3 | Small | LOW |
| P8 Misc | `pass-misc` | `TestUpdateWithExpr\|TestSQLiteSuite/atomic\|analyzeD\|analyze8` | 8 | Small | LOW |

## Progress Tracking

**After implementing each phase, run the full test suite and record results:**

```bash
# Quick verification for a specific phase
go test -v -run "<pattern>" . 2>&1 | grep -E "PASS|FAIL" | tail -5

# Full suite verification
go test -v . 2>&1 | grep -E "--- (PASS|FAIL)" | wc -l
go test -v . 2>&1 | grep "FAIL" | wc -l

# Progressive improvement tracking
go test -v . 2>&1 > /tmp/test_results.txt
echo "PASS: $(grep -c '--- PASS' /tmp/test_results.txt), FAIL: $(grep -c '--- FAIL' /tmp/test_results.txt)"
```

## Initial baselines (to update as phases complete)

| Checkpoint | PASS | FAIL | Date |
|------------|------|------|------|
| Baseline | 395 | 286 | — |
| After PF-0 | | | |
| After P1 | | | |
| After P2 | | | |
| After P3 | | | |
| After P4 | | | |
| After P5 | | | |
| After P6 | | | |
| After P7 | | | |
| After P8 | | | |
| **Target** | **681** | **0** | ✅ |

## Detailed Sub-Plans

Each sub-plan file contains complete execution steps, root cause analysis, and verification commands:

| Plan | File | Focus |
|------|------|-------|
| PF-0: Aggregate | `PLAN-PF0-AGGREGATE.md` | Fix all aggregate function evaluation paths |
| P1: ALTER TABLE | `PLAN-P1-ALTER.md` | Full ALTER TABLE implementation |
| P2: ANALYZE | `PLAN-P2-ANALYZE.md` | ANALYZE command + sqlite_stat1 |
| P3: ATTACH | `PLAN-P3-ATTACH.md` | Multi-database support |
| P4: Auth | `PLAN-P4-AUTH.md` | Authorization interface |
| P5: Auto-Index | `PLAN-P5-AUTOINDEX.md` | Transient index creation for joins |
| P6: FTS | `PLAN-P6-FTS.md` | FTS3/4/5 virtual table |
| P7: amatch | `PLAN-P7-AMATCH.md` | Approximate match extension |
| P8: Misc | `PLAN-P8-MISC.md` | Remaining fixes |
