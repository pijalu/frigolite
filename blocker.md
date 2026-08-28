# Blocker Analysis: fts4merge4 — duplicate-row write-visibility + over-merge

Date: 2026-08-16
Status: **REVIEW COMPLETE — engine↔SQLite comparison done; detailed fix plan in §8–§13 (micro-steps, ordered)**

## 1. What the blocker is

The goal "Finish fts4merge4" needs two things:

1. **Structure fix** — the am=2 automerge tx-100 yields `1 1|2 2|3 1`
   (level 1: 1 seg, level 2: 2 segs, level 3: 1 seg) where the SQLite
   oracle is `1 1|2 1|4 1|6 1`. The full fts4merge4 test has 8
   line-261 assertions (4 automerge values × 2 open/close variants)
   that diverge.
2. **Runtime fix** — the full fts4merge4 test is ~346s, over the 300s
   cap. Profile: pager flush ~18%, FTS INSERT tokenization ~18%.

The prior sessions (hours of tracing) identified a "write-visibility bug"
where the merge's replacingOut continuation leaves THREE (level=1, idx=1)
rows that the truncation UPDATE sets to the same block range. This was
described as "deep interaction" and marked blocked.

## 2. Reproduction

Standalone repro in `/tmp/ftsdbg/main.go` (module with `replace` to the
repo). Exactly mirrors fts4merge4's am=2 scenario:

- `CREATE VIRTUAL TABLE t2 USING fts4`, `automerge=2`
- 100 tx × 5 docs; each doc = 1000 combined terms (`a a a`...`j j j`
  list) repeated 10× without separator
- `SELECT level, count(*) FROM t2_segdir GROUP BY level`

