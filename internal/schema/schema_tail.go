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
	m.cacheValid = false
	m.entriesCache = nil

	searchName := oldName
	if dotIdx := strings.Index(oldName, "."); dotIdx >= 0 {
		searchName = oldName[dotIdx+1:]
	}

	tree := btree.NewBTree(m.pager, 1, true)

	// Locate the matching cell, capture its rowid, type, name, tbl_name and
	// rootpage so the replacement can reuse them (the b-tree orders by rowid,
	// so re-inserting with the same rowid keeps the row in its original
	// position).
	var foundRowID int64 = -1
	var foundType, foundTbl, foundRoot interface{}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
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
	// Invalidate schema cache since the schema has changed
	m.cacheValid = false
	m.entriesCache = nil

	// Strip schema prefix if present (e.g. "aux.t4" -> "t4")
	searchName := name
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		searchName = name[dotIdx+1:]
	}

	tree := btree.NewBTree(m.pager, 1, true)
	_, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
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
	tree := btree.NewBTree(m.pager, 1, true)
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
			right := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
			if right > 0 {
				stack = append(stack, right)
			}
			ncell := int(binary.BigEndian.Uint16(pg.Data[coff+3 : coff+5]))
			for i := 0; i < ncell; i++ {
				if coff+8+2*i+2 > len(pg.Data) {
					return fmt.Errorf("database disk image is malformed")
				}
				cellOff := int(binary.BigEndian.Uint16(pg.Data[coff+8+2*i : coff+10+2*i]))
				// Skip corrupt interior cells whose child pointer is out of
				// range (SQLite's findCell masks the pointer; a garbage cell
				// offset is tolerated, not fatal — fts3corrupt4 10.2: an
				// interior cell at offset 0 yields an out-of-range child).
				if cellOff >= 8 && cellOff+4 <= len(pg.Data) {
					child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
					if child > 0 && child <= m.pager.NumPages() {
						stack = append(stack, child)
					}
				}
			}
		case storage.PageTypeLeafTable:
			// Validate the RAW cell pointers (unmasked): a pointer at/beyond
			// the page end is genuine corruption. Normal reads mask the
			// pointer (SQLite findCell), but the corruption walk must detect
			// it (fts3corrupt4 24.1: t2's 4310 pointer on a 4096 page fails
			// an INSERT that grows the file).
			ncell := int(binary.BigEndian.Uint16(pg.Data[coff+3 : coff+5]))
			ps := int(m.pager.PageSize())
			for i := 0; i < ncell; i++ {
				if coff+8+2*i+2 > len(pg.Data) {
					return fmt.Errorf("database disk image is malformed")
				}
				cp := int(binary.BigEndian.Uint16(pg.Data[coff+8+2*i : coff+10+2*i]))
				if cp >= ps {
					return fmt.Errorf("database disk image is malformed")
				}
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
		stack := []uint32{ent.RootPage}
		for len(stack) > 0 {
			pageNum := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			pg, err := m.pager.ReadPage(pageNum)
			if err != nil {
				continue
			}
			coff := contentOffset(pageNum)
			if len(pg.Data) < coff+8 {
				continue
			}
			ptype := pg.Data[coff]
			switch ptype {
			case storage.PageTypeInteriorTable:
				if len(pg.Data) < coff+12 {
					continue
				}
				right := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
				if right > 0 {
					stack = append(stack, right)
				}
				ncell := int(binary.BigEndian.Uint16(pg.Data[coff+3 : coff+5]))
				for i := 0; i < ncell; i++ {
					if coff+8+2*i+2 > len(pg.Data) {
						continue
					}
					cellOff := int(binary.BigEndian.Uint16(pg.Data[coff+8+2*i : coff+10+2*i]))
					if cellOff >= 0 && cellOff+4 <= len(pg.Data) {
						child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
						if child > 0 {
							stack = append(stack, child)
						}
					}
				}
			case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
				ncell := int(binary.BigEndian.Uint16(pg.Data[coff+3 : coff+5]))
				cc := int(binary.BigEndian.Uint16(pg.Data[coff+5 : coff+7]))
				cpe := coff + 8 + 2*ncell
				if cc >= cpe && cc <= int(m.pager.PageSize()) {
					free += int64(cc - cpe)
				}
			case 0x00:
				free += int64(m.pager.PageSize())
			}
		}
	}
	return free
}
