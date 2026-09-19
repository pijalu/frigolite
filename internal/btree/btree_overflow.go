// Overflow-chain handling for btree cells: preparing cells whose payload
// exceeds the local limit, writing the chain of overflow pages, and
// reassembling a cell's full payload from its chain.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// prepareCell writes overflow pages for a cell whose payload exceeds what
// fits in the cell itself, mutating the cell to reference the overflow chain
// (PayloadLen = full length, LocalLen = local portion, Overflow = first
// overflow page). The Payload slice is left intact so callers can still use
// the full key for ordering comparisons. For cells that fit, it leaves the
// cell unchanged.
func (t *BTree) prepareCell(c *storage.Cell, ownerPgno uint32) error {
	// Already prepared (balance_deeper re-enters the insert on the child
	// page after the root-level call): keep the existing overflow chain
	// instead of allocating a duplicate one.
	if c.Overflow != 0 && c.LocalLen > 0 && c.PayloadLen == len(c.Payload) {
		return nil
	}
	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}
	plen := len(c.Payload)
	local := storage.LocalPayloadSize(plen, int(t.usableSize), cellType)
	if local >= plen {
		return nil
	}
	first, err := t.writeOverflowPages(c.Payload[local:], ownerPgno)
	if err != nil {
		return err
	}
	c.Overflow = first
	c.PayloadLen = plen
	c.LocalLen = local
	return nil
}

// writeOverflowPages stores payload on a chain of overflow pages and returns
// the first page number. Each overflow page holds up to pageSize-4 payload
// bytes, prefixed with a 4-byte big-endian next-page pointer (0 = last).
// ownerPgno is the btree page holding the cell that owns the chain: the
// first overflow's ptrmap entry is PtrmapOverflow1 with parent=ownerPgno
// (btree.c fillInCell ptrmapPutOvfl, src/btree.c:9557), continuation pages
// are PtrmapOverflow2 chained to the previous overflow (src/btree.c:9766).
func (t *BTree) writeOverflowPages(payload []byte, ownerPgno uint32) (uint32, error) {
	chunk := int(t.usableSize) - 4
	var first uint32
	var prev *pager.Page
	pos := 0
	for pos < len(payload) {
		pg, err := t.allocNextOverflow(prev, ownerPgno)
		if err != nil {
			return 0, err
		}
		end := pos + chunk
		if end > len(payload) {
			end = len(payload)
		}
		copy(pg.Data[4:], payload[pos:end])
		if prev == nil {
			first = pg.PageNum
		} else {
			binary.BigEndian.PutUint32(prev.Data[0:4], pg.PageNum)
			if err := t.pager.WritePage(prev); err != nil {
				return 0, err
			}
		}
		prev = pg
		pos = end
	}
	// The final page's next-pointer is 0 = last.
	if prev != nil {
		binary.BigEndian.PutUint32(prev.Data[0:4], 0)
		if err := t.pager.WritePage(prev); err != nil {
			return 0, err
		}
	}
	return first, nil
}

// allocNextOverflow allocates the next overflow page of a chain: the first
// page hangs off the cell's owner (PtrmapOverflow1, allocOverflow) and every
// continuation page hangs off the previous overflow page (PtrmapOverflow2,
// allocOverflowNext).
func (t *BTree) allocNextOverflow(prev *pager.Page, ownerPgno uint32) (*pager.Page, error) {
	if prev == nil {
		return t.allocOverflow(ownerPgno)
	}
	return t.allocOverflowNext(prev.PageNum)
}

// readOverflow expands a decoded cell's local payload to the full payload by
// following the overflow chain. The returned cell is a copy when expansion is
// needed; the input cell is never mutated so it can still be re-encoded.
// Cells without overflow are returned unchanged.
//
// The chain is bounded by the file's own geometry (btree.c accessPayload
// parity): every overflow page lives in the database file, so a payload can
// never exceed numPages usable-size chunks. A corrupt cell whose payload
// length promises more than the file can hold reports
// "database disk image is malformed" BEFORE any allocation — without this
// bound a garbage payload length makes the assembly allocate gigabytes
// (corrupt-2.x junk-in-the-file probes).
func (t *BTree) readOverflow(c *storage.Cell) (*storage.Cell, error) {
	if c.Overflow == 0 {
		return c, nil
	}
	if c.PayloadLen < 0 ||
		uint64(c.PayloadLen) > uint64(t.pager.NumPages())*uint64(t.usableSize-4) {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	full := make([]byte, 0, c.PayloadLen)
	full = append(full, c.Payload...)
	pageNum := c.Overflow
	remaining := c.PayloadLen - len(c.Payload)
	for remaining > 0 {
		pg, err := t.pager.ReadPage(pageNum)
		if err != nil {
			return nil, err
		}
		next := binary.BigEndian.Uint32(pg.Data[0:4])
		chunk := int(t.usableSize) - 4
		n := chunk
		if n > remaining {
			n = remaining
		}
		full = append(full, pg.Data[4:4+n]...)
		remaining -= n
		pageNum = next
		if pageNum == 0 && remaining > 0 {
			return nil, fmt.Errorf("btree: overflow chain ended early")
		}
	}
	c2 := *c
	c2.Payload = full
	c2.Overflow = 0
	return &c2, nil
}
