# P8.INCRVACUUM.phase5.5 — btree.c port plan

## Goal
Make the failing testgen packages (`autovacuum`, `incrvacuum`, `incrvacuum2`)
green by porting the missing pieces of `src/btree.c` into Go. The transpiler
work (Layer A) is already done in commit 6910a084; the engine (Layer B) is
what's left.

## Current state (verified)
- `go build ./... && go vet ./... && go test -run TestSOLID_ ./...` → green
- 2/5 target packages green: `autovacuum2` (0.36s), `incrvacuum3` (0.94s)
- 3/5 FAIL:
  - `autovacuum` (2.82s) — 75 SELECT body-mismatches; btree corrupted by
    relocate ops during DELETE→PRAGMA incremental_vacuum cycles
  - `incrvacuum` (0.69s) — many body-mismatches, same root cause
  - `incrvacuum2` (60s timeout) — INCREMENTAL-mode hang in `Query`
- `internal/btree/btree_rebalance_test.go`: 4 of 5 tests fail (rebalance gap)
  - `TestRebalanceDeleteMost`, `TestRebalanceDeleteRange`,
    `TestRebalanceAlternating`, `TestRebalanceInsertAfterBulkDelete`,
    `TestRebalanceFreePage` (the last passes because freelist count grows
    as we go through FreePage, not via rebalance)

## Root cause
The engine lacks a faithful port of `btree.c::balance_nonroot` (line 8206),
plus the helpers it depends on:
- `copyNodeContent` (8124) — interior page copy for tree height change
- `balance_quick` (7968) — rightmost-overflow fast path
- `rebuildPage` (7605), `pageInsertArray` (7723), `pageFreeArray` (7780)
- `editPage` (7834) — apply cell redistributions to a single page
- `balance_deeper` (9010) — root split
- `balance_nonroot` (8206) — the main rebalance
- `balance` (9091) — entry that dispatches to the above

Currently `DeleteCellsWhere` leaves empty leaves in the tree (cell count 0,
content pointer at pageSize). The parent interior page still references them
and re-fills them with new inserts, but the corrupt-state risk is real:
a `relocatePage` (incremental vacuum) trying to move a free page's parent
sees the wrong parent in the ptrmap (it was set when the page was first
allocated, not when it was last re-attached).

## Strategy
Mirror the C structure 1:1. Do not "simplify" (project rule). The key
data structures (CellArray, NB=5, page bookkeeping) translate directly.
Skip the C-only "apCell* refcount and dirty flags" surface — our
in-memory pager means WritePage is always available.

## Port order
1. **copyNodeContent** (small, isolated) — gives us the "shallower" path
2. **balance_quick** (small, isolated) — handles the rightmost-overflow case
3. **rebuildPage + pageInsertArray + pageFreeArray + editPage** (medium,
   foundational) — needed by balance_nonroot
4. **balance_deeper** (small, isolated) — root split
5. **balance_nonroot** (large, the heart) — the main rebalance
6. **balance** entry (small, glue) — dispatches based on overflow + page
   position in the cursor stack
7. **Wire allocBtreeNode/allocRootpage/allocOverflow** into existing
   `AllocatePage` call sites in `btree_insert.go` (8 sites identified)
8. **Wire balance()** into `DeleteCellsWhere` and `InsertCell` post-mutation
9. **Focused UTs** mirroring `btree_rebalance_test.go` patterns:
   - `TestBalanceQuick` — overflow-cell redistribution
   - `TestBalanceNonroot` — 5-sibling gather + size-balanced redistribution
   - `TestBalanceDeeper` — root-split into a new level
   - `TestCopyNodeContent` — interior page roundtrip with ptrmap rewrite
   - `TestBalanceEmptyLeaf` — DELETE→balance→coalesce roundtrip

## What's in scope
- The full btree.c port above.
- Investigating `incrvacuum2` hang (likely Query path in INCREMENTAL mode
  reading relocated pages whose parent ptrmap wasn't updated).
- Investigating `autovacuum-9.2` file-size mismatch (likely page-size
  assumption — 172 vs 1024 suggests a multi-page file being compared to
  a single-page count).

## What's out of scope
- WAL mode (P7)
- Window functions / CTE (G4)
- JSON / RTree / session / RBU (G6/G7)
- TCL testgen transpiler improvements (Layer A is done)

## Verification
- Per turn: `go build ./... && go vet ./...`
- After each ported function: focused UTs
- After each milestone (copyNodeContent, balance_quick, etc.):
  re-run `go test -run "TestRebalance|TestBalance" ./internal/btree/`
- Final: full verify command from P8.INCRVACUUM.phase5
  `go build ./... && go vet ./... && go test -run TestSOLID_ ./... && \
   go test ./tools/tcl2go/ -count=1 -timeout 60s && \
   go test -tags testgen ./testgen/autovacuum/ ./testgen/autovacuum2/ \
     ./testgen/incrvacuum/ ./testgen/incrvacuum2/ ./testgen/incrvacuum3/ \
     -count=1 -timeout 300s`

## DoD
- All 5 testgen target packages: 0 FAIL
- build / vet / staticcheck / SOLID: green
- 0 regression in pre-existing test suites
- Each ported function has a focused UT
- `lessons_learned.md` updated with key insights
- UCL §1i (no N-A shortcuts) satisfied — every gap is ported, not skipped

## Risk
- The btree.c port is the largest single piece of missing engine
  code in the project. It is the canonical SQLite algorithm and has
  many corner cases. Multi-turn scope (5-10 focused turns).
- dlv-debug skill is available for inspecting page bytes / btree walks
  when a UT surfaces a corner case.
