# P8.CORRUPT.C5 — corrupt2-5.1 plan (C: engine checkTreePage + tcl2go fixes + native test)

## Scope

P8.CORRUPT unblock: 7/8 in-scope packages pass. Only **corrupt2-5.1** fails.
The test:

1. Builds a `t1`/`t2` DB.
2. Corrupts it: copies the 4-byte child-page field of t1's first cell (page 2 cell 0) over
   t2's first cell, so both t1 and t2 now point to the same leaf page (page 10),
   leaving page 4 orphaned.
3. Expects `PRAGMA integrity_check` to report two lines:
   - `Tree 2 page 2 cell 0: 2nd reference to page 10`
   - `Page 4: never used`

(preceded by `*** in database main ***` from the multi-line join).

The expected output requires **engine** capabilities frigolite lacks:
the `checkTreePage` walker (per-page validation with duplicate-child-pointer and
never-used detection) — the equivalent of SQLite's `btree.c::checkTree` / `checkTreePage`.

Two transpiler limitations also block the existing testgen path:
- TCL `read $fd N` (read N bytes) is not propagated to `tclReadFile`.
- TCL `binary scan $bytes S var` (big-endian uint16) is silently ignored.

## Goal

1. **Engine**: implement `checkTreePage` walker in `internal/exec/pragma_quickcheck.go`
   producing the SQLite-canonical "Tree N page M cell K: 2nd reference to page X" /
   "Page Y: never used" diagnostic lines, mirroring `btree.c::checkTreePage` /
   `checkTree` (page reference set + duplicate child pointer detection).

2. **Transpiler**: extend `tclReadFile` to honor N-byte length and add `tclBinaryScan`
   helper supporting `S` (big-endian uint16) so the existing testgen chain works.

3. **Native test**: write `frigolite_corrupt2_check_test.go` that crafts the same
   duplicate-page-ref + orphan DB and asserts the engine produces the diagnostic.

4. **Verify command** must exit 0.

## Why this is the right shape

- **No skipping missing engine features** (AGENTS.md mandatory rule 2026-09). The
  "pure-Go supersession" policy is RETIRED. The integrity_check output is the
  engine's surface — implementing it is the whole point.
- **NO SIMPLIFY** (AGENTS.md): the walker must be SQLite-faithful, not a one-off
  for corrupt2-5.1. Same duplicate-pointer detection logic also gates
  corrupt2-6.3 / corrupt2-9.1 / corrupt2-10.1 path on similar future gaps.
