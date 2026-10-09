# R13-L7 — index interior cells must be REAL entries (sqlite-compatible index b-trees)

Status: DONE (2026-10, commit with the layout evidence). frigolite index b-trees
are now structurally sqlite-compatible: an index split MOVES the boundary cell up
into the parent instead of copying it, index cursors surface interior cells as
entries, and deleting an entry that lives on an interior page uses sqlite's
predecessor move. `TestPinIndexInteriorLayout` pins the layout; `CountEntries`
is back to sqlite3BtreeCount's exact rule (leaf + interior index cells).

Evidence (exact objective repro, `/tmp/r13l7`, 5000-row table + `CREATE INDEX i1`):

```
sqlite3 /tmp/r13l7.db "PRAGMA integrity_check;"        => ok      (was: wrong # of entries in index i1)
sqlite3 /tmp/r13l7.db "select count(*) from t;"        => 5000    (was: 5049 — sqlite counts the index tree)
sqlite3 ... "select pagetype,count(*),sum(ncell) from dbstat group by pagetype;"
  => internal|2|103   leaf|106|9953     (leaf cells 10002 -> 9953: the 49 index
                                        dividers moved OFF the leaves into the
                                        interior pages, so the tree holds each
                                        key exactly once)
```

Pin test (pure Go page census + optional sqlite3 oracle), both phases green:

```
before deletes: entries = rows, interior pages hold cells, integrity ok, count(*) = rows
after deleting every 3rd b value (exercises interior-entry deletion):
  entries = remaining rows, integrity ok, count(*) = remaining rows
```

Gates: `go test ./internal/...` clean; `go test -run TestSOLID_ .` ok; harness
leaf-failure counts on clean baselines index 164 ≤ 179, delete 46 ≤ 50,
where 87 ≤ 108; `tools/quality_gate.sh` on the touched files clean (the one
staticcheck U1000 in internal/pager/cookie_cache_test.go is pre-existing).

Authoritative work plan for goal `trusty.vireo` (goal objective/completion
criterion/verifyCommand in the goal tool).

## 0. EXECUTION NOTES (implemented — kept as the design record)

Work was ATOMIC: the tree is broken until the split, cursor traversal, seek and
interior delete are all updated. What landed, in order, plus the traps found:

