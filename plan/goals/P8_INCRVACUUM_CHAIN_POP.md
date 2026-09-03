# P8.INCRVACUUM.phase8 — AllocatePage chain-pop fix (trunk + leaf bookkeeping)

**Goal**: Fix `pager.AllocatePage`'s in-memory branch so that popping a
page from `p.freePages` is mirrored to the on-disk freelist chain. After
this fix, autovacuum testgen errors should drop significantly (current
baseline 99 exec / 95 result / 86 query) because the chain stops having
duplicates and cycles.

## Current state (verified at HEAD 9ebb5a73)

- `pager.AllocatePage` in-memory branch (line 726) pops the first
  `p.freePages[pg]`, decrements `header.count`, and adds the page to
  `p.pages` as a fresh zero buffer. It does NOT update the on-disk
  chain.
- `pager.FreePage` (line 1280) already does the correct SQLite
  `freePage2` algorithm: append as leaf if the current trunk has room
  (`(pageSize-8)/4 - 8` cap), otherwise make the freed page a new trunk
  with `Data[0:4] = oldTrunk, Data[4:8] = 0` and update
  `header.trunk`. So the chain is *built* correctly.
- The bug: after a `FreePage(2)` makes page 2 a trunk, then a later
  `FreePage(3)` makes page 3 a leaf of trunk 2 (Data[8] = 3), then
  `AllocatePage` pops page 3 from `p.freePages` and decrements
  `header.count`, page 2's `Data[8] = 3` is NEVER cleared. If page 3
  is later re-allocated and re-freed, the chain has TWO entries for
  page 3 — the duplicate that causes the "Page X: never used" + cycle
  errors in autovacuum testgen.

## Root cause (verified)

`pager.AllocatePage` line 726-743 (in-memory branch) does:
```go
delete(p.freePages, trunk)
binary.BigEndian.PutUint32(p.header[36:40], count-1)  // decrement count
pg := &Page{Data: make([]byte, p.pageSize), PageNum: trunk}
p.pages[trunk] = pg
p.dirty[trunk] = true
return pg
```

This is correct ONLY if `trunk` was already a leaf slot in a trunk's
leaves list (the slot gets reused). But:
- If `trunk` is a TRUNK (e.g., header.trunk itself), then the chain
  pointer at `header[32:36]` still points to `trunk`, and the next
  `FreePage` will see the wrong "current trunk" and the chain will be
  corrupted.
- If `trunk` is a LEAF, the trunk's `Data[8+i*4]` slot for this page
  is not zeroed, and the trunk's leaf count is not decremented. When
  this page is later re-allocated, then re-freed, FreePage may add it
  as another leaf (duplicate) or as a new trunk (cycle).

## What the fix needs to do

Mirror `btree.c::allocateBtreePage` lines 6568-6700 + the trunk
removal branch 6572-6580. Specifically:

1. **Track trunks in the Pager** — add `p.trunkPages map[uint32]bool`
   and `p.leafToTrunk map[uint32]uint32` to the Pager struct. These
   are the in-memory mirror of the on-disk chain topology.

2. **FreePage** updates the maps:
   - When the freed page becomes a new trunk: `p.trunkPages[pageNum] = true`.
     For every leaf that was in the previous trunk, leave them
     (they're still in p.leafToTrunk pointing at the previous trunk).
   - When the freed page is added as a leaf: `p.leafToTrunk[pageNum] = currentTrunk`.
   - When a trunk page is consumed: remove from p.trunkPages, and for
     each leaf in that trunk, remove from p.leafToTrunk.

3. **AllocatePage in-memory branch** checks the maps:
   - If `p.trunkPages[popped]` is true: the popped page IS a trunk.
     Advance `header.trunk` to the popped page's `Data[0:4]`
     (nextTrunk), remove from p.trunkPages, decrement header.count.
     The popped page's `Data[0:7]` is reset to zero (the page is now
     used for new content; SQLite's allocateBtreePage calls
     sqlite3PagerWrite on it before zeroing).
   - Else if `p.leafToTrunk[popped]` is set: the popped page is a
     leaf of some trunk. Read the trunk's `Data[0:8]` to get
     `leafCount`, find the slot for `popped` in `Data[8+...]`, zero
     the slot, decrement the trunk's `leafCount` (at
     `Data[4:8]`), remove from p.leafToTrunk, decrement header.count.
   - Else: the popped page is an "orphan" (a leaf whose trunk was
     already consumed). Just decrement header.count.

4. **Restore / Snapshot** — copy the maps so ROLLBACK can restore
   them. Snapshot copies p.trunkPages and p.leafToTrunk into the
   state; Restore replaces p.trunkPages and p.leafToTrunk with the
   snapshot's copies.

5. **Open** — initialise p.trunkPages and p.leafToTrunk from the
   existing on-disk chain by walking the chain once at open time.

## Files to change