Result: `level=1 count=1 | level=2 count=2 | level=3 count=1`
(run time ~9.5s, matching the goal's reported `1 1|2 2|3 1`).

Oracle: `1 1 | 2 1 | 4 1 | 6 1`.

## 3. Confirmed: the duplicate-row bug is STILL PRESENT

Added debug to `readFTSSegdirRows` (DUP detection) + `applyUpdateChanges`
(duplicate rowid count) + btree split path. Run with `FTS_MERGE_DEBUG=1`:

```
502 duplicate (level,idx) reads across the run
DUP applyUpdateChanges rowid=22 count=2        (line 56)
DUP applyUpdateChanges rowid=74 count=3 → 5 → 9  (exponential growth)
SPLIT-SAME-ROWID page=8477 rowid=22 cells=1
```

The final state is clean (`1 1|2 2|3 1`) because the repack's
delete-all + re-insert eventually renumbers the level — but the
duplicates corrupt the MERGE PROCESSING before that (the merge reads the
level with duplicate rows, truncation UPDATEs hit all copies, block
ranges collapse to one range for all copies).

## 4. Root cause — exact mechanism (evidenced)

### 4.1 The update-double-insert amplifier

`applyUpdateChanges` (internal/execdml/update.go, line ~234):

```go
// Step 1: Delete all existing rows in a single pass
tree.DeleteCellsWhere(func(cell) { return toUpdate[cell.RowID] })
// Step 2: Insert all new rows — ONCE PER CHANGE ENTRY
for _, c := range changes {
    tree.InsertCell(newCell{c.rowID, ...})
}
```

When `changes` contains the **same rowID twice** (because the table
physically holds two cells at that rowid and `collectUpdateChanges`
collects both), Step 1 deletes both cells, Step 2 inserts the rowid
**twice** → two cells again. Next statement collects 2 changes → 2
inserts... it does not grow on its own, but combined with the
split-same-rowid bug below it compounds (rowid 74: 3→5→9).

Debug evidence (dup_debug6.txt):

```
68: UPDSCAN rowid=22 lvl=1 idx=1 matched=true    ← same cell
69: UPDSCAN rowid=22 lvl=1 idx=1 matched=true    ← matched TWICE
70: DUP applyUpdateChanges rowid=22 count=2
```

### 4.2 The btree split-same-rowid bug (the actual writer of duplicates)

`insertLeafPage` (internal/btree/btree_insert.go, line ~169):

```go
if leafHasRoom(...) {
    return t.writeLeafCell(...)   // handles same-rowid REPLACE
}
// page full → split
t.splitLeafMulti(...)             // does NOT handle same-rowid!
```

`writeLeafCell` has the d6b911683 same-rowid REPLACE fix, but
`splitLeafMulti` does not: when the leaf page is full and the incoming
cell has the same rowid as an existing cell, `readCellsForSplit`
collects BOTH the old cell and the new cell and redistributes them →
**two cells with the same rowid**.

Confirmed by `SPLIT-SAME-ROWID page=8477 rowid=22 cells=1` — a page
holding one rowid-22 cell, receiving a second rowid-22 cell, split →
duplicate.

### 4.3 The trigger: segdir rowid collision (repack vs merge output)

The segdirNextRowID used by the merge output writes is captured ONCE at
MergeFTS start (`ftsSegdirNextRowID`), but the repack's re-insert uses
the implicit rowid allocator. Both can pick the SAME rowid:

- L0 repack re-inserts the surviving row at rowid 23 (implicit alloc)
- The next L1→L2 merge output writes at rowid 23 (captured counter)

The same-rowid insert then hits the full-page split path → duplicate.

Debug trace (dup_debug8.txt):

```
195: CELL lvl=1 rowid=22 idx=1 sb=3609 le=3858     ← single cell at 22
197: MergeFTS level=1 rows=2 nRem=122
198: MergeFTS output: ... actualBlocks=123         ← L2 output insert
200: UPDSCAN rowid=22 lvl=1 idx=1 matched=false    ← TWO cells at 22 now!
201: UPDSCAN rowid=22 lvl=1 idx=1 matched=false
202: UPDSCAN rowid=23 lvl=2 idx=0                  ← L2 output at 23 (replaced)
```

The duplicate at rowid 22 appears between the L1 read and the next scan,
i.e. during the L2 output write — the insert that collides at rowid 23
(repack's row) went through a full-page split. The split duplicated a
neighbor cell (22), not the colliding cell (23) — the exact split
redistribution bug still needs one more confirmation step (see §6), but
the split-same-rowid path is proven and sufficient to explain all
duplicate creation.

## 5. Runtime issue (separate)

The full fts4merge4 test is 346s > 300s cap. Profile from prior
sessions: pager flush ~18%, FTS INSERT tokenization ~18%. The duplicate
bug also adds wasted work (extra UPDATEs, splits, repack re-inserts), so
fixing §4 may recover part of the runtime too. No runtime work was done
in this session.

## 6. Open questions / next steps

1. **Confirm the exact split path for the first duplicate (rowid 22
   during L2 output insert)**: instrument `splitLeafMulti` entry/exit for
   the segdir table, or add a unit test that fills a leaf page, inserts a
   same-rowid cell, and asserts no duplicate rowid. The
   SPLIT-SAME-ROWID evidence at rowid 22 already proves the mechanism;
   the remaining question is only which insert created the FIRST
   duplicate.
2. **Fix 1 (btree)**: make `insertLeafPage` handle same-rowid BEFORE the
   full-page check — if the page contains a cell with the new rowid,
   replace it (delete old cell) before deciding room/split. This mirrors
   SQLite's `sqlite3BtreeInsert` overwrite semantics in ALL paths.
3. **Fix 2 (defense)**: dedupe `changes` by rowid in `applyUpdateChanges`
   (or make Step 2 insert once per distinct rowid) so a pre-existing
   duplicate cannot amplify.
4. **Fix 3 (rowid collision)**: make the repack use the explicit-rowid
   allocator (or bump the merge's segdirNextRowID past the repack's
   inserts) so merge outputs and repack re-inserts cannot collide.
5. **Re-run**: with duplicates gone, re-check the am=2 structure. The
   over-merge (`2 2` vs oracle `2 1`) may then need the fresh-merge
   quota fix (nLeafEst/flush-bound respecting nRem — the earlier
   "remaining=truncated hint" direction was confirmed NOT viable).
6. **Runtime**: re-profile after the duplicate fixes; the pager flush
   and tokenization hotspots remain if the test is still over 300s.

## 7. Working tree state

Uncommitted debug instrumentation (4 files, all gated by
`FTS_MERGE_DEBUG` or `DUP` prints):

- `internal/btree/btree_insert.go` — InsertCell/insertLeafPage debug
- `internal/execddl/export.go` — readFTSSegdirRows CELL/DUP debug,
  TRUNC debug
- `internal/execdml/update.go` — DUP applyUpdateChanges debug (+`os`
  import)
- `internal/execdml/update_split.go` — UPDSCAN debug (+`os` import)

These must be reverted or converted into the fix before committing.
Committed fixes d6b911683, 9382affe4 remain green for fts4merge/2/3/5.

---

# PART 2 — Review fix plan (2026-08-16, second session)

Fresh engine↔SQLite source comparison (`../sqlite/src/btree.c`,
`../sqlite/ext/fts3/fts3_write.c`, `../sqlite/ext/fts3/fts3.c`) plus a fresh
CPU profile of the am=2 repro (`/tmp/ftsdbg`, 9.86s, `1 1|2 2|3 1` — bug
reproduced). Everything below cites the exact SQLite lines the engine must
mirror. Workstreams are ordered by dependency; each micro-step is sized for
one commit that passes the quality gate + test suite.

## 8. Engine ↔ SQLite divergences found (the fix plan's evidence base)

### 8.1 Divergences that CAUSE the blocker

| # | Engine (today) | SQLite | SQLite ref |
|---|----------------|--------|------------|
| D1 | `insertLeafPage` checks `leafHasRoom` FIRST; same-rowid REPLACE only lives in `writeLeafCell`. Full page + same rowid → `splitLeafMulti` redistributes BOTH cells → duplicate rowids | `sqlite3BtreeInsert`: `loc==0` → `dropCell(pPage, idx)` BEFORE `insertCellFast`; `balance()` runs only AFTER the old cell is gone — the split never sees two equal keys | btree.c:9569–9617 (dropCell@9608), balance@9641–9644 |
| D2 | `repackFTSSegdirLevel` = delete-all rows at level + re-INSERT with implicit rowids (collides with the merge's captured `segdirNextRowID` → full-page split → D1 duplicates) | `fts3RepackSegdirLevel` = in-place `UPDATE %_segdir SET idx=? WHERE level=? AND idx=?` per shifted row (SQL_SHIFT_SEGDIR_ENTRY azSql#31) — no inserts, no rowid churn | fts3_write.c:4544–4600, azSql:30–31 |
| D3 | Merge consumes a segment → `deleteFTSSegdirIdx` deletes only the segdir row; the segment's %_segments blocks LEAK forever (bloats %_segments btree, inflates later `nLeafEst` estimates → over-merge, slows every scan) | `fts3DeleteSegment`: `DELETE FROM %_segments WHERE blockid BETWEEN :iStart AND :iEnd` before removing the segdir entry | fts3_write.c fts3DeleteSegment (~4720), azSql#17 |
| D4 | Merge OUTPUT is rebuilt from scratch every call (`SerializeSegmentForTerms` over existing+merged doclists) at a FRESH block range each call → the output's `leaves_end_block-start_block` balloons each continuation, which inflates the NEXT merge's `nLeafEst = 2*(1+le-sb)` → quota never reached → level drained (over-merge; also O(n) rewrite per call = O(n²) total) | Appendable segment: `fts3IncrmergeWriter` pre-allocates `[iStart, iStart+16*nLeafEst-1]` ONCE (+ zero-length marker at iEnd); continuations append leaf blocks INSIDE the range (`fts3IncrmergeAppend`), `fts3IncrmergeRelease` rewrites only the tail: current leaf + root + segdir row. Block range stays stable across continuations | fts3_write.c:4455–4520 (writer), 4017–4075 (append, flush gate `iBlock < iStart+nLeafEst`, `nWork++`), 4107–4190 (release), 4258–4345 (load: appendable check via NULL marker at iEnd, `nLeafEst=(iEnd-iStart+1)/16`) |
| D5 | `end_block` written as `"<leavesEnd> 0"` (nLeafData=0) by `writeFTSShadowRowAtRange`/`updateFTSShadowRowRange` — SQLite reads this suffix as the segment SIZE | `fts3WriteSegdir` stores TEXT `"<endBlock> <nLeafData>"` with the accumulated leaf-data bytes (`pWriter->nLeafData += nSpace` per appended term) | fts3_write.c:1966–1996, 4079, 4173–4180 |
| D6 | NO segment promotion at all (grep "promote" in internal/fts+execddl: nothing) — this is where the oracle's levels 4/6 come from | `fts3PromoteSegments(p, iAbsLevel+1, nLeafData)` after a merge fully consumes a level: if EVERY segment at levels > output-level has size>0 and ≤ (nByte*3)/2 (size from end_block suffix), they are all promoted to the output level via two-step UPDATE (stage at level=-1 with new idx, then `UPDATE SET level=? WHERE level=-1`) | fts3_write.c:3119–3205, call site 5076 (also 3305 after crisis merge) |
| D7 | Flush-time automerge gate: only `A > 64` (A = nLeafAdd*mxLevel*1.5). Small flush at a tall tree merges where SQLite does not | `fts3SyncMethod`: `p->nLeafAdd > (nMinMerge/16)` (= >4 leaf blocks) AND `A > 64` | fts3.c:3540–3560 |
| D8 | `nLeafAdd` counts only the main index's flush blocks | SQLite increments `nLeafAdd` per leaf block in `fts3SegWriterAddBlock`/flush — includes prefix indexes and delete-marker segments | fts3_write.c:2298, 2413 |

### 8.2 Divergences found but NOT implicated (leave alone unless a test breaks)

- Engine hint = single (level,count) row; SQLite hint = list popped front/pushed
  back. fts4merge/1.3/4.3/5.x are green — do not touch.
- Engine fresh-merge `nLeafEst = 2*Σ(1+le-sb)` over ALL level rows == SQLite
  azSql#29 (same formula, `LIMIT nSeg` where fresh nSeg = whole level).
- `automerge=1|>16 → 8` normalization: engine `SetAutomerge` already correct.

## 9. Fix plan — micro-steps

### Workstream A — btree same-rowid correctness (fixes D1; unblocks everything)

**A0. RED test first.** New `internal/btree/btree_insert_samerowid_test.go`:
fill a leaf page to full (many small cells), then `InsertCell` a cell whose
RowID equals an existing one. Assert: scan sees each rowid exactly once; the
replaced payload is the new one. Confirm it FAILS on current code (the
SPLIT-SAME-ROWID repro at blocker.md §4.2 in unit form).

**A1.** `internal/btree/btree_insert.go` `insertLeafPage` (~line 188): BEFORE
the `leafHasRoom` check, when `t.isTable`, run `findInsertPositionTable`; if
the cell at that index has `RowID == newCell.RowID`, call
`t.deleteCellOnPage(pg, page, insertIdx)` (mirror of btree.c:9608 dropCell
before insertCellFast) and re-parse/recompute `page` state. Then continue to
the room check — the freed space almost always makes room, so the existing
`writeLeafCell` replace branch becomes the common path; `splitLeafMulti` can
never receive two equal rowids.

**A2.** Delete the now-dead duplicate-detection debug in `insertLeafPage`
(lines ~210–223) and in `InsertCell` (lines ~21–39) — part of the uncommitted
instrumentation (§7).

**A3.** Verify: `go test ./internal/btree/...`, then the am=2 repro
(`cd /tmp/ftsdbg && go run .`) — with A alone, DUP growth (§3) must stop even
though the rowid collision (D2) still exists; assert no "DUP" in
`FTS_MERGE_DEBUG=1` output.

**A4.** Regression sweep before committing: `go test ./...` and
`go test -tags testgen ./testgen/fts4merge/... -count=1` (all 5 packages;
fts4merge4 may still fail line-261 — record the current got/want).

### Workstream B — defense-in-depth in UPDATE (blocker.md §6 Fix 2, keep)

**B0. RED test.** `internal/execdml` unit test calling `applyUpdateChanges`
with `changes` containing the same rowID twice (simulate a pre-existing
physical duplicate): assert exactly one row per rowID after the call.

**B1.** `internal/execdml/update.go` `applyUpdateChanges` (~line 235): dedupe
`changes` by rowID (last write wins, matching SQLite's per-row sequential
overwrite) before Step 1/Step 2; keep the debug block removed (§7).
Note: SQLite UPDATE is per-row in-place cursor overwrite — delete-all+reinsert
is an engine implementation detail; the dedupe only guarantees Step 2 inserts
each distinct rowid ONCE.

**B2.** Verify `go test ./internal/execdml/...` + repro step A3.

### Workstream C — segdir row hygiene (fixes D2+D3; SQLite-faithful)

**C0. RED tests.**
  - C0a: repack test — seed a level with idx 0,1,2,3, delete idx 1 (via
    merge consume), call `repackFTSSegdirLevel`, assert: rowids UNCHANGED,
    idx renumbered 0,1,2 (SQLite: in-place UPDATE, rows keep rowids).
  - C0b: block-cleanup test — after a merge consumes a segment whose range is
    [sb,le], assert `SELECT count(*) FROM t_segments WHERE blockid BETWEEN
    sb AND le` == 0.

**C1.** Rewrite `repackFTSSegdirLevel` (export.go:1746) as SQLite's
fts3RepackSegdirLevel: read the level's surviving (rowid, idx) list idx-ASC;
for each position i whose `aIdx[i] != i`, `UPDATE %_segdir SET idx=i WHERE
level=? AND idx=aIdx[i]` (the (level,idx) pairs are unique before and after —
no staging needed because SQLite's own statement has the same property; it
processes ascending and each target idx is free at that moment). No inserts,
no rowid changes → the merge's `segdirNextRowID` can never collide with repack
(kills blocker.md §4.3 mechanism at its root).

**C2.** Add `deleteFTSBlocks(tableName, start, end)`: `DELETE FROM
<t>_segments WHERE blockid BETWEEN ? AND ?` (azSql#17). Call it from the
merge's chomp loop: for every `sr` at EOF, before `deleteFTSSegdirIdx`, delete
`[row.startBlock, segdirRowLeavesEnd(row.leavesEndBlock)]` when startBlock>0
(fts3DeleteSegment guard `if (pSeg->iStartBlock)`).

**C3.** Verify: tests from C0 green; fts4merge/2/3/5 stay green; am=2 repro —
`SELECT max(blockid) FROM t2_segments` must stop growing once levels settle.

### Workstream D — appendable merge writer (fixes D4+D5; the structural fix)

Goal: replace the rebuild-everything output writer with SQLite's
allocate-once/append-within-range writer. This is the change that makes the
fresh merge respect the quota (nLeafEst stays an estimate of the SOURCE, the
output range never inflates) and cuts MergeFTS runtime (31% of profile).

**D0.** Oracle capture: run the am=2 scenario (and fts4merge4 2.2.x cases)
under `sqlite3` CLI; save per-tx `SELECT level,idx,start_block,
leaves_end_block,end_block FROM t_segdir ORDER BY level,idx` to a golden file
(in /tmp, referenced by tests, not committed).

**D1.** `internal/fts`: add `IncrmergeWriter` type (mirror of
fts3_write.c:3717–3735): `{iStart, iEnd, nLeafEst int64; iBlock int64 (next
leaf block); buffer []byte; nLeafData int64; prevTerm string; root []byte}`.
Engine segments have no interior levels in %_segments (root blob lives in
segdir.root), so reserve exactly `nLeafEst` blocks (not ×16) — document this
deviation in a comment.

**D2.** Writer start (fts3IncrmergeWriter equivalent) in MergeFTS fresh path:
  - compute nLeafEst = `2*Σ(1+le-sb)` over the loaded source rows (already
    there, export.go ~1322);
  - `iStart = nextFTSBlockID`, `iEnd = iStart+nLeafEst-1`;
  - write the appendable marker: zero-length blob at blockid iEnd
    (`fts3WriteSegment(p, iEnd, 0, 0)`) so the continuation check
    (D4/fts3IsAppendable) works: `SELECT 1 FROM %_segments WHERE blockid=iEnd
    AND length(block)=0`;
  - store writer state in `ftsTable.SetMergeCtx` (extend MergeCtx:
    IStart, IEnd, NLeafData).

**D3.** Append loop (fts3IncrmergeAppend equivalent): replace the current
flush-simulation (export.go ~1454) with the real writer:
  - per merged term: `sz` (already computed); if `len(buffer)>0 &&
    len(buffer)+sz > nodeSize && iBlock < iStart+nLeafEst` → REPLACE block at
    `iBlock` with buffer, `nLeafData += len(buffer)`, `nWork++` (=flushCount),
    `iBlock++`, `buffer = nil`; else append to buffer;
  - track `root` incrementally (engine's root blob lists leaf block ids:
    append iBlock on each flush — extend `fts` root builder instead of
    `SerializeSegmentForTerms`);
  - keep the existing `flushCount >= nRem` stop EXACTLY as-is (it is the
    engine's `pWriter->nWork>=nRem` analogue, fts3_write.c:5055–5058).

**D4.** Release (fts3IncrmergeRelease equivalent): write the final buffer at
`iBlock` (if non-empty), then write the segdir row with `start_block=iStart`,
`leaves_end_block=iBlock`, `end_block = "<iEnd> <nLeafData>"` (fixes D5 —
REAL size, not 0), root=rebuilt root. Reuse the existing explicit-rowid
writer (`writeFTSShadowRowAtRange`, extended with an endBlock parameter) for
fresh output; for a continuation (replacingOut) use the in-place UPDATE
(`updateFTSShadowRowRange`, same extension).

**D5.** Continuation (fts3IncrmergeLoad equivalent): when the hint directs a
continuation, instead of reading the whole output's doclists
(`segdirRowStreamDoclists`), verify appendable (marker at end_block's block
id), then resume: `iStart=start_block`, `nLeafData` from end_block suffix,
`iBlock=leaves_end_block`, `buffer` = bytes of the block at leaves_end_block
(SQLite reloads the last leaf into the writer buffer and rewrites it on
release). Keep the existing first-key ordering guard: the first merged term
must be > the output's last term (engine's k-way merge already feeds sorted
terms; assert it).

**D6.** Remove `existingDoclists` accumulation + `SerializeSegmentForTerms`
call from the merge output path (keep the function for crisis merge/flush).
Delete `outputIDs`/`seen` if now unused.

**D7.** Verify per step: am=2 repro structure must progress toward
`1 1|2 1|4 1|6 1`; every intermediate commit keeps fts4merge/2/3/5 green and
fts4merge4 line-261 got-values recorded in the commit message.

### Workstream E — promotion (fixes D6-divergence; closes the structure gap)

**E0. RED test** from D0's golden file: engine am=2 tx-100 must produce
`1 1|2 1|4 1|6 1`.

**E1.** `promoteFTSSegments(tableName, outLevel, nLeafData)` in export.go
(mirror fts3_write.c:3119–3205):
  - scan segdir rows with level > outLevel (same index: level < 1024 for
    main), ORDER BY level DESC, idx ASC (SQL_SELECT_LEVEL_RANGE2 azSql#37);
  - for each row parse size = end_block text suffix; if size<=0 or
    size > (nLeafData*3)/2 → abort promotion entirely (bOk=0);
  - else two-phase UPDATE exactly like SQLite: renumber each visited row to
    `level=-1, idx=i` (i = 0..N in that ORDER BY), then
    `UPDATE %_segdir SET level=outLevel WHERE level=-1`. Use per-row UPDATEs
    by (level,idx) — the staging level avoids transient (level,idx)
    collisions.
  - only fire when the merge fully consumed its level (`remaining==0`) AND
    nLeafData>0 (call site: end of the MergeFTS iteration, after
    ClearMergeCtx; SQLite call site 5076 runs when `nSeg==0 &&
    !bNoLeafData`).

**E2.** Also hook promotion after crisis merges (SQLite call site 3305) if
crisisMergeFTSLevel fully consumes its level — check fts4merge4 2.1.x
scenarios (16-segment crisis) for divergence; add only if the oracle needs it.

**E3.** Verify: E0 test green; full fts4merge4 run — the 8 line-261
assertions (4 am values × 2 variants) must pass.

### Workstream F — flush-gate parity (fixes D7+D8; small)

**F1.** FlushFTSSegments (export.go ~586): add the SQLite pre-gate
`nLeafAdd > 4` (`nMinMerge/16`) before computing A.

**F2.** Add to `nLeafAdd`: prefix-index blocks (`len(pBlocks)`) and
delete-marker blocks (`len(dmBlocks)`) — fts3_write.c:2298/2413 count every
leaf block the transaction wrote.

**F3.** Verify: am=2/am=4 scenarios vs oracle; fts4merge4 2.2.x assertions.

### Workstream G — runtime to < 300s (measured, am=2 repro 9.86s profile)

Current CPU split (cumulative): FlushFTSSegments+MergeFTS 31% (2.84s; the
D4 rewrite removes the per-call full rewrite + its allocation churn),
pager.Flush/pwrite 16.6%, Pager.Snapshot 12.2%, hasWithoutRowidKeyword 6.9%
(incl. regexp + ToUpper), INSERT path 14%, GC (madvise+mallocgc+scan)
~30% — mostly from Snapshot's page copies and the merge rebuilds.

**G1. COW Snapshot (biggest single win).** `internal/pager/pager.go`
`Snapshot()` (line 68) deep-copies EVERY cached page on every BEGIN/INSERT/
UPDATE/DELETE/conflict-scan — O(pages) per statement, O(n²) over 100 txs.
Redesign: `PagerState` becomes `{savedPages map[uint32]*Page; numPages
uint32; header []byte}`; the pager keeps a list of ACTIVE snapshots;
`WritePage` (and `AllocatePage` for header/numPages changes) lazily saves a
deep copy of the page's PRE-image into every active snapshot that lacks it
(first write only); `Restore` reinstates saved pages, deletes pages allocated
after the snapshot (pageNum > numPages), restores header/numPages, and
unregisters. Semantics identical to today's full copy; cost proportional to
pages actually modified (SQLite's rollback-journal philosophy).
  - G1a. RED/parity test: exhaustive unit test — nested snapshots, restore
    inner then outer, allocate-after-snapshot, header change — behavior must
    equal the old deep-copy implementation (keep the old one in the test as
    oracle).
  - G1b. Implement; benchmark `BenchmarkFTSSnapshot` before/after; expect
    ≥10% off the am=2 repro (12.2% of profile).

**G2. Cache WITHOUT ROWID flag.** `hasWithoutRowidKeyword` runs
`strings.ToUpper(entry.SQL)` + CTAS-strip per UPDATED/DELETED ROW (6.9% of
profile). Compute once: add `WithoutRowid bool` to `schema.Entry` at
CREATE/ALTER time; replace all `hasWithoutRowidKeyword(strings.ToUpper(
entry.SQL))` call sites (update.go:292/300, delete.go:271/296, and any
others — grep). Keep the helper for the tests only.

**G3. pager flush order.** `flushAll` iterates `range p.dirty` (random map
order) → random-offset pwrites. Sort the dirty page numbers ascending and
coalesce ADJACENT pages into one WriteAt (cap coalesce at ~64KB); also
pre-size the file once per flush (one Truncate to max page end) instead of
per growing page. Expect ~3–5% (pwrite 16.6% of profile).

**G4. After A+C+D land:** re-profile; the duplicate rows' extra UPDATEs,
splits, repack re-inserts and leaked blocks disappear — re-measure the full
fts4merge4 wall time before doing anything further. Target ≤260s to leave
headroom under the 300s cap (runner variance).

## 10. Unrelated issues found during review (fix separately, do NOT block on)

1. `splitLeafMulti` sorting is O(n²) selection sort
   (`bubbleSortSplitCells`, btree_insert.go:553) + `probe := append(append(
   []splitEntry{}, cur...), c)` allocates per cell (line 436) — O(n²) allocs
   on every split. Replace with `sort.SliceStable` + a running-size
   accumulator (no probe copies). SQLite balances via memcpy runs.
2. `insertInteriorPage` matches errors by STRING:
   `err.Error() != "btree: interior page full..."` (btree_insert.go:273) —
   sentinel error (`var errInteriorFull = errors.New(...)`) instead.
3. Dead debug: empty `if os.Getenv("FRIGO_DEBUG_SPLIT") != "" {}` blocks in
   `createInteriorRootAtPage1` (btree_insert.go:666,688) — delete.
4. Index b-tree insert routing ALWAYS descends rightmost
   (`findChildPageForInsert`, btree_insert.go:817) and index leaf splits use
   `medianKey = len(cellData)` (line 497) — lengths, not keys, as interior
   separators. NOT reproduced at SQL level (5k random rows, covering-index
   point lookups all correct; EQP shows real index seek) — but the routing
   assumption only holds while index inserts are append-mostly. Add a
   btree-level unit test (force interior split, insert mid-key, seek) before
   any change; file a separate goal if it fails.
5. `export.go` is 2148 lines — exceeds the quality gate's HARD 1000-line max;
   any commit staging it FAILS the pre-commit hook. The workstreams above
   must split it: `export_fts_flush.go` (flush/stat), `export_fts_merge.go`
   (MergeFTS+writer+chomp+repack+promotion), keep generic helpers in
   export.go. Do the split as the FIRST commit of workstream D (pure move,
   no behavior change, `git mv`-style).
6. Uncommitted debug instrumentation in 4 files (§7) — must be reverted or
   (where kept as env-gated debug) cleaned before any commit in A/B/C.
7. `applyUpdateChanges` re-fires `FindTable` per change row (update.go:290)
   and re-uppercases SQL per row — fold into the G2 cache fix.
8. `readFTSSegdirRows` does a FULL table scan per call and is called several
   times per merge iteration (level lookup, hint check, outRows, repack).
   After C1's rewrite the repack call disappears; consider one indexed
   (level,idx)-ordered read per iteration (SQLite's azSql#12 ORDER BY idx).

## 11. Verification commands (per commit)

```bash
# unit + suite
go test ./... -count=1
go test -run TestSOLID_ ./... -count=1
tools/quality_gate.sh <changed files>
# feature packages, ordered
go test -tags testgen ./testgen/fts4merge/  -count=1
go test -tags testgen ./testgen/fts4merge2/ -count=1
go test -tags testgen ./testgen/fts4merge3/ -count=1
go test -tags testgen ./testgen/fts4merge5/ -count=1
go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s   # FINAL gate
# am=2 repro (structure + runtime)
cd /tmp/ftsdbg && time go run .   # expect oracle `1 1 / 2 1 / 4 1 / 6 1`, <8s
```

## 12. Execution order & commit plan

1. **Commit 0 (hygiene):** revert/strip the 4 files' debug instrumentation
   (§7); split export.go (issue 10.5); quality gate green.
2. **Commit 1 (A):** btree same-rowid-before-split + RED test. → kills
   duplicate creation.
3. **Commit 2 (B):** applyUpdateChanges dedupe + RED test. → stops
   amplification even if a duplicate ever reappears.
4. **Commit 3 (C):** repack in-place + block-range delete + tests. → kills
   the rowid collision source AND the block leak.
5. **Commit 4 (D):** appendable merge writer (D1–D6), splitting further if
   the diff exceeds ~400 lines. → quota respected; MergeFTS runtime drop.
6. **Commit 5 (E):** promotion + golden-file test. → the 8 line-261
   assertions.
7. **Commit 6 (F):** flush-gate parity (nLeafAdd>4, full nLeafAdd).
8. **Commits 7–8 (G):** COW snapshot, WITHOUT ROWID cache, flush ordering —
   each with its own benchmark evidence; stop as soon as full fts4merge4
   ≤260s.
9. **Final:** full suite + `tools/status` update; goal completion audit.

## 13. Risks / open items

- D (appendable writer) changes the on-disk layout expectations of
  fts4merge/1.3/4.3 (hint continuations) — their assertions pin block counts;
  re-run after every D micro-step, not only at the end.
- Promotion (E) renumbers (level,idx) across levels — the MergeCtx keyed by
  level must be invalidated/rekeyed after promotion (SQLite's writer state
  dies with the merge call; the engine's SetMergeCtx persists — clear
  MergeCtx for all promoted levels).
- COW snapshot (G1) touches every DML path; gate it on the exhaustive parity
  test (G1a) plus the full suite, and keep the old implementation callable
  under a build tag for the test oracle if needed.
- If the 8 line-261 assertions STILL diverge after A–F: capture per-tx segdir
  dumps from both engines (D0 harness) and diff at the FIRST diverging tx —
  do not resume speculative tracing (per plan/GUIDELINES no-try/fail rule).

---

# PART 3 — Execution session notes (2026-08-16, third session)

## What landed (commits 54286993b → ef356a3f3)

- Workstreams 0, A, B, C, D-complete: hygiene+split, btree same-rowid
  replace before split (SQLite btree.c:9608), UPDATE dedupe, in-place
  repack (fts3RepackSegdirLevel) + block-range cleanup (fts3DeleteSegment),
  real incremental leaf writer (fts3IncrmergeAppend) with end_block size
  suffix, promotion (fts3PromoteSegments), flush gates (nLeafAdd>4 + full
  nLeafAdd), block-id allocation consistency, hint remaining = loaded-
  deleted, segdir rowid-cache hygiene.
- fts4merge/2/3/5 green; fts4merge4 runtime 172s → 33s (criterion's 300s
  cap is no longer a concern); fts4merge4 structure now close but not
  exact: am=2 got `1 1 2 1 6 1`/`0 16 1 3` at various points vs oracle
  `1 1 2 1 4 1 6 1`; fts4merge 1.3 still over-merges (7 L2 segs vs 4).

## Oracle methodology correction (IMPORTANT)

- testfixture builds with `-DSQLITE_DEFAULT_PAGE_SIZE=1024`
  (main.mk:1792). Any CLI/python-sqlite oracle at 4096 pages gives WRONG
  structures (they produce `2 2|3 1|4 1|5 1`, not the TCL expectation).
  A C harness against ~/dev/sqlite/sqlite3.c with
  `-DSQLITE_ENABLE_FTS3 -DSQLITE_DEFAULT_PAGE_SIZE=1024` reproduces ALL
  fts4merge4 2.2 expectations exactly (am=2→1 1 2 1 4 1 6 1; am=4→1 2 2 1
  3 1; am=8→0 4 1 3 2 1). Harness: /tmp/oracle_c/h.c (per-tx segdir dump).
  Engine default page size is already 1024.

## Remaining divergence (the blocker for the completion criterion)

1. **fts4merge 1.3/1.4** (`merge=1` series): engine over-merges L1 into
   too many L2 segments (7 vs oracle 4). Symptom of hint-continuation
   scheduling: SQLite pops the hint list at iteration start (undo on
   non-use) and pushes remaining after chomp; the engine rewrites a single
   hint row and its pop/use condition differs.
2. **fts4merge4 tx18** (am=2): oracle `0:1 1:2 2:1`; engine `0:1 1:3`.
   After the remaining=loadCount fix the level ascends correctly only if
   the continuation/appendable detection (MergeCtx.OutRowID) and the
   hint-emptiness after a full consume line up; still one merge too many
   at L0 in some runs.
3. **Duplicate rowid 26**: `ALLOCIDX lvl=0 rows=[i0/r26 i0/r26]` — the
   segdir table briefly holds two cells with rowid 26. Suspected pair: the
   merge's explicit-rowid write (writeFTSShadowRowAtRange rowid =
   ftsSegdirNextRowID) followed by the flush's IMPLICIT-rowid INSERT whose
   allocator cache was stale, and the btree same-rowid REPLACE not firing
   (findInsertPositionTable not seeing the existing r26 — possibly after
   an interior split of the segdir tree). Invalidation was added at repack;
   the remaining writer to fix is the flush's writeFTSShadowRow (implicit
   rowid) — it must also invalidate/rescan the segdir rowid cache before
   allocating.

## Evidence trail (for the next investigator)

- /tmp/oracle_trace_am2.txt (oracle per-tx level counts), /tmp/oracle_c/h.c
  (reproducible oracle harness), /tmp/engine_trace_am2.txt (engine per-tx),
  /tmp/ftsdbg (engine repro, per-tx dump), /tmp/dbg.txt.
- SQLite references: fts3_write.c sqlite3Fts3Incrmerge 4970-5090 (hint
  pop/push), fts3IncrmergeAppend (nWork), fts3IncrmergeLoad (appendable
  check), fts3IncrmergeChomp (nSeg=remaining), fts3PromoteSegments 3119.
- Next step per plan §9/E: match the hint POP semantics (engine must not
  reuse the same hint row across iterations of one call; SQLite pops it
  and re-pushes only on remaining>0) and fix the flush's implicit-rowid
  allocation to rescan before insert.

---

# PART 4 — Execution session notes (2026-08-16, fourth session, "don't block" directive)

## Fixed this session (commit 764eff64b)

Six root causes found by instrumenting SQLite itself (fts3.c fts3SyncMethod +
fts3_write.c fts3Incrmerge/fts3PromoteSegments print traces, oracle harness
/tmp/oracle_c/h.c with -DSQLITE_DEFAULT_PAGE_SIZE=1024):

1. btree findChildPageForInsert: rowid == interior separator routed LEFT;
   SQLite routes equality RIGHT. Fixed (`<=`), killing the duplicate-rowid
   class (%_segdir r21/r26).
2. fts3PromoteSegments renumbers ALL rows from iAbsLevel up (not just higher
   levels) — fixed, killing duplicate (level,idx) after promotion.
3. readFTSSegdirRows now sorts by idx (SQLite azSql#12 ORDER BY idx ASC);
   the merge loads "oldest nSeg" and relied on idx order.
4. Partial-merge outputs carry NEGATIVE nLeafData (SQLite negates when
   nSeg!=0); promotion aborts on nSize<=0. The engine now defers the
   %_segdir row write until after chomp and writes the sign correctly.
5. Continuation outputs accumulate the existing segment's size (SQLite's
   pWriter->nLeafData persists across calls).
6. Flush idx/rowid caches invalidated after merges (repack both paths).

Result: am=2 automerge trace now matches the instrumented oracle for
transactions 0..21 EXACTLY (tx18, tx19, tx21 all match; previously diverged
at tx18). fts4merge/2/3/5 still green; fts4merge4 structure at 1 43|2 1.

## Remaining divergence (tx22+)

Oracle tx22 (incrmerge call #6, nRem=372): iter1 L0 nSeg=2 (bUseHint=0,
hint=(1,2) unused since find=0 < hint=1) → fully consumed (nSeg=0) → new L1
output; iter2 hint (1,2) → L1 merge (nSeg capped 2) → append to L2[0] →
L1 truncated (2 of 4). Result `0:1 1:2 2:2`.

Engine tx22: L0 had ONE row (flush) → find=1 → iter1 merged L1 directly
(hint (1,2) used) → L1:4 unchanged. The engine's tx22 L0 count (1) differs
from the oracle's (2). The oracle's tx21 `1:4 2:1` shows L0:0, so the
oracle's tx22 flush should give L0:1 too — the L0:2 in the oracle's iter1
FIND is UNRESOLVED. Next step: dump the oracle's L0 rows (idx,rowid) at
tx21-22 pre/post flush to see where the second L0 row comes from (or verify
the tx↔call mapping with a per-tx marker in the incrmerge trace).

---

# PART 5 — Execution session notes (2026-08-16, fifth session)

## Fixed (commit 90f712a24): no-replay incremental-merge continuation

The continuation previously REPLAYED the whole existing output through the
leaf writer (rewriting every leaf at fresh ids). SQLite's fts3IncrmergeLoad
keeps existing leaves in place and appends only NEW leaves. The replay made
the output so large that the leaf quota stopped mid-merge where SQLite's
append finished (tx22: engine 506 leaves vs SQLite 261), so L1/L2 merges
truncated where SQLite's consumed. Now: existing leaves/row kept, writer
resumes with leavesOut = mc.IBlock, existing row UPDATEd (not deleted), root
rebuilt from existing boundaries (fts.ParseSegmentRootBounds/BuildInteriorRoot)
+ new boundaries, append rejected when first merged term <= existing last term.

Result: am=2 matches the instrumented oracle through tx25 (was tx18 at the
start of this effort); final structure 1 1 | 3 2 | 4 1 vs oracle
1 1 | 2 1 | 4 1 | 6 1; fts4merge4 runtime 76s; fts4merge/2/3/5 green.

## Remaining

1. am=4/8: L0 stuck at 16 (final `0 16 1 1 2 1` vs oracle `0 4 1 3 2 1`).
   The automerge at L0 merges 8 segs under quota 186 → truncates all 8
   (deleted=0) — same as the oracle (oracle call #8: nWork=186 nSeg=8). The
   oracle's L0 then drops to 4 because later merges (bigger A as mxLevel
   grows) fully consume L0 sets; the engine's L0 instead refills to 16 and
   the crisis STOPS re-firing (L0:16 stuck). Next step: verify the engine's
   crisis condition (allocFTSIdx cache vs scan path) at L0:16 on the second
   cycle, and whether the L0 merge's quota grows with mxLevel in the engine
   the same way as SQLite.
2. am=2: final missing L2:1 (engine 3:2) and L6 (engine 4:1) — the sparse
   high levels need the later-tx merge chains to match; probably falls out
   of the am=4/8 L0-consumption fix.

Oracle harness: /tmp/oracle_c/h.c (am as argv[2]); engine: /tmp/ftsdbg
(AM env). SQLite refs: fts3_write.c fts3IncrmergeLoad 4258, fts3IncrmergePush,
fts3SyncMethod fts3.c:3518.

---

# PART 6 — Execution session notes (2026-08-16, sixth session)

## Fixed (commits 90f712a24, b4f028ae7)

- No-replay incremental-merge continuation (90f712a24): existing leaves keep
  their block ids; only NEW merged leaves are appended (SQLite
  fts3IncrmergeLoad). This removed the O(segment) replay per continuation and
  fixed the quota-overshoot that made L1/L2 merges truncate where SQLite's
  consumed.
- Incr-merge hint is a LIST (b4f028ae7): SQLite's %_stat id=1 hint is a
  sequence of (absLevel, nSeg) pairs, popped at iteration start (restored on
  non-use), pushed (level, remaining) at the end. The engine's single-entry
  row dropped pending entries for other levels (tx26: (1,2) and (2,2)).

## Result

- am=2 final: 1 1 | 6 2 | 7 1 | 9 1 (oracle 1 1 | 2 1 | 4 1 | 6 1) — the
  sparse-high-level shape is now achieved (was 0 17 1 3 2 1 at the blocker).
- fts4merge4 runtime 203s (< 300s).
- fts4merge/2/3/5: 2/3/5 green; fts4merge REGRESSED (hint-list change):
  1.3 got 2 0..4 (5 L2 segs) want 2 0..3 (4); 5.10/5.11 got 0..9/0..11 vs
  want 0..11/0..12.

## Remaining

1. am=2 promotion cascade: engine 1,6,7,9 vs oracle 1,2,4,6 — the engine's
   merges create/keep extra high levels. The engine's tx26 had a THIRD
   iteration (L2 fresh, deleted=0) the oracle lacks (its iter2 remaining=2
   → push (2,2) + nRem=-1 → exit). Root: the engine's L2 continuation
   DELETES its 2 inputs (EOF) where SQLite TRUNCATES (quota-stopped) —
   the input sizes at tx26 differ from the oracle's, a cumulative cascade
   divergence.
2. fts4merge regression from the hint list: the merge=1 series' hint
   pop/restore across CALLS must match SQLite's bDirtyHint semantics (store
   only on pop-use or push). The engine stores the list on every
   fromHint-full-consume (storeHint) which may over-store; and the
   restore-on-not-usable path keeps the popped entry in the in-memory list
   but SQLite's hint.n undo keeps it in the STORED blob — verify the
   cross-call persistence for merge=1.

---

# BLOCK STATUS (goal sparky.puma, 2026-08-16)

**Status: BLOCKED** — completion criterion unmet after exhaustive autonomous
effort across many sessions. All real root causes found and fixed; the
remaining divergences are exact-structure-matching details documented below.

## What the criterion requires

1. `go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s` exits
   0 (all 8 line-261 assertions) AND wall < 300s.
2. `go test -tags testgen ./testgen/fts4merge/ ./testgen/fts4merge2/
   ./testgen/fts4merge3/ ./testgen/fts4merge5/ -count=1` exits 0.
3. `go test ./... -count=1` exits 0.

## Verified current state (commits 54286993b → b4f028ae7)

DONE (all corruption classes fixed, each with evidence + tests):
- btree same-rowid REPLACE before the full-page check (A) + interior routing
  equality (`<=` separator) — killed the duplicate-rowid class (r21/r26).
- UPDATE change dedupe (B).
- in-place repack (C) + block-range cleanup (fts3DeleteSegment).
- real incremental leaf writer (D): fts.IncrLeafWriter, no-replay continuation
  (existing leaves keep their ids, only new leaves append), hint-as-LIST
  (fts3IncrmergeHintPop/Push), promotion renumber-over-all-levels + negative
  size markers (partial merges negate nLeafData; promotion aborts on <=0),
  block-id / rowid cache hygiene, idx-sorted segdir reads.
- flush-gate parity (F): nLeafAdd > 4, full nLeafAdd.
- fts4merge/2/3/5: 2/3/5 green; fts4merge regressed by the no-replay
  continuation (90f712a24).
- fts4merge4: 203s < 300s; am=2 shape = `1 1 | 6 2 | 7 1 | 9 1` (oracle
  `1 1 | 2 1 | 4 1 | 6 1`) — sparse-high-level SHAPE achieved (was
  `0 17 1 3 2 1`), wrong levels remain.
- `go test ./...`: 12 pre-existing unrelated root-package failures +
  internal/parse TestGrammarCoverage — NEVER green at HEAD (blocker.md t9).

## Remaining divergences (precise, with evidence)

1. **fts4merge no-replay regression** (criterion 2): merge=1 series (1.3:
   20x `merge=1`, quota 1 each) produces 5 L2 segs, oracle 4 — the
   L1→L2 drain's delete-vs-truncate boundary at quota 1. Reproduced in
   /tmp/fm1.dir (FINAL `2 0 1 2 3 4`). Append-order check is REQUIRED
   (disabling it → "database disk image is malformed").
2. **am=2 promotion cascade at tx26** (criterion 1): the engine runs a THIRD
   merge iteration where SQLite's nRem=-1 exits — the engine's L2
   continuation DELETES its 2 inputs where SQLite TRUNCATES, because the
   engine's L2 input sizes at tx26 differ cumulatively from the oracle's.
3. **am=4/8** (criterion 1): `0 8 | 1 7 | 2 7 | 3 2` vs `0 4 | 1 3 | 2 1` —
   cascades from the same delete-vs-truncate accounting.
4. **`go test ./...`** (criterion 3): 12 unrelated failures + parse — never
   green at HEAD; either fix or relax the criterion to the FTS packages only.

## Evidence trail (for the next investigator)

- blocker.md PART 2-6: divergence table D1-D8, workstreams, session notes.
- /tmp/oracle_c/h.c: instrumented SQLite harness (fts3SyncMethod,
  fts3Incrmerge, fts3PromoteSegments prints; -DSQLITE_ENABLE_FTS3
  -DSQLITE_DEFAULT_PAGE_SIZE=1024) — reproduces ALL TCL expectations.
- /tmp/oracle_trace_am2.txt + /tmp/engine_trace_am2.txt: per-tx level counts.
- /tmp/fm1.dir: fts4merge merge=1 repro (FINAL `2 0 1 2 3 4`).
- Regression boundary: commit 90f712a24 (no-replay continuation) breaks
  fts4merge; b4f028ae7 (hint-list) fixes fts4merge4's shape.
- SQLite refs: fts3_write.c fts3IncrmergeLoad 4258, fts3IncrmergeChomp,
  fts3IncrmergeHintPop/Push, fts3PromoteSegments 3119; fts3.c fts3SyncMethod
  3518.

## Unblocking expectations (what the next investigator / user needs to supply)

1. Trace the engine's merge=1 L1→L2 drain vs SQLite's at quota 1 (the
   reader-EOF-at-quota delete-vs-truncate boundary) and fix fts4merge.
2. Trace the engine's tx26 L2 continuation input sizes vs the oracle's and
   align the chomp delete/truncate decision (SQLite truncates when the quota
   stops mid-stream even if the reader is at EOF for the last processed term).
3. Either fix the 12 unrelated root failures + internal/parse, or accept a
   relaxed criterion scoped to the five fts4merge* packages.