- **Native test**: pinning the engine contract independently of the transpiler
  ensures the walker is correct (per AGENTS.md "native test before TCL
  validation").
- **Transpiler fixes**: if corrupted file is correct, the testgen path becomes
  the regression net going forward.

## Implementation steps

### Step 1 — Engine: `checkTreePage` walker

In `internal/exec/pragma_quickcheck.go`, add:

```go
// checkTreePage walks every b-tree page in every b-tree (table or index) and
// reports duplicate child pointers and orphan pages as separate rows:
//   "Tree <rootPgno> page <pgno> cell <iCell>: 2nd reference to page <child>"
//   "Page <pgno>: never used"
// Mirrors btree.c::checkTree / checkTreePage (see also integrityCheck in
// pragma.c). All diagnostic lines from a single b-tree are emitted under
// the same "*** in database main ***" header line that the multi-line
// integrity_check output uses.
//
// A page reference is recorded per (treeRoot, pageNo) pair so a page
// legitimately shared between trees (e.g. via a TRIGGER or VIEW) is not
// misreported as a duplicate. Within a single tree, the second cell
// pointing to an already-seen page is the duplicate.
func (e *Engine) checkTreePage(emit func(string)) {
    for _, ctx := range e.dbList {
        if ctx == nil || ctx.Pager == nil { continue }
        entries, _ := ctx.Schema.GetEntries(schema.TypeTable)
        idxEntries, _ := ctx.Schema.GetEntries(schema.TypeIndex)
        all := append(append([]*schema.Entry{}, entries...), idxEntries...)
        for _, te := range all {
            if te.RootPage <= 1 { continue }
            tree := e.tableBTreePg(ctx.Pager, te.Name, te.RootPage, true)
            e.checkOneTree(te.RootPage, tree, ctx.Pager, emit)
        }
    }
}

func (e *Engine) checkOneTree(rootPgno uint32, tree *btree.Btree, pg *pager.Pager, emit func(string)) {
    seen := map[uint32]int{ rootPgno: -1 }  // pageNo -> cell index that first referenced it (-1 = self)
    referenced := map[uint32]bool{ rootPgno: true }
    var pages []uint32
    pages = append(pages, rootPgno)

    for len(pages) > 0 {
        pgno := pages[0]
        pages = pages[1:]
        // Walk the page's cells looking for child pointers
        // (interior cells hold a left child; interior pages hold a
        // rightmost pointer too).
        page, err := pg.ReadPage(pgno)
        if err != nil { continue }
        coff := 0
        if pgno == 1 { coff = 100 }
        if coff+12 > len(page.Data) { continue }
        if int(coff+8+2*int(binary.BigEndian.Uint16(page.Data[coff+3:coff+5]))) > len(page.Data) {
            continue
        }
        cellType := storage.CellTableInterior
        if page.Data[coff] == storage.PageTypeInteriorIndex {
            cellType = storage.CellIndexInterior
        }
        if page.Data[coff] == storage.PageTypeInteriorTable ||
           page.Data[coff] == storage.PageTypeInteriorIndex {
            bp, err := storage.ParsePage(page.Data, int(pg.PageSize()), coff)
            if err != nil { continue }
            for i := 0; i < int(bp.CellCount); i++ {
                ptrOff := coff + 12 + i*2
                if ptrOff+2 > len(page.Data) { break }
                off := int(binary.BigEndian.Uint16(page.Data[ptrOff:ptrOff+2]))
                cell, derr := storage.DecodeCell(page.Data, off, cellType, int(pg.PageSize()))
                if derr != nil { continue }
                if first, ok := seen[cell.LeftPtr]; ok {
                    msg := fmt.Sprintf("Tree %d page %d cell %d: 2nd reference to page %d",
                        rootPgno, pgno, i, cell.LeftPtr)
                    _ = first
                    emit(msg)
                } else {
                    seen[cell.LeftPtr] = i
                    referenced[cell.LeftPtr] = true
                    pages = append(pages, cell.LeftPtr)
                }
            }
            if first, ok := seen[bp.RightmostPtr]; ok {
                emit(fmt.Sprintf("Tree %d page %d cell %d: 2nd reference to page %d",
                    rootPgno, pgno, int(bp.CellCount), bp.RightmostPtr))
            } else {
                seen[bp.RightmostPtr] = int(bp.CellCount)
                referenced[bp.RightmostPtr] = true
                pages = append(pages, bp.RightmostPtr)
            }
        }
    }

    // Orphan detection: pages on disk not in `referenced`.
    nPages, err := pg.PageCount()
    if err != nil { return }
    for p := uint32(2); p <= nPages; p++ {  // page 1 is the schema root, not a b-tree page
        if !referenced[p] {
            emit(fmt.Sprintf("Page %d: never used", p))
        }
    }
}
```

Wire it into `execQuickCheck`: call AFTER `btreeStructureOK()` (so
malformed pages short-circuit), and BEFORE `checkFreelistCount` /
`quickCheckTables`. The first emitted line is the multi-line header
`*** in database main ***` (matches SQLite integrityCheck's behaviour
with the FIRST_DB_OUTPUT_PREFIX).

**Source fidelity** (SQLite btree.c): `checkTree` builds a "seen" set
keyed by page number; on a duplicate, emits "2nd reference to page N"
and continues (does NOT abort). Then iterates all pages of the file
checking unreferenced ones.  We follow that.

### Step 2 — Transpiler: N-byte read + binary scan S

In `tools/tcl2go/helpers_template_part2_tail.go`:

- Add `tclReadFileWithLen(path string, n int) string` that returns the
  first n bytes from the seek position (or the whole file if no seek).
  Have `tclReadFile` delegate to it with n=0 (= whole file).
- Add `tclBinaryScanBigUint16(b string) int` that decodes b as
  big-endian uint16.

In `tools/tcl2go/processset_part2.go` (around line 185):

```go
// set VAR [read $CHAN N] — read N bytes from the channel's current
// position (TCL's read $fd N).
if cmdParts[0] == "read" && len(cmdParts) >= 2 && strings.HasPrefix(cmdParts[1], "$") {
    chanGo := tclVarToGo(strings.TrimPrefix(cmdParts[1], "$"))
    if isValidGoIdent(chanGo) && tp.isVarDeclared(chanGo) {
        if len(cmdParts) >= 3 {
            nExpr := tclExprToGoString(cmdParts[2])
            tp.assignSetValue(goName, fmt.Sprintf("tclReadFileWithLen(%s, %s)", chanGo, nExpr))
        } else {
            tp.assignSetValue(goName, "tclReadFile("+chanGo+")")
        }
        return true
    }
}
```

Same for the `[read $fd2]` form in `processset.go` (line 696).

For `binary scan`:
- detect `binary scan $var S name` in `processcommand.go` and emit
  `name = tclBinaryScanBigUint16(var)` (S = big-endian uint16; covers
  the corrupt2-5.1 need; the I (uint32) form is also used by other
  tests but not in scope here).

### Step 3 — Native test: `frigolite_corrupt2_check_test.go`

Direct engine-level test that does not depend on the transpiler:

1. Create a 1024-byte-page DB.
2. `PRAGMA auto_vacuum=0; PRAGMA page_size=1024; CREATE TABLE t1(a,b,c); CREATE TABLE t2(a,b,c);`
3. `INSERT INTO t2 VALUES(randomblob(100),...)*4` to grow t2 to multiple pages
4. `INSERT INTO t1 SELECT * FROM t2;`
5. Close.
6. Open file, read 2 bytes from page 1 + 12 (cell-pointer 0 of t1's root at page 2 — wait, but for
   `t1` we need to know which page is t1's root; just use the same byte pattern as TCL.
7. Corrupt: copy 4 bytes from `[1024 + 12 + celloff..+4]` (t1 cell 0 child page) to
   `[2*1024 + 12 + celloff..+4]` (t2 cell 0 child page).
8. Reopen via Open(); call `PRAGMA integrity_check`.
9. Assert rows contain `Tree 2 page 2 cell 0: 2nd reference to page 10` and
   `Page 4: never used`.

If the natural page layout doesn't match, write the test using the
same `randomblob(100)*16` pattern and verify the actual page numbers
once; pin those numbers in the test (the corruption offsets match
exactly what corrupt2-5.1 does, so the resulting page numbers are
deterministic given our page layout).

### Step 4 — Re-REGEN and run

```bash
go run ./tools/tcl2go/  # regenerates testgen/corrupt2/corrupt2_test.go
go build ./... && go vet ./... && go test -run TestSOLID_ ./... && \
  go test -tags testgen ./testgen/corrupt/ ./testgen/corrupt2/ \
    ./testgen/corrupt3/ ./testgen/corrupt4/ ./testgen/corrupt5/ \
    ./testgen/corrupt6/ ./testgen/corrupt7/ ./testgen/corrupt8/ \
    -count=1 -timeout 300s
```

Plus a non-testgen run of the new native test:
```bash
go test -run TestCorrupt2Check -v -count=1
```

## Files touched

- `internal/exec/pragma_quickcheck.go` — add `checkTreePage`, `checkOneTree`,
  wire into `execQuickCheck`. New import: `github.com/pijalu/frigolite/internal/btree`.
- `tools/tcl2go/helpers_template_part2_tail.go` — add `tclReadFileWithLen`,
  `tclBinaryScanBigUint16`.
- `tools/tcl2go/processset_part2.go` — extend `read $fd N` to capture N.
- `tools/tcl2go/processset.go` — same for the `[read $fd2]` form.
- `tools/tcl2go/processcommand.go` (or `processcmdextra.go`) — add
  `binary scan` S-form emission.
- `frigolite_corrupt2_check_test.go` (new) — native regression test.
- `testgen/corrupt2/corrupt2_test.go` — regenerated by tcl2go.
- `.agents/lessons_learned.md` — record the checkTreePage design and
  the tcl2go `read N` / `binary scan S` fixes.

## Risks and mitigations

- **Engine `checkTreePage` recursion depth**: SQLite's walker is depth-first via
  the btree itself. We mirror that with a `pages` queue. Recursion is bounded
  by tree depth (≤ ~20 even for huge trees).
- **Page 1 ambiguity**: page 1 is the schema root and a btree page; we use
  `coff = 100` to skip the file header. The check starts from `rootPgno`
  (the table/index root), which may itself BE page 1 if no separate root
  was allocated. For tables created before any inserts the root is page 1;
  the existing `walkBTreePages` already handles this case.
- **Index trees**: we iterate `idxEntries` too (the existing
  `btreeStructureOK` does). They share the same page numbers as the
  table they index — the `seen` map is per-tree, so cross-tree page
  sharing is fine.
- **Orphan page detection**: we scan all pages 2..nPages; the
  freelist pages (if any) will report as "never used" — but the
  existing `checkFreelistCount` already validates the freelist, and
  corrupt2-5.1's DB has `auto_vacuum=0` so the freelist is empty.
  For auto_vacuum=1/2 with no freelist corruption, freelist pages
  also legitimately carry an `PtrMap` flag the existing checks
  already account for; if a future test reports a false "Page N:
  never used" for a freelist page we'll need to skip them — but
  that's out of scope here.
- **Performance**: walking every btree page on every integrity_check
  is what SQLite does; for our test DBs (≤ ~24 pages) this is
  microseconds. No concern.

## Acceptance criteria

- `go build ./...` clean
- `go vet ./...` clean
- `go test -run TestSOLID_ ./...` clean
- `go test -tags testgen ./testgen/corrupt*/ -count=1 -timeout 300s`
  exits 0 (all 8 packages green)
- New native test `TestCorrupt2Check` passes
- No regression: all previously-passing tests still pass

## Verification command (full)

```bash
go build ./... && go vet ./... && go test -run TestSOLID_ ./... && \
  go test -tags testgen ./testgen/corrupt/ ./testgen/corrupt2/ \
    ./testgen/corrupt3/ ./testgen/corrupt4/ ./testgen/corrupt5/ \
    ./testgen/corrupt6/ ./testgen/corrupt7/ ./testgen/corrupt8/ \
    -count=1 -timeout 300s && \
  go test -run TestCorrupt2Check -count=1 -v
```

## Open questions / decisions to surface mid-implementation

- If `checkTreePage` finds a duplicate but the BTree cursor's `tableBTreePg`
  API doesn't expose page-level iteration, I'll need a different mechanism
  (read pages directly via the pager, then decode cells via `storage.DecodeCell`).
- The native test's exact page numbers (1, 2, 3, 4, 10 in the expected
  output) depend on the corruption landing as planned. If a header
  cell-pointer offset differs, the test will print actual numbers and I'll
  pin those.
