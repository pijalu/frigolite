# Plan — S66: index-btree leaf density (2026-11-26)

Mode: test-first/perf — the two `temptable2` leaves are RED with the true
assertions restored in S65; SQLite oracle and `dbstat` histograms give an
exact density target.

## Evidence

- TCL source (`temptable2.test:121-124`, `147-150`): page count must be in
  (280,300) and (270,290) after 10-then-500 and 500-then-1 temp rows.
- Oracle CLI 3.54, page_size=1024: counts 287 and 277.
- Frigolite focused gate: 316 and 304 (S65 fixture).
- Staged probe: table-only density exact (127 pages for 500 rows, same as
  SQLite). Index creation adds 176 pages vs SQLite's ~128.
- `dbstat` index-leaf `ncell` histogram:
  - SQLite: 1×1, 20×2, 49×3, 78×4.
  - Frigolite: 12×1, 59×2, 58×3, 49×4.
- `splitLeafMulti` calls `balanceIndexDividerPartitions` on a single full
  leaf plus pending cell. C applies that right-to-left redistribution only
  inside `balance_nonroot`, which has gathered up to five sibling pages;
  without siblings it moves a cell from the full left page to the new right
  page (4+1 → 3+1 regular), under-filling almost every split page.

## Change (one coherent slice)

1. Add a native pure-Go pin driving both TCL sequences through
   `frigolite.Open(":memory:")` and asserting the two page-count ranges.
   Confirm RED before engine edits.
2. Adjust the index leaf split so a single-page split keeps the left page
   full and starts the new right page from the pending divider (SQLite's
   append-side behavior), instead of applying sibling-aware redistribution
   without siblings. Candidate: skip `balanceIndexDividerPartitions` for
   local single-page splits (or gate it on a real sibling gather). Keep
   page types, parent dividers, and empty-leaf handling intact.
3. If histogram parity is still short, add the minimal sibling gather for
   adjacent index leaves; do not rewrite the table-leaf balance path.

## Verify

- Native pin RED before fix, GREEN after.
- `FRIGOLITE_TEST=temptable2` focused gate: `4.1.4`/`5.1.2` GONE, no NEW.
- `dbstat` histogram after fix trends to SQLite's 4-cell-heavy shape;
  table-only page count stays 127.
- Full gates for `journal`, `index*`, `btree*`, `vacuum*`, `wal*` show no NEW
  versus `/tmp/t30_full_gate.log` leaf sets.
- `go test ./internal/... -count=1`; quality normalized NEW=0 vs
  `/tmp/t30_quality.log`.
- Detached full gate; raw-byte leaf diff NEW=0 vs t30 and durable t10.

## Phase 2 evidence (after the leaf gather landed)

- Native pin moved 319/312 → 311/306; still RED.
- Corrected `dbstat` histogram (column `pagetype`, not the earlier `pgtype`
  which returned NULL and lumped leaves+interior):
  Frigolite `i2` interior `1×10, 2×15, 3×25, 4×2` (52 pages) and leaf
  `1×1, 2×40, 3×36, 4×47` (124 pages); SQLite 3.54 on the same shape has
  interior `1×1, 2×8, 3×12, 4×15` (36 pages) and leaf `2×18, 3×33, 4×63`
  (114 pages). Interior density is the dominant remaining gap.
