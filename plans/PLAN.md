# Frigolite — Master Test Passage Plan (Updated)

## Current State
- **Top-level tests**: 39 PASS, 2 FAIL (TestUpdateWithExpr + TestSQLiteSuite)
- **TestSQLiteSuite sub-tests**: 192 FAIL across 19 suites
- **JSON test data files**: 691 (converted from SQLite TCL tests)
- **Engine source**: `engine.go` (~7108 lines), plus supporting packages (11548 total)
- **Original SQLite source**: `/Users/muaddib/dev/sqlite` (1192 test files, full C reference)
- **Go toolchain**: `/Users/muaddib/dev/go` (Go standard library)

## Goal
Make ALL tests pass by implementing missing SQL features in SOLID, idiomatic Go.
No CGO, no external dependencies.

## Guiding Principles

1. **Test surface is sacred** — do not modify tests except for setup/teardown issues
2. **Implement features in order of dependency** — parser → execution → storage
3. **Each category is a self-contained goal** — achievable and verifiable independently
4. **SOLID design** — single responsibility per package, clean interfaces, dependency inversion
5. **Go stdlib first** — use Go standard library when possible (slices, maps, strings, regexp, hash/crc32, compress, container/heap, sort, math, etc.)
6. **C API as Go interface** — reimplement C-style callbacks as Go interfaces (auth → Authorizer, etc.)
7. **Sequential execution** — categories ordered for optimal context reuse
8. **Verification** — each sub-plan has explicit completion checks (`go test -run <pattern>`)
9. **Regression prevention** — after each phase, run full `make quality` and SOLID checks

## Current Phase Status

| Phase | Name | Failures | Status | Priority |
|-------|------|----------|--------|----------|
| P0 | Aggregate Fixes | 0 | ✅ DONE | Foundation |
| P1 | ALTER TABLE | 65 | ⏳ PARTIAL | High |
| P2 | ANALYZE | 57 | ❌ TODO | High |
| P3 | ATTACH DATABASE | 20 | ❌ TODO | Medium |
| P5 | Auto-Index | 15 | ❌ TODO | Medium |
| P6 | Full-Text Search | ~284 | ❌ TODO | High |
| P7 | amatch | 3 | ❌ TODO | Low |
| P8 | Misc | 12 | ❌ TODO | Low |

**Note:** P4 (Auth) tests pass already — no work needed.

## Dependency Chain

```
P0 (Aggregate) [DONE]
    ↓
P1 (ALTER TABLE) [65 failures — CURRENT BLOCKER]
    ↓
P2 (ANALYZE) [57 failures — needs stable schema from P1]
    ↓
P3 (ATTACH) [20 failures — needs multi-db schema]
    ↓
P5 (Auto-Index) [15 failures — needs stable query planner]
    ↓
P6 (FTS) [~284 failures — depends on stable engine]
    ↓
P7 (amatch) [3 failures]
    ↓
P8 (Misc) [12 failures — cleanup after all others]
```

## Work Categories (Ordered for Execution)

### Phase 0: Foundation Fixes (P0) — ✅ DONE
**File:** `PLAN-PF0-AGGREGATE.md`

All aggregate tests pass:
- 7 manual aggregate tests ✅
- aggnested ✅ (0 FAIL)
- aggorderby ✅ (0 FAIL)
- aggfault ✅ (0 FAIL)

### Phase 1: ALTER TABLE (P1) — ⏳ 65 FAILURES
**Files:** `PLAN-P1-ALTER.md`, `PLAN-P1A-ALTER-PREREQS.md`, `PLAN-P1B-PARSER-FIXES.md`

| Sub-Phase | Failures | Description |
|-----------|----------|-------------|
| P1B (Parser/Engine Fixes) | ✅ COMPLETE | WINDOW clause, CTE+VALUES, constraint names, circular views |
| P1 (Main ALTER TABLE) | 65 | Trigger validation, RENAME COLUMN, PRAGMA legacy_alter_table, JSON rebaseline |

**Remaining by suite:**
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| altertab3 | 31 | Trigger body subquery validation, WINDOW clause formatting, CTE+VALUES edge cases |
| alterlegacy | 17 | PRAGMA legacy_alter_table, error message format, trigger SQL formatting |
| altertab2 | 11 | RENAME COLUMN validation (likelihood()), RENAME TABLE validation |
| alterdropcol | 3 | DROP COLUMN validation edge cases |
| altermalloc2 | 3 | Error handling / allocation edge cases |

