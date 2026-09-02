# Btree Gap Assessment: frigolite vs btree.c

## Source references
- SQLite btree.c: `/Users/muaddib/dev/sqlite/src/btree.c` (11544 lines, 73 exported + 104 static = 177 unique functions)
- Frigolite btree: `/Users/muaddib/dev/frigolite/internal/btree/` (22 files, 19217 total lines including tests; ~3000 lines non-test)

## Coverage matrix (exported API)

| btree.c function | frigolite equivalent | Status |
|---|---|---|
| `sqlite3BtreeOpen` | `pager.Open` + `NewBTree` | ✅ (via pager) |
| `sqlite3BtreeClose` | `BTree.Close` (via `pager.Pager.Close`) | ✅ |
| `sqlite3BtreeSetPageSize` | n/a (pager-level) | ✅ |
| `sqlite3BtreeGetPageSize` | `BTree.PageSize()` | ✅ |
| `sqlite3BtreeSetAutoVacuum` | `pager.SetAutoVacuum` | ✅ |
| `sqlite3BtreeGetAutoVacuum` | `pager.AutoVacuum` | ✅ |
| `sqlite3BtreeBeginTrans` | `Engine.Begin` (via exec layer) | partial |
| `sqlite3BtreeCommit` | `Engine.Commit` (via exec layer) | partial |
| `sqlite3BtreeRollback` | `Engine.Rollback` (via exec layer) | partial |
| `sqlite3BtreeBeginStmt/Savepoint` | MISSING | ❌ (no savepoint support) |
| `sqlite3BtreeCursor` | `BTree.OpenCursor` | ✅ |
| `sqlite3BtreeCloseCursor` | `Cursor.Close` | ✅ |
| `sqlite3BtreeFirst/Last` | `Cursor.First/Last` | ✅ |
| `sqlite3BtreeNext/Previous` | `Cursor.Next/Prev` | ✅ |
| `sqlite3BtreeMoveto` (Table/Index) | `Cursor.Seek/SeekGE/SeekLE/SeekGT/SeekLT` | ✅ |
| `sqlite3BtreeEof` | `Cursor.EOF` | ✅ |
| `sqlite3BtreeInsert` | `BTree.InsertCell` | ✅ |
| `sqlite3BtreeDelete` | `BTree.DeleteCell` | ✅ |
| `sqlite3BtreePayload/PayloadSize` | `Cursor.Payload/PayloadSize` | ✅ |
| `sqlite3BtreeCreateTable` | `pager.AllocateRootpage` | ✅ |
| `sqlite3BtreeClearTable` | `BTree.FreeTable` | ✅ |
| `sqlite3BtreeDropTable` | `BTree.FreeTable` | ✅ |
| `sqlite3BtreeGetMeta/UpdateMeta` | `pager.HeaderPageCount/Header*` | partial |
| `sqlite3BtreeIncrVacuum` | `BTree.IncrVacuumStep` | ✅ |
| `sqlite3BtreeCommitPhaseOne/Two` | `pager.Flush` + autovacuum | partial |
| `sqlite3BtreeTripAllCursors` | MISSING | ❌ (no multi-cursor tracking) |
| `sqlite3BtreeIntegrityCheck` | exec/pragma_quickcheck (high-level) | partial |
| `sqlite3BtreePutData/IncrblobCursor` | MISSING | ❌ (no incremental BLOB I/O) |
| `sqlite3BtreeCursorHasMoved` | MISSING | ❌ |
| `sqlite3BtreeCursorRestore/Hint` | MISSING | ❌ |
| `sqlite3BtreeClearCache` | pager.InvalidateCache | ✅ |
| `sqlite3BtreeLockTable` | MISSING | ❌ (no shared cache table lock) |
| `sqlite3BtreeSecureDelete` | MISSING | ❌ |
| `sqlite3BtreeSetVersion` | MISSING | ❌ (no schema version field) |
| `sqlite3BtreeSetCacheSize/SpillSize/MmapLimit` | MISSING | ❌ |
| `sqlite3BtreeSetPagerFlags` | MISSING | ❌ |
| `sqlite3BtreeIsInBackup` | MISSING | ❌ |
| `sqlite3BtreeSchema/SchemaLocked` | MISSING | ❌ (no shared schema cache) |
| `sqlite3BtreeCheckpoint` | MISSING | ❌ (WAL checkpoint at btree layer) |
| `sqlite3BtreeTxnState` | MISSING | ❌ |
| `sqlite3BtreeCount` | `Cursor.Count` (via execquery) | partial |
| `sqlite3BtreeIsReadonly/IsEmpty` | MISSING | ❌ |
| `sqlite3BtreeClosesWithCursor` | MISSING | ❌ |
| `sqlite3BtreeSharable/ConnectionCount` | MISSING | ❌ (no shared cache mode) |
| `sqlite3BtreeTransferRow` | MISSING | ❌ |

