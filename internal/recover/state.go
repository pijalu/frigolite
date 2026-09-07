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
	switch ptype {
	case 0x05, 0x02: // interior
		page, perr := storage.ParsePage(pgd.Data, int(r.pg.PageSize()), coff)
		if perr != nil {
			return
		}
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pgd.Data, coff+4, i, int(r.pg.PageSize())))
			if cellOff+4 <= len(pgd.Data) {
				r.markTree(binary.BigEndian.Uint32(pgd.Data[cellOff:cellOff+4]), m)
			}
		}
		r.markTree(binary.BigEndian.Uint32(pgd.Data[coff+8:coff+12]), m)
	case 0x0d, 0x0a: // leaf: mark each cell's overflow chain
		page, perr := storage.ParsePage(pgd.Data, int(r.pg.PageSize()), coff)
		if perr != nil {
			return
		}
		ptrBase := coff
		if ptype == 0x02 || ptype == 0x0a {
			ptrBase = coff + 4
		}
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pgd.Data, ptrBase, i, int(r.pg.PageSize())))
			r.markCellOverflow(pgd.Data, cellOff, ptype, m)
		}
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
	maxLocal := usable - 35
	minLocal := ((usable - 12) * 32 / 255) - 23
	if ptype == 0x0a || ptype == 0x02 {
		maxLocal = ((usable - 12) * 64 / 255) - 23
	}
	local := plen
	if plen > maxLocal {
		surplus := minLocal + (plen-minLocal)%(usable-4)
		local = surplus
		if local > maxLocal {
			local = minLocal
		}
	}
	if local+4 > len(pageData)-pos {
		return
	}
	next := binary.BigEndian.Uint32(pageData[pos+local : pos+local+4])
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

// parsePageAt parses the btree page image at data with the page-1 header
// offset applied.
func parsePageAt(pgd *pager.Page, coff, pageSize int) (*storage.BTreePage, error) {
	return storage.ParsePage(pgd.Data, pageSize, coff)
}