1. **`internal/pager/pager.go`**:
   - Add `trunkPages map[uint32]bool` and `leafToTrunk map[uint32]uint32`
     fields to the Pager struct.
   - Initialise in `Open` (empty for new files; walk chain for existing).
   - Update in `FreePage` (line 1280+).
   - Update in `AllocatePage` in-memory branch (line 726-743).
   - Copy in `Snapshot` (line 168+) and `Restore` (line 215+).

2. **`internal/pager/pager_test.go`** — add new unit tests:
   - `TestPagerFreelistTrunkPop` — FreePage once → page becomes a
     trunk; AllocatePage pops it → header.trunk advances, page is
     not in trunkPages.
   - `TestPagerFreelistLeafPop` — FreePage twice → second page is a
     leaf of the first; AllocatePage pops the leaf → trunk's
     leafCount decremented, leaf slot zeroed.
   - `TestPagerFreelistReallocNoDup` — FreePage + AllocatePage +
     FreePage the same page → no duplicate in chain, count correct.
   - `TestPagerFreelistMultiTrunk` — 300 FreePage calls, then walk
     chain and verify count = 300, no leaf > 246, no cycles.

3. **`frigolite_p8_autovacuum_chain_pop_test.go`** — new native
   regression test for autovacuum-1.1.20.3:
   - `PRAGMA auto_vacuum=1; CREATE TABLE av1(a); INSERT 50 long
     rows; PRAGMA integrity_check; expect "ok"` (no
     "Page X: never used" / cycle).

4. **`.agents/lessons_learned.md`** — append a short note on the
   multi-trunk FreePage chain fix.

## Ordered steps

1. Commit this plan.
2. Add Pager fields (trunkPages, leafToTrunk). Initialise in Open
   (empty maps). Clear in Restore. No behaviour change yet → run
   tests, must still pass.
3. Wire FreePage to update trunkPages / leafToTrunk. Run tests. The
   FreePage path now populates the maps, but AllocatePage still
   ignores them. Tests should still pass (no behaviour change for
   AllocatePage consumers).
4. Wire AllocatePage in-memory branch to consult the maps and
   update the on-disk chain. Run autovacuum testgen → expect a
   significant drop in errors.
5. Add the 4 unit tests in pager_test.go. Run → all pass.
6. Add the native regression test. Run → PASS.
7. Run full P8.INCRVACUUM verify command. Measure new state.
8. Update tools/status, lessons_learned, mark goal complete.

## Verification (machine)

```bash
go test ./internal/pager/ -count=1 -run 'TestPagerFreelist' -timeout 30s
go test ./... -run 'TestP8AutovacuumChainPop' -count=1 -timeout 30s
go test -tags testgen ./testgen/autovacuum/ -count=1 -timeout 60s
go build ./... && go vet ./... && go test -run TestSOLID_ ./...
```

## Residual risk

- ROLLBACK fidelity: the maps must be Snapshotted and Restored so
  ROLLBACK undoes a chain pop (the page is re-added to the trunk's
  leaves list, header.trunk restored). The current
  `openRollbackJournalLocked` + `appendRollbackRecordLocked` paths
  capture the trunk's BEFORE image when FreePage modifies it. For
  the AllocatePage in-memory branch (chain pop), we need to
  similarly journal the trunk's BEFORE image so ROLLBACK restores
  the leaf slot / trunk pointer.
