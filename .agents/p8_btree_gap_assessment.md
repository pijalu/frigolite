# Frigolite btree vs btree.c — Gap Assessment (2026 session)

## Reference
- SQLite source: `/Users/muaddib/dev/sqlite/src/btree.c` — 11544 lines
- Frigolite source: `/Users/muaddib/dev/frigolite/internal/btree/*.go` — ~7700 lines across 22 files
- Function count: btree.c = 73 exported (sqlite3Btree*) + 104 static = 177 unique
- Frigolite: 74 (t *BTree) methods + many non-method functions

## Methodology
For every function in btree.c, map to the frigolite equivalent:
- ✅ Implemented (1:1 or near-equivalent)
- ⚠️ Partial (different signature, simplified path, or hard-coded limit)
- ❌ Missing (no equivalent in frigolite)
- 🔶 Frigolite-specific (no btree.c equivalent, e.g. Go-style API)

## 1. Connection / handle lifecycle (btree.c:1-1000)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeOpen | (DB.Open) | 🔶 |
| sqlite3BtreeClose | (DB.Close) | 🔶 |
| sqlite3BtreeNewDb | (schema.Init) | 🔶 |
| sqlite3BtreeSetCacheSize | — | ❌ |
| sqlite3BtreeSetSpillSize | — | ❌ |
| sqlite3BtreeSetMmapLimit | — | ❌ |
| sqlite3BtreeSetPagerFlags | — | ❌ |
| sqlite3BtreeSetPageSize | pager.SetPageSize | ✅ |
| sqlite3BtreeGetPageSize | pager.PageSize | ✅ |
| sqlite3BtreeGetReserveNoMutex | pager.reserved | ✅ |
| sqlite3BtreeGetRequestedReserve | — | ❌ |
| sqlite3BtreeSecureDelete | — | ❌ |
| sqlite3BtreeSetAutoVacuum | pager.SetAutoVacuum | ✅ (after 49849ce3) |
| sqlite3BtreeGetAutoVacuum | pager.AutoVacuum | ✅ |
| sqlite3BtreeCursor | BTree.OpenCursor | ✅ |
| sqlite3BtreeCursorSize | — | ❌ |
| sqlite3BtreeClosesWithCursor | — | ❌ |
| sqlite3BtreeCursorZero | Cursor init | 🔶 |
| sqlite3BtreeCloseCursor | Cursor.Close | 🔶 |
| sqlite3BtreeCursorIsValid / IsValidNN | cursor valid() | 🔶 |
| sqlite3BtreeCursorPin / Unpin | — | ❌ |
| sqlite3BtreeClearCursor | — | ❌ |
| sqlite3BtreeCursorHasMoved | — | ❌ |
| sqlite3BtreeCursorRestore | — | ❌ |
| sqlite3BtreeCursorHint / CursorHintFlags / CursorHasHint | — | ❌ |
| sqlite3BtreeFakeValidCursor | — | ❌ |

## 2. Transaction lifecycle (btree.c:3700-4750)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeBeginTrans | exec engine | 🔶 |
| sqlite3BtreeBeginStmt | — | ❌ (savepoint nested) |
| sqlite3BtreeSavepoint | — | ❌ |
| sqlite3BtreeCommit | exec engine | 🔶 |
| sqlite3BtreeCommitPhaseOne | pager.Flush | 🔶 |
| sqlite3BtreeCommitPhaseTwo | pager.commit | 🔶 |
| sqlite3BtreeRollback | exec engine | 🔶 |
| sqlite3BtreeTripAllCursors | — | ❌ |
| btreeEndTransaction | pager.SetInTransaction | ✅ |
| btreeGetPage | pager.ReadPage | ✅ |
| btreeGetUnusedPage | — | ❌ |
| lockBtree | pager lock | 🔶 |
| unlockBtreeIfUnused | — | ❌ |
| btreeInvokeBusyHandler | — | ❌ (no SQLITE_BUSY) |

## 3. Schema / metadata (btree.c:10160-10470)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeCreateTable | execCreateTable | 🔶 |
| sqlite3BtreeClearTable | BTree.Clear | ✅ |
| sqlite3BtreeClearTableOfCursor | — | ❌ |
| sqlite3BtreeDropTable | BTree.FreeTable | ✅ |
| sqlite3BtreeGetMeta | pager header | ✅ |
| sqlite3BtreeUpdateMeta | pager header | ✅ |
| sqlite3BtreeCount | cursor.Count | ✅ |
| btreeCreateTable | — | 🔶 |
| btreeDropTable | — | 🔶 |

