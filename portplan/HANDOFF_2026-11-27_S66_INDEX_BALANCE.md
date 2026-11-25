# Handoff — S66 coherent index balance (INCOMPLETE, 2026-11-27)

Worktree: `5b5472021` plus uncommitted engine changes and the pre-existing
native pin/guard tests. No commit has been made.

## What landed in the worktree

- `internal/btree/btree_index_balance_pack.go`: `cntNew` packing with C's
  overfull/grow/right-to-left passes. The caller fixes the page count; a
  caller-requested extra page is supported for leaves only.
- `internal/btree/btree_index_balance_window.go`: gather of up to three parent
  children, parent divider decoding, parent-room check, and parent cell
  rebuild (replace boundary payloads and repack all parent cells).
- `internal/btree/btree_index_balance_leaf.go`: index leaf split path.
- `internal/btree/btree_index_balance_interior.go`: index interior split path,
  currently fixed-page-count only.
- `splitLeafMulti` calls the coherent leaf path first, then the proven
  `spillIndexLeafPrelude`, then the ordinary split.
- `retryChildSplitApply` calls the coherent interior path, then the tail split.
- `writeInteriorCells` now uses the exact cell-pointer bound
  (`co+ptroff+(count+1)*2`); the former +2 slack caused page-overflow errors
  on temp pages once the coherent interior path was active.

## Measurements

- Deterministic 500-row guard: `integrity_check=ok`, page_count 300
  (baseline 308; target ≤281; SQLite 3.54: 279).
- Native pin: 308–317 / 300–304 (targets <300 / <290) — still RED.
- Focused `temptable2` vs detached HEAD: NEW=0 leaf failures; only `4.1.4`
  and `5.1.2` remain. Family-line duration differences are noise.
- Interior extension past the gathered pages corrupts the tree
  (`database disk image is malformed`); do not retry without a real upper
  parent cascade.
- `bBulk` in the right-to-left adjustment regressed 300 → 304; disabled.

## Restart point

The remaining ~19-page gap needs a C-faithful way to let the parent absorb
extra divider cells (upper-parent split/cascade) or a leaf packing variant
that keeps `integrity_check=ok` while raising 4-cell leaf occupancy. Do not
re-enable the interior +1/+2 page extension; it corrupts. Keep the parent
rebuild: appending replacement cells at `CellContent` causes false full-parent
bails. Do not run focused gates concurrently in one worktree; compare raw-byte
failure-name sets from equally focused runs.

Full details: `portplan/PLAN_2026-11-26_S66_INDEX_DENSITY.md` §Outcome.

## 2026-11-28 update — BLOCKED on parent-room contradiction

`commitIndexBalanceParent`/`indexBalanceRoom` are now generalized parent-boundary
plans; the interior pack uses `oldN+1` with no anchor guard and no carrier
split result, and the leaf extra-page path uses the same plan. A fresh interior
page is skipped by the pre-write chain-free pass (its header is written by
`writeInteriorCells`), which fixes the `database disk image is malformed` seen
whenever a 4-page interior window was placed.

Measured cleaned worktree (no diagnostics): guard `integrity_check=ok`,
page_count **289**; native pins still RED (~310 / ~299, random ±3). All 54
remaining pack bails on the deterministic fixture are parent-full (41 leaf,
13 interior); zero are pack failures. Bypassing the room check gives a real
parent-full write error; a fixed-page retry worsens the pins; a tail-split
parent cascade raises the page count to 304 and can leave the grandparent
unbalanced.

The prescribed atomic-bail contract forbids the one operation that C uses here
(a coherent recursive balance of the full parent one level up), so the ≤281
target cannot be reached within the stated approach. Do not commit these
edits as a completed S66 unit. Need direction: allow the recursive parent
cascade, or relax the density targets.

## Resolution — ACCEPTED (2026-11-29)

User decision: the engine-only density variance is accepted. Keep the
recursive parent-boundary cascade (`btree_index_balance_cascade.go`) already
in the worktree; correctness is judged by integrity and SQLite file-format
compatibility, not by matching SQLite's page count.

Work done under the accepted resolution:

- Fixed a real correctness bug exposed by `TestIndexKeyRowIDsBySeekMatchesWalk`
  (RED in the worktree, GREEN at detached HEAD): the gather paths appended
  `sizes[i]` before sorting `all`/`entries` and the pack then indexed the two
  arrays positionally. A swapped 6/7-byte length let the pack accept 113 cells
  needing 1017 of 1016 usable bytes, which failed in `writeLeafHalf` with
  `btree: split failed: leaf half full`. The leaf, interior and cascade
  gathers now rebuild `sizes` from their sorted entries.
- `TestS66SQLiteFileCompatibility` writes the same deterministic 500-row blob
  index with both engines and opens each file with the other: sqlite3 3.54
  reports `integrity_check=ok`, count 500 and all 500 payloads on Frigolite's
  file; Frigolite reports the same on sqlite3's file.
- The two native pin tests now assert temp-btree integrity and the exact row
  count with a loose page bound; SQLite's density bands are documented as
  accepted engine variance, not asserted. The two JSON leaves are superseded
  in `harnessSkipSubtests` with pointers to those native tests; a focused
  `FRIGOLITE_TEST=temptable2` run is green and its fail-name diff vs
  `/tmp/s66_head_fails.txt` is NEW=0.
- Deleted the unreachable two-page interior-spill helpers (staticcheck U1000)
  that the coherent path left behind; `staticcheck ./internal/btree/...` is
  clean. `make quality` still fails on repo-wide campaign debt that is
  identical at HEAD (complexity, file-size, unrelated staticcheck).

Evidence: S66 verify command passes; `go test ./internal/... -count=1` green;
focused `temptable2` fail-name diff vs `/tmp/s66_head_fails.txt` NEW=0 after
superseding `4.1.4`/`5.1.2`; focused `index` and `btree` family diffs vs
HEAD NEW=0; deterministic guard `integrity_check=ok`, page_count 304 (293
before the size-array fix; SQLite 3.54: 279). Final full parity gate (12
shards, one process per shard): 1024 leaf failures, NEW=0 vs
`/tmp/t30_full_gate.log`; GONE=4 (resetdb-100/300 flakes + the two
superseded density leaves). The three bestindexF names that look NEW vs
the older s35g list are pre-existing at HEAD 5b5472021. With this gate the
TODO queue is closed.
