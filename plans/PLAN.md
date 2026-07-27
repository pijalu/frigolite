# Frigolite — Master Test Passage Plan (Updated 2026-07-27)

## Current State (Latest Full Run)

| Suite Category | Count | Status |
|---------------|-------|--------|
| Top-level tests (hand-written) | 39 PASS, 1 FAIL | ✅ Mostly PASS |
| TestSQLiteSuite sub-tests | ~460 FAIL across 25+ suites | ❌ Needs work |
| **TestUpdateWithExpr** | 1 FAIL | Manual test failing |
| Total JSON test files | 696 | Test data |

**Engine source:** `internal/exec/engine.go` (~8300 lines)
**Compatibility tests:** 1088 auto-generated test functions in `frigolite_sqlite_compat_test.go`

## Development Principles

1. **Test surface is sacred** — never modify tests except for setup/teardown issues
2. **Go stdlib first** — always prefer Go standard library (slices, maps, strings, hash, sort, regexp, compress, container/heap, etc.)
3. **SOLID design** — single responsibility per package, clean interfaces, dependency inversion
4. **C API → Go interface** — reimplement C-style callbacks as Go interfaces (auth→Authorizer, FTS→vtab.Module)
5. **No CGO, no external dependencies** — pure Go only
6. **Sequential execution** — categories ordered for optimal context reuse; complete one before starting next
7. **Regression prevention** — after each phase, run SOLID checks (`make quality` + `go test -run TestSOLID_ ./...`)
8. **Verification** — each sub-plan has explicit completion checks in the same format

## Dependency Chain

```
P1 (ALTER TABLE) [~108 failures — CURRENT BLOCKER]
    ↓
P2 (ANALYZE) [~48 failures — needs stable schema from P1]
    ↓
P3 (ATTACH DATABASE) [10 failures — needs multi-db architecture]
    ↓
P5 (Auto-Index) [13 failures — needs stable query planner]
    ↓
P6 (Full-Text Search) [~284 failures — largest phase]
    ↓
P7 (amatch) [2 failures — small vtab module]
    ↓
P8 (Misc) [5 failures — cleanup after all others]

Plus: TestUpdateWithExpr (can be fixed in P8 or independently)
```

## Phase Execution Order

Each phase is a self-contained goal. Complete one, verify zero-fail for the target suites, then proceed.

```
Session 1:  P1  — ALTER TABLE (108 failures, engine+parser work)
Session 2:  P2  — ANALYZE (48 failures, system table + statistics)
Session 3:  P3  — ATTACH (10 failures, multi-database architecture)
Session 4:  P5  — Auto-Index (13 failures, query planner work)
Session 5:  P6  — Full-Text Search (~284 failures, largest effort)
Session 6:  P7  — amatch (2 failures, small new module)
Session 7:  P8  — Misc (5 failures, cleanup)
```

## Progress Tracking

| Phase | Sub-phases | Current | Target |
|-------|-----------|---------|--------|
| P0 | Aggregate Fixes | ✅ 0 FAIL | 0 FAIL |
| P1 | ALTER TABLE | ❌ 108 FAIL | 0 FAIL |
| P2 | ANALYZE | ❌ 48 FAIL | 0 FAIL |
| P3 | ATTACH DATABASE | ❌ 10 FAIL | 0 FAIL |
| P5 | Auto-Index | ❌ 13 FAIL | 0 FAIL |
| P6 | Full-Text Search | ❌ ~284 FAIL | 0 FAIL |
| P7 | amatch | ❌ 2 FAIL | 0 FAIL |
| P8 | Misc | ❌ 5 FAIL | 0 FAIL |

## Key Reference Files

| Resource | Location |
|----------|----------|
| Original SQLite C source | `/Users/muaddib/dev/sqlite/src/` |
| Original SQLite tests | `/Users/muaddib/dev/sqlite/test/` |
| JSON test data | `/Users/muaddib/dev/frigolite/testdata/` |
| Handover doc | `/Users/muaddib/dev/frigolite/HANDOVER.md` |
| Code review | `/Users/muaddib/dev/frigolite/REVIEW.md` (if exists) |
| Go standard library | `/Users/muaddib/dev/go/` (Go toolchain) |

## Final Verification

After all phases:

```bash
# 1. Run all tests
go test -count=1 ./... 2>&1

# 2. Quality gates
make quality

# 3. SOLID architecture checks
go test -run TestSOLID_ ./...

# 4. Count remaining failures (should be 0)
go test -v -count=1 . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```
