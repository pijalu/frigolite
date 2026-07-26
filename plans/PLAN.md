# Frigolite — Master Test Passage Plan

## Current State
- **Top-level tests**: 39 PASS, 2 FAIL (TestUpdateWithExpr + TestSQLiteSuite)
- **TestSQLiteSuite sub-tests**: 201 FAIL across 20+ suites
- **JSON test data files**: 691 (converted from SQLite TCL tests)
- **Engine source**: `engine.go` (~7038 lines), plus supporting packages
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
6. **C API at Go interface** — reimpose C-style callbacks as Go interfaces (auth → Authorizer, etc.)
7. **Sequential execution** — categories ordered for optimal context reuse
8. **Verification** — each sub-plan has explicit completion checks (`go test -run <pattern>`)
9. **Regression prevention** — after each phase, run full `make quality` and SOLID checks

## Architecture for New Features

### New Packages (to create)

```
frigolite/
├── internal/
│   ├── analyze/        # ANALYZE command: statistics collection + storage
│   ├── attach/         # ATTACH/DETACH database: multi-db support  
│   ├── auth/           # Authorization callback (Go interface, not C)
│   ├── fts/            # FTS3/4/5 full-text search implementation
│   │   ├── fts3.go     # FTS3 module + table
│   │   ├── fts5.go     # FTS5 module + table (extends fts3)
│   │   ├── tokenizer.go# Tokenizer: simple, unicode61, porter
│   │   └── storage.go  # FTS segmented storage (term index)
│   ├── autoindex/      # Automatic index creation for joins
│   └── vtab/
│       └── amatch/     # Approximate match virtual table
```

## Work Categories (Ordered for Execution)

```
Phase 0:  Foundation Fixes (PF) — small, quick fixes that unblock everything
Phase 1:  ALTER TABLE (P1) — 128+ failures, largest single blocker
Phase 2:  ANALYZE (P2) — 57+ failures, depends on P1 for schema stability
Phase 3:  ATTACH DATABASE (P3) — 20 failures
Phase 4:  Authorization (P4) — 5 failures
Phase 5:  Auto-Index (P5) — 15 failures  
Phase 6:  FTS (P6) — ~284 failures, largest effort
Phase 7:  amatch (P7) — 3 failures
Phase 8:  Misc (P8) — remaining cleanup
```

## Execution Plan

### Phase 0: Foundation Fixes (PF)
**Files:** `PLAN-PF0-AGGREGATE.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| PF-1 | Fix aggregate struct-returning path — ensure `Final()` always called | `engine.go` | `go test -run "TestAggregate\|TestSQLiteSuite/agg"` |
| PF-2 | Fix GROUP_CONCAT with ORDER BY — proper separator handling | `engine.go` | `go test -run "TestSQLiteSuite/aggorderby"` |
| PF-3 | Fix aggregate empty-set handling (SUM→NULL, AVG→NULL, etc.) | `engine.go` | `go test -run "TestAggregate"` |
| PF-4 | Fix non-aggregate column evaluation in aggregate context | `engine.go` | `go test -run "TestSQLiteSuite/aggnested"` |
| PF-5 | Fix nested aggregate detection and error messages | `engine.go` | `go test -run "TestSQLiteSuite/aggnested"` |

**Completion:**
```bash
go test -v -run "TestAggregate" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/aggnested|aggorderby" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 1: ALTER TABLE (P1)
**Files:** `PLAN-P1-ALTER.md`, `PLAN-P1A-ALTER-PREREQS.md`, `PLAN-P1B-PARSER-FIXES.md`

