// Package schema manages the database schema (sqlite_schema table).
package schema

import (
	"encoding/binary"

	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/storage"
)

func (m *Manager) UpdateEntryFull(oldName, newName, newSQL string) error {
	m.invalidateForMutation()

	searchName := oldName
	if dotIdx := strings.Index(oldName, "."); dotIdx >= 0 {
		searchName = oldName[dotIdx+1:]
	}

	tree := btree.NewSchemaBTree(m.pager)
	defer tree.Close() // schema tree and its cursors are function-local

	// Locate the matching cell, capture its rowid, type, name, tbl_name and
	// rootpage so the replacement can reuse them (the b-tree orders by rowid,
	// so re-inserting with the same rowid keeps the row in its original
	// position).
	var foundRowID int64 = -1
	var foundType, foundTbl, foundRoot interface{}
	if _, err := tree.DeleteCellsWhereFullPayload(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil || len(rec.Values) < 5 {
			return false
		}
		if !strings.EqualFold(toString(rec.Values[1]), searchName) {
			return false
		}
		foundRowID = cell.RowID
		foundType = rec.Values[0]
		foundTbl = rec.Values[2]
		foundRoot = rec.Values[3]
		return true
	}); err != nil {
		return err
	}
	if foundRowID < 0 {
		return fmt.Errorf("no such table: %s", oldName)
	}

	values := []interface{}{foundType, newName, foundTbl, foundRoot, newSQL}
	record, err := storage.EncodeRecord(values)
	if err != nil {
		return err
	}
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   foundRowID,
		Payload: record,
	}
	return tree.InsertCell(cell)
}

// RemoveEntry removes a schema entry by name.
func (m *Manager) RemoveEntry(name string) error {
	return m.RemoveEntryOfType(name, "")
}

// RemoveEntryOfType removes a schema entry by name, optionally restricted to
// a specific object type. When schemaType is empty every object with the name
// is removed (RemoveEntry behavior); otherwise only rows whose type column
// matches are deleted. The type filter matters for objects that share a name
// with another object (e.g. a trigger and a table both named "t2"): DROP
// TRIGGER t2 must not delete the table t2.
func (m *Manager) RemoveEntryOfType(name string, schemaType SchemaType) error {
	// Schema changed: drop the entry cache and bump the schema cookie.
	m.invalidateForMutation()

	// Strip schema prefix if present (e.g. "aux.t4" -> "t4")
	searchName := name
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		searchName = name[dotIdx+1:]
	}
	m.forgetSessionEntry(searchName, schemaType)

	tree := btree.NewSchemaBTree(m.pager)
	defer tree.Close() // schema tree and its cursors are function-local
	_, err := tree.DeleteCellsWhereFullPayload(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return false
		}
		if len(rec.Values) >= 2 && strings.EqualFold(toString(rec.Values[1]), searchName) {
			if schemaType == "" {
				return true
			}
			return len(rec.Values) >= 1 && strings.EqualFold(toString(rec.Values[0]), string(schemaType))
		}
		return false
	})
	return err
}

// nextRowID generates a new rowid for schema entries.
// It scans the schema page (page 1) to find the maximum existing rowid
// and returns the next available value.
func (m *Manager) nextRowID() int64 {
	tree := btree.NewSchemaBTree(m.pager)
	defer tree.Close() // schema tree and its cursors are function-local
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 1
	}
	var maxID int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		if cell.RowID > maxID {
			maxID = cell.RowID
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return maxID + 1
}

func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toInt64(v interface{}) int64 {
	if v == nil {
		return 0
	}
	i, ok := v.(int64)
	if ok {
		return i
	}
	return 0
}