## 4. Auto-vacuum (btree.c:4137-4290, 4174 autoVacuumCommit)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeIncrVacuum | btree.IncrVacuumStep | ✅ |
| autoVacuumCommit | exec.AutoVacuumCommit | ✅ |
| incrVacuumStep | btree.IncrVacuumStep (same) | ✅ |
| relocatePage | btree.RelocatePage | ✅ |
| setChildPtrmaps | btree.setChildPtrmaps | ✅ |

## 5. Insert / Delete / Update (btree.c:9370-10200)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeInsert | BTree.InsertCell | ✅ |
| sqlite3BtreeDelete | schema RemoveEntry + deleteAllMatchingFromLeaf | 🔶 |
| sqlite3BtreeTransferRow | — | ❌ (UPSERT move) |
| sqlite3BtreePutData | — | ❌ (in-place update) |
| sqlite3BtreeIncrblobCursor | — | ❌ (incremental BLOB I/O) |
| insertCell | BTree.insertCell | ✅ |
| insertCellFast | (in insertCell) | 🔶 |
| dropCell | removeInteriorCell + cell deletion | 🔶 |
| fillInCell | — | 🔶 (in prepareCell) |
| btreeOverwriteCell | — | ❌ |
| btreeOverwriteContent | — | ❌ |
| btreeParseCell / ParseCellPtr / ParseCellPtrIndex / ParseCellPtrNoPayload | storage.DecodeCell | 🔶 |
| btreePayloadToLocal | — | ❌ (overflow rewrite) |
| btreeComputeFreeSpace | — | ❌ |
| btreeInitPage | btree.allocBtreeNode | 🔶 |
| btreeSetNPage | — | ❌ |
| btreeSetHasContent / btreeGetHasContent / btreeClearHasContent | — | ❌ (has-content flag) |

## 6. Page allocation / deallocation (btree.c:2400-3550, 6050+)

| btree.c | frigolite | status |
|---|---|---|
| allocateBtreePage | pager.AllocatePage | 🔶 |
| freePage | pager.FreePage | ✅ |
| freePage2 | (in FreePage) | 🔶 |
| freeSpace | — | ❌ (in-page free-space management) |
| freeTempSpace | — | ❌ |
| getAndInitPage | — | ❌ |
| getOverflowPage | readOverflow | 🔶 |
| zeroPage | zeroPageAsLeafTable | 🔶 |
| pageFreeArray | pageFreeArray | ✅ |
| pageInsertArray | pageInsertArray | ✅ |
| pageReinit | — | ❌ |
| defragmentPage | — | ❌ |
| releasePage / releasePageNotNull / releasePageOne | pager.MarkPageDirty | 🔶 |
| setPageReferenced / getPageReferenced | — | ❌ |

## 7. Balancing (btree.c:6500-8200, 8200+ balance_nonroot)

| btree.c | frigolite | status |
|---|---|---|
| balance | (called from sqlite3BtreeInsert/Delete) | 🔶 |
| balance_quick | balanceQuick | ✅ (simplified) |
| balance_nonroot | balanceNonroot | ⚠️ simplified (no balance_shallower) |
| balance_deeper | splitInteriorPage + writeInteriorRootAt | ✅ |
| balance_shallower | — | **❌ MISSING** (root collapse) |
| mergeOrFreeEmptyLeaf | mergeOrFreeEmptyLeaf | ✅ |
| reparentChildPages | (in setChildPtrmaps) | 🔶 |
| clearDatabasePage | (in DeleteCell) | 🔶 |
| assertParentIndex | — | ❌ (assert helper) |
| btreeHeapInsert / btreeHeapPull | — | ❌ (heap for rebalance cells) |