## Coverage matrix (static helpers)

| btree.c static | frigolite equivalent | Status |
|---|---|---|
| `accessPayload` | `Cursor.accessPayload` | ✅ |
| `allocateBtreePage` | `BTree.allocBtreeNode` | partial (no BTALLOC_EXACT, no BTALLOC_LE separate paths) |
| `balance` | (combined into balanceNonroot + balanceQuick) | ✅ |
| `balance_deeper` | MISSING | ❌ (root split not implemented) |
| `balance_nonroot` | `BTree.balanceNonroot` | partial (Phase 4 redistribution buggy — root cause of 2.5.1) |
| `balance_quick` | `BTree.balanceQuick` | ✅ |
| `balance_shallower` | MISSING | ❌ (root collapse not implemented) |
| `autoVacuumCommit` | `Engine.AutoVacuumCommit` (in exec layer) | partial |
| `incrVacuumStep` | `BTree.IncrVacuumStep` | ✅ |
| `relocatePage` | `BTree.RelocatePage` | ✅ |
| `ptrmapGet/Put/PutOvflPtr/CheckPages` | pager has equivalents | ✅ |
| `setChildPtrmaps` | `BTree.setChildPtrmaps` | ✅ |
| `getAndInitPage` | pager internal | ✅ |
| `getOverflowPage` | `BTree.overflowCellAt` + pager | partial |
| `pageInsertArray/FreeArray` | `BTree.pageInsertArray/FreeArray` | ✅ |
| `pageReinit` | MISSING | ❌ |
| `defragmentPage` | MISSING | ❌ |
| `freePage` (Frees btree page via cursor) | `pager.FreePage` (lower-level) | partial |
| `freeSpace` (truncate db file) | `pager.Truncate` | ✅ |
| `freeTempSpace` | n/a | n/a |
| `insertCell` | `BTree.InsertCell` | ✅ |
| `insertCellFast` | MISSING (frigolite's insert uses single path) | ❌ |
| `dropCell` | `BTree.DeleteCell` | ✅ |
| `fillInCell` | `BTree.fillInCell` | ✅ |
| `copyNodeContent` | `BTree.copyNodeContent` | ✅ |
| `copyPayload` | `BTree.copyPayload` | ✅ |
| `editPage` | `BTree.editPage` | ✅ |
| `rebuildPage` | `BTree.rebuildPage` | ✅ |
| `zeroPage` | `BTree.zeroPageAsLeafTable` | partial (no schema-cookie use) |
| `btreeInitPage` | `BTree.initPage` (inline in alloc) | partial |
| `btreeParseCell/Parser/Ptr/NoPayload` | `storage.DecodeCell` | ✅ |
| `btreeParseCellPtrIndex` | `storage.DecodeCell` (with cellType=IndexLeaf) | ✅ |
| `btreeGetPage/UnusedPage` | pager | ✅ |
| `btreeGetHasContent/SetHasContent/ClearHasContent` | MISSING | ❌ (bPageHasContent flag) |
| `btreeSetNPage` | n/a (managed by pager) | ✅ |
| `btreeOverwriteCell/Content` | `BTree.rebuildPage` + writeSingleCellAtEnd | partial |
| `btreePayloadToLocal` | MISSING | ❌ |
| `btreeHeapInsert/Pull` | MISSING | ❌ (no row heap) |
| `lockBtree/unlockBtreeIfUnused` | MISSING (no mutex) | ❌ |
| `moveToChild/Leftmost/Rightmost/Parent/Root` | `Cursor.moveTo*` | ✅ |
| `cursorOwnsBtShared/HoldsMutex/IsAtLastEntry/OnLastPage` | MISSING (no shared-cache cursors) | ❌ |
| `cursorHoldsMutex` | MISSING | ❌ |
| `countValidCursors` | MISSING | ❌ |
| `anotherValidCursor` | MISSING | ❌ |
| `clearDatabasePage` | `BTree.Clear` (similar) | ✅ |
| `decodeFlags` | `parse` (in parse package) | ✅ |
| `newDatabase` | `BTree.NewBTree` | ✅ |
| `saveAllCursors/CursorKey/CursorPosition` | MISSING | ❌ |
| `restoreCursorPosition` | MISSING | ❌ |
| `invalidateAllOverflowCache/IncrblobCursors` | MISSING | ❌ |
| `releasePage/NotNull/One` | pager internal | ✅ |
| `releasePageOne` | pager internal | ✅ |
| `querySharedCacheTableLock/Set/...` | MISSING | ❌ |
| `setDefaultSyncFlag` | pager internal | ✅ |
| `setPageReferenced/GetPageReferenced` | MISSING (no ref tracking) | ❌ |
| `sharedLockTrace` | MISSING | ❌ |
| `downgradeAllSharedCacheTableLocks` | MISSING | ❌ |
| `clearAllSharedCacheTableLocks` | MISSING | ❌ |
| `hasReadConflicts` | MISSING | ❌ |
| `hasSharedCacheTableLock` | MISSING | ❌ |
| `removeFromSharingList` | MISSING | ❌ |
| `modifyPagePointer` | MISSING | ❌ |
| `indexCellCompare` | `storage.CompareKeys` (in storage) | ✅ |
| `cursorHoldsMutex` | MISSING | ❌ |
| `assertParentIndex` | n/a (debug only) | n/a |
| `checkTreePage/Ref/List/AppendMsg/Oom/Progress` | exec/pragma_quickcheck (high-level) | partial |
| `checkPtrmap` | pager.ReadPtrmap + ValidatePtrmap | partial |
| `btreeInvokeBusyHandler` | MISSING | ❌ (no busy handler) |
| `btreeEndTransaction` | pager internal | partial |
| `cursorOnLastPage` | `Cursor.EOF` (partial) | partial |

## Critical gaps (blocker for autovacuum-2.5.1+)

1. **`balance_shallower` (root collapse)** — MISSING. After 528 drops, the root has 0 cells but the rmp chain is broken. Without shallower, the root stays as an interior page with stale rmp.
2. **`balance_nonroot` Phase 4 cell redistribution** — buggy. Cells move between siblings incorrectly, leaving some leaves disconnected from their parents.
3. **`saveAllCursors` / `cursor restore`** — MISSING. After rebalance, cursors pointing to moved cells are invalid (no restore path). Currently broken on ROLLBACK.
4. **`pageReinit` (defragment leaves after rebalance)** — MISSING. After cell redistribution, page defragmentation isn't done; pages may have gaps that prevent new inserts.

## Medium gaps (no testgen impact but design debt)

5. **Shared cache table locks** (`lockBtree`, `querySharedCacheTableLock`, etc.) — MISSING. Multi-connection to the same file is unsafe.
6. **Savepoints** (`sqlite3BtreeBeginStmt/Savepoint`) — MISSING. The exec engine doesn't expose SAVEPOINT.
7. **Incremental BLOB I/O** (`sqlite3BtreePutData/IncrblobCursor`) — MISSING. Large BLOB writes/reads are loaded into memory.
8. **Shared-schema cache** (`sqlite3BtreeSchema/SchemaLocked`) — MISSING. The btree layer doesn't share schema with other connections.
9. **`btreeHeapInsert/Pull` (row heap for fast empty-page-when-inserted)** — MISSING. This affects space utilization in fragmented btrees.
10. **Page reference tracking** (`setPageReferenced/GetPageReferenced`) — MISSING. Pages aren't pinned in cache, leading to unnecessary re-reads.

## Low-priority gaps

11. **Read-only btree check** (`sqlite3BtreeIsReadonly`) — MISSING.
12. **Schema version field** (`sqlite3BtreeSetVersion`) — MISSING.
13. **Secure delete flag** (`sqlite3BtreeSecureDelete`) — MISSING. Zeroed pages on free.
14. **mmap/spill size limits** — MISSING (pager-level; not needed yet).
15. **`copyPayload` vs `btreeOverwriteContent`** — partial; frigolite does the work in `rebuildPage` instead of separate in-place overwrite.
16. **CHECK-side page free** (`freeTempSpace`) — n/a for single-file mode.

## Implementation plan (closure)

### Phase A: Btree rebalance correctness (resolves 2.5.1+)
1. Implement `balance_shallower` in `internal/btree/btree_balance_shallower.go` — port from btree.c lines 8800-9050.
2. Rewrite `balanceNonroot` Phase 4 cell redistribution to use the size-balanced distribution from btree.c lines 8500-8800 (currently using simplified "first cell goes to page 0" heuristic).
3. Implement `pageReinit` in `internal/btree/btree_reinit.go` — defragment leaves after rebalance.
4. Add `saveAllCursors` / `cursor restore` for rebalance-safety (R-Tree cursors and INCURSOR cursors need this).
5. Test: re-run `go test -tags testgen ./testgen/autovacuum/ -count=1`. Expect 0 errors.

### Phase B: Btree savepoint support
1. Implement `BeginStmt`, `Savepoint(STMT_SAVEPOINT/RELEASE/ROLLBACK)` in btree layer.
2. Wire through `exec` engine to expose `SAVEPOINT`/`RELEASE`/`ROLLBACK TO` SQL.
3. Add testgen for `savepoint.test`.

### Phase C: Shared cache table locks
1. Implement `lockBtree`, `setSharedCacheTableLock`, `querySharedCacheTableLock` in btree layer.
2. Enable shared cache mode in pager (currently single-connection).
3. Test: open same file from two frigolite handles, verify mutual exclusion.

### Phase D: Incremental BLOB I/O
1. Implement `sqlite3BtreePutData` and `sqlite3BtreeIncrblobCursor` for streaming BLOB writes.
2. Wire through vdbe for `sqlite3_blob_open` / `sqlite3_blob_write` / `sqlite3_blob_close`.
3. Testgen: `incrblob.test`.

### Phase E: Btree integrity_check fidelity
1. Port the rest of `sqlite3BtreeIntegrityCheck` (11102-line function with `checkTreePage`, `checkRef`, `checkList`, `checkAppendMsg`).
2. Add deep checks: btree structure, ptrmap consistency, cell-pointer sort order, page free space, etc.
3. Test: `integrity_check` matches SQLite output for all known test cases.

### Phase F: Refactor + documentation
1. Update `.agents/lessons_learned.md` with btree rebalance lessons.
2. Update `PORTPLAN.md` to mark btree as feature-complete.
3. Add a per-feature-family test for each new btree function.

## Definition of done

- `go test -tags testgen ./testgen/...` — all packages pass with 0 errors.
- `go test -run TestSOLID_ ./...` — pass.
- `go test -run TestP8_ ./...` — all phase 8 tests pass.
- New tests for each added btree feature.
- Lessons learned updated.
