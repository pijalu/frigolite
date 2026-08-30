# P8.INCRVACUUM Engine Port Plan

Detailed port plan for the autovacuum / incremental-vacuum engine gaps that
block 4 of 5 packages (autovacuum, autovacuum2, incrvacuum, incrvacuum2).
incrvacuum3 is already green.

Reference source: `ori/sqlite/src/btree.c`, `ori/sqlite/src/pager.c`,
`ori/sqlite/src/tclsqlite.c`. frigolite equivalents: `internal/btree/`,
`internal/pager/`, `internal/exec/`.

## 1. Current state (verified)

- `pager.FreePage` exists, preserves next-trunk pointer, zeros data.
- `pager.AllocatePage` consumes the on-disk freelist before extending the
  file (corrupt2-14.x dependency). Single-trunk + leaf-pages pattern.
- `pager.DecrementFreelistCount(n)` exists (header count only; no relocate).
- `pager.IsPtrmapPageNo` exported; `findOrphans` skips ptrmap pages
  (incrvacuum3 GREEN).
- `pager.SetAutoVacuum` / `AutoVacuum` toggles ptrmap reservation.
- `exec/pragma_quickcheck_trees.go::findOrphans` skips ptrmap + freelist
  pages.
- `PRAGMA incremental_vacuum` yields 1 row per call (DecrementFreelistCount
  path), but nFree=0 in practice so yields nothing.
- Empirical blocker: after `DROP TABLE` in INCREMENTAL mode, `freelist_count=0`
  and `page_count` unchanged (commit 105c51d9 lesson).

## 2. What is missing (in dependency order)

### Gap A — FreePage on emptied non-root leaves (engine)
**Where**: `internal/btree/btree_tail.go::deleteAllMatchingFromLeaf` and
`internal/btree/btree.go::DeleteCell`.

