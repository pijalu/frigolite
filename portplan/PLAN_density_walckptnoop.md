# PLAN — the btree page-density unit (walckptnoop-1.1..1.5)

## The divergence (probe-isolated, 2026-11-19)

Native probe `frigolite_densprobe_test.go` (TEMPORARY, delete when done):
the walckptnoop-1.0 workload (1000 rows, `hex(myrandomblob(64))` — the C
LCG replicated as a UDF; page_size 1024, auto_vacuum NONE, secure_delete
OFF) run on (a) the C oracle's checkpointed file (testfixture 3.51.0,
/tmp/densprobe/testdir/test.db) and (b) our engine:

| metric            | C-oracle            | ours                     |
|-------------------|---------------------|--------------------------|
| page_count        | 299                 | 323                      |
| t1 (table) leaves | 143, 0 interior     | **143, 0 — IDENTICAL**   |
| t1 fill %         | min 81 avg 94.8 max 95 | min 81 avg 94.8 max 95 |
| t1 cells/leaf     | 6 / 6.99 / 7        | 6 / 6.99 / 7             |
| i1 (index) leaves | 129, 0 interior     | **145**                  |
| i1 fill %         | min 40 avg 91.6 max 95 | min 14 avg 79.7 **max 81** |
| i1 cells/leaf     | 3 / 6.76 / **7**    | 1 / 5.9 / **6**          |
| i1 leaf payload max | 938               | 804                      |
| ckpt noop nLog    | 298                 | 323                      |

The TABLE tree matches C byte-for-byte. The INDEX tree diverges: our
index leaves cap at 6 cells / 81% fill where C reaches 7 cells / 95%
fill.

## Root cause

Our index-leaf split (`splitLeafMulti` → `partitionSplitCells`) is a
greedy LEFT prefix pack with NO adjustment pass; `writeSplitPartitions`
then moves each partition ≥1's FIRST cell up into the parent as the
boundary divider, so every index leaf page loses one cell to its divider
and caps at (greedy-run − 1) cells.

C's balance_nonroot runs a SECOND pass after the greedy left-packing
(btree.c, "The packing computed by the previous block is biased toward
the siblings on the left side…" — the right-to-left redistribution loop,
`for(i=k-1; i>0; i--)`):

```c
r = cntNew[i-1] - 1;            /* the left page's last cell */
d = r + 1 - leafData;           /* the current divider (leafData=0 for INDEX) */
do {
  szR = cachedCellSize(&b, r);  /* the left's last cell */
  szD = b.szCell[d];            /* the current divider */
  if( szRight!=0 && (bBulk || szRight+szD+2 > szLeft-(szR+(i==k-1?0:2))) ) break;
  szRight += szD + 2;           /* the old divider joins the right page */
  szLeft  -= szR + 2;           /* the left loses its last cell to the parent */
  cntNew[i-1] = r;
  r--; d--;
} while( r>=0 );
```

The rotation: the left page's LAST cell becomes the new divider (moved
UP), and the OLD divider drops INTO the right page as its first cell.
The move continues while the right page can absorb the old divider
without the left dropping below the right's post-move size. Result: the
pages end BALANCED (the left ≥ the right) instead of left-full/right-
remnant, and no page loses a cell to its divider beyond the boundary
cell — the right page's original first cell stays ON the page.

`leafData = pPage->intKeyLeaf` — TRUE only for TABLE leaves; INDEX
pages (leaf and interior) run leafData=0 (the divider is a real entry
that participates in the rotation). `bBulk` (the append/balance_quick
path) SKIPS the adjustment — which is why our TABLE path (balance_quick
for appends) matches C exactly today.

## The fix

In `splitLeafMulti`'s INDEX path (btree_leaf_split.go), after
`partitionSplitCells` and replacing `balanceIndexDividerPartitions`:

1. Re-represent the greedy partitions as (pageCells[0..k-1], dividers[1..k-1]):
   pageCells[0] = parts[0]; for i≥1: pageCells[i] = parts[i][1:],
   dividers[i] = parts[i][0].