**Completion:**
```bash
for suite in altertab3 alterlegacy altertab2 alterdropcol altermalloc2; do
  go test -v -run "TestSQLiteSuite/$suite" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq || exit 1
done
echo "All ALTER TABLE suites pass"
```

---

### Phase 2: ANALYZE (P2) — ❌ 57 FAILURES
**File:** `PLAN-P2-ANALYZE.md`

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| analyzeC | 24 | ANALYZE creates/manages sqlite_stat1 table |
| analyzeE | 23 | ANALYZE with various edge cases |
| analyze7 | 14 | ANALYZE behavior with specific schemas |
| autoanalyze1 | 11 | Automatic analyze after data changes |
| analyze6 | 3 | ANALYZE with indexes |
| analyzeD | 2 | ANALYZE with corruption |
| analyze8 | 2 | ANALYZE with expressions in indexes |

**Key steps:**
1. Create sqlite_stat1 infrastructure (system table definition, schema storage)
2. Implement ANALYZE table scan (count rows, distinct prefix values via B-tree traversal)
3. Implement ANALYZE name resolution (table-specific and all-table)
4. Read statistics during query planning (use sqlite_stat1 for cost estimation)
5. Handle sqlite_stat1 modifications (INSERT/UPDATE/DELETE stat table)
6. Handle edge cases (empty tables, no indexes, expression indexes, partial indexes)

**Completion:**
```bash
go test -v -run "TestSQLiteSuite/analyze|autoanalyze1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 3: ATTACH DATABASE (P3) — ❌ 20 FAILURES
**File:** `PLAN-P3-ATTACH.md`

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| attach3 | 20 | ATTACH/DETACH don't create separate database contexts |

**Key steps:**
1. Create DatabaseContext struct and multi-db Engine architecture
2. Implement ATTACH execution (open target DB, init schema, add to context map)
3. Implement DETACH execution (remove context, close pager)
4. Support schema-qualified table references (`schema.table` lookup)
5. Cross-database operations (SELECT, INSERT, CREATE, DROP across schemas)
6. Attached database transactions (BEGIN/COMMIT/ROLLBACK across all dbs)
7. Edge cases (duplicate attach, missing file, detach in txn, reserved names)

**Completion:**
```bash
go test -v -run "TestSQLiteSuite/attach3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 5: Auto-Index (P5) — ❌ 15 FAILURES
**File:** `PLAN-P5-AUTOINDEX.md`

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| autoindex3 | 6 | EXPLAIN QUERY PLAN shows SCAN instead of AUTO for auto-indexed joins |
| autoindex4 | 7 | Wrong JOIN results (NULL padding), panic in sortRowsWithMaps |
| autoindex2 | 2 | EXPLAIN QUERY PLAN shows SEARCH instead of AUTO |

**Key steps:**
1. Implement PRAGMA automatic_index (ON/OFF toggle, default ON)
2. Create auto-index infrastructure (temporary in-memory B-tree for join lookups)
3. Implement auto-index creation heuristics (equality condition, inner table, size threshold)
4. Implement auto-index lifecycle (create, populate, use, drop per SELECT)
5. Modify nested-loop join (probe using auto-index instead of full table scan)
6. Fix sortRowsWithMaps panic (index out of range)
7. Fix NULL padding in JOIN results

**Completion:**
```bash
go test -v -run "TestSQLiteSuite/autoindex" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 6: Full-Text Search (P6) — ❌ ~284 FAILURES
**File:** `PLAN-P6-FTS.md`

**Scope:** Implement FTS3/4/5 virtual table modules. Largest effort phase.

**Key steps:**
1. FTS module framework (FTS3Module implementing vtab.Module)
2. Tokenizer (split text, lowercase, handle unicode - simple + unicode61)
3. Term index storage (in-memory/content table mapping term→{docid,col,pos})
4. MATCH operator (parse, execute, return matching rowids)
5. Query syntax (single term, phrase, prefix, column prefix, NEAR)
6. FTS4 features (content=, compress=, matchinfo=, snippet(), offsets())
7. FTS5 compatibility (different syntax, bm25 ranking, rank column)
8. FTS aux functions (snippet, offsets, matchinfo)
9. FTS fault/integrity handling
10. FTS4 aux tables (fts4aux, fts4content)

**Completion:**
```bash
go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 7: amatch (P7) — ❌ 3 FAILURES
**File:** `PLAN-P7-AMATCH.md`

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| amatch1 | 3 | No amatch virtual table implementation |