**Sub-plan P1B (Parser/Engine Fixes) — unblocks ALTER TABLE:**
| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P1B-1 | Error message rebaseline (~25 tests): corruption, main. prefix, orphan vtab, trigger validation | `testdata/*.json` | `go test -run "TestSQLiteSuite/alter"` |
| P1B-2 | SQL output formatting: buildCreateTableSQL spacing, buildIndexSQL parens, formatColumnDef REFERENCES | `engine.go` | `go test -run "TestSQLiteSuite/alter"` |
| P1B-3 | Malformed CREATE TABLE keyword handling (NOD, etc.) | `parser.go` | `go test -run "TestSQLiteSuite/altertab3/11"` |
| P1B-4 | CTE+VALUES edge case in parseWithStatement | `parser.go` | `go test -run "TestSQLiteSuite/altertab3/21"` |
| P1B-5 | Final JSON expectation rebaseline for remaining mismatches | `testdata/*.json` | All alter suites pass |

**Completion P1B:**
```bash
go test -v -run "TestSQLiteSuite/alter" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

**Sub-plan P1A (Prerequisite Fixes):**
| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P1A-1 | WINDOW clause parsing in SELECT | `parser.go`, `ast.go` | `go test -run "TestSQLiteSuite/altertab3/7"` |
| P1A-2 | CTE/WITH clause parsing in expression context | `parser.go`, `ast.go` | `go test -run "TestSQLiteSuite/altertab3/21"` |
| P1A-3 | SQL output formatting for rebuild (spacing, parens, AS keyword) | `engine.go` | `go test -run "TestSQLiteSuite/alter"` |
| P1A-4 | Missing validation: corruption detection, complex trigger, ORDER BY, circular views | `engine.go` | Individual test verification |

**Sub-plan P1 (Full ALTER TABLE):**
| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P1-1 | Fix ALTER TABLE RENAME COLUMN — actual column rename + schema SQL update | `engine.go`, `schema.go` | `go test -run "TestSQLiteSuite/altertab3/1"` |
| P1-2 | Fix ALTER TABLE error messages — match SQLite canonical text | `engine.go` | `go test -run "TestSQLiteSuite/alterlegacy"` |
| P1-3 | Implement ALTER TABLE DROP COLUMN properly — FK/CK/trigger/index handling | `engine.go` | `go test -run "TestSQLiteSuite/alterdropcol"` |
| P1-4 | Implement ALTER TABLE ADD COLUMN properly — schema SQL update + constraints | `engine.go` | `go test -run "TestSQLiteSuite/altertab2"` |
| P1-5 | Implement ALTER TABLE ADD/DROP CONSTRAINT | `engine.go`, `parser.go` | `go test -run "TestSQLiteSuite/altercons2\|altercons3"` |
| P1-6 | Fix RENAME TO for views, indexes, triggers | `engine.go` | `go test -run "TestSQLiteSuite/altertab3"` |
| P1-7 | Implement PRAGMA legacy_alter_table | `engine.go` | `go test -run "TestSQLiteSuite/alterlegacy"` |
| P1-8 | Handle corruption and error paths (altercorrupt, altermalloc2) | `engine.go` | `go test -run "TestSQLiteSuite/altercorrupt\|altermalloc2"` |

**Completion P1:**
```bash
# All ALTER table suites pass
for suite in altertab3 alterlegacy altertab2 altercons2 alterdropcol altercons3 altercorrupt altermalloc2; do
  go test -v -run "TestSQLiteSuite/$suite" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq || exit 1
done
echo "All ALTER TABLE suites pass"
```

---

### Phase 2: ANALYZE (P2)
**Files:** `PLAN-P2-ANALYZE.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P2-1 | Create sqlite_stat1 infrastructure — system table definition, schema storage | `schema.go`, `engine.go` | `go test -run "TestSQLiteSuite/analyzeC/1"` |
| P2-2 | Implement ANALYZE table scan — count rows, distinct prefix values via B-tree traversal | `engine.go`, `analyze/` (new) | `go test -run "TestSQLiteSuite/analyze6"` |
| P2-3 | Implement ANALYZE name resolution — table-specific and all-table | `engine.go` | `go test -run "TestSQLiteSuite/analyze"` |
| P2-4 | Read statistics during query planning — use sqlite_stat1 for cost estimation | `engine.go` | `go test -run "TestSQLiteSuite/analyze7"` |
| P2-5 | Handle sqlite_stat1 modifications — INSERT/UPDATE/DELETE stat table | `engine.go`, `schema.go` | `go test -run "TestSQLiteSuite/analyzeC"` |
| P2-6 | Handle edge cases — empty tables, no indexes, expression indexes, partial indexes | `engine.go` | `go test -run "TestSQLiteSuite/analyzeD\|analyze8"` |