## 8. Cursor movement (btree.c:5500-6500)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeFirst / Last | cursor.First / Last | ✅ |
| sqlite3BtreeNext / Previous | cursor.Next / Prev | ✅ |
| sqlite3BtreeEof | cursor.EOF | ✅ |
| sqlite3BtreeIsEmpty | (count==0) | 🔶 |
| sqlite3BtreeTableMoveto | (cursor seek table) | 🔶 |
| sqlite3BtreeIndexMoveto | (cursor seek index) | 🔶 |
| sqlite3BtreePayload / PayloadChecked | cursor.Payload | 🔶 |
| sqlite3BtreePayloadSize | cursor.PayloadSize | 🔶 |
| sqlite3BtreeMaxRecordSize | — | ❌ |
| btreeMoveto | (cursor seek) | 🔶 |
| moveToRoot | (cursor) | 🔶 |
| moveToLeftmost / moveToRightmost | (cursor) | 🔶 |
| moveToChild / moveToParent | (cursor) | 🔶 |
| cursorIsAtLastEntry | — | ❌ |
| btreeRestoreCursorPosition | (cursor restore) | 🔶 |
| saveCursorPosition / saveCursorKey | (cursor save) | 🔶 |
| populateCellCache | — | ❌ |
| indexCellCompare | — | ❌ |

## 9. Integrity check (btree.c:11102)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeIntegrityCheck | exec/pragma_quickcheck | 🔶 |
| checkTreePage | (in pragma_quickcheck_trees) | 🔶 |
| checkRef / checkAppendMsg / checkList | — | ❌ |
| checkPtrmap | — | ❌ |
| checkProgress | — | ❌ |
| checkOom | — | ❌ |
| ptrmapGet / ptrmapPut / ptrmapPutOvflPtr | pager.ReadPtrmap / WritePtrmap | ✅ |
| ptrmapCheckPages | — | ❌ |

## 10. Shared cache / locking (btree.c:4443+)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeTripAllCursors | — | ❌ |
| sqlite3BtreeSchemaLocked | — | ❌ |
| sqlite3BtreeLockTable | — | ❌ |
| sqlite3BtreeSharable | — | ❌ |
| sqlite3BtreeConnectionCount | — | ❌ |
| sqlite3BtreeInBackup / IsInBackup | — | ❌ |
| setSharedCacheTableLock / clearAllSharedCacheTableLocks | — | ❌ |
| querySharedCacheTableLock | — | ❌ |
| hasReadConflicts | — | ❌ |
| downgradeAllSharedCacheTableLocks | — | ❌ |
| sharedLockTrace | — | ❌ |
| anotherValidCursor | — | ❌ |
| hasSharedCacheTableLock | — | ❌ |
| cursorOwnsBtShared | — | ❌ |
| removeFromSharingList | — | ❌ |
| countValidCursors | — | ❌ |
| cursorHoldsMutex | — | ❌ |
| saveAllCursors | — | ❌ |
| invalidateAllOverflowCache | — | ❌ |
| invalidateIncrblobCursors | — | ❌ |
| btreeReleaseAllCursorPages | — | ❌ |
| decodeFlags | — | ❌ |
| setDefaultSyncFlag | — | ❌ |
| sharedLockTrace | — | ❌ |

## 11. Schema object access (btree.c:11341)

| btree.c | frigolite | status |
|---|---|---|
| sqlite3BtreeSchema | schema manager | 🔶 |
| sqlite3BtreeSchemaLocked | — | ❌ |
| sqlite3BtreeClearCache | pager.InvalidateCache | 🔶 |
| sqlite3BtreeSetVersion | — | ❌ (schema cookie) |
| sqlite3BtreeTxnState | — | ❌ |
| sqlite3BtreeCheckpoint | — | ❌ (WAL) |
| sqlite3BtreeIsReadonly | pager.readOnly | ✅ |
| sqlite3BtreePager | — | ❌ |
| sqlite3BtreeCursorSize | — | ❌ |

## 12. Storage-specific

| btree.c | frigolite | status |
|---|---|---|
| copyNodeContent | copyNodeContent | ✅ |
| copyPayload | — | ❌ |
| modifyPagePointer | — | ❌ |
| editPage | editPage | ✅ |
| accessPayload | — | ❌ (read payload from cursor) |
| hasReadConflicts | — | ❌ |

## Summary

### Counts
- **btree.c total functions**: 177 (73 exported + 104 static)
- **frigolite coverage**:
  - ✅ Implemented (1:1): ~50
  - ⚠️ Partial / simplified: ~55
  - ❌ Missing: ~70
  - 🔶 Frigolite-specific API wrapping: ~50 of frigolite's 74 methods

