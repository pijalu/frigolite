# R11 RESEARCH — per-operation work-diff: sqlite3 C vs frigolite Go

Base: frigolite main `d14cdb3ef` (PERF.PARITY7 close) vs sqlite3 `c2aa8e3875` (3.51.0),
`/Users/muaddib/dev/sqlite/src/`. Read-only research; no engine code changed.

Mission numbers (same ops): insert 1.45x, point 1.14x, update 1.58x, delete 1.65x,
scan 1.04x (parity). Per-statement budgets: update ~1.30us vs C ~0.72us;
delete ~1.04us vs C ~0.62us; insert ~0.96us vs C ~0.66us; point ~1.07us vs C ~0.94us.
Gaps to explain: insert ~300ns, point ~130ns, update ~580ns, delete ~420ns.

Annotation legend:
- **MATCHES-C** — frigolite does the same work C does.
- **FRIGOLITE-EXTRA** — work C skips entirely (the gold; each is a cut candidate).
- **FRIGOLITE-DIFFERENT** — same semantics, different mechanism (flagged when C's
  mechanism is cheaper).

Already-covered areas (reference only, do not re-derive):
ParsedBTree memo introduction (pager side, landed); evalNumericLit boxing
(sibling scope); statement-journal skip for can't-abort DML (landed, R10.DML);
template/clone parse cache + slot stash (R8/R9/LITCACHE); result pooling
(R8.POINT); btree wrapper + cursor pooling (PERF.BTREEUSE).

---

## 1. INSERT — `INSERT INTO t VALUES(...)` (autocommit, rowid-allocating)

### C work list

| Step | C function (file:line) | Notes |
|---|---|---|
| Parse (literal mode) | sqlite3RunParser / parse.y | bench shape parses per statement on BOTH sides; frigolite template-caches (net frigolite win) |
| Transaction begin | vdbe.c:4100 `OP_Transaction` -> sqlite3BtreeBeginTrans (btree.c:3777), lockBtree (btree.c:3278) | pager shared lock + page-1 header check from CACHE; no-op early-return when already in TRANS_WRITE |
| Rowid | vdbe.c:5587 `OP_NewRowid` -> sqlite3BtreeLast (btree.c) + sqlite3BtreeIntegerKey | **O(log n) rightmost descent**, never a scan; MAX_ROWID falls to random rowids |
| Record encode | `OP_MakeRecord` (vdbe.c) | walks Mem register array (unions) — zero allocation, zero boxing |
| Insert | vdbe.c:5746 `OP_Insert` -> sqlite3BtreeInsert (btree.c:9370) | packs BtreePayload from 2 registers; `OPFLAG_USESEEKRESULT` reuses the seek's `loc`; NCHANGE/LASTROWID are two integer adds |
| Cursor save | btree.c:9399 | only when `BTCF_Multiple` (>1 cursor on the tree) — skipped in steady state |
| Duplicate check | btree.c:9458 | `BTCF_ValidNKey && pX->nKey==pCur->info.nKey` -> overwrite; append: `loc=-1` from seekResult, **no search at all** (`idx = ++pCur->ix`, btree.c:9612) |
| Cell encode | fillInCell (btree.c:7035) into pBt->pTmpSpace | no allocation; overflow only when payload > maxLocal |
| Page placement | insertCellFast (btree.c:7389) + allocateSpace | content-gap compare (one branch) when no freeblocks; pointer-array shift + memcpy |
| Balance | balance (btree.c:9091) | runs only on `nOverflow` (btree.c:9641) — zero invocations on the non-split path; quick-balance (btree.c:7968) is one allocateBtreePage + rebuildPage of ONE cell |
| nFree accounting | MemPage.nFree updated arithmetically by insertCell/dropCell | page header fields maintained INCREMENTALLY in the cached MemPage (parsed once at page load, btreeInitPage) |
| Commit | sqlite3VdbeHalt (vdbeaux.c:3311) -> vdbeCommit -> sqlite3BtreeCommitPhaseOne (btree.c:4285) -> sqlite3PagerCommitPhaseOne (pager.c:6461) | clean-read early-out `eState<CACHEMOD` (pager.c:6487); dirty list is the pcache's pre-built linked list, ONE pass (pager_write_pagelist pager.c:4425); change counter stamped inline on page 1 (pager.c:4472); ONE SIZE_HINT before first write; **file growth is implicit in pwrite — no truncate per page**; db-file truncate exactly ONCE iff bDoTruncate (pager_truncate pager.c:2652) |