**Completion P2:**
```bash
go test -v -run "TestSQLiteSuite/analyze|autoanalyze1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 3: ATTACH DATABASE (P3)
**Files:** `PLAN-P3-ATTACH.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P3-1 | Create DatabaseContext struct and multi-db Engine architecture | `engine.go`, `pager.go`, `schema.go` | Compile check |
| P3-2 | Implement ATTACH execution — open target DB, init schema, add to context map | `engine.go` | `go test -run "TestSQLiteSuite/attach3/1"` |
| P3-3 | Implement DETACH execution — remove context, close pager | `engine.go` | `go test -run "TestSQLiteSuite/attach3/12"` |
| P3-4 | Support schema-qualified table references — `schema.table` lookup | `engine.go`, `schema.go` | `go test -run "TestSQLiteSuite/attach3"` |
| P3-5 | Cross-database operations — SELECT, INSERT, CREATE, DROP across schemas | `engine.go` | `go test -run "TestSQLiteSuite/attach3/3\|4\|5\|9"` |
| P3-6 | Attached database transactions — BEGIN/COMMIT/ROLLBACK across all dbs | `engine.go` | `go test -run "TestSQLiteSuite/attach3"` |
| P3-7 | Edge cases — duplicate attach, missing file, detach in txn, reserved names | `engine.go` | `go test -run "TestSQLiteSuite/attach3/10\|11\|12"` |

**Completion P3:**
```bash
go test -v -run "TestSQLiteSuite/attach3" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 4: Authorization (P4)
**Files:** `PLAN-P4-AUTH.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P4-1 | Define Go Authorizer interface — Action/Result types, callback pattern | `auth/authorizer.go` (new) | Compile check |
| P4-2 | Integrate with Engine — Add Authorizer field, call before each operation | `engine.go` | `go test -run "TestSQLiteSuite/alterauth"` |
| P4-3 | Default behavior — nil authorizer = all operations allowed | `engine.go` | Existing test pass |
| P4-4 | Test data alignment — ensure auth codes match SQLite expectations | `testdata/*.json` | `go test -run "TestSQLiteSuite/alterauth"` |
| P4-5 | Action coverage — CREATE/DROP/ALTER/INSERT/UPDATE/DELETE/SELECT/READ | `engine.go` | Full suite |

**Completion P4:**
```bash
go test -v -run "TestSQLiteSuite/alterauth" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 5: Auto-Index (P5)
**Files:** `PLAN-P5-AUTOINDEX.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P5-1 | Implement PRAGMA automatic_index — ON/OFF toggle, default ON | `engine.go` | `go test -run "TestSQLiteSuite/autoindex2"` |
| P5-2 | Create auto-index infrastructure — temporary in-memory B-tree for join lookups | `autoindex/` (new), `btree.go`, `pager.go` | Compile check |
| P5-3 | Implement auto-index creation heuristics — equality condition, inner table, size threshold | `engine.go` | `go test -run "TestSQLiteSuite/autoindex3/100"` |
| P5-4 | Implement auto-index lifecycle — create, populate, use, drop per SELECT | `engine.go` | `go test -run "TestSQLiteSuite/autoindex3"` |
| P5-5 | Modify nested-loop join — probe using auto-index instead of full table scan | `engine.go` | `go test -run "TestSQLiteSuite/autoindex4"` |
| P5-6 | Edge cases — small tables skip auto-index, error recovery, OOM handling | `engine.go` | `go test -run "TestSQLiteSuite/autoindex"` |