- Temporary counters over the pin setup: 88 interior spill attempts —
  29 bail at `k<=0` (anchor is the parent's FIRST child, no left sibling),
  3 root anchors, 6 parent-full, 50 succeed; 38 tail splits fire.
  `balance_nonroot` sets `nxDiv` toward the right when `iParentIdx==0`
  (src/btree.c:8283-8288), so SQLite balances anchor+right sibling there.

## Phase 2 change

1. Add the symmetric interior spill for the `k==0` case: gather the anchor
   interior page, the parent's cell-0 divider (re-encoded as an interior
   entry with the anchor's rightmost child) and the right sibling page,
   fold the pending child-split divider into the anchor's entries, sort,
   partition with the existing `partitionInteriorRuns` /
   `rotateInteriorRunBoundaries`, and write runs 0→anchor, 1→right sibling,
   optional 2→new page. Re-key parent cell 0 with runs[1][0]'s payload and
   insert runs[2][0]'s divider for the 3-run window. Preserve rightmost
   pointers exactly as `commitSpillInteriorWindow` does.
2. All prechecks and the new page allocation precede the first write; bail
   leaves the tree untouched and the existing tail split is the fallback.
3. Re-run the native pin, corrected histogram, focused gate, families and
   full/quality gates.

## Phase 3 outcome — attempts REVERTED (2026-11-26)

All engine edits from phases 1–2 were reverted; the worktree is back at
`5b5472021` plus the native pin and this plan. Focused `temptable2` diff
versus a detached HEAD worktree: **NEW=0** (GONE=17 is random-data variance).

- Controlled A/B with a deterministic LCG key set (same 500 keys fed to
  SQLite 3.54 and Frigolite):
  - SQLite: 279 pages; index tree 36 interior + 116 leaf pages.
  - Frigolite at HEAD plus right-side interior spill: 300 pages;
    48 interior + 125 leaf pages.
  - Right-side interior spill alone saved 5 pages (307 → 302) but broke
    `integrity_check` with `2nd reference to page N` and missing index
    entries — a duplicate child pointer, not a density issue.
  - Left-sibling gather for index leaves made the deterministic count WORSE
    (300 → 302); the earlier pin "improvement" (319/312 → 311/306) was
    random-data noise (the pin ranges are 305–319 and 303–306).
  - A full three-page leaf window (left + current + right sibling, two
    parent dividers re-keyed) never produced three runs in the deterministic
    fixture, and in the harness produced 125 NEW failures with duplicate
    child references. Reverted.
- Root cause not fixed: index pages are underfilled (leaves 2.85 entries
  average vs SQLite 3.3; interiors 2.5 vs 3.1). A correct fix needs a true
  `balance_nonroot` port that gathers siblings and drops their dividers into
  the cell array before repacking — the ad-hoc spill paths cannot be extended
  one window at a time without corrupting child pointers.

## Freelist hypothesis — invalidated (2026-11-26)

`page_count` includes freelist pages, so a freelist-reuse gap was tested on
the same deterministic fixture: SQLite 3.54 `freelist_count=0`, Frigolite
`freelist_count=0`, `integrity_check=ok`, page_count 279 vs 308. The gap is
real split density, not unreclaimed pages; do not chase freelist reuse.

## Spill-bail shape (measured)

Instrumented `spillIndexLeafLeft` on the deterministic fixture: 79 successful
2-page spills, 10 parent-rekey bails, and **50 boundary bails, ALL with
exactly 10 window entries** (left cells + parent divider + full page cells +
pending cell). A 2-page index window holds 8 cells + 1 consumed divider =
9 entries, so these are one entry over and need a third page. They occur at
parent-edge leaves where a two-sided sibling gather is unavailable.

## Edge-window variant — invalidated (2026-11-27)

A left-sibling + new-page gather for the 50 one-over windows (no rotation,
left/current kept full) passed `integrity_check` but raised deterministic
page_count 308 → 318. Reverted. Together with the earlier left-sibling gather
(300 → 302) this closes the incremental-window search: every partial
gather/regroup variant either corrupts child pointers or loses pages.

## Next session (completed by the coherent-port plan below)

1. Do NOT re-try the reverted spill variants; their failure signature is
   recorded above.
2. Port `balance_nonroot`'s cell-array gather + `cntNew` packing as a single
   coherent operation for both leaf and interior index pages, with the
   parent's dividers removed into the array (src/btree.c:8268-8610).
3. Validate with the deterministic LCG fixture before any harness run: the
   target is ≤281 pages with index histogram close to SQLite's
   `internal 1/7/12/16, leaf 19/41/56`.
4. `frigolite_index_density_t31_integrity_test.go` is the permanent guard:
   same 500 deterministic blobs, asserts `integrity_check=ok` (the reverted
   variants failed here with duplicate child references), reports page count.
   Run it before the focused family gate.

## Risks

- Index divider cells are full-key payloads; freeing/reparenting their
  overflow chains is delicate (duplicate child references are the observed
  failure mode of partial ports).
- Never run focused gates concurrently in one worktree.

## Coherent-port plan (2026-11-27, in progress)

Mode: test-first. Baseline re-confirmed: guard logs 308 pages and both native
pin tests are RED (316 / 310 vs the TCL ranges).

Change: one new index-only balance operation replaces the two per-case spill
preludes (`spillIndexLeafPrelude`,
`spillInteriorPrelude`). It follows `balance_nonroot` in one pass:

1. Locate the splitting page's parent cell index `k`. Gather `[left, anchor]`,
   `[anchor, right]`, or `[left, anchor, right]` (three pages whenever both
   siblings exist and the caller is not bulk-loading directly under the root).
   The parent cell before each gathered page is decoded and appended to the
   cell array after that page — for leaves as a payload-only leaf cell, for
   interiors as `(leftPage.RightmostPtr, payload)`; it therefore becomes an
   ordinary entry and is never treated as a child pointer.
2. Merge and sort by full key. For interiors the pending child-split divider is
   folded into the anchor's entries first using the existing
   `foldPendingDivider` transformation (`(C,Kold) → (C,D1), (P1,Kold)`).
3. Compute `cntOld[]` (entry index after each page plus its divider) and each
   page's used bytes from the actual entry encodings. Run C's `cntNew` loop:
   spill overfull pages right, grow left-to-right, then the right-to-left
   balance adjustment. Bail without writes unless the result reuses exactly
   the gathered pages — parent divider count stays unchanged, so no parent
   page insert/delete or new sibling is needed here; the existing
   `splitLeafMulti` / interior tail split remain the fallback.
4. Commit through the existing leaf/interior page writers: rewrite every
   gathered page with its `cntNew` run, keep each interior page's rightmost
   pointer from its consumed boundary entry, reparent overflow chains, then
   re-key each parent divider with the FIRST CELL OF THE RIGHT PAGE
   (`cntNew[p]`), using the existing `encodeDividerCell` path with the LEFT
   page number as the parent cell's child pointer. Parent room is prechecked
   before the first write; all parent rekeys share one defragment/persist.

Verification order (never two `go test` processes in one worktree): deterministic
guard (`integrity_check=ok`, page count ≤281), native pin, focused `temptable2`
diff vs detached HEAD (raw-byte leaf set, NEW=0), then full/quality gates.

## Outcome — incomplete (2026-11-27)

The coherent window operation is implemented in `btree_index_balance_{pack,
window,leaf,interior}.go`. It gathers parent-child windows, folds parent
dividers into the sorted cell array, runs the C-style `cntNew` pack, rebuilds
the parent cell content in place, and replaces both spill preludes at their
call sites. The proven `spillIndexLeafPrelude` remains as a fallback after a
coherent-leaf bail; the interior tail split remains the fallback after a
coherent-interior bail.

Measured:

- Deterministic 500-row guard: `integrity_check=ok`, page_count **300**
  (baseline 308; target ≤281; SQLite 3.54: 279).
- Native pin: 308–317 / 300–304 (targets <300 / <290) — still RED.
- Focused `temptable2` diff vs detached HEAD: NEW=0 leaf failures; only
  `4.1.4` and `5.1.2` remain. The family-line duration diff is noise.
- Interior +2 page extension corrupts the tree (`database disk image is
  malformed`); right-side extension variants were not made safe.
- `bBulk` in the right-to-left adjustment made the page count worse
  (300 → 304) and is disabled at the pack call.
- Parent rekeys must REBUILD the parent cell content, not append replacement
  cells at `CellContent`: equal-size rekeys otherwise exhaust the page and
  force many pack bails.
- `writeInteriorCells` required the exact cell-pointer bound
  (`co+ptroff+(count+1)*2`); its former +2 slack made the coherent interior
  path report `interior spill: page overflow` in temp pages.

Not finished: closing the remaining ~19-page density gap without NEW harness
failures. No commit has been made.

## Parent-boundary rebuild (2026-11-28, in progress)

Mode: debug/fix. The 39 four-page interior bails are not a packing bug; they
are a parent-boundary bug. The old handoff could only re-key rightmost anchors,
so the fix is one parent plan shared by leaf and interior:

1. Model the gathered parent window as `remove = gather.boundaries`,
   `insertAt = boundaries[0]`, and `k-1` final boundary cells built from
   `cntNew[]` plus the final output pages. The parent cell after the window
   (`gather.trailing`) has only its 4-byte child pointer rewritten to the
   last output page; `-1` means the header rightmost pointer. Every other
   parent cell keeps its exact image and overflow ownership. The rebuilt
   parent is laid out as `survivors[:insertAt] + newCells + survivors[insertAt:]`.
2. Precompute final cell sizes and bail if the rebuilt parent would overflow
   BEFORE allocating an extra output page or encoding any divider. Encode
   all new cells before the first child-page write; commit the parent once.
3. Interior packing gets `oldN+1` pages, no left/middle-anchor guard, no
   `bBulk`, and no carrier split result. The extra page is inserted
   immediately after the gather anchor in output order. The final page's
   rightmost child remains the original trailing page's rightmost pointer.
4. Leaf extra-page packing replaces the split-result handoff with the same
   parent plan; the fixed `k==oldN` path is behavior-equivalent through it.
5. Validate deterministic guard + native pins, then focused `temptable2`
   raw-name diff, then full/quality/SOLID gates, then commit.

## Blocked evidence (2026-11-28)

The generalized parent plan is implemented and coherent. Deterministic guard:
`integrity_check=ok`, page_count **289** (300 before; target ≤281; SQLite 3.54:
279). `dbstat`: i2 internal 40 pages/121 cells, i2 leaf 122/379 — the gap is
still under-filled index pages, not freelist or table pages.

Every remaining coherent-pack bail is now parent-full, not a pack failure:
41 leaf + 13 interior on the deterministic fixture (0 pack bails). All have a
4-cell parent that cannot accept the 5th ~213-byte divider. Three controls:

- Bypass `indexBalanceRoom`: `btree: index balance: parent 3 full` (the
  overflow is real, not a conservative estimate).
- Retry the same window with the fixed `k==oldN` pack when the extra parent
  slot does not fit: deterministic stays 289, native pins worsen to
  311/307 (from 310/299).
- Split the parent via the existing tail-split chain and retry: deterministic
  rises to 304 and the cascade is incomplete when the grandparent is also
  full.

Reaching ≤281 requires the C behavior that is explicitly excluded by the
prescribed approach: balance the full parent one level up with a coherent
sibling gather (recursive `balance_nonroot`), not a tail split and not an
atomic bail to the existing fallback. Direction needed: allow that recursive
parent cascade, or relax the page-count/native-pin targets.