- **0.1 cursor model** (`btree.go` + `btree_cursor_interior.go`):
  `Cursor.onInteriorCell` marks a position on an interior index page's cell;
  `Next()` steps into the following child's subtree (`stepIntoChildAfterInterior`);
  `navigateToNextChild` emits the finished child's divider cell for INDEX trees
  (table trees keep skipping interior cells); `seekInInteriorIndex` treats an
  equal divider as a HIT. TRAP: every leaf-landing position setter must clear
  `onInteriorCell` (stale flags decoded leaf cells as interior → "corrupt index
  record"); `ReadCell` must use the interior pointer-array base (coff+4).
- **0.2 split** (`btree_leaf_split.go`): the boundary cell is removed from the
  right sibling (`writeSplitPartition`) and its overflow chain freed; the parent
  receives it as a real interior cell. TRAP: greedy packing leaves the LAST
  partition with one cell, so stripping its divider wrote an EMPTY leaf
  (sqlite's accounting reserves the divider) → `balanceIndexDividerPartitions`
  hands the previous partition's last cell over instead.
- **0.3 lower bound / lookups** (`btree_index_lower_bound.go`,
  `btree_indexseek.go`): `indexPageAt`/`indexCellOffset` replace `indexLeaf`;
  `indexStepForward` steps an interior position with Next() (a cellIdx++ would
  read the following divider out of order); the fallback walks
  (`walkIndexInterior`) yield interior cells in in-order position.
- **0.4 interior-entry delete** (`btree_delete_index_seek.go`): sqlite's
  predecessor move — the slot is rewritten with the LAST entry of its left
  subtree (`takeLastEntryOfSubtree`, recursive); if that subtree holds no entries
  (this engine keeps emptied leaves) the slot is dropped and the empty subtree's
  pages released with it. TRAPS: (a) in-order is subtree(L_0), c_0, subtree(L_1),
  …, c_{n-1}, subtree(R), so the page's own last cell — not the left subtree's
  max — is the last entry; taking the wrong one broke key order and dropped live
  keys; (b) the recursion must not also rewrite intermediate cells (that
  duplicated the payload); (c) interior mutations must run on a by-value copy of
  the parsed header + RefreshParsedBTree (the pager memo struct is shared);
  (d) equal-run scanning (`collectIndexEqualRun`, `skipIndexEqualRun`) and
  `indexCursorCellPayload` must SKIP EMPTY LEAVES, otherwise a run is cut short
  and a DELETE silently misses rows (measured: 19 of 3000 rows survived).
- **0.5 count**: `CountEntries` back to sqlite3BtreeCount's exact rule.
- **0.6 audit**: `balanceNonroot` has no production callers; `btree_drop.go` frees
  divider chains by walking the tree; vacuum paths are unaffected.

## 1. Defect (reproduced)

frigolite's index b-tree stores every key in a LEAF page and repeats one key per
split as an interior "divider copy" (`splitMedianKey`, `btree_leaf_split.go:299`).
sqlite stores each index key exactly once, either on a leaf or on an interior
page (`balance_nonroot` pushes one real cell per boundary up into the parent,
`src/btree.c:8791-8849`, `bCntNew[i]` cell becomes the parent cell and is *not*
written to any sibling page, `src/btree.c:8764-8767`).

sqlite's readers therefore count interior index cells as entries
(`checkTreePage`: `if( pPage->leaf || pPage->intKey==0 ) pCheck->nRow += nCell;`
`src/btree.c:10892`), and `PRAGMA integrity_check` reports the index over-count as
`wrong # of entries in index <name>` (`src/pragma.c:1792-1820` compares the table
count register with the index count register).

Reproduce (5000-row table + index written by frigolite):

```
cd /tmp/r13l7 && go run . /tmp/r13l7.db 5000   # scratch writer, see §6
sqlite3 /tmp/r13l7.db "PRAGMA integrity_check;"
=> wrong # of entries in index i1
sqlite3 /tmp/r13l7.db "select count(*) from t;"
=> 5049        # sqlite answers count(*) from the INDEX tree: 5000 leaf + 49 interior
sqlite3 /tmp/r13l7.db "select pagetype,count(*),sum(ncell) from dbstat group by pagetype;"
=> internal|2|103   (table-interior separators + 49 index dividers)
   leaf|106|10002   (table 5000 + index 5000 + sqlite_schema 2)
```

So the defect is worse than a PRAGMA message: sqlite's index-driven `count(*)`
returns 5049 for 5000 rows. After the fix that same statement must return 5000
and `integrity_check` must print `ok`.

sqlite 3.54.0 is the oracle (`/Users/muaddib/dev/sqlite` sources,
`/usr/bin/sqlite3`).

## 2. Required end state (no simplification)

1. On an index leaf split, the divider cell is **removed** from the sibling page
   and **moved** into the parent: the parent's interior cell is a real entry.
   Total cells over the whole index tree = number of index entries.
2. Index cursors must surface interior cells as entries, in key order:
   in-order traversal of an interior page = subtree(left child of c0), c0,
   subtree(left child of c1), c1, ..., subtree(rightmost).
3. Seek on an interior page: `key == divider` is a HIT (the entry lives there);
   `key < divider` descends left; `key > divider` descends right.
4. Deleting an entry that lives on an interior page uses sqlite's rule
   (`src/btree.c:9877-9944`): move to the predecessor entry (largest entry in the
   deleted cell's left child subtree — always on a leaf), delete that leaf cell,
   then copy it into the interior slot. Net: interior cell count unchanged, leaf
   loses one entry.
5. `btree.CountEntries` reverts to sqlite3BtreeCount's exact rule
   (`src/btree.c:10465`): count `nCell` for a leaf or for an index page; follow
   every child pointer (interior table cells are rowid separators, not entries).
6. `PRAGMA integrity_check` on a frigolite-written multi-page index prints `ok`.

## 3. Code touchpoints (btree package unless noted)

Split / layout:
- `btree_leaf_split.go`: `partitionSplitCells` (keep), add an index-specific
  boundary pass: page i holds `cells[p_{i-1}+1 : p_i]`, divider_i = `cells[p_i]`
  (removed). Guarantee ≥1 cell per page (a page with only its divider is
  invalid): if `len(newPart[i])==0`, move the previous page's last cell to the
  front of page i (bubble left); if that is impossible, leave the page empty
  (legal, but avoid), and let the divider carry an overflow chain.
- `writeSplitPartitions` / `splitMedianKey`: dividers must be captured BEFORE
  the divider cell is dropped from the partition, and the dropped cell's
  overflow chain freed (`freeOverflowChain`) or re-parented. Divider payload is
  written with a fresh chain by `encodeDividerCell` (already does).
- `leafSplitResult` already carries `medianKey`/`medianPayload`; keep.
- `btree_interior_page.go`: `applyChildSplits`/`rekeyCarrierChainIndex`/
  `applyChildSplitsRightmost` stay (they install dividers as real cells; the
  routing convention comment must change from "copy of the right sibling's first
  key, equal goes right" to "real entry, equal is a hit").
- `btree_balance_nonroot.go`: rebalance must keep the "one real divider per
  boundary" invariant when redistributing; check `nNew-1` dividers are taken
  from the gathered cells (not duplicated) — currently assumes copies.

Routing / seek:
- `btree.go` `seekInInteriorIndex` (equal ⇒ position on the interior cell, hit),
  `Next`/`Prev`/`ReadCell`/`ReadCellData` (interior position support),
  `descendToFirstLeaf*`, `navigateToNextChild` (emit divider cells).
- `btree_cursor_save.go` `routeInteriorIndex`, `interiorIndexCell`,
  `seekIndexLeafWithPath`, `currentKey`, cursor save/restore of an interior
  position.
- `btree_indexseek.go` `walkIndexInterior`/`walkIndexLeaves`,
  `collectIndexEqualRun`, `indexKeyRowIDsBySeek`, `SeekIndexKey`,
  `SeekIndexLowerBound`, `indexCellCompare`.
- `btree_index_lower_bound.go` `interiorIndexLowerBoundChild`,
  `descendIndexLowerBound*`, `advanceIndexLowerBound` (interior positions).
- `btree_insert.go`: index insert descent (equality routes right; the key cannot
  really be equal because index records end with the rowid).
- `btree_delete_index_seek.go`: after locating the target, if it is an interior
  cell apply the predecessor-move rule (§2.4); `dropIndexLeafCell` gains an
  interior variant, or a helper `deleteInteriorIndexEntry` in
  `btree_delete_one.go`/`btree_shallower.go`.
- `btree_count.go`: revert to exact sqlite3BtreeCount rule + drop the deviation
  comment.
- `internal/execquery/select_count_star.go` / `skipscan.go` / `explain.go`:
  callers of `CountEntries` are unaffected (rule change only).
- Check `btree_scan.go` `ScanTableLeaves` (table-only) and any index leaf-only
  walkers: `walkIndexLeaves`-based paths must include interior cells.

## 4. Invariants to keep (tests will catch)

- Every interior index cell key lies strictly between its left subtree's max and
  the next child's min (in-order sequence strictly increasing by the tree
  comparator; index records carry the rowid so keys are unique).
- `Σ nCell` over the tree == number of index entries == number of table rows.
- No cell's overflow chain is orphaned or double-owned (integrity_check reports
  `Page N: never used` / ptrmap errors otherwise).
- No untracked free bytes (fragmentation check) — `defragmentInterior` already
  handles re-key chains.

## 5. Verification

- New pin test `TestPinIndexInteriorLayout` (repo root, `frigolite_*_test.go`):
  build a multi-page index in pure Go (frigolite Open/Exec), then assert
  (a) the index tree's total cell count (leaf + interior index cells) equals the
  row count, via the internal btree API or by parsing the file;
  (b) at least one interior page exists that holds cells;
  (c) no key appears twice in the in-order traversal (in-order strictness);
  (d) if `sqlite3` is on `PATH`, `PRAGMA integrity_check` prints `ok` (skip
  otherwise, the count assertions are the pure-Go core).
- Manual oracle: `sqlite3 /tmp/r13l7.db "PRAGMA integrity_check;"` => `ok` and
  `sqlite3 /tmp/r13l7.db "select count(*) from t;"` => 5000.
- `go test ./internal/btree/... ./internal/execdml/... ./internal/execddl/...`
  and the repo integration tests green.
- Harness leaf-failure counts on clean baselines not exceeded: index ≤179,
  delete ≤50, where ≤108 (move `test.db*` aside first, compare leaf subtests
  only).
- Commit with the layout evidence.

## 6. Scratch repro/verification harness

`/tmp/r13l7/main.go` (module `r13l7`, `replace github.com/pijalu/frigolite => /Users/muaddib/dev/frigolite`):
opens a file DB, creates `CREATE TABLE t(a INT, b INT)`, inserts N rows, then
`CREATE INDEX i1 ON t(a)`, closes. Run: `cd /tmp/r13l7 && go run . /tmp/r13l7.db 5000`.
Then `sqlite3 /tmp/r13l7.db "PRAGMA integrity_check; select count(*) from t where a=5;"`.
Note: `frigolite.Exec` returns one value (`*Result`, field `Error`).

## 7. Risks / notes

- This is an atomic convention change: the tree is broken until the split, the
  cursor traversal, the seek, and the interior delete are all updated.
- Perf: additions are split-only (rare) + one extra branch in Next/ReadCell
  (index scans). The R13 perf gate (point-update ≥6e5 ops/s) is NOT part of this
  goal's verify command, but keep the hot paths (memory pager, point update)
  untouched; the delete-by-seek path gains a predecessor descent only when the
  target is an interior cell.
- `PRAGMA integrity_check` also runs the table-vs-index tandem scan
  (`src/pragma.c:2058-2143`): every table row must be found in the index
  (`OP_Found`), and the counts must match. So find/seek must locate interior
  entries.