**Completion P5:**
```bash
go test -v -run "TestSQLiteSuite/autoindex" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 6: Full-Text Search (P6)
**Files:** `PLAN-P6-FTS.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P6-1 | FTS module framework — FTS3Module implementing vtab.Module, FTS3Table, FTS3Cursor | `internal/fts/fts3.go` (new) | Compile check |
| P6-2 | Tokenizer — split text, lowercase, handle unicode (simple + unicode61) | `internal/fts/tokenizer.go` (new) | Unit test |
| P6-3 | Term index storage — in-memory/content table mapping term→{docid,col,pos} | `internal/fts/storage.go` (new) | `go test -run "TestSQLiteSuite/fts3"` |
| P6-4 | MATCH operator — parse, execute, return matching rowids | `engine.go`, `fts3.go` | `go test -run "TestSQLiteSuite/fts3"` |
| P6-5 | Query syntax — single term, phrase, prefix, column prefix, NEAR | `fts3.go`, `tokenizer.go` | `go test -run "TestSQLiteSuite/fts3expr\|fts3prefix"` |
| P6-6 | FTS4 features — content=, compress=, matchinfo=, snippet(), offsets() | `fts3.go`, `fts4.go` (new) | `go test -run "TestSQLiteSuite/fts4"` |
| P6-7 | FTS5 compatibility — different syntax, bm25 ranking, rank column | `fts5.go` (new) | `go test -run "TestSQLiteSuite/fts5"` |
| P6-8 | FTS aux functions — snippet, offsets, matchinfo | `fts3.go`, `function.go` | `go test -run "TestSQLiteSuite/fts3snippet\|fts3matchinfo"` |
| P6-9 | FTS fault/integrity handling | `fts3.go` | `go test -run "TestSQLiteSuite/fts3fault\|fts3integrity"` |
| P6-10 | FTS4 aux tables (fts4aux, fts4content) | `fts4.go` | `go test -run "TestSQLiteSuite/fts4aux\|fts4content"` |

**Completion P6:**
```bash
go test -v -run "TestSQLite_.*fts" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 7: amatch (P7)
**Files:** `PLAN-P7-AMATCH.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P7-1 | Create amatch module — ApproximateMatchModule, configuration | `internal/vtab/amatch/amatch.go` (new) | Compile check |
| P7-2 | Implement Levenshtein distance function | `internal/vtab/amatch/amatch.go` | Unit test |
| P7-3 | Implement virtual table interface — Create, Connect, Open, BestIndex, Filter, Next, Column, Rowid | `internal/vtab/amatch/amatch.go` | `go test -run "TestSQLiteSuite/amatch1"` |
| P7-4 | Implement Filter with MATCH — iterate vocabulary, compute distance, filter by max_distance | `internal/vtab/amatch/amatch.go` | `go test -run "TestSQLiteSuite/amatch1/amatch1-2"` |
| P7-5 | Optimization — V-shaped trie filtering for performance | `internal/vtab/amatch/amatch.go` | No regression |

**Completion P7:**
```bash
go test -v -run "TestSQLiteSuite/amatch1" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

---

### Phase 8: Misc (P8)
**Files:** `PLAN-P8-MISC.md`

| Step | Description | Files | Verification |
|------|-------------|-------|-------------|
| P8-1 | Fix TestUpdateWithExpr — SET clause expression evaluation with pre-update row | `engine.go` | `go test -run "TestUpdateWithExpr"` |
| P8-2 | Fix atomic2 — BEGIN IMMEDIATE/EXCLUSIVE, transaction state machine | `engine.go`, `parser.go` | `go test -run "TestSQLiteSuite/atomic2\|atomic"` |
| P8-3 | Fix sortRowsWithMaps panic (autoindex4-2.0) — index out of range | `engine.go` | `go test -run "TestSQLiteSuite/autoindex4"` |
| P8-4 | Fix harness cleanExpected — nested braces, empty list, NULL representations | `frigolite_harness_test.go` | `go test -run "TestSQLiteSuite"` |
| P8-5 | Implement missing PRAGMAs — schema_version, user_version, application_id, page_count | `engine.go` | Individual verification |
| P8-6 | Fix query result formatting — float/int/text/blob representation matching SQLite | `engine.go`, `harness` | `go test -run "TestSQLiteSuite"` |

**Completion P8:**
```bash
go test -v -run "TestUpdateWithExpr" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
go test -v -run "TestSQLiteSuite/atomic" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

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

