package exec

import (
	"encoding/binary"
	"fmt"
)

// checkFreelistCount validates the on-disk freelist: counts pages reachable
// from the header-declared trunk chain and compares against the header-
// declared count. A mismatch is reported as "Freelist: size is N but
// should be M" (mirrors btree.c checkList's trailing "size is %u but
// should be %u"). corrupt2.test 14.2/14.3/14.5: write "size=2" to header
// byte 36 while the chain still carries 3 free pages; integrity_check
// must surface the mismatch.
//
// Per-problem messages mirror btree.c checkList + checkRef (run with
// zPfx="Freelist: ") exactly:
//
//	"Freelist: invalid page number N"              — checkRef: page 0 or beyond the file
//	"Freelist: 2nd reference to page N"            — checkRef: page revisited
//	"Freelist: failed to get page N"               — sqlite3PagerGet failed
//	"Freelist: freelist leaf count too big on page N" — k > usableSize/4-2
//
// Unlike SQLite's single accumulator row, each message is emitted as its
// own row; the TCL flatten comparison renders the two forms identically.
// The walk stops at the first invalid trunk (checkRef → break) but keeps
// scanning leaves after a bad leaf, exactly like the C loop.
func (e *Engine) checkFreelistCount(emit func(string)) string {
	ctx, iPage, headerCount, ok := e.freelistHeaderState()
	if !ok {
		return ""
	}
	numPages := ctx.Pager.NumPages()
	used := make(map[uint32]bool)
	consumed, errored := e.walkFreelistTrunks(ctx, iPage, numPages, used, emit)
	if consumed != headerCount && !errored {
		return fmt.Sprintf("*** in database main ***\nFreelist: size is %d but should be %d", consumed, headerCount)
	}
	return ""
}

// freelistHeaderState reads the header-declared freelist first-trunk page
// and page count. ok is false when there is nothing to verify: no database,
// no pager, a truncated header, or an all-zero freelist declaration.
func (e *Engine) freelistHeaderState() (ctx *DatabaseContext, firstTrunk, count uint32, ok bool) {
	if len(e.dbList) == 0 {
		return nil, 0, 0, false
	}
	ctx = e.dbList[0]
	if ctx == nil || ctx.Pager == nil {
		return nil, 0, 0, false
	}
	hdr := ctx.Pager.Header()
	if len(hdr) < 40 {
		return nil, 0, 0, false
	}
	firstTrunk = binary.BigEndian.Uint32(hdr[32:36])
	count = binary.BigEndian.Uint32(hdr[36:40])
	if firstTrunk == 0 && count == 0 {
		return nil, 0, 0, false
	}
	return ctx, firstTrunk, count, true
}

// walkFreelistTrunks walks the header-declared trunk chain, counting pages
// reachable from it (checkRef → break on the first invalid trunk reference
// but keep scanning leaves after a bad leaf, exactly like the C loop).
func (e *Engine) walkFreelistTrunks(ctx *DatabaseContext, iPage, numPages uint32, used map[uint32]bool, emit func(string)) (consumed uint32, errored bool) {
	for iter := 0; iPage != 0 && iter < 100000; iter++ {
		next, pages, errd, stop := e.scanFreelistTrunk(ctx, iPage, numPages, used, emit)
		consumed += pages
		if errd {
			errored = true
		}
		if stop {
			return consumed, true
		}
		iPage = next
	}
	return consumed, errored
}

// scanFreelistTrunk processes one freelist trunk page. stop reports a fatal
// condition that ends the walk (the trunk reference itself is unusable —
// btree.c checkRef → break); errored reports any problem found, including
// an oversized leaf count, after which the walk continues with the next
// trunk. pages counts the trunk page itself plus its reachable leaves.
func (e *Engine) scanFreelistTrunk(ctx *DatabaseContext, iPage, numPages uint32, used map[uint32]bool, emit func(string)) (nextTrunk, pages uint32, errored, stop bool) {
	// checkRef (btree.c:10633): out-of-range or duplicate page reference.
	if used[iPage] {
		emit(fmt.Sprintf("Freelist: 2nd reference to page %d", iPage))
		return 0, 0, true, true
	}
	if freelistRefCheck(iPage, numPages, emit) {
		return 0, 0, true, true
	}
	used[iPage] = true
	pages++
	pg, err := ctx.Pager.ReadPage(iPage)
	if err != nil {
		emit(fmt.Sprintf("Freelist: failed to get page %d", iPage))
		return 0, pages, true, true
	}
	data := pg.Data
	if len(data) < 8 {
		emit(fmt.Sprintf("Freelist: failed to get page %d", iPage))
		return 0, pages, true, true
	}
	// SQLite freelist trunk format (btree.c:10701):
	//   offset 0-3: next trunk page number
	//   offset 4-7: leaf count (4 bytes, not 2!)
	//   offset 8+: leaf page numbers (4 bytes each)
	nextTrunk = binary.BigEndian.Uint32(data[0:4])
	leafCount := binary.BigEndian.Uint32(data[4:8])
	// btree.c checkList: "freelist leaf count too big on page %u"
	// when k > usableSize/4 - 2. usableSize == pageSize (no reserved
	// bytes), matching maxTrunkLeaves + 2.
	if leafCount > uint32(ctx.Pager.PageSize())/4-2 {
		emit(fmt.Sprintf("Freelist: freelist leaf count too big on page %d", iPage))
		return nextTrunk, pages, true, false
	}
	leafPages, leafErrored := e.scanFreelistLeaves(data, leafCount, numPages, used, emit)
	return nextTrunk, pages + leafPages, leafErrored, false
}

// scanFreelistLeaves checks a trunk page's leaf entries. Per-leaf semantics
// mirror btree.c checkRef run per leaf: a revisited leaf is "2nd reference",
// a zero or out-of-range slot is "invalid page number N" (the C code does
// not skip zero slots); neither breaks the leaf loop.
func (e *Engine) scanFreelistLeaves(data []byte, leafCount, numPages uint32, used map[uint32]bool, emit func(string)) (pages uint32, errored bool) {
	for i := uint32(0); i < leafCount; i++ {
		off := 8 + i*4
		if int(off)+4 > len(data) {
			break
		}
		leaf := binary.BigEndian.Uint32(data[off : off+4])
		if used[leaf] && leaf != 0 && leaf <= numPages {
			emit(fmt.Sprintf("Freelist: 2nd reference to page %d", leaf))
			errored = true
			continue
		}
		if freelistRefCheck(leaf, numPages, emit) {
			errored = true
			continue
		}
		used[leaf] = true
		pages++
	}
	return pages, errored
}

// freelistRefCheck mirrors btree.c checkRef for the freelist walk: page 0
// or beyond the file emits "Freelist: invalid page number N" and reports
// true (a failed reference).
func freelistRefCheck(pgno, numPages uint32, emit func(string)) bool {
	if pgno == 0 || pgno > numPages {
		emit(fmt.Sprintf("Freelist: invalid page number %d", pgno))
		return true
	}
	return false
}