**Key steps:**
1. Create amatch package with module, virtual table, cursor types
2. Implement Levenshtein distance function
3. Implement Module interface (Create, Connect, BestIndex, Open)
4. Implement Cursor interface (Next, Column, Rowid)
5. Implement Filter with MATCH query, vocabulary lookup, distance computation
6. Register in vtab registry
7. Engine integration for vocabulary table access

**Completion:**
```bash
go test -v -run "TestSQLiteSuite/amatch1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 8: Misc (P8) — ❌ 12 FAILURES
**File:** `PLAN-P8-MISC.md`

| Test | Failures | Issue |
|------|----------|-------|
| affinity2 | 5 | Column affinity handling, type conversion edge cases |
| atomic2 | 2 | Atomic commit behavior / ROLLBACK specifics |
| TestUpdateWithExpr | 1 | UPDATE with expression evaluation ordering |
| analyze8 | 2 | Moved from P2 — ANALYZE with expression indexes |

**Key steps:**
1. Fix TestUpdateWithExpr — SET clause expression evaluation with pre-update row
2. Fix atomic2 — BEGIN IMMEDIATE/EXCLUSIVE, transaction state machine
3. Fix affinity2 — column affinity type affinity application edge cases
4. Fix sortRowsWithMaps panic (autoindex4-2.0) — index out of range
5. Fix harness cleanExpected — nested braces, empty list, NULL representations

**Completion:**
```bash
go test -v -run "TestUpdateWithExpr" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/affinity2" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/atomic2" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Execution Order (Optimal Context Reuse)

```
Session 1:  P1 — ALTER TABLE (65 failures, continuation of current work)
Session 2:  P2 — ANALYZE (57 failures, heavy engine work)
Session 3:  P3 — ATTACH (20 failures, multi-db architecture)
Session 4:  P5 — Auto-Index (15 failures, query planner work)
Session 5:  P6 — Full-Text Search (~284 failures, largest effort)
Session 6:  P7 — amatch (3 failures, small new module)
Session 7:  P8 — Misc (12 failures, cleanup)
```

## Key Reference Files

| Resource | Location |
|----------|----------|
| Original SQLite tests | `/Users/muaddib/dev/sqlite/test/` |
| Original SQLite source | `/Users/muaddib/dev/sqlite/src/` |
| JSON test data | `/Users/muaddib/dev/frigolite/testdata/*.json` |
| Converter scripts | `/Users/muaddib/dev/frigolite/tools/` |
| Handover doc | `/Users/muaddib/dev/frigolite/HANDOVER.md` |
| Code review | `/Users/muaddib/dev/frigolite/REVIEW.md` |

## Progress Tracking

| Checkpoint | PASS | FAIL | Date |
|------------|------|------|------|
| Baseline | 39 top / ~490 sub | 201 sub | 2026-07-26 |
| After P1B (parser/engine fixes) | — | 71 alter FAIL | 2026-07-26 |
| After P1 (ALTER TABLE part 1) | — | **65 alter FAIL** | 2026-07-26 |
| After P1 (final) | — | 0 alter FAIL | TBD |
| After P2 (ANALYZE) | — | 0 analyze FAIL | TBD |
| After P3 (ATTACH) | — | 0 attach FAIL | TBD |
| After P5 (Auto-Index) | — | 0 autoindex FAIL | TBD |
| After P6 (FTS) | — | 0 fts FAIL | TBD |
| After P7 (amatch) | — | 0 amatch FAIL | TBD |
| After P8 (Misc) | — | 0 misc FAIL | TBD |
| **Target** | **All** | **0** | ✅ |

## Final Verification

After all phases, run the complete test suite:

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

## Goal Integration

Each phase maps to a goal with:
1. **Objective** — clear statement of what to implement
2. **Completion criterion** — machine-checkable (zero FAIL for targeted suites)
3. **Verify command** — `go test -run "<pattern>" .` with grep -c FAIL check
4. **Steps** — broken down into ordered todo items

When a phase is done, the next phase's goal is automatically queued and starts.
Blocking conditions (e.g., P2 needs P1 done) are enforced naturally by the dependency chain.