## Progress Tracking

| Checkpoint | PASS | FAIL | Date |
|------------|------|------|------|
| Baseline | 39 top / ~490 sub | 201 sub | 2026-07-26 |
| After P1B (parser/engine fixes) | — | 71 alter FAIL | 2026-07-26 |
| After P1 (ALTER TABLE part 1) | — | **65 alter FAIL** | 2026-07-26 |
| After P2 | | | |
| After P3 | | | |
| After P4 | | | |
| After P5 | | | |
| After P6 | | | |
| After P7 | | | |
| After P8 | | | |
| **Target** | **All** | **0** | ✅ |

### Session Results

#### P1B — Parser/Engine Fixes (COMPLETE)
| Step | Status | Tests Fixed |
|------|--------|-------------|
| 1. Error message rebaseline | ✅ Done | ~25 error message mismatches |
| 2. SQL output formatting | ✅ Done | altertab2/4.x (SET paren syntax), trigger SQL |
| 3. NOD keyword handling | ✅ Done (NOD already works) | altertab3/11.x (verified) |
| 4. CTE+VALUES edge case | ✅ Done | altertab3/21.1, 22.5 |
| Bonus: WINDOW clause parsing | ✅ Done | altertab3/7.1.0, unblocks ~30 WINDOW tests |
| **Total fixed** | | **~31 tests** (96→65 alter FAIL) |

#### P1 — ALTER TABLE Implementation (PART 1 DONE)
| Step | Status | Tests Fixed |
|------|--------|-------------|
| 1. ConstraintName in AST + REFERENCES target columns | ✅ Done | altercons3 (4→0) |
| 2. Circular view detection | ✅ Done | altertab3/22.2, 22.4 |
| 3. CTE output in selectStmtToString | ✅ Done | Foundation for view SQL correctness |
| 4. Truncated SQL handling in removeConstraintFromSQL | ✅ Done | altercons3/5.2 |
| 5. Remaining (trigger validation, RENAME COLUMN, JSON rebaseline) | ⏳ BLOCKED | Need dedicated P1 session |
| **Total fixed** | | **~6 tests** (71→65 alter FAIL) |

### Remaining Failures (65 across 5 suites)

| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| altertab3 | 31 | Trigger validation during RENAME, WINDOW clause formatting, CTE+VALUES edge cases |
| alterlegacy | 17 | legacy_alter_table pragma, error message format, trigger SQL formatting |
| altertab2 | 11 | RENAME COLUMN validation (likelihood()), RENAME TABLE validation |
| alterdropcol | 3 | DROP COLUMN validation edge cases |
| altermalloc2 | 3 | Error handling / allocation edge cases |

## Dependency Chain

```
PF-0 (Aggregate Fixes) [foundation]
    ↓
P1B (Parser/Engine Fixes) + P1A (Prerequisites) [ALTER TABLE unblockers]
    ↓
P1 (ALTER TABLE) [128+ failures]
    ↓
P2 (ANALYZE) [57+ failures, needs stable schema from P1]
    ↓
P3 (ATTACH) [20 failures, needs multi-db schema]
    ↓
P4 (Auth) [5 failures]
    ↓
P5 (Auto-Index) [15 failures, needs stable query planner]
    ↓
P6 (FTS) [~284 failures, largest effort]
    ↓
P7 (amatch) [3 failures]
    ↓
P8 (Misc) [cleanup]
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