2. For i = k-1 down to 1, rotate while the C condition holds (sizes in
   full-page terms: used = content + 2·ncell + 8):
   - szR = size(dividers[i]... the LEFT page's last cell),
     szD = size(dividers[i]) initially (the current divider),
   - move while szRight + szD + 2 ≤ szLeft − szR − (i==k-1 ? 0 : 2),
     then: pageCells[i-1] loses its last cell → dividers[i] = that cell;
     pageCells[i] gains the OLD divider at the front.
   - szRight += szD + 2; szLeft −= szR + 2 (C's accounting, with the
     sizes re-derived from the new boundaries each iteration).
3. `writeSplitPartitions` consumes the new shape: pageCells verbatim,
   dividers to the parent (its current divider extraction changes from
   "partition[pi][0]" to the explicit divider list).
4. Keep the degenerate one-cell-page guard (C's `szRight != 0` break —
   the adjustment continues while the right page is empty, so empty
   right pages cannot survive; the write path's empty-leaf tolerance
   stays as the corrupt-input safety net).

## Verify

- The probe: our i1 must reach 129 leaves / 95% max fill / 7 cells/leaf
  max; ckpt noop nLog → 298.
- `FRIGOLITE_TEST=walckptnoop` → the family 0-fail (1.1-1.5 join 1.0/1.6-
  1.9).
- The 15-family identity diff vs portplan/baselines/s35g_gate_fails.txt
  (the density change touches EVERY index btree: run the full WAL
  superset + the index-bearing families: where*, index*, join*, select*,
  fts*, rtree* ride the FRIGOLITE_TEST=wal superset for the WAL ones;
  the btree change's blast radius needs the FULL-suite gate
  `go test -run '^TestSQLiteSuite$'` (the only complete net per the
  working rules) before commit).
- `go test ./internal/...`.

## REFINED DIAGNOSIS (2026-11-19, after the first implementation attempt)

The adjustment-pass port alone made the workload WORSE (198 leaves vs the
pre-fix 145 — C is 129). The stepwise trace (TestDensityStepwise) exposed
the REAL architecture gap:

1. **C's balance_nonroot redistributes across a SIBLING WINDOW** — the
   overflowing page + its 2 LEFT siblings (nOld=3; nxDiv=iParentIdx−1,
   i=2−bulk; the left/right edge cases clamp; src/btree.c:8268-8284).
   The window's cells + the parent's window dividers (real index entries,
   dropped from the parent into b.apCell for leafData=0) redistribute
   TOGETHER: the excess spills into the siblings' free space and a new
   page appears only when the whole window overflows. THE DENSITY COMES
   FROM THE SPILL, not from the split point.
2. **Our index splits are a single-page splitter** — splitLeafMulti
   redistributes ONE page's cells + the new cell; every overflow creates
   ≥1 new page. (Our balanceNonroot port IS the window machinery, but it
   is wired as the DELETE-path rebalancer (maybeRebalanceAfterDelete,
   btree_tail.go) and collectBalanceCells refuses non-table-leaf
   siblings. The INSERT path never had the window.)
3. **bBulk** (C's balance_nonroot arg 4 = `iPage==1` — the split's page
   sits DIRECTLY under the root, src/btree.c:9188) passes TRUE for the
   depth-≤2 splits: the gather shrinks to nOld=2 (the page + 1 left
   sibling) and the adjustment's break fires on the first populated right
   page (the `szRight!=0 && (bBulk || cond)` form — an EMPTY right page
   still takes one rotation). Verified: our root-leaf split → balance_
   deeper → the child's balance is C's iPage=1 case (6/1 shape, refill to
   7, row-for-row like C's trace).
4. The adjustment pass itself (ported: balanceIndexDividerPartitions over
   the partition list, the boundary rotation — the left's last cell
   becomes the divider, the old divider joins the right page) is CORRECT
   for the window context but cannot help a single-page split: with no
   left siblings to spill into, the rotation just thins the split pages.

## THE PORT (the actual fix — a full unit)

Route the INDEX LEAF overflow through the balance window:

a. Gather: the page + 2 left siblings (C's non-bBulk nOld=3; nOld=2 for
   bBulk — depth ≤ 2; the edge clamps). gatherBalanceSiblings exists.
b. collectBalanceCells: an index mode — each sibling's cells via
   IndexLeafCellSizeAt, PLUS the window's parent dividers interleaved as
   cell entries (C's !leafData gathering branch: the divider AFTER each
   sibling's cells in key order; leafData = intKeyLeaf = TABLE-leaf only).
c. The distribution: packBalancePages + the adjustment pass (C's form,
   bBulk-aware) over the array.
d. The pages: the survivors (reuse the window's pages; allocate only the
   surplus; free the surplus pages per the vacuum rule the table path
   implements — the HIGHEST-numbered pages to the freelist).
e. The parent edits: the window's dividers REPLACED (the boundary cells
   move up as the parent's new divider entries — real index payloads) +
   the surplus pointers inserted. The INSERT path's parent-edit contract
   (leafSplitResult: a NEW page + a divider) must extend to divider
   REPLACEMENTS for reused siblings — applyChildSplits /
   rebalancePartialWindow handle the table case; the index needs the
   payload-shaped equivalents.
f. The interior INDEX splits: splitInteriorPage is a fixed 2-way split —
   C's interior index pages run the same balance_nonroot (leafData=0,
   the dividers participate as d=r+1). Carded behind the leaf case.

## Verify (unchanged from the plan above, plus)

- The stepwise trace must match C's shape (the depth-≤2 pages 6/1,
  refill to 7; the deeper splits spill left and stay dense).
- The probe: i1 → 129 leaves / 95% max fill; ckpt noop → 298.
- The FULL-suite gate before commit (the change touches every index
  btree); the 15-family identity diff vs s35g_gate_fails.txt.

## State (2026-11-19)

The first attempt (the adjustment + bBulk without the window) REVERTED —
the committed tree (1f248d87d) keeps the pre-unit behavior (145 leaves;
walckptnoop 1.1-1.5 stay failing). The probe file
(frigolite_densprobe_test.go) stays for the restart. The unit is carded
behind the queue position: it is the NEXT unit after the current session
slice, or the restart point for a fresh session.

## 2026-11-19: the implementation attempts recorded (the unit stays OPEN)

**Attempt 1 — the leaf spill (splitLeafMulti's left-sibling redistribution
+ bBulk + the partition adjustment + the defrag parity)**: the probe
DENSITY win was real (131 leaves / 30 interiors / 308 frames from
145/70/345; C 129/22/298 — the index fill 93.4% avg matching C's 91.6%).
Two sub-bugs found and fixed along the way: the divider's leaf-form entry
must carry the FULL payload (the interior cell's Payload is the LOCAL
part — readOverflow before the re-encode), and the spill's parent re-key
leaks dead space without the defragment (finishChildSplits' parity).
**The FULL-suite gate: 162 NEW failing leaves** (e_vacuum, atomic2,
crashM, io, pagerfault, subjournal, temptable2, sort2, index4, join5,
analyze9, journal2, memjournal2, resetdb-300...) — the 2-page spill
corrupts trees on the broader corpus (the overflow-chain/ptrmap
bookkeeping across the moved cells, the shapes the walckptnoop workload
never exercises). REVERTED.

**Attempt 2 — the 3-page interior spill (retryChildSplitApply's
redistribution)**: implemented (the window gather + the dividers-in-array
with C's rightmost-pointer leftChild + the boundaries + the parent
re-keys) but the integration with the split+apply cascade oscillated (the
placed spills degraded the totals: 170 leaves/42 interiors/323) and the
anchor-tracking bugs compounded. REVERTED with the rest.

**What the restart needs** (the work is scoped, not guessed):
1. The leaf window must be the FULL C shape: the overflow chains of every
   moved cell re-parented to their final owner (ptrmapPutOvflPtr), the
   divider's old chain freed, the secure_delete/pending-byte interactions,
   and the schemas the corpus exercises (the WITHOUT ROWID trees, the
   spilled-payload dividers, auto_vacuum pointer maps). The probe
   (TestDensityProbe in this conversation's record; recreate in ~50
   lines) measures the shape per change.
2. The interior window: the pending divider RIDES IN the redistribution
   (C's overflow cell joins b.apCell — the balance IS the application),
   replacing the split+apply separation. Our splitInteriorPage +
   applyChildSplits split makes the integration oscillate; port the C
   shape wholesale (the balance call replaces retryChildSplitApply's
   split+apply loop for index interiors).
3. The gates per slice: the FULL suite (the only complete net — the
   walckptnoop workload alone misses the corruption shapes), then the
   15-family identity diff.
