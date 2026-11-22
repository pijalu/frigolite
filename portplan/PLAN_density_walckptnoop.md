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

## 2026-11-19 (session 2): the corruption mode PINNED — the promoted-entry visibility

The leaf spill was re-applied with the fixes (the divider's full-payload
reassembly, the chain preparation for the landing divider entry, the
right-set chain re-parenting, the parse-memo refreshes, the defrag
parity) and the auto_vacuum repro (frigolite_avprobe_test.go:
2000-row index, LCG keys) pinned the residual corruption EXACTLY:

- integrity_check: "wrong # of entries in index i1" + TWO deterministic
  rows (23, 1508) "missing from index i1" — every run, the same rows.
- The leaf cells 1919 + the interior cells 83 = 2002 entries for 2000 rows:
  TWO KEYS APPEAR TWICE (a leaf copy + the interior divider).
- The scans SURFACE the promoted entries: `WHERE b >= b23 ORDER BY b`
  returns (b23, 23) FIRST; the tree walk finds no cycles, no duplicated
  child pointers, all pages type-correct.
- **The EQUALITY probes miss them**: `WHERE b = b23` → 0 rows;
  integrity_check's per-row presence seek (SeekIndexKey with the FULL
  key) → not found. The prefix/full seek asymmetry (the prefix seek 0
  rows, the full seek 1 row) localizes the bug to the SEEK's routing or
  comparison at the interior divider whose payload equals the probe's
  PREFIX (the shorter-probe comparison).

