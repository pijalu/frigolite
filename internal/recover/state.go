// Recovery state: page-reachability bookkeeping for lost_and_found orphan
// detection (sqlite3recover.c walks reachability through the sqlite_dbdata
// page reader; frigolite walks the pager directly).

package recover

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// recoveryState carries the input database handle and options through one
// recovery pass.
type recoveryState struct {
	pg         *pager.Pager
	opts       Options
	reachRoots []uint32
}

// markTree marks every page of the btree rooted at pgno as reachable
// (interior children, rightmost pointer, and overflow chains).
func (r *recoveryState) markTree(pgno uint32, m map[uint32]bool) {
	if m[pgno] || pgno < 1 || pgno > r.pg.NumPages() {
		return
	}
	m[pgno] = true
	pgd, err := r.pg.ReadPage(pgno)
	if err != nil {
		return
	}
	coff := contentOffset(pgno)
	ptype := pgd.Data[coff]
	pageSize := int(r.pg.PageSize())
	switch ptype {
	case 0x05, 0x02: // interior
		page, perr := storage.ParsePage(pgd.Data, pageSize, coff)
		if perr != nil {
			return
		}
		r.markInteriorChildren(pgd.Data, coff, pageSize, int(page.CellCount), m)
	case 0x0d, 0x0a: // leaf: mark each cell's overflow chain
		page, perr := storage.ParsePage(pgd.Data, pageSize, coff)
		if perr != nil {
			return
		}
		r.markLeafOverflowChains(pgd.Data, coff, ptype, pageSize, int(page.CellCount), m)
	}
}

// markInteriorChildren marks the interior page's cell left-children and the
// rightmost pointer as reachable.
func (r *recoveryState) markInteriorChildren(data []byte, coff, pageSize, cellCount int, m map[uint32]bool) {
	for i := 0; i < cellCount; i++ {
		cellOff := int(storage.CellPointer(data, coff+4, i, pageSize))
		if cellOff+4 <= len(data) {
			r.markTree(binary.BigEndian.Uint32(data[cellOff:cellOff+4]), m)
		}
	}
	r.markTree(binary.BigEndian.Uint32(data[coff+8:coff+12]), m)
}

// markLeafOverflowChains marks every cell's overflow chain of a leaf page.
func (r *recoveryState) markLeafOverflowChains(data []byte, coff int, ptype byte, pageSize, cellCount int, m map[uint32]bool) {
	ptrBase := coff
	if ptype == 0x02 || ptype == 0x0a {
		ptrBase = coff + 4
	}
	for i := 0; i < cellCount; i++ {
		cellOff := int(storage.CellPointer(data, ptrBase, i, pageSize))
		r.markCellOverflow(data, cellOff, ptype, m)
	}
}

// markCellOverflow follows one cell's overflow chain, marking each page.
func (r *recoveryState) markCellOverflow(pageData []byte, cellOff int, ptype byte, m map[uint32]bool) {
	if cellOff < 0 || cellOff >= len(pageData) {
		return
	}
	usable := int(r.pg.UsableSize())
	var plen, pos int
	if ptype == 0x0d {
		// table leaf: payload-length varint, then rowid varint.
		plen64, n1 := varintAt(pageData, cellOff)
		_, n2 := varintAt(pageData, cellOff+n1)
		plen, pos = int(plen64), cellOff+n1+n2
	} else {
		// index leaf: payload-length varint only.
		plen64, n1 := varintAt(pageData, cellOff)
		plen, pos = int(plen64), cellOff+n1
	}
	local := leafLocalSize(plen, usable, ptype == 0x0d)
	if local+4 > len(pageData)-pos {
		return
	}
	next := binary.BigEndian.Uint32(pageData[pos+local : pos+local+4])
	r.markOverflowChain(next, m)
}

// markOverflowChain follows one overflow chain, marking each page until the
// chain ends or reaches an already-marked page.
func (r *recoveryState) markOverflowChain(next uint32, m map[uint32]bool) {
	for next != 0 && next <= r.pg.NumPages() && !m[next] {
		m[next] = true
		pgd, err := r.pg.ReadPage(next)
		if err != nil {
			break
		}
		next = binary.BigEndian.Uint32(pgd.Data[0:4])
	}
}

// freelistHead returns header[32:36] (the first freelist trunk page).
func (r *recoveryState) freelistHead() uint32 {
	hdr := r.pg.Header()
	if len(hdr) < 36 {
		return 0
	}
	return binary.BigEndian.Uint32(hdr[32:36])
}