// ValidateAllTableRoots walks every table's root-page btree, parsing each
// page so structural corruption (out-of-range cell content/pointers, a
// crash-written page) is detected. Real SQLite validates the schema's root
// pages when preparing an INSERT ... SELECT (fts3corrupt4 24.1: a corrupt
// t2 btree fails the INSERT even though the statement reads a CTE).
func (m *Manager) ValidateAllTableRoots() error {
	entries, err := m.GetEntries("")
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.RootPage > 0 && ent.Type == TypeTable {
			if err := m.walkRootPage(ent.RootPage); err != nil {
				return fmt.Errorf("database disk image is malformed")
			}
		}
	}
	return nil
}

func (m *Manager) walkRootPage(rootPage uint32) error {
	seen := map[uint32]bool{}
	stack := []uint32{rootPage}
	for len(stack) > 0 {
		pageNum := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pageNum] {
			continue
		}
		seen[pageNum] = true
		pg, err := m.pager.ReadPage(pageNum)
		if err != nil {
			return err
		}
		coff := contentOffset(pageNum)
		if len(pg.Data) < coff+12 {
			return fmt.Errorf("database disk image is malformed")
		}
		ptype := pg.Data[coff]
		switch ptype {
		case storage.PageTypeInteriorTable:
			stack = m.appendCheckedInteriorChildren(stack, pg.Data, coff)
		case storage.PageTypeLeafTable:
			// Validate the RAW cell pointers (unmasked): a pointer at/beyond
			// the page end is genuine corruption. Normal reads mask the
			// pointer (SQLite findCell), but the corruption walk must detect
			// it (fts3corrupt4 24.1: t2's 4310 pointer on a 4096 page fails
			// an INSERT that grows the file).
			if err := checkLeafCellPointers(pg.Data, coff, int(m.pager.PageSize())); err != nil {
				return err
			}
		case 0x00:
			// A type-0x00 page is an empty/unused page; SQLite tolerates it
			// (a crash may leave zeroed pages in a tree). Skip it.
			continue
		default:
		}
		if _, perr := storage.ParsePage(pg.Data, int(m.pager.PageSize()), coff); perr != nil {
			return perr
		}
	}
	return nil
}

// appendCheckedInteriorChildren pushes an interior table page's rightmost
// pointer and its in-range cell children; out-of-range cell offsets are
// tolerated (SQLite's findCell masks the pointer; a garbage cell offset is
// not fatal — fts3corrupt4 10.2: an interior cell at offset 0 yields an
// out-of-range child), and children are bounded by the page count.
//
// The interior cell pointer array starts at coff+12: coff+8 holds the
// rightmost child pointer (btree.c checkTreePage's
// cellStart = hdr+12-4*leaf). Reading it at coff+8 treated the right child's
// low 16 bits as a cell offset and followed a bogus child — often page 2,
// the auto-vacuum pointer-map page — making a healthy INSERT ... SELECT fail
// its growth guard. The pointer is masked with pageSize-1 like findCell.
func (m *Manager) appendCheckedInteriorChildren(stack []uint32, data []byte, coff int) []uint32 {
	right := binary.BigEndian.Uint32(data[coff+8 : coff+12])
	if right > 0 {
		stack = append(stack, right)
	}
	ncell := int(binary.BigEndian.Uint16(data[coff+3 : coff+5]))
	for i := 0; i < ncell; i++ {
		if coff+14+2*i > len(data) {
			continue
		}
		cellOff := int(binary.BigEndian.Uint16(data[coff+12+2*i:coff+14+2*i]) & uint16(len(data)-1))
		if cellOff >= coff+12 && cellOff+4 <= len(data) {
			child := binary.BigEndian.Uint32(data[cellOff : cellOff+4])
			if child > 0 && child <= m.pager.NumPages() {
				stack = append(stack, child)
			}
		}
	}
	return stack
}