**What**: when a leaf page is emptied by a DELETE/DROP, free the page
into the on-disk freelist (only when the leaf is NOT the btree root —
the root can only be replaced via a parent-walk rebalance which is
out-of-scope for this slice; root-emptied leaves are left as empty
leaves, matching SQLite's behaviour for small tables).

**SQLite source**: `src/btree.c::sqlite3BtreeFreeUnusedPgs`
(`freePage2`), `balance_nonroot`'s dropCell path. The Go port:
- when `len(newPtrs) == 0 && leafNum != t.rootPage`,
  call `t.pager.FreePage(leafNum)` and remove the parent's pointer to
  this leaf.
- when `len(newPtrs) == 0 && leafNum == t.rootPage`, leave as empty leaf
  (existing behaviour).

**Parent-pointer walk**: parent lookup. frigolite's btree keeps a
`parent` field on each cell? No — current btree walks from root every
time. A parent lookup needs either a parent map (built during
collectLeafPages) or a root→leaf traversal that records parent pgno per
leaf. The simpler approach: during `DeleteCellsWhere`, traverse the btree
depth-first and keep a `(leafNum, parentPgno, cellIdx)` table; after
emptying a non-root leaf, null the parent cell.

**UT coverage**:
- `internal/btree/btree_tail_test.go::TestFreePageEmptiedLeaf`: 2-leaf
  btree; DELETE all rows from one leaf; assert `pager.GetFreelistCount()=1`.
- `internal/btree/btree_tail_test.go::TestFreePageRootEmptied`: single-
  leaf btree (root = leaf); DELETE all rows; assert no FreePage call
  (root left as empty leaf); assert freelist_count=0.

### Gap B — Pointer-map (ptrmap) read/write
**Where**: `internal/storage/ptrmap.go` (new), wired from
`internal/pager/pager.go`.

**What**: pointer-map pages are reserved at intervals (`ptrmapPageno` in
btree.c) and contain 5-byte entries per page they cover
(`(parent_type, parent_pgno, ...)`). The ptrmap is required by the page-
relocation step (relocatePage) to know what parent page to update.

**SQLite source**: `src/btree.c::ptrmapPageno`, `ptrmapPut`,
`ptrmapGet`. `src/pager.c` read/write helpers.

**Go port**:
- `storage/ptrmap.go::PtrmapPageNo(pgno, pageSize uint32) uint32`
  (already exists as `pager.PtrmapPageNo`, move to storage for layering).
- `storage/ptrmap.go::PtrmapEntry(pgno, pageSize uint32) (parentType
  byte, parentPgno uint32)`: read 5-byte entry from the ptrmap page
  covering pgno.
- `storage/ptrmap.go::WritePtrmapEntry(pg, pgno, pageSize, parentType,
  parentPgno)`: write a 5-byte entry.
- `pager.ReadPtrmap(pgno uint32) (parentType byte, parentPgno uint32,
  err error)`: page-in-cache lookup + on-disk fallback.
- `pager.WritePtrmap(pgno uint32, parentType byte, parentPgno uint32)
  error`: mark the containing ptrmap page dirty.

**UT coverage**:
- `internal/storage/ptrmap_test.go::TestPtrmapPageNo`: table of pgno →
  ptrmap page per SQLite's `ptrmapPageno` (pending-byte page skip).
- `internal/storage/ptrmap_test.go::TestPtrmapRoundtrip`: write entry
  for pgno=10, read it back, assert match.
- `internal/pager/pager_test.go::TestPtrmapReadWrite`: through the
  pager API; after pager auto-vacuum is on, write + read entries.

### Gap C — Page relocation (relocatePage)
**Where**: `internal/btree/btree_vacuum.go` (new), called by both
incrVacuumStep (Gap D) and autoVacuumCommit (Gap E).

**What**: move the content of page `from` to a free page `to`. Steps:
1. Load both pages; copy data (header + body) from `from` to `to`.
2. Read the ptrmap entry for `from` to find its parent.
3. Walk the parent btree and update the cell/pointer that referenced
   `from` to now reference `to`.
4. Update the ptrmap entry for `to` (parent now points to `to`).
5. Mark `to` dirty, free `from` (FreePage).

**SQLite source**: `src/btree.c::relocatePage` (~80 lines, intricate).

**Go port**:
- `btree_vacuum.go::relocatePage(to, from uint32, btree *BTree) error`.
- Use existing `pager.ReadPage`/`WritePage` for the move.
- Parent lookup: walk the btree depth-first from `t.rootPage` and find
  the cell that points to `from`. Update it to `to`. (Interior pages
  have child pgno at cell start; leaf pages have no children.)

**UT coverage**:
- `internal/btree/btree_vacuum_test.go::TestRelocatePage`: 2-leaf
  btree; relocate the rightmost leaf; assert content preserved, parent
  cell updated, ptrmap updated, from page now on freelist.

### Gap D — incrVacuumStep (incremental vacuum one-step)
**Where**: `internal/btree/btree_vacuum.go::incrVacuumStep` (new).

**What**: do one page-swap step (the building block of
`PRAGMA incremental_vacuum(N)`). Steps:
1. Find the last page of the file (largest pgno, `pager.NumPages()`).
2. If the last page is on the freelist, just decrement
   `pager.FileSize()` by 1 page (truncate) and return.
3. If the last page is in use, find a free page (allocate from freelist
   via `pager.AllocatePage` with `BTALLOC_LE` mode, which means
   "use the lowest free page"). Call `relocatePage` to move the last
   page's content to that free page. Decrement file size.

**SQLite source**: `src/btree.c::sqlite3BtreeIncrVacuum`,
`incrVacuumStep` (~120 lines).

**Go port**:
- `btree_vacuum.go::IncrVacuumStep(n int) (steps int, err error)`:
  do up to `n` steps; return how many were done. nil error means "no
  more work" when nFree reaches 0.
- `pager.AllocatePage()` already consumes freelist. Need a
  `pager.AllocatePageLE()` variant that returns the lowest free page
  (for the swap target). If no free page available, error
  `SQLITE_FULL`.
- `pager.TruncateFile(nPages uint32) error`: new method that
  truncates the underlying file to `nPages * pageSize` bytes. Updates
  `pager.numPages`.

**UT coverage**:
- `internal/btree/btree_vacuum_test.go::TestIncrVacuumStepBasic`:
  1 table, 2 leaves, DELETE one leaf (→ freelist has 1 page, file has
  N pages). Call `incrVacuumStep(1)`. Assert file size decreased by
  1 page, leaf content moved to the free page (file now contiguous).
- `internal/btree/btree_vacuum_test.go::TestIncrVacuumStepFreelistOnly`:
  freelist has 3 pages at end of file (no in-use last page). Call
  `incrVacuumStep(3)`. Assert file size decreased by 3 pages, freelist
  count = 0.
- `internal/pager/pager_test.go::TestTruncateFile`: file with 10
  pages, truncate to 5. Assert on-disk file is 5*pageSize bytes,
  subsequent reads of page 6 return `page not found`.

### Gap E — autoVacuumCommit (full-mode commit hook)
**Where**: `internal/btree/btree_vacuum.go::AutoVacuumCommit` (new),
called from `internal/exec/engine_core.go` at COMMIT time when
`pager.AutoVacuum() == true` (FULL mode, not INCREMENTAL).

**What**: drain the freelist down to zero (or callback-imposed limit).
For each step, call `incrVacuumStep(1)` until the file is fully shrunk
to its content.

**SQLite source**: `src/btree.c::autoVacuumCommit` (~80 lines).

**Go port**:
- `btree_vacuum.go::AutoVacuumCommit(schema string, callback
  AutovacPagesCallback) (nVac int, err error)`: drain freelist.
  Before each batch, call the callback (if set) with
  `(schema, fileSize, nFree, pageSize)`; the callback returns
  desired nVac per batch. Clamp to remaining nFree.
- `internal/exec/engine_core.go::commit()` calls
  `engine.AutoVacuumCommit("main", cb)` before writing the commit
  marker, when `pager.AutoVacuum() && !incrementalMode`.
- `internal/exec/engine.go`: new
  `RegisterAutovacuumPagesCallback(fn AutovacPagesCallback)` method.
- `internal/exec/types.go::AutovacPagesCallback` type:
  `func(schema string, fileSize, nFree, pageSize uint32) uint32`.

**UT coverage**:
- `internal/btree/btree_vacuum_test.go::TestAutoVacuumCommit`: FULL
  mode, DELETE FROM large table, COMMIT, assert file shrunk.
- `internal/btree/btree_vacuum_test.go::TestAutoVacuumCommitCallback`:
  callback returns 0 → no vacuum; callback returns N/2 → partial
  vacuum; callback returns N → full vacuum.
- `internal/exec/exec_vacuum_test.go::TestRegisterCallback`: callback
  fires with correct args.

### Gap F — sqlite3_autovacuum_pages C-API callback
**Where**: `internal/exec/engine.go` + `internal/vtab/` (testgen
binding).

**What**: surface a Go method to register the callback, plumb it
through commit (Gap E). The testgen needs a helper that the transpiled
testgen code can call instead of `sqlite3_autovacuum_pages`. Since
frigolite has no C-API, expose it as
`engine.SetAutovacuumPagesCallback(fn)`.

**Transpiler** (small, in same slice): the testgen emits a TCL proc
`autovac_page_callback` already (see `autovacuum2-1.3`); the
transpiler's `collect.go` already registers it as a Go function
(proc→db.func). We just need the testgen's `// sqlite3_autovacuum_pages
db autovac_page_callback` line to actually call
`db.SetAutovacuumPagesCallback(fn)`. This is a transpiler patch:
recognize the command and emit the engine call.

**UT coverage**:
- `internal/exec/exec_vacuum_test.go::TestAutovacuumPagesCallback`:
  register callback; do an autovacuum commit; assert callback fired
  with correct args.

### Gap G — Transpiler gaps (autovacuum, incrvacuum, incrvacuum2)
**Where**: `tools/tcl2go/collect.go`, `tools/tcl2go/processcmdextra.go`.

**What**: each gap has a small, focused fix:
- `make_str {char len} { set str [string repeat $char. $len]; return
  [string range $str 0 [expr $len-1]] }`: extend
  `collect.go::constantProcValue` to recognize this pattern and emit
  a `tclMakeStr(char, len)` Go call.
- `file_pages {} { return [expr [file size test.db] / 1024] }`:
  recognize and emit `tclFileSize("test.db") / 1024`.
- `[eval concat $list]`: add an `eval` handler in
  `cmdexpr.go` that splices its arg (eval == no-op for a list result).
- `[lsort -integer [eval ...]]`: add `-integer` flag handling to
  `cmdExprLsort`; on missing flag, default to -ascii.
- `[join $list " separator "]` separator preservation: the
  transpiler already passes the separator to `strings.Join` in some
  paths; verify and fix the case where it's a quoted arg.

**UT coverage**:
- `tools/tcl2go/transpiler_test.go::TestTranspileMakeStr`: feed a
  snippet with `make_str` call, assert generated Go contains
  `tclMakeStr(...)`.
- `tools/tcl2go/transpiler_test.go::TestTranspileFilePages`: assert
  generated Go contains the file-size expression.
- `tools/tcl2go/transpiler_test.go::TestTranspileEvalConcat`: assert
  generated Go contains the spliced list.
- `tools/tcl2go/transpiler_test.go::TestTranspileLsortInteger`:
  assert generated Go calls `sort.Slice` with the int-comparator.
- `tools/tcl2go/transpiler_test.go::TestTranspileJoinSeparator`:
  assert generated Go uses the right separator.

## 3. Goal decomposition (ordered)

1. **Goal: P8.INCRVACUUM.phase1** — Gap A (FreePage on emptied non-root
   leaves) + UT. Slice: small, ~150 lines btree + ~80 lines test.
2. **Goal: P8.INCRVACUUM.phase2** — Gap B (ptrmap R/W) + UT. Slice:
   ~200 lines storage + ~60 lines test.
3. **Goal: P8.INCRVACUUM.phase3** — Gap C (relocatePage) + Gap D
   (incrVacuumStep) + UT. Slice: ~300 lines btree_vacuum.go + ~150
   lines test.
4. **Goal: P8.INCRVACUUM.phase4** — Gap E (autoVacuumCommit) + Gap F
   (callback) + UT + testgen integration. Slice: ~200 lines exec +
   ~150 lines test.
5. **Goal: P8.INCRVACUUM.phase5** — Gap G (transpiler) + UT. Slice:
   ~200 lines tools/tcl2go + ~150 lines test.
6. **Goal: P8.INCRVACUUM.complete** — run all 5 testgen packages; mark
   the original P8.INCRVACUUM goal complete; close out the phase.

Each phase goal:
- Pre-goal: state the scope (which Gap), files touched, ordered steps.
- Todo list: per-step task; mark in_progress / done as work proceeds.
- UT coverage: focused unit test added in the same commit.
- Verify command: the package's tests + the new UT + `go build ./...`
  + `go vet ./...` + `go test -run TestSOLID_ ./...`.

## 4. Risks / open questions

- **Root-page free**: Gap A only frees non-root leaves. A btree whose
  root is the only leaf will not grow the freelist on DELETE. The
  testgen tests are mostly multi-leaf (autovacuum-1.x inserts 20 rows
  into a single column with long values → multiple pages) so the
  limitation is acceptable for these tests. Document in lessons.
- **Pending-byte page**: SQLite reserves page
  `sqlite_pending_byte / pageSize + 1` (≈ page 4097 for 1024-byte
  pages with default 1GB pending byte). Not allocated as a real btree
  page. The page-swap must skip it. Track as a UT.
- **Pointer-map consistency**: ptrmap updates and the page-swap must
  be atomic-ish: if a crash happens between the move and the ptrmap
  update, recovery sees a stale ptrmap. SQLite solves this with the
  journal. For this slice, we accept the limitation (write both
  pages in the same transaction; commit abort rolls back).
- **Auto-vacuum on commit vs INCREMENTAL on demand**: Gap E fires at
  COMMIT for FULL mode only. INCREMENTAL mode never auto-commits;
  `PRAGMA incremental_vacuum` is the only consumer (Gap D path).

## 5. Completion criteria

- `go test -tags testgen ./testgen/{autovacuum,autovacuum2,incrvacuum,
  incrvacuum2,incrvacuum3}/ -count=1 -timeout 300s` exits 0.
- `go test -run TestSOLID_ ./...` exits 0.
- `go build ./...` exits 0.
- `go vet ./...` exits 0.
- `tools/status` reports all 5 INCRVACUUM packages as green.
- `TestParseSkipMaps` floor stays at 288 (the 5 un-skips remain).
- New UT tests for each Gap pass.

## 6. Files touched (cumulative across phases)

- `internal/btree/btree_tail.go` (Gap A: free leaf call + parent walk)
- `internal/btree/btree_vacuum.go` (new, Gaps C/D/E)
- `internal/btree/btree_tail_test.go` (Gap A UT)
- `internal/btree/btree_vacuum_test.go` (new, Gaps C/D/E UT)
- `internal/storage/ptrmap.go` (new, Gap B)
- `internal/storage/ptrmap_test.go` (new, Gap B UT)
- `internal/pager/pager.go` (Gap B: ptrmap read/write methods; Gap D:
  AllocatePageLE, TruncateFile)
- `internal/pager/pager_test.go` (Gap B/D UT)
- `internal/exec/engine.go` (Gap F: RegisterAutovacuumPagesCallback)
- `internal/exec/engine_core.go` (Gap E: commit hook for FULL mode)
- `internal/exec/exec_vacuum_test.go` (Gap E/F UT)
- `tools/tcl2go/collect.go` (Gap G: make_str, file_pages)
- `tools/tcl2go/cmdexpr.go` (Gap G: eval concat)
- `tools/tcl2go/processcmdextra.go` (Gap G: lsort -integer)
- `tools/tcl2go/transpiler_test.go` (new, Gap G UT)
- `.agents/lessons_learned.md` (per-phase progress notes)