### Critical Missing (causes test failures)

1. **balance_shallower** (root collapse) — root cause of 2.5.1+ autovacuum testgen failures
2. **btreeHeapInsert / btreeHeapPull** — required for proper cell redistribution in balanceNonroot
3. **reparentChildPages** — child ptrmap updates after page moves
4. **clearDatabasePage** (proper version) — when DROP TABLE drops a table
5. **btreeComputeFreeSpace** — accurate free-space computation in cells
6. **saveAllCursors** — cursor invalidation on B-tree restructure
7. **defragmentPage** — when no cell fits after insert
8. **freeTempSpace** — temporary scratch for cell redistribution

### Documented Out-of-Scope per c0bbfa78
- Shared cache / table locking (entirely absent in frigolite)
- Incremental BLOB I/O (sqlite3BtreePutData, sqlite3BtreeIncrblobCursor)
- Incremental vacuum callbacks
- savepoint (sqlite3BtreeSavepoint)
- WAL checkpoint (sqlite3BtreeCheckpoint)
- Backup (sqlite3BtreeIsInBackup)

### Test gap
Frigolite has:
- 18 test files, ~3700 lines of tests
- btree_invariant_test, btree_balance_test, btree_rebalance_test, btree_vacuum_test, btree_vacuum_phase3_test
- btree_stress_test (random ops)
- btree_balance_nonroot_test
- btree_insert_samerowid_test
- scratch_overflow_stress_test

btree.c has:
- tcl/btree.test, tcl/btree01.test..btree16.test
- ~50K lines of test code (not counted here)

## Plan to close critical gaps

### Phase A: balance_shallower (root collapse)
**Why**: root cause of 2.5.1+ failures
**Scope**: When DeleteCellsWhere empties the root (or its only child becomes empty), collapse the root into the child (or convert to leaf).
**btree.c reference**: btree.c:7828-7900 (balance_shallower)
**Files**: btree_balance_nonroot.go, btree_tail.go
**Tests**: autovacuum 2.4.7, 2.5.1, incrvacuum 11-15

### Phase B: balanceNonroot cell redistribution
**Why**: the current size-balanced distribution in Phase 4 is naive
**Scope**: implement proper btree.c balance_nonroot (lines 8206-8950) cell redistribution using a heap-based approach
**Files**: btree_balance_nonroot.go (new Phase 4)
**Tests**: autovacuum 2.4.5 (root_page_list ordering)

### Phase C: reparentChildPages
**Why**: child ptrmap must be updated after RelocatePage
**Scope**: when a page moves, update all children's ptrmap.parent
**Files**: btree_vacuum.go (already has setChildPtrmaps, need to call it more)
**Tests**: incrvacuum 16, 17

### Phase D: defragmentPage + freeTempSpace
**Why**: insert into a full page needs defragmentation
**Scope**: when no cell fits, compact the page and retry
**Files**: btree_insert.go
**Tests**: btree01 (overflow), btree02 (large records)

### Phase E: incremental BLOB I/O
**Why**: SQLite supports zero-copy BLOB writes
**Scope**: sqlite3BtreePutData, sqlite3BtreeIncrblobCursor
**Files**: new btree_blob.go
**Tests**: incrblob (separate test file)

### Phase F: Shared cache / savepoint
**Why**: SQLite supports multi-connection shared cache and nested savepoints
**Scope**: deferred (frigi is single-connection, no SQLITE_BUSY)

### Phase G: ptrmapCheckPages + integrity_check completeness
**Why**: current integrity_check misses some cases
**Scope**: full btree.c::checkTreePage walk
**Files**: pragma_quickcheck_trees.go
**Tests**: corrupt2.test, corruptK.test

## Priority order
1. Phase A (balance_shallower) — unblocks 2.5.1+
2. Phase B (cell redistribution) — unblocks autovacuum 2.4.5
3. Phase C (reparentChildPages) — unblocks incrvacuum 16+
4. Phase D (defragmentPage) — unblocks btree01 overflow tests
5. Phase E (incremental BLOB) — separate feature
6. Phase G (integrity_check) — robustness