// checkLeafCellPointers validates a leaf page's RAW cell pointers (unmasked):
// a pointer at/beyond the page end is genuine corruption. Normal reads mask
// the pointer (SQLite findCell), but the corruption walk must detect it
// (fts3corrupt4 24.1: t2's 4310 pointer on a 4096 page fails an INSERT that
// grows the file).
func checkLeafCellPointers(data []byte, coff, ps int) error {
	ncell := int(binary.BigEndian.Uint16(data[coff+3 : coff+5]))
	for i := 0; i < ncell; i++ {
		if coff+8+2*i+2 > len(data) {
			return fmt.Errorf("database disk image is malformed")
		}
		cp := int(binary.BigEndian.Uint16(data[coff+8+2*i : coff+10+2*i]))
		if cp >= ps {
			return fmt.Errorf("database disk image is malformed")
		}
	}
	return nil
}

// EstimateFreeSpace returns the approximate free bytes across all table/index
// btree pages (the sum of the cell-content-area gaps). A write whose
// serialized size exceeds this must grow the database file, which is when
// SQLite's allocation path validates the pointer-map/auto-vacuum state.
func (m *Manager) EstimateFreeSpace() int64 {
	entries, err := m.GetEntries("")
	if err != nil {
		return 0
	}
	var free int64
	seen := map[uint32]bool{}
	for _, ent := range entries {
		if ent.RootPage == 0 || seen[ent.RootPage] {
			continue
		}
		seen[ent.RootPage] = true
		free += m.freeSpaceWalk(ent.RootPage, seen)
	}
	return free
}

// freeSpaceWalk walks one btree (cycle-safe via seen) summing the
// cell-content-area gaps of its leaf pages; unused pages count as fully
// free. See EstimateFreeSpace.
func (m *Manager) freeSpaceWalk(rootPage uint32, seen map[uint32]bool) int64 {
	var free int64
	stack := []uint32{rootPage}
	for len(stack) > 0 {
		pageNum := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pageNum] {
			continue
		}
		seen[pageNum] = true
		pg, err := m.pager.ReadPage(pageNum)
		if err != nil {
			continue
		}
		coff := contentOffset(pageNum)
		if len(pg.Data) < coff+8 {
			continue
		}
		pageFree, children := freeSpacePage(pg.Data, coff, int(m.pager.PageSize()))
		free += pageFree
		stack = append(stack, children...)
	}
	return free
}

// freeSpacePage returns a page's cell-content gap and the children its
// interior b-tree header points to. Callers pass a page buffer with at least
// coff+8 bytes; an interior page shorter than coff+12 has no child pointers.
func freeSpacePage(data []byte, coff, pageSize int) (int64, []uint32) {
	switch data[coff] {
	case storage.PageTypeInteriorTable:
		if len(data) >= coff+12 {
			return 0, appendInteriorChildren(nil, data, coff)
		}
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		ncell := int(binary.BigEndian.Uint16(data[coff+3 : coff+5]))
		cc := int(binary.BigEndian.Uint16(data[coff+5 : coff+7]))
		cpe := coff + 8 + 2*ncell
		if cc >= cpe && cc <= pageSize {
			return int64(cc - cpe), nil
		}
	case 0x00:
		return int64(pageSize), nil
	}
	return 0, nil
}

// appendInteriorChildren pushes an interior table page's rightmost pointer
// and every in-range cell child onto the walk stack; see freeSpaceWalk and
// walkRootPage.
func appendInteriorChildren(stack []uint32, data []byte, coff int) []uint32 {
	right := binary.BigEndian.Uint32(data[coff+8 : coff+12])
	if right > 0 {
		stack = append(stack, right)
	}
	ncell := int(binary.BigEndian.Uint16(data[coff+3 : coff+5]))
	ps := len(data)
	for i := 0; i < ncell; i++ {
		if coff+14+2*i > ps {
			continue
		}
		cellOff := int(binary.BigEndian.Uint16(data[coff+12+2*i:coff+14+2*i]) & uint16(ps-1))
		if cellOff >= coff+12 && cellOff+4 <= ps {
			child := binary.BigEndian.Uint32(data[cellOff : cellOff+4])
			if child > 0 {
				stack = append(stack, child)
			}
		}
	}
	return stack
}