### frigolite work list

| Step | Go (file:line) | Verdict |
|---|---|---|
| Parse | template slot stash, `InsLitVals` (LITCACHE) | MATCHES-C-or-better |
| Statement entry | internal/exec/engine_core.go:918 execEntry; :985 execDBFileChecks (autocommit); checkExternalMod pread on first FindTable (R8 leftover) | FRIGOLITE-DIFFERENT — C's OP_Transaction reads the header from the cached page 1; frigolite issues fstat+pread per autocommit statement (latched in explicit txn) |
| Preflight | engine_core_tail.go:113/143/213, all memoized (pfAST/pfDML slots) | MATCHES-C (C does these at prepare) |
| Rowid | rowid.go:83 plainNextRowID — cache hit = `cached+1`; **cache miss = scanMaxRowID (rowid.go:125) FULL TABLE SCAN decoding every cell**; cache is invalidated by every point UPDATE/DELETE (update_point.go:539, delete_point.go:126) | FRIGOLITE-EXTRA (catastrophic on interleaved workloads; see Top-10 #5) |
| Statement journal | insert_row.go:72-76 `pg.BeginStatement()` gated on ForeignKeys() | MATCHES-C (C's subjournal also skipped in autocommit; both gate) |
| Values | InsLitVals stash read in evalTuple (no re-parse, no re-box) | MATCHES-C-or-better |
| Record encode | insert_exec_tail.go:324 appendEncodedInsertRecord -> storage.AppendEncodeRecord over `[]interface{}` | FRIGOLITE-DIFFERENT — C walks flat Mem unions; frigolite walks boxed interface{} (boxes already exist from the stash, so the cost is the type-switch hop, not allocation) |
| Write tree | insert_exec_tail.go:345 insertWriteTree (core.go:391): identity key compare + Reinit | MATCHES-C (BtShared reuse) |
| Insert entry | btree_insert.go:17 InsertCell | — |
| Quick append | btree_append_cursor.go:171 insertQuickAppend: claimQuickAppend (mutex, :127) -> LocalPayloadSize (:182) -> **verifyQuickLeaf (:147): ReadPage + fresh storage.ParsePageInto + last-cell rowid decode** -> prepareCell (:191) -> encodeCellScratch (:194) -> leafHasRoom (:195) -> saveAllCursors (:205) -> writeLeafCell (:210) -> slot update (second mutex, :220-226) | FRIGOLITE-DIFFERENT — C's append is `ValidNKey` register compare + `++ix` + memcpy; frigolite re-verifies the leaf from raw bytes EVERY insert (rollback-safety the C cursor state machine gets for free) + 2 mutex round-trips |
| Insert position | btree_leaf_split.go:27 writeLeafCell **calls findInsertPositionTable (:489, binary walk with 2 GetVarint per probe) UNCONDITIONALLY — even on the proven append** (claimQuickAppend proved `rowID > maxKey`) | **FRIGOLITE-EXTRA** (Top-10 #3) |
| Duplicate drop | btree_insert.go:171 dropTableLeafDuplicateRowid (generic path only) — a SECOND binary walk | FRIGOLITE-EXTRA on the generic path (C knows `loc` from the moveto; frigolite re-derives) |
| Space | allocateSpaceOnPage (freeblock scan only when freeblocks exist) + shiftCellPtrsRight + page1-last WritePage | MATCHES-C |
| Page parse | insertPage parseInsertScratch (btree_insert.go:131) per level; after the write, the NEXT statement's ParsedBTree memo (pagerparse.go:94) misses (bytes changed) -> full ParsePageInto + hdrSnap make+copy + memo alloc | FRIGOLITE-EXTRA (Top-10 #1 — C maintains MemPage incrementally) |
| Split path (amortized) | splitLeafMulti + splitStaging pool + balance_deeper/nonroot ports | MATCHES-C |
| Indexes/triggers/FK | maintainIndexesOnInsert (insert_core.go:553); fireAfterInsertRowTriggers gated by hasTriggersForTable; FK check gated on ForeignKeys() | MATCHES-C |
| Autocommit flush | engine_core_tail.go:637-735: dirtyDatabases (one lock per pager) -> updateFileChangeCounter (page-1 dirty, pragma_header.go:94) -> autovacuumDrainIfDirty -> flushAttachedPagers -> pager.Flush | MATCHES-C for the bump; flush internals differ (see Top-10 #2) |
| Pager flush | pagercommit.go:338 flushFilePagesLocked: openRollbackJournalLocked per commit -> **flushOrderLocked (:280): fresh slice + sort.Slice per flush** -> per-page flushPage (:428): **file.Truncate before EVERY page beyond EOF (:452-463) + quota check**, journalBeforeImageLocked pread (:485), WriteAt -> finalize journal -> refreshKnownFileStamp | FRIGOLITE-EXTRA (Top-10 #2) — C: pre-built dirty linked list, one SIZE_HINT, no per-page truncate; C journals at first-write time (PGHDR already journalled flag) instead of re-reading the disk at flush |

---

## 2. POINT SELECT — `SELECT c FROM t WHERE id=?`

### C work list

- vdbe.c:5493 `OP_SeekRowid` -> sqlite3BtreeTableMoveto (btree.c:5769):
  cursor-valid/ValidNKey short-circuit (btree.c:5785: `info.nKey==intKey`
  returns with ZERO page touches; `+1` rowid goes through BtreeNext);
  per-page binary search over RAW cell bytes (btree.c:5855-5886) — the
  MemPage was parsed ONCE when the page entered the cache and stays parsed
  across statements (no validation, no re-parse, no snapshot).
- OP_Column reads the cell body in place; only requested columns extracted.
- No transaction/commit work for pure reads (`p->bIsReader`, Halt early-out).

### frigolite work list

| Step | Go (file:line) | Verdict |
|---|---|---|
| Shape/plan memos | rowid-seek plan shape memo (R9.POINT), pfAST/pfDML, bareRefs memo | MATCHES-C-or-better |
| Tree/cursor | execquery opens via cached tree; btree.OpenCursorAtRoot (btree.go:284) — cursor pool Get + resetFor + registerTreeCursor (mutex); ReleaseIdleCursors at return (unregister, mutex) | FRIGOLITE-DIFFERENT — C's OP_OpenRead takes a BtCursor from the BtShared free list under the already-held mutex (~20ns); frigolite pays 2 lock pairs/statement (PERF.BTREEUSE residue) |
| Seek | btree.go:557 SeekToRowID -> seekTableLeafWithPath (btree_cursor_save.go:379): ReadPage + **ParsedBTree memo (:386: atomic load + bytes.Equal over header span + parsedMatchesSnapshot re-derivation)** -> seekInLeafTable probe loop (btree.go:589, hoisted base + 1-byte varint fast path) -> routeInteriorTable (:417) | MATCHES-C on the probes; FRIGOLITE-EXTRA = the memo validation itself (C validates nothing) — on pure-read pages the memo hits and this is ~20-40ns |
| Fetch | rowid_seek.go:80 fetchSeekStructRow -> ReadCellData (btree.go:855: restoreIfNeeded + cachePage + skipEmptyLeaves) -> DecodeRecordValuesInto (single fused decode, target-slot masked) | MATCHES-C (C also only extracts needed columns) |
| Fill | fillSeekRowPhaseOne; dead-affinity gating; IPK targeted fill; bareRefSeekOutput fusion (R8.POINT) | MATCHES-C-or-better |
| Commit | read statements skip flush walks (reads skip the drain; nothing dirty) | MATCHES-C |

Residue: the ~130ns gap is cursor churn (2 mutex pairs) + memo validation +
decode boxing of the selected column + the Exec funnel's fixed preamble
(progress check, funnel slices, BeginStmtTime, counter resets). No single
large lever remains; see Top-10 #7/#8/#10.

---

## 3. POINT UPDATE — `UPDATE t SET c=? WHERE id=?`

### C work list

- OP_Transaction (as INSERT).
- OP_SeekRowid (one descent; `movetoTarget` cached for OP_Delete).
- OP_Column per needed column into registers (affinity applied at COMPILE
  time, once); SET expressions evaluated into registers.
- OP_MakeRecord (re-encode from registers — zero alloc).
- Write: OP_Insert with OPFLAG_ISUPDATE|USESEEKRESULT -> sqlite3BtreeInsert
  `loc==0`: same-size in-place **memcpy** (btree.c:9586-9607, no
  dropCell/insertCell, no balance); size-changed -> dropCell + insertCellFast.
- Index maintenance: only indexes whose columns CHANGED are touched (update.c
  chng-mask).
- Commit: as INSERT (one dirty-list pass; the in-place update dirties one page).

### frigolite work list

| Step | Go (file:line) | Verdict |
|---|---|---|
| Gates | update_point.go:29 pointUpdateEligible + :54 shape + :67 target + :91 SET-target checks (validateDMLExprs memo, R10) | MATCHES-C (compile-time in C) |
| Plan | :114 planDMLSeek (seek.go:51, memo) | MATCHES-C |
| Write tree | :125 pointWriteTree (core.go:463) | MATCHES-C |
| Collect | :382 collectPointUpdateRow: OpenCursorAtRoot (:390) -> SeekToRowID (:394, ParsedBTree memo) -> ReadCellData (:405) -> decodePointUpdateRecord (:464, DecodeRecordValuesInto into pooled ptDecode) -> pointUpdateValueSlots (:426 COPY out of pool) -> **applyUpdateColumnDefaults x2 (:429-430)** -> applyTypedPointUpdateSet (:438) or RowMap path (:444) -> recomputeUpdateGenerated | FRIGOLITE-EXTRA: the x2 defaults walk per statement (C applies defaults at prepare; a record that already stores all declared columns needs NEITHER walk — the record's stored-count is known); FRIGOLITE-DIFFERENT: decode boxes every decoded slot (C: Mem unions) — sibling scope |
| Pre-checks | :140 preCheckUpdate (NOT NULL/CHECK) + :149 checkUpdateConflicts (P1 change-gate skips scans when nothing constrained moved) | MATCHES-C |
| Old index entries | :154 deleteUpdateIndexEntriesFor | MATCHES-C |
| Encode | :498 appendEncodedRecord + :502 appendEncodedCell (pooled buffers) | MATCHES-C |
| Write | :506 OverwriteCellByRowIDAt (btree_update_inplace.go:43): saveAllCursors (:44) -> ReadPage -> **fresh storage.ParsePage (:49) — NOT the memo** -> DecodeCellInto old cell (:59) -> rowid check (:62) -> overwriteLeafCellAtDecoded (:113): TableLeafCellSizeAt (:115, second header parse) + declineInPlaceOverwrite (:119) -> memcpy; declined -> :514 DeleteCellByRowID (full re-seek) + :517 InsertCell | FRIGOLITE-EXTRA: the fresh ParsePage bypasses the memo that the collect seek JUST filled (~60-120ns, Top-10 #4); FRIGOLITE-EXTRA: TableLeafCellSizeAt re-parses the cell header bytes the DecodeCellInto just walked (C's xParseCell fills CellInfo ONCE) |
| New index entries | :527 writeUpdateIndexEntriesFor(old,new) | MATCHES-C (verify the changed-columns filter matches C's chng-mask — C skips indexes whose columns did not move) |
| Rowid cache | :539 InvalidateRowIDCache (map write; deliberately skips the bump) | FRIGOLITE-DIFFERENT — C has no map; C's next OP_NewRowid does an O(log n) BtreeLast; frigolite's invalidate turns the NEXT insert into a FULL SCAN (Top-10 #5) |
| Commit | as INSERT; the write invalidated the leaf memo -> next statement re-parses it (Top-10 #1) | FRIGOLITE-EXTRA |

---

## 4. POINT DELETE — `DELETE FROM t WHERE id=?`

### C work list

- OP_SeekRowid + OP_Delete (vdbe.c:5901) -> sqlite3BtreeDelete (btree.c:9802):
  `BTCF_Multiple`-gated saveAllCursors; BTREE_CLEAR_CELL/xParseCell fills
  CellInfo ONCE (size + overflow); dropCell (btree.c:7228) O(1) — pointer
  unlink + freeblock push; balance skip = ONE compare (`nFree*3 <= usable*2`,
  btree.c:9963); moveToRoot state reset.
- OP_IdxDelete per index — key extracted ONLY when indexes exist (delete.c;
  no record decode when triggers/FKs/RETURNING absent).
- No statement subjournal in autocommit; commit as INSERT.

### frigolite work list

| Step | Go (file:line) | Verdict |
|---|---|---|
| Gates | delete_point.go:183 pointDeleteEligible + :31 planDMLSeek + :39 andTermCount==1 | MATCHES-C |
| Journal | deliberately NO per-path scope (engine scope carries it; R10.DML skip for can't-abort) | MATCHES-C |
| Collect | :74 finishPointDelete: CheckProgress (:77) -> OpenCursorAtRoot (:82) -> needValues gate (:92 — skips the record decode entirely when no indexes/hooks; explicitly mirrors OP_Delete) -> SeekToRowID (:93) -> decodePointDeleteRow when needValues | MATCHES-C (the needValues gate is a faithful port) |
| Write | :115 DeleteCellByRowIDAt (btree_tail.go:265): saveAllCursors (:266) + invalidateAppendCursor (mutex, :267-269) -> deleteSingleTableRowID (btree_delete_one.go:24) -> pointDeleteTarget (:58): ParsedBTree (:70) + **BTreePage STRUCT COPY (:74)** + **next-cell rowid probe (:82)** + DecodeCellInto (:86) + rowid check (:91) -> dropCellFromLeafPage (O(1)) -> WritePage -> maybeRebalanceAfterDeleteHinted (:278, parent hint) | MATCHES-C on the O(1) drop; FRIGOLITE-EXTRA: decode fills a full Cell struct then TableLeafCellSizeAt re-parses the same header bytes (C: one xParseCell); FRIGOLITE-EXTRA: the next-cell rowid probe (C's cursor seek + `loc==0` uniqueness proof covers this without a second probe) |
| Indexes/hooks | :119 maintainIndexesOnDelete + :122 fireDeletePreupdate (gated) | MATCHES-C |
| Rowid cache | :126 InvalidateRowIDCache | FRIGOLITE-DIFFERENT (see update / Top-10 #5) |
| Result | :138 stageDelResult (scratch slot) | MATCHES-C-or-better |
| Commit | as INSERT — plus the memo re-parse of the mutated leaf (Top-10 #1) and flush internals (Top-10 #2) | FRIGOLITE-EXTRA |

Delete residue decomposition (~420ns over C): memo re-parse after previous
mutation (~120-200ns), funnel + flush internals (~80-150ns), cursor churn +
3-4 mutex pairs (~60-100ns), pointDeleteTarget redundant probes (~40-90ns),
rowid-cache map write + result staging (~20-40ns).

---

## 5. Top-10 frigolite-only costs (ranked by estimated ns/statement)

| # | Lever | Ops | Est ns/stmt | (a) C evidence absent | (b) Go site | (c) Cut proposal | (d) Risk |
|---|---|---|---|---|---|---|---|
| 1 | **ParsedBTree memo thrash**: every mutating statement invalidates the leaf/root memos; the NEXT statement re-parses (full ParsePageInto) + allocates the memo struct + hdrSnap copy | ins/upd/del | 120-250 | C parses a page ONCE at cache load (btreeInitPage) and maintains MemPage.nCell/nFree/cellOffset INCREMENTALLY in insertCell/dropCell/allocateSpace — no re-parse ever | pagerparse.go:94 (miss path :104-131) after every writeLeafCell/dropCellFromLeafPage | Mutating btree paths PUBLISH their parse: writeLeafCell/dropCell already know post-write CellCount/CellContent/freeblock — refresh the memo in place (patch hdrSnap bytes + parsed fields) instead of invalidating | Medium — must mirror the 64KiB content-start wrap and keep the parsedMatchesSnapshot canary true; freeblock-chain edits touch bytes the snapshot covers |
| 2 | **Commit write-path shape**: fresh slice + sort.Slice per flush; file.Truncate before EVERY page beyond EOF (+ quota hook per page); journal-before-image re-read per page | ins (file) | 100-250 | C: dirty pages form a pre-built pcache linked list (sqlite3PcacheDirtyList) — no sort; ONE SIZE_HINT fcntl (pager.c:4456-4464); growth is implicit in pwrite; db-file truncate ONCE iff bDoTruncate (pager.c:2652) | pagercommit.go:280 flushOrderLocked, :452-463 flushPage Truncate, :485 journalBeforeImageLocked | Keep an insertion-ordered dirty slice (append on markDirty; in-place stable partition, page 1 last) at flush; defer file growth to ONE truncate/size-adjust after the write loop (pendingFileTruncate pattern already exists); journal at markDirty time (C's PGHDR journalled flag) instead of re-reading the disk at flush | Low-medium — journal ordering vs truncate ordering is the delicate part (R8 measured 24% of remaining insert CPU here); quota must still see total growth |
| 3 | **Append insert pays a leaf-wide binary search**: writeLeafCell calls findInsertPositionTable unconditionally (2 GetVarint per probe, ~7 probes per 100 cells) even when claimQuickAppend PROVED rowID > maxKey | ins | 60-150 | C's append path runs NO search: `loc=-1` from USESEEKRESULT, `idx = ++pCur->ix` (btree.c:9612); ValidNKey short-circuit (btree.c:9458) | btree_leaf_split.go:38-42 (called from the quick path with dupAlreadyDropped=true at btree_append_cursor.go:210) | Thread `insertIdx` into writeLeafCell: quick path passes CellCount directly (the claim proved rightmost); generic path keeps the search | Low — pure plumbing; the proof already lives in claimQuickAppend |
| 4 | **Point-UPDATE overwrite bypasses the memo it just filled**: OverwriteCellByRowIDAt does a fresh storage.ParsePage + TableLeafCellSizeAt re-parse of the same cell header | upd | 80-150 | C: the cursor's MemPage is already parsed; xParseCell fills CellInfo ONCE (btree.c:9583 BTREE_CLEAR_CELL); overwrite is a bare memcpy after two bounds checks | btree_update_inplace.go:49 (fresh ParsePage), :115 (second header parse) | Route through pg.ParsedBTree + reuse the collect seek's parsed page (the position is re-validated by the rowid check anyway); fuse size+overflow into one header parse | Low — the fresh parse exists only because `page` may be stale; the rowid check at :62 already guards that |
| 5 | **Rowid allocation = FULL TABLE SCAN on cache miss**: scanMaxRowID decodes every cell (cursor.Next loop); EVERY point UPDATE/DELETE invalidates the cache first | ins after upd/del | 0 on current bench shapes; **O(n) cliff interleaved** | C: OP_NewRowid = sqlite3BtreeLast (one rightmost descent, O(log n)) + IntegerKey (vdbe.c:5621-5633) — never a scan | rowid.go:125 scanMaxRowID (called :97 on cache miss); invalidated at update_point.go:539, delete_point.go:126 | Replace the scan with a rightmost-leaf descent (readTreePage along RightmostPtr, last-cell rowid — the primitive rightmostTableLeaf (btree_append_cursor.go:262) already implements) | Low — semantics identical to C's BtreeLast; empty-tree and MAX_ROWID cases already special-cased |
| 6 | **Point-DELETE target double-parse**: DecodeCellInto fills a full Cell, then TableLeafCellSizeAt re-parses the same header bytes; plus the next-cell rowid probe and a BTreePage struct copy | del | 40-90 | C: BTREE_CLEAR_CELL's xParseCell produces size+overflow in ONE pass (btree.c:9908); no next-cell probe (the seek's loc==0 is the uniqueness proof) | btree_delete_one.go:38 (size re-parse), :82 (next-cell probe), :74 (struct copy) | Fuse: one header parse returning (size, overflowPtr); drop the next-cell probe when the seek matched on equality AND the fast path already verified `delCell.RowID == rowID` at idx (the seek's equality match is the same proof) | Low — keep the probe for the duplicate-decline case only (when it actually declines) |
| 7 | **Cursor churn per point statement**: OpenCursorAtRoot = pool Get + resetFor + registerTreeCursor (mutex); ReleaseIdleCursors/Close = unregister (mutex); 2 lock pairs/statement | pt/upd/del | 30-60 | C: OP_OpenRead/OP_OpenWrite pull a BtCursor from the BtShared free list under the ALREADY-HELD bt mutex; no extra critical sections | btree.go:284 OpenCursorAtRoot; btree_cursor_save.go registration | Single mutex acquisition covering register-at-open + unregister-at-release (one lock domain move), or a per-tree cursor slot owned by the cached write tree (no registry round-trip for tree-scoped seek cursors) | Medium — registry discipline is load-bearing for saveAllCursors (PERF.BTREEUSE history); needs the generation-lease treatment |
| 8 | **Quick-append verification + 4 lock ops**: verifyQuickLeaf re-parses the leaf from raw bytes EVERY insert (rollback/reallocation guard); claim + update = 2 quickAppendReg mutex pairs; plus saveAllCursors | ins | 50-120 (on top of #3) | C: the parked cursor's ValidNKey IS the state machine — no re-verification (journal rollback restores cursor state via saveCursorPosition/restore, not re-parses) | btree_append_cursor.go:147 verifyQuickLeaf, :127/:220 mutex pairs | Store (leaf, lastCellOffset, lastCellBytesHash/first8) in the slot; verify = compare 8-16 bytes at the saved offset instead of ParsePageInto; merge claim+update into one mutex window (claim returns a *slot pointer, update re-locks only on the rare contended path) | Medium — the verify is the rollback-safety net (documented at btree_append_cursor.go:35-42); a byte-compare preserves the property (rollback rewrites page images) at 1/10 the cost |
| 9 | **Exec funnel fixed preamble**: progress check, counter/aux resets, SetResultFrame, funnel tree marks, validateLoadedTriggers/Schema (memoized but re-entered), dirtyDatabases HasDirtyPages lock per pager, flushAttachedPagers walk, BeginStmtTime | all | 40-90 | C: sqlite3VdbeExec loop + Halt walk a ~20-entry opcode array and p->apCsr (<=3); no per-statement map/list walks beyond that | engine_core.go:771-887, engine_core_tail.go:637-735 | Fold the per-statement preamble into one cached "statement epoch" word (bump = one store); gate validateLoaded* behind the schema fingerprint they already use (single compare); dirtyDatabases/flushAttachedPagers already early-out — collapse their two walks into the dirty scan's one pass | Low — mechanical; the funnel's aliasing discipline (per-depth slots) must be preserved |
| 10 | **Decode boxing on UPDATE (+ SELECT's selected columns)**: DecodeRecordValuesInto boxes each decoded slot (int64/string allocs); C decodes into Mem unions (zero alloc) | upd/pt | 30-80 | C: sqlite3VdbeMemFromBtree + serial-type decode into Mem unions in the register file — nothing heap-allocated | storage DecodeRecordValuesInto call sites (update_point.go:474; rowid_seek.go:114) | Structural: typed parallel slots (int64/float64/[]byte flags) on the point lanes, boxing only at the public boundary — sibling scope (storage/execexpr lane work); interim: reuse the boxed values' backing when the previous statement's types match (shape memo) | High (structural, crosses package boundaries) — do last; the interim memo is low-risk but modest |

Estimated coverage: update ~580ns gap ≈ #1(150) + #4(120) + #2(part) + #6 + #7 + #9 + #10;
delete ~420ns ≈ #1(150) + #2(80) + #6(60) + #7(50) + #9(60) + staging; insert ~300ns ≈
#3(100) + #8(80) + #1(120, non-append rows) + #2(file-mode only); point ~130ns ≈ #7 + #9 + #10(part).

Not ranked (no bench cost, correctness-of-scope): #5 is the single most
surprising C-vs-frigolite difference — an O(log n) C primitive answered with
an O(n) scan. Fix it before any interleaved benchmark exists to hide it.

## 6. Cross-cutting notes

- The autocommit funnel is where C spends NOTHING and frigolite spends
  structurally: C's per-statement transaction begin/commit rides opcode
  dispatch (OP_Transaction early-return, eState<CACHEMOD commit early-out);
  frigolite's is a Go function funnel (execEntry/preflight/flush) whose fixed
  cost is now ~10% of a point-op budget. Lever #9 is the honest cut; do not
  chase it below the C opcode-dispatch floor.
- File-backed benches amplify #2; memory-backed benches hide it. Measure #1/#3/#4
  with the paired-interleave protocol (R9.POINT discipline).
- Sibling-owned: evalNumericLit boxing, ParsedBTree memo introduction (this doc
  proposes its NEXT step, publish-on-write), statement-journal skip (landed).