The 81 other promoted entries verify fine — the two failures are
boundary/position-specific (the probe's keys sort 13cf < 13d7 — row
1508's key is LEFT of row 23's). The seek-machinery interaction (the
comparator's short-probe semantics at the interior binary search, or the
descent's equality route) needs a debugger pass (dlv-debug; break in
SeekIndexKey's descent for the failing key) — the NEXT session's first
action. The spill code itself (the redistribution, the chains, the
ptrmaps, the memos) verified sound by the repro's tree walk.

**State**: the leaf spill re-reverted; the committed tree stays at
4e9200799 (1190 leaves). The AV probe (frigolite_avprobe_test.go,
uncommitted) is the deterministic repro for the seek work.

## 2026-11-19 (session 3): the corruption PINNED TO CONCRETE ENTRIES

The leaf spill was re-applied with every fix from session 2 PLUS the chain
preparation for the landing divider entry (prepareCell + the re-encode)
and the right-set chain re-parenting. The AV repro's entry dump
(frigolite_avdump_test.go — walks every i1 page, decodes every record,
leaf AND interior) pinned the corruption to CONCRETE entries:

- The tree: 2002 entries for 2000 rows — TWO KEYS APPEAR TWICE, each as a
  leaf cell AND an interior divider:
  - rowid=930 (b=000013b9b8bd...) duplicated on LEAF PAGE 88
  - rowid=1666 (b=000013ee5cdd...) duplicated on LEAF PAGE 42
- The tree structure is otherwise sound: no cycles, no duplicated child
  pointers, all pages type-correct; the scans surface every entry in
  order; the density shape is the bulk-split [31|1] (120 one-cell leaves,
  20 thirty-one-cell leaves — C's bBulk parity).
- The integrity_check's "row N missing" ordinals are the SCAN ORDINALS
  shifted by the duplicates (pragma_integrity_index.go's 1-based scan
  ordinal, NOT the rowid).

**The bug's location**: the spill's LEFT-PAGE WRITE emits two same-key
cells (writeLeafHalf over leftEntries where leftEntries contains the
duplicate) — i.e. the sorted `all` array itself holds the same key twice
at redistribution time for these two inserts, while the entry dump of the
array's inputs (the left's cells, the divider, the page's cells + new)
shows disjoint sets. The next session's FIRST action: the dlv-debug pass
(SKILL: dlv-debug) on spillIndexLeafLeft's `all` construction for the
insert whose key is b=000013b9b8bd — break after the sort, dump
all[i].key for i around the boundary, and find which input contributes
the second copy. Everything else in the spill verified sound (the density
shape, the structure, the chains, the memos, the defrag parity).

**State**: the leaf spill re-reverted; the committed tree at 4e9200799
(1190 leaves). The AV repro (frigolite_avprobe_test.go,
frigolite_avdump_test.go, uncommitted) is the deterministic harness for
the debugger pass.

## 2026-11-19 (session 3, final): the self-check EXONERATES the redistribution

The leaf spill was re-applied with a SELF-CHECK inside spillIndexLeafLeft:
after the sort, compare adjacent entries' keys — a duplicate there IS the
corruption, dumping every input (the left's cells tagged L, the divider D,
the page's cells tagged R, the boundary). Result across the 2000-insert
AV workload: **the check NEVER fired — every redistribution window held
unique keys**, while the tree STILL carries the two duplicated entries
(leaf 88: rowid 930 x2; leaf 42: rowid 1666 x2; the dump: 2001 entries,
1999 unique).

**Conclusion**: the duplicate does NOT originate in the spill's sorted
window. The remaining suspects, in order:
1. **The fall-through split's interaction with the post-spill page state**:
   the spill (placed=false, moved=true) rewrites the page with its reduced
   cell set; the caller re-reads the page and runs partitionSplitCells →
   writeSplitPartitions. The split's partitions are built from the
   re-read cells — the re-read's decode of the REWRITTEN page must be
   byte-exact (the parse memo was refreshed; the ReadPage path re-parses).
2. **The spill's parent re-key + the caller's divider application**: the
   spill re-keys cell(k-1) directly; a subsequent leaf split in the same
   window applies its divider chain through applyChildSplits, which
   locates the anchor by child pointer — the re-keyed cell's payload is
   the spill's boundary, and the chain's carrier is read from it: a
   mismatch there re-inserts an entry that already exists.
3. **The leaf spill running on the leaf whose LEFT sibling was ITSELF
   spilled in the same statement's cascade** — the two rewrites racing
   through the shared parent.

The dlv-debug pass (break in writeLeafHalf when the page being written
would carry two equal adjacent keys) is the decisive instrument; the AV
repro is deterministic (frigolite_avprobe_test.go, 2000 inserts, LCG
seed 7) and frigolite_avdump_test.go pins the duplicates to leaf pages
88 and 42 immediately after the workload.

**State**: the leaf spill re-reverted; the committed tree at cc222896a
(1190 leaves + the scoping docs). The spill code lives in this document's
history and the conversation; the AV harness is committed.

## HANDOVER 2026-11-20: the WRITELEAFDUP capture — the duplicate is in the RE-READ, not the window

**State**: the leaf spill is IN THE WORKTREE (uncommitted on top of
cc222896a/721f36641) with TWO diagnosis instruments:
1. SPILLDUP (spillIndexLeafLeft): after the sort, compares adjacent
   window keys — **never fired across the whole workload**.
2. WRITELEAFDUP (writeLeafHalf): compares adjacent keys of every write
   set — **FIRED**: splitLeafMulti's FIRST-partition writeLeafHalf,
   page 42, idx 2, len 32, with the FULL key dump:
   [0]=row 30's entry, [1]==[2]=row 1666's entry (b=000013ee5cdd...) x2,
   [3..31]=rows 1670+ — the duplicate is (b1666, 1666) twice.

**The conclusion**: the spill's redistribution windows are clean; the
duplicate enters through the FALL-THROUGH SPLIT's cell set — the re-read
(readCellsForSplit over the post-spill page) or the page's pre-state.
The panic's stack: insertLeafPage → splitFullLeaf → splitLeafMulti →
writeLeafHalf (btree_leaf_split.go:222) — the spill returned
placed=false, so NOTHING was written by the spill for this insert; the
page's cells carried the duplicate already, or the re-read doubled it.

**The next concrete step (this session continues here)**: add the same
adjacent-key check to splitLeafMulti's `cells` right after the sort (the
re-read's cell set) — if it fires there, dump the page's CellCount and
the decoded cells: the duplicate is either in the page's pre-state (an
earlier spill's left-page write) or in the re-read decode. If it does
NOT fire there, the duplicate appears in the partitions → the
adjustment's rotation.

**Verify commands**: `go test -run TestAVSpillProbe -v -count=1` (the
deterministic repro; the panic captures the writer's stack),
`go test -run TestAVWalk2 -v -count=1` (the entry dump: leaf 88:
rowid 930 x2; leaf 42: rowid 1666 x2).

## HANDOVER 2026-11-21: THE BUG FIXED — the parent re-key's room check hoisted before any page write; the leaf-spill slice verified on all 15 failing families

**The root cause of the WRITELEAFDUP corruption** (handover above): the
spill's parent re-key ran its encode+room check AFTER the left/page
writes. When the parent could not take the wider divider, the bail left
the pages redistributed but the divider un-rekeyed — the old divider's
key overlapped a page's range, and the re-read produced the duplicate
(leaf 88: rowid 930 x2; leaf 42: rowid 1666 x2).

**The fix** (spillIndexLeafLeft, btree_leaf_split.go): the whole
re-key block hoisted ABOVE the first page write —
freeOverflowChain(boundary) → encodeDividerCell(leftPgno, …) → the
parent-content room check (`int(parent.CellContent)-len(rekeyed) <
rekeyPtrEnd` → bail with NOTHING changed) → only then the
divider chain-prep (prepareCell for the landing divider entry), the
leftPg write (writeLeafHalf + ParsePageInto + RefreshParsedBTree +
reparentSplitOverflowChains), the right-set re-parent, the pg write,
and finally the parent re-key with abandonDividerCell +
defragmentInterior + memo refresh. The re-key's ordering invariant is
now structural: no page write happens before the parent is proven to
fit the wider divider.

**The diagnosis instruments were removed** (SPILLDUP self-check,
WRITELEAFDUP panic, SPILLDIV trace, min2, the SPLITDUP re-read check);
`go build ./...` and `go vet ./internal/btree/` clean.

**Probes after the fix (clean build)**:
- TestAVSpillProbe: `IC: ok` — integrity_check clean on the AV FULL db.
- TestAVDump: 2000 entries dumped, 0 duplicated, 84 leaves.
- TestAVWalk2: dbstat leaf cells 1917, interior 83, leaves 84; no MISSING.
- TestDensityProbe: ours page_count 312, i1 leaves 134, fill avg 87.6%
  max 95% (C: 299/129/91.6%); ckpt noop {0 312 0}.

**Families sweep — all 15 failing families, current build**: batch 1
(e_vacuum 5, temptable2 2, io 11 — all baseline; atomic2 0, crashM 0);
batch 2/3 redo (subjournal 0, memjournal2 0, pagerfault 0, analyze9 0,
alterdropcol 0, changes 3 baseline, expridx1 10 baseline, journal2 5
baseline, memjournal 0). ONE flagged NEW: resetdb/resetdb-300.

**resetdb-300 is NOT the slice's regression** — the attribution:
- It fails at HEAD (no slice) 3/3 deterministic with the IDENTICAL got
  line `[wal 210 26000 ok wal 8192 14]` (C wants `… 12`): page_count 14.
- The s35g baseline lacks it because the baseline was captured on the
  PRE-slice-1 engine (the file was committed WITH slice 1
  ae4ccd13a, whose engine delta is the walrecover restructure +171 /
  wal.go +36 / the xSleep/xRead plumbing — `git log ae4ccd13a..HEAD --
  internal/` shows no other engine change; resetdb.json unchanged).
- /tmp/dens_gate.log (Nov 19, attempt-1 spill on the post-slice-4
  engine) shows -300 already failing with page_count 13; today's HEAD
  14; today's slice 14 (byte-identical got line) — the slice is
  page_count-neutral for this case; the divergence predates the
  density unit (most plausibly slice 1's recovery db-size tracking).
  The pre-slice-1 A/B could not compile (SetWalHook signature changed
  in slice 1; the root package's hooks files require it).
- DISPOSITION: allowlist resetdb-300 in the FULL-gate identity diff as
  a known pre-existing divergence with this attribution; the WAL
  db-size tracking divergence itself stays on the residue backlog (it
  is a real 14-vs-12 page_count divergence to fix in a WAL unit, not a
  btree-density matter).

**Next steps (this session continues here)**:
1. FULL-suite gate in the background
   (`FRIGOLITE_TEST= go test -run '^TestSQLiteSuite$' -count=1
   -timeout 150m`), raw-scan the fails, diff vs
   portplan/baselines/s35g_gate_fails.txt + the resetdb-300
   allowlist — expect walckptnoop 1.1–1.5 improved or gone and zero
   other NEW.
2. Commit the leaf-spill slice (code + TODO/handoff/lessons).
3. Then the interior balance replacing the split+apply separation
   (the plan's next section).

**Verify commands**: `go test -run TestAVSpillProbe -v -count=1`,
`go test -run TestDensityProbe -v -count=1`,
`FRIGOLITE_TEST=resetdb go test -run '^TestSQLiteSuite$' -count=1`
(7 fails, -300 among them with `… 8192 14` at HEAD too).

## SLICE 2 PLAN: the interior window spill (the anchored case) — C's balance_nonroot at the interior level

**The problem with the current split+apply separation**: when a child's
divider chain cannot land on its anchor interior page
(childSplitsHaveRoom → errInteriorFull), retryChildSplitApply
(btree_insert.go:393) TAIL-SPLITS the anchor (splitInteriorPage — the
greedy tail split: the left page keeps everything that fit, only the
LAST cell moves to the fresh sibling) and hands the new divider up.
Each firing creates a ~1-cell interior page — our densprobe interiors
are 31 vs C's 22, the tail-split husks are the delta. C instead
redistributes: balance_nonroot (src/btree.c:8206) gathers the page +
up to two siblings + the parent's boundary dividers into one cell
array, re-partitions ALL of it evenly across the window's pages, and
only creates a new page when the window itself overflows.

**Scope — the anchored case only** (findChildCellIndex idx >= 0): the
densprobe workload is a random-key INDEX tree — the anchor is almost
never the rightmost pointer, and the TABLE trees already match C
byte-for-byte (the 2026-11-19 diagnosis), so the rightmost/append
window (applyChildSplitsRightmost) stays on the existing tail-split
path. The spill fires from retryChildSplitApply's top:

    spillInteriorDividersLeft(pg /*anchor*/, parentPgno, childPageNum,
                              childSplits) → (placed bool, err)
    if !placed → fall through to the existing splitInteriorPage loop.

**The window shape** (mirror of spillIndexLeafLeft, one level up):
- Gather: left sibling's interior entries + the parent's boundary
  divider (the cell between the two pages, read in leaf-form… no — in
  INTERIOR form: (leftChild, key/payload)) + the anchor's entries +
  the pending divider entries (childSplits[i] as
  (leftChild=pageNum, key/medianPayload)). Sort by key; the pending
  dividers must sort adjacent to their anchor children (they do: a
  divider sorts immediately above its leftChild's subtree bound).
- Partition: the greedy pack over interior-entry sizes
  (sz = 4-byte child + key varint/payload + 2-byte ptr) + C's
  right-to-left adjustment (balanceIndexDividerPartitions' shape, with
  bulk = the anchor's parent is the ROOT — C's bBulk=(iPage==1)).
- The boundary entry between the halves moves UP: the left page's
  rightmost pointer takes its leftChild; the parent's boundary cell is
  re-keyed with its key/payload (table: the rowid bound via
  encodeDividerCell; index: the record payload, chain prepared for the
  PARENT page — prepareCell parity).
- The rest of the entries split across the two pages; children that
  move pages are re-parented (reparentSplitRightChildren parity);
  index dividers moving pages re-encode their overflow chains for the
  target page; the boundary's old chain is freed
  (freeOverflowChain parity).
- Placed=true requires: ALL pending dividers landed post-redistribution
  AND the right set fits one page (the rightBytes bail, leaf-spill
  parity) — else return placed=false with NOTHING changed and let the
  tail-split fallback run.

**The ordering invariant (the leaf spill's hard-won lesson, verbatim)**:
the parent re-key's encode + room check runs BEFORE any page write. A
post-write bail would leave the two interior pages redistributed but
the boundary un-rekeyed — the same duplicate/overlap corruption class.

**Atomicity**: the spill's prechecks (the boundary encode size, the
parent room, both pages' fit) all run before the first
PrepareWrite/byte write; every bail path before that point leaves the
tree untouched. The apply inside applyChildSplits stays untouched —
the spill REPLACES the retry loop's first attempt, not the apply.

**Files**: internal/btree/btree_insert.go (the hook in
retryChildSplitApply), internal/btree/btree_interior_page.go (the new
spillInteriorDividersLeft + the gather/partition helpers beside
splitInteriorPage), possibly a small shared partition helper extracted
from spillIndexLeafLeft's boundary computation.

**Ordered steps**:
1. Add the gather helper (left sibling location via the parent — the
   cell BEFORE the anchor cell index; rightmost/first-child edges →
   placed=false).
2. Add the partition + boundary re-key + the two-page write with the
   re-parenting and chain bookkeeping.
3. Wire the hook in retryChildSplitApply (before the re-parse loop).
4. Probe gate: TestDensityProbe (expect the i1 interiors to drop from
   31 toward C's 22, page_count 312 → ~300), TestAVSpillProbe +
   TestAVDump + TestAVWalk2 (integrity/entry parity), then the 15-family
   sweep (the same python raw-scan diff), then the FULL-suite gate.
5. Commit per the slice with the handover.

**Risks**: the boundary re-key's carrier form (table rowid bound vs
index payload) must match the parent page type; the interior entries'
overflow chains (big index keys) moving between pages; the left
sibling may itself be mid-cascade (freshly created by an earlier
applyChildSplitChain leg — order[] bookkeeping in
childOrderAmongSplits); secure_delete zeroing parity is NOT applicable
here (no cell drop without replacement — the boundary is re-keyed, not
dropped), matching the leaf spill's resolution.

## HANDOVER 2026-11-21 (2): the leaf-spill slice GATED identity-clean — the refactor, the dividerCellLen precheck, and the flake attributions

**The gate**: FULL suite on the frozen slice shape (/tmp/dens_gate3.log,
1194.9s): 1191 fails vs the s35g baseline's 1193. GONE = walfault-11/
-12/-15 (the baseline predates the S35g slices) + atof1-2.40 (the
documented S32 random-dice, a benign draw). NEW = 2, both verified
non-regressions: resetdb-300 (the documented load flap + the stale
pre-slice-1 baseline, handover 1) and temptable2-4.1.1 (NEW flake
class: **the engine's randomblob is unseeded**, so the 10 keys' sort
order varies per run and the tiny-cache temp index's shape flips
page_count 9/10 — verified 0/1/1 across three identical runs on the
FROZEN shape; same class as where2-2.4's random() flake and the
temptable2-9.x small-cache dithers — watchlist). **REAL NEW = 0.**

**walckptnoop 1.1-1.5 stay failing but IMPROVED**: mxFrame 451 → **348**
vs C's 298 (the density probe's sibling workload: 312 vs 299 pages /
134 vs 129 i1-leaves / 87.6% vs 91.6% fill). The remaining ~50-frame
delta is the interior tail-split husks (ours 31 interiors vs C's 22) —
slice 2 (the interior window spill, the plan above) is the closer.

**The pre-commit-grade refactor** (the strict quality gate would have
rejected the pre-refactor shape: spillIndexLeafLeft gocognit 70 /
gocyclo 59):
- The spill unit moved to **internal/btree/btree_leaf_spill.go** (the
  window type + the driver + the phase helpers: gather → pick-boundary
  → precheck → commit; writeSpillLeafHalf dedupes the two page writes;
  indexPartitionFullUsed/rotateIndexDividerRight extract
  balanceIndexDividerPartitions' pieces).
- **precheckSpillRekey now predicts the re-key's size with
  dividerCellLen** instead of encoding first: the original order
  (freeOverflowChain → encode → room check) stranded the still-live
  divider's freed chain AND leaked the fresh chain when the room check
  bailed — both only become safe once the room check has passed. The
  dividerCellLen prediction is byte-exact (applyChildSplitsRightmost's
  established parity).
- readCellsForSplit's withNew param dropped (newCell==nil expresses the
  no-incoming-cell case; the spill's left-sibling read passes nil).
- Probes re-verified byte-identical after the refactor (IC ok; 2000/0
  dup/84 leaves; 134 leaves/87.6%/312 frames).

**State**: the slice is COMMIT-ready — code (btree_leaf_split.go,
btree_leaf_spill.go NEW, btree_insert.go) gated identity-clean on the
FULL suite. Next session: the interior window spill per the SLICE 2
PLAN section above.

**Verify commands**: `go test -run TestAVSpillProbe|TestAVDump|
TestAVWalk2|TestDensityProbe -v -count=1`; the identity diff:
python raw-scan of /tmp/dens_gate3.log vs
portplan/baselines/s35g_gate_fails.txt (the two flake allowances).

## HANDOVER 2026-11-22: SLICE 2 GATED — the interior window spill (the anchored case) lands

**The implementation** (internal/btree/btree_interior_spill.go NEW): when
an anchor interior page cannot take a child split's divider, the divider
is offered to the anchor's LEFT sibling through the parent's boundary
cell — balance_nonroot's redistribution one level up. INDEX trees only
(the table trees already match C's interior shape byte-for-byte); the
rightmost-anchored divider folds in too (C's end-insert: the anchor
child becomes a regular cell carrying the split's median and the split's
page takes the rightmost pointer). The window partitions into 2 or 3
page runs (the greedy pack + the right-to-left adjustment with bulk =
the anchor directly under the root — C's bBulk=(iPage==1)); the 3-run
window allocates ONE middle page and inserts one new divider cell into
the parent (both parent-side effects room-checked with dividerCellLen
predictions BEFORE any write). Windows needing more than 3 pages and
no-left-sibling/root anchors fall through to the tail-split.

**Two corruption bugs found and fixed during the probe gate** (both
caught by the densprobe's new integrity_check line):
1. **The ptrBase off-by-8**: collectInteriorEntries takes the pointer
   array base MINUS 8 (storage.CellPointer adds 8 internally —
   splitInteriorPage's `ptrBase := coff + ptroff - 8` documents it);
   passing coff+ptroff read 8 bytes past the array and the whole window
   decoded junk — the commit then wrote garbage dividers (dbstat showed
   a t1 interior page as i1's: the tree walk crossed into foreign
   pages; "database disk image is malformed").
2. **The old boundary divider's entry vanished**: C drops the parent's
   dividers into the cell array and re-places them
   (src/btree.c:8451-8482), with the divider's left pointer REPLACED by
   the left page's rightmost child (src/btree.c:8464-8469) — the entry
   represents the subtree between the left page's last cell and the
   divider key. Without it, the re-keyed boundary DELETED the old
   divider's entry from the tree ("row N missing from index" — the
   tail-split path preserves it via the carrier cell, the spill must
   preserve it via the array entry).

**Measured** (the densprobe workload, page_size=1024): interiors 31 →
**25** (C: 22), page_count 312 → **308** (C: 299), IC ok, 1000 entries,
the ordered scan clean; walckptnoop mxFrame 348 → **337** (C: 298; the
card stays open). The AV probes unchanged (IC ok, 2000 entries 0 dup,
84 leaves — the AV workload's interior spills all bail: no-left-sibling
or root anchors).

**The gates**: the 15-family sweep — zero NEW except the resetdb-300
flap; the FULL suite (/tmp/dens_gate4.log): GONE = the S35g slices +
atof1-2.40, NEW = resetdb-300 (flap) + temptable2-4.1.1 (the unseeded
randomblob draw) + where2-2.4 (the DOCUMENTED random() flake — 0/3 on
re-runs) — **REAL NEW = 0**.

**Remaining to C parity (the next slice)**: C's balance window takes up
to THREE old siblings ([left, page, right] — NB=3) plus two new pages;
our window is [left, anchor] + one new. Extending the gather to the
right sibling (and the leaf spill likewise) is the remaining density
delta (~9 pages on the densprobe, ~39 frames on walckptnoop).

**Verify commands**: `go test -run 'TestDensityProbe|TestAVSpillProbe|
TestAVDump|TestAVWalk2' -v -count=1` (IC ok everywhere; ours
308/136/25 vs C 299/129/22).

## SLICE 3 PLAN: the 3-old-sibling window ([left,] anchor, right…) — C's full NB=3 balance window

**C's window shape** (src/btree.c:8272-8310): up to THREE consecutive old
pages starting at the anchor's left sibling when it exists (nxDiv =
iParentIdx-1), else at the anchor itself; nOld = 3 (2 when bBulk — the
anchor directly under the root). The dividers BETWEEN the window's pages
(nOld-1 of them) are DROPPED from the parent and join the cell array
(each with its left pointer replaced by the left page's rightmost child,
:8464-8469; for leaf windows in leafData=0 trees the divider joins as
the key-only leaf-form entry, :8461-8463). The partition produces up to
NB+2=5 runs: the first nOld runs REWRITE the old pages in order, the
rest allocate new pages. The parent then takes nNew-1 new divider cells
(the cuts), replacing the nOld-1 dropped ones — the cell-count delta =
nNew-nOld (the parent may overflow → the loop balances it, our cascade
hands it up).

**Our slice-2 shape** ([left, anchor] + up to one new page) is C's
bBulk case. Slice 3 generalizes BOTH spills (the interior spill in
btree_interior_spill.go, the leaf spill in btree_leaf_spill.go):
- The gather: the window = up to 3 consecutive children starting at the
  anchor's left sibling (or the anchor when none); the dropped dividers
  = the cells between the window's pages, each folded into the array
  with the rightmost-child replacement (the interior) or in leaf form
  (the index leaf spill — the existing divEntry form, one per boundary).
- The partition: the greedy pack + the rotation over up to 5 runs; the
  guards: every run ≥1 regular cell (≥2 for pi>0 with the consumed
  divider), the last run's fit; bulk keeps the 2-old window (no right
  sibling gathered).
- The parent-side: drop nOld-1 divider cells, insert nNew-1 new divider
  cells (each pointing at its run's left page; the run pages = the old
  pages in order then the new ones). Room check with dividerCellLen
  predictions before any write; the ptr-slot surgery = drop 2 (or
  nOld-1) slots, insert nNew-1 at the first dropped slot; the defrag
  reclaims the dropped bytes.
- The commit: rewrite the old pages in run order (PrepareWrite + clear +
  writeInteriorCells/writeLeafHalf + reparent + WritePage each), then
  the parent re-key/insert block.

**Ordered steps**: 1) the interior spill's 3-old window (the gather, the
run-to-page mapping, the parent drop/insert); 2) the leaf spill's
3-old window (the two leaf-form boundary entries, the same parent
machinery); 3) the probe gate (TestDensityProbe: expect interiors → 22,
page_count → ~300, walckptnoop mxFrame → ~300); 4) the 15-family sweep;
5) the FULL gate; 6) commit with the handover.

**Risks**: the parent's cell-count delta changes its geometry (the
count can SHRINK for nNew=2 3-run... no — nNew ≥ nOld always here since
the content only grows; delta ≥ 0 ✓). The right sibling may be the
parent's RIGHTMOST pointer (k == CellCount-1 → cell k+1 doesn't exist —
R = the rightmost, no cell k+1 to preserve). The leaf spill's boundary
entries in leaf form must re-encode their chains for the landing page
(the existing divEntry prepareCell path).

## HANDOVER 2026-11-22 (2): SLICE 3 ATTEMPTED AND REVERTED — the 3-old-sibling window regresses; the remaining delta stays carded

**Both 3-old attempts were implemented, measured, and REVERTED** (the
worktree is back at the committed slice-2 state 755d3c3b0; probes green:
AV 2000 entries 0 dup, IC ok everywhere, ours 308 pages / 136 i1-leaves
/ 25 interiors vs C 299/129/22):

1. **The interior 3-old window** ([left, anchor, right] + up to 2 new
   pages, the parent drop/insert with the cell-count delta): measured
   315-317 pages / 30-32 interiors — WORSE than slice 2's 308/25. The
   bisect (an nOld cap + a spill-off env) showed the regression comes
   from BOTH the no-left [anchor, right] windows and the 3-old gathers:
   the redistribution's cut keys differ from the tail-split's dividers,
   which perturbs the downstream leaf-split landscape.
2. **The leaf 3-old window** ([left, leaf, right], redistribution-only,
   the double re-key): measured 330 pages / 154 leaves AND corrupted the
   AV walk (2028 entries, duplicated pairs on leaf pages). Two real bugs
   were found and fixed in the draft before abandoning it: the right
   sibling must be read from CELL k+1's child (cell k points at the LEAF
   itself — the first draft duplicated the leaf's cells into the window,
   the interleaved-duplicate signature), and the run count must EXACTLY
   match the gathered pages (the spill is redistribution-only). The
   remaining corruption was NOT fully root-caused: the dup pairs are
   P-segment pairs with identical bytes and distinct cell pointers, the
   leaf page's direct decode is clean (31 cells, 0 dups, each key once)
   but its stored order measures UNSORTED — pointing at an unsorted-leaf
   write path or a parent-re-key interaction left unresolved.

**The lesson (recorded in lessons_learned.md)**: the redistribution's
cut keys are not density-neutral — they change every downstream split's
medians. A window extension must be evaluated on the FULL probe set, not
the target metric alone; both attempts looked structurally correct and
measured worse.

**The density unit's state**: slices 1-2 landed (the leaf spill + the
interior spill, identity-clean FULL gates). The remaining delta
(densprobe 308 vs 299 pages; walckptnoop mxFrame 337 vs 298) stays
CARDED behind slice 3's identified lever (the 3-old window) — resumable
from this handover with the two draft bugs' fixes already known. The
densprobe now asserts integrity_check per run (it caught TWO real
corruptions during slice 2 — the ptrBase off-by-8 and the vanishing
boundary entry).