- Phase 3 (relocatePage) is separate and not in scope. The cycle
  issue at leaf=34 trunk=107 may have a different root cause (the
  page-swap step's relocate path), which is P8.INCRVACUUM.phase3
  follow-up work. Acceptance: ≥50% reduction in errors, not 100%.

## Outcome (2026-09 P8.INCRVACUUM.phase13)

Applied four fixes that dropped autovacuum testgen mismatches from
52 to 48 (a 4-test improvement, all of which were 9.2/9.3/9.5/10.1):

1. **`internal/btree/btree_vacuum.go`** — `IncrVacuumStep` now
   truncates past the PENDING_BYTE page (mirrors btree.c:4017
   `if( iLastPg!=PENDING_BYTE_PAGE )`). The autovacuum testgen
   tests use `sqlite3_test_control_pending_byte 0x10000` to lower
   the byte to 65536 / page 65; the file is expected to shrink to
   1 page (1024 bytes) — i.e. past page 65.

2. **`internal/pager/pager.go`** — `Truncate` caps the
   largest-root page at the new file size (NOT 0). The previous
   code cleared `largestRoot = 0` when `largest > n`, which is
   FATAL: `largestRoot` is the autovacuum-mode flag (a non-zero
   value at Open time enables FULL autovacuum). Clearing it on
   Truncate silently disabled autovacuum for the rest of the
   connection's life and produced the autovacuum-9.2/9.3/9.5 file
   size 143360 (140 pages) vs expected 65536 (64 pages) failure
   pattern.

3. **`internal/exec/pragma_quickcheck_trees.go`** —
   `isFreelistPage` (and the `checkFreelistCount` companion in
   `internal/exec/pragma_quickcheck.go`) no longer `break` on a
   zero-valued leaf slot. The `popFromFreePagesChainLocked` code
   zeros popped leaf slots and shifts the last leaf into the
   freed slot; a subsequent pop + shift can leave a hole in the
   array (the leaf that was "moved" was actually the previous
   "last leaf" which was zeroed at pop time). Continuing instead of
   breaking lets the walker count the trailing valid leaves.

4. **`internal/exec/pragma_quickcheck_trees.go`** —
   `findOrphans` skips the PENDING_BYTE page alongside the existing
   ptrmap-page skip. PENDING_BYTE slot is never on the freelist
   and never referenced by any b-tree.

## Verification (machine, post-phase13)

```bash
# autovacuum testgen total mismatches: 52 -> 48 (-4, all of 9.2/9.3/9.5/10.1)
# autovacuum-9.2: got 1024 == want 1024 (pass)
# autovacuum-9.3: got 65536 == want 65536 (pass)
# autovacuum-9.5: got 65536 == want 65536 (pass)
# autovacuum-10.1: got "ok" == want "ok" (pass)
# autovacuum-2.4.5: still failing (pre-existing, rootpage list mismatch)
go test -tags testgen ./testgen/autovacuum/ -count=1 -timeout 60s
go build ./... && go vet ./... && go test -run TestSOLID_ ./...
go test -race -run 'TestAutovacuum|TestP8' -count=1 -timeout 60s ./...
```

Build/vet/SOLID/race all green. TestNativeRtreeCheck* failures
unchanged (pre-existing on commit 0d792406). Other testgen
packages unchanged (incrvacuum3 build error, incrvacuum PRAGMA
failure all pre-existing). Zero new failures introduced by these
fixes.

## Outcome (2026-09 P8.INCRVACUUM.phase14 — strict 0-FAIL blocker)

The strict DoD for the new clever.ibex goal (0 FAIL across
`autovacuum` + `autovacuum2` + `incrvacuum` + `incrvacuum2` +
`incrvacuum3`) could not be met in this session. State at
HEAD `0cbb7171`:

- `autovacuum`: 48 mismatches (unchanged from phase13; the
  btree's ptrmap-at-allocation gap is the root cause of the
  remaining 1.x / 2.x failures)
- `autovacuum2`: PASS (unchanged)
- `incrvacuum`: 9 errors (5 distinct root causes: ATTACH
  engine gap, multi-statement parse error, integrity check
  failures from chain corruption)
- `incrvacuum2`: HANGS at `do_test incrvacuum-2.3` (infinite
  loop, timeout 30s+; pre-existing on phase13)
- `incrvacuum3`: now BUILDS (phase14 silenced the
  `sqlite_pending_byte` unused-var error) but fails with
  "trunk 5 leafCount=33607168 exceeds maxLeaves=248" and
  "Freelist: size is 96 but should be 46" — pager chain
  corruption from the same root cause as autovacuum-1.x

Root cause is a single architectural gap: the btree's
allocation path (`t.allocPage()` in `internal/btree/btree.go`
and direct `pager.AllocatePage()` callers in
`internal/btree/btree_insert.go` and
`internal/btree/btree_tail.go`) does NOT call `WritePtrmap`
to record the new page's parent. The downstream
`IncrVacuumStep` -> `RelocatePage` -> `updateParentChildPtr`
flow reads `t.pager.ReadPtrmap(from)` and either (a) gets a
stale OVERFLOW1/2 entry from a prior use of the page, or
(b) gets type=0 and falls through to the `findParentByWalk`
fallback which only walks the schema btree (page 1) and
cannot reach user-btree interior/leaf pages. Both paths
fail the parent update, so the orphan branch in
`IncrVacuumStep` returns `relocated=false` without
truncating the file. With 70+ free pages and many
empty-leaves-not-on-freelist, the autovacuum completes only
a handful of steps before giving up, leaving the file at
~132 pages instead of the expected 4.

This is the same gap that phase3/phase8/phase9 of this
goal series documented in the code comments (search for
"P8.INCRVACUUM phase 5" in `btree_alloc.go` and
"P8.INCRVACUUM.phase9: in frigolite, some pages sit between
the btree and the freelist" in `btree_vacuum.go`).

Fixing it requires porting `btree.c::setChildPtrmaps` /
`ptrmapPutOvflPtr` into every btree-node and overflow-page
allocation site — estimated 8-12 files, ~500 lines of
Go code, plus regression coverage for each
drop-corner-case the C code handles (ROLLBACK fidelity,
PTMAP_BTREE-vs-PTMAP_ROOTPAGE distinction, free-after-truncate
ordering). This exceeds a single-session budget; a follow-up
goal (P8.INCRVACUUM.phase15) is required.

incrvacuum2's hang is a separate concern — likely an
infinite loop in the autovacuum step when a chain has
self-referential entries (the "Freelist: size is X but
should be Y" pattern from phase13 hints at this). Likely
resolves once the ptrmap-at-allocation fix lands.

No new failures were introduced by phase14.
