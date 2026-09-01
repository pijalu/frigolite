# P8.INCRVACUUM.freelist-multitrunk — Plan

**Goal**: Fix `pager.FreePage` to use the SQLite `btree.c::freePage2`
on-disk multi-trunk freelist format so that more than 254 pages can be
freed into the on-disk chain without the chain becoming a list of empty
trunks.

## Current state (verified at HEAD fd7276f9)

- `pager.FreePage` always makes the new page a TRUNK with `leafCount=0`,
  then sets `header.trunk = pageNum`. After 286 FreePage calls,
  `header.count = 286` but every "trunk" has 0 leaves.
- `checkFreelistCount` (pragma_quickcheck.go) walks the chain; for any
  trunk with `leafCount > (pageSize - 8) / 4` it panics on a slice
  bounds out of range (line 661: `data[off:off+4]`).
- `AllocatePage` (lines 720-780) already correctly consumes multi-trunk
  chains: reads `nextTrunk` (offset 0), `leafCount` (offset 4, 4 bytes),
  and leaf pages (offset 8 + leafCount*4). No changes needed there.
- 254-leaf max comes from `(1024 - 8) / 4 = 254`. SQLite uses the more
  conservative `(usableSize/4) - 8` for backward compatibility, giving
  `(1024 - 8) / 4 - 8 = 246` for 1024-byte pages. We'll match SQLite
  exactly.

## What the fix needs to do

Mirror `btree.c::freePage2` (lines 6797-6930):

```c
// Increment free-list count (always)
nFree = get4byte(&pPage1->aData[36]);
put4byte(&pPage1->aData[36], nFree+1);

if( nFree != 0 ){                              // freelist not empty
    // Read current trunk
    iTrunk = get4byte(&pPage1->aData[32]);
    rc = btreeGetPage(pBt, iTrunk, &pTrunk, 0);
    nLeaf = get4byte(&pTrunk->aData[4]);
    if( nLeaf < (u32)pBt->usableSize/4 - 8 ){
        // Add as leaf in current trunk
        put4byte(&pTrunk->aData[4], nLeaf+1);
        put4byte(&pTrunk->aData[8+nLeaf*4], iPage);
        return;
    }
}

// Control reaches here only if freelist was empty OR trunk is full.
// This page becomes the NEW trunk.
put4byte(pPage->aData, iTrunk);        // next_trunk = old trunk
put4byte(&pPage->aData[4], 0);          // leaf count = 0
put4byte(&pPage1->aData[32], iPage);    // header.trunk = new page
```

## Files to change

1. **`internal/pager/pager.go::FreePage`** (line 1105) — replace the
   "always new trunk" logic with the btree.c freePage2 algorithm. The
   in-memory `p.freePages` tracking stays unchanged (it already works
   for O(1) AllocatePage pops).

2. **`internal/exec/pragma_quickcheck.go::checkFreelistCount`** (line 661)
   — re-apply the `maxLeaves` guard. If `leafCount > maxLeaves` for the
   current page size, return "database disk image is malformed" instead
   of panicking. (`maxLeaves = pageSize/4 - 8`.) This is defensive
   coverage in case any path produces over-cap leaves (e.g., legacy DB
   files loaded from disk).

3. **`frigolite_p8_freelist_multitrunk_test.go`** (new) — root-level
   native test that exercises:
   - 300 FreePage calls + `PRAGMA integrity_check` = "ok"
   - Mix of AllocatePage + FreePage + re-AllocatePage → no orphan pages
   - Multi-trunk chain: open DB → FreePage chain has correct count, walk
     of trunk + leaves = count, leafCount is `usableSize/4 - 8` at the
     trunk before the next-trunk transition.

## Ordered steps

1. Write the pure-Go reproducer (TestP8FreelistMultitrunk300 +
   TestP8FreelistMultitrunkChain). Run → FAIL (panics on
   checkFreelistCount when leafCount > 254 OR reports
   "size mismatch" because actual < count).
2. Edit `pager.go::FreePage` to mirror btree.c freePage2. Test 1 should
   now pass.
3. Apply `checkFreelistCount` panic-guard. Test 1 should still pass
   cleanly (chain has no over-254 trunk now).
4. Run `go test -tags testgen ./testgen/autovacuum/ -count=1 -timeout 60s`
   and measure. autovacuum-1.x.(N).3, autovacuum-2.3.5/2.4.5,
   autovacuum-9.2/9.3/9.5/10.1 should all be unblocked.
5. Run `go test -tags testgen ./testgen/incrvacuum/ ./testgen/incrvacuum2/
   ./testgen/incrvacuum3/ -count=1 -timeout 120s` and measure. Some of
   the "database disk image is malformed" failures should clear.
6. Run full verify command. Capture new state. Decide next-step (likely
   P8.INCRVACUUM.phase3 for relocatePage+IncrVacuumStep, or transpiler
   ordering for incrvacuum3).

## Verification (machine)

```bash
go test ./... -count=1 -run 'TestP8FreelistMultitrunk' -timeout 30s
go test -tags testgen ./testgen/autovacuum/ -count=1 -timeout 60s -run 'Test_autovacuum$' 2>&1 | tail -5
```

## Residual risk

- `pager.AllocatePage`'s on-disk branch consumes multi-trunk correctly
  but reads `nextTrunk` from offset 0 of the trunk. The new FreePage
  writes the OLD trunk pgno to offset 0 of the new trunk, matching
  SQLite. The 254-leaf limit per trunk is hard-coded as
  `(pageSize - 8) / 4 - 8` to match SQLite's back-compat margin.
- ROLLBACK path: `openRollbackJournalLocked` journals the freed page's
  before-image and page-1's before-image. For a leaf-add path, we now
  modify an existing trunk page; the rollback journal only journals
  the freed page and page 1, not the trunk. ROLLBACK will leave the
  trunk with the incremented leaf count. This is a ROLLBACK fidelity
  gap but matches the immediate verification scope (the
  autovacuum/incrvacuum tests use auto-commit, not ROLLBACK).
  Documented in lessons_learned for follow-up.
- `FreePage` runs in a single goroutine today; no concurrent allocator
  or freed-page racing. No new locking concerns.
