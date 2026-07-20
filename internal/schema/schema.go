// Package schema manages the database schema (sqlite_schema table).
package schema

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// contentOffset returns the b-tree page header offset for a page number.
func contentOffset(pageNum uint32) int {
	if pageNum == 1 {
		return pager.HeaderSize
	}
	return 0
}

// SchemaType is the type of a schema entry.
type SchemaType string

const (
	TypeTable SchemaType = "table"
	TypeIndex SchemaType = "index"
	TypeView  SchemaType = "view"
	TypeTrigger SchemaType = "trigger"
)

// Entry represents a row in sqlite_schema.
type Entry struct {
	Type      SchemaType
	Name      string
	TblName   string
	RootPage  uint32
	SQL       string
}

// Manager manages the database schema.
type Manager struct {
	pager *pager.Pager
}

// NewManager creates a new schema manager.
func NewManager(pg *pager.Pager) *Manager {
	return &Manager{pager: pg}
}

// Init creates the sqlite_schema table if it doesn't exist.
// Also writes the database header for new databases.
func (m *Manager) Init() error {
	// Check if page 1 (root of sqlite_schema) exists
	if m.pager.NumPages() > 0 {
		return nil // already initialized
	}

	// Ensure database header is set
	if m.pager.Header() == nil {
		dh := storage.DefaultHeader(m.pager.PageSize())
		m.pager.SetHeader(dh.Encode())
	}

	// Create page 1 as a leaf table page
	// Note: for page 1, the b-tree content starts at byte 100 (after header)
	pg := m.pager.AllocatePage()
	if pg.PageNum != 1 {
		return fmt.Errorf("schema: expected page 1, got %d", pg.PageNum)
	}

	// Set page type to leaf table (at Data[0] which is after the header)
	coff := contentOffset(pg.PageNum)
	pg.Data[coff] = storage.PageTypeLeafTable

	// Zero out the rest of the header (firstFree, cellCount, cellContent, fragFree)
	for i := coff + 1; i < coff+8; i++ {
		pg.Data[i] = 0
	}

	return m.pager.WritePage(pg)
}

// AddEntry adds a new entry to the schema.
func (m *Manager) AddEntry(entry *Entry) error {
	// Convert schema entry to a record and insert into page 1
	values := []interface{}{
		entry.Type,
		entry.Name,
		entry.TblName,
		int64(entry.RootPage),
		entry.SQL,
	}
	record, err := storage.EncodeRecord(values)
	if err != nil {
		return err
	}

	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   m.nextRowID(),
		Payload: record,
	}

	tree := btree.NewBTree(m.pager, 1, true)
	return tree.InsertCell(cell)
}

// GetEntries returns all schema entries of the given type.
func (m *Manager) GetEntries(schemaType SchemaType) ([]*Entry, error) {
	var entries []*Entry
	tree := btree.NewBTree(m.pager, 1, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}

	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		if len(rec.Values) >= 5 {
			entry := &Entry{
				Type:     SchemaType(toString(rec.Values[0])),
				Name:     toString(rec.Values[1]),
				TblName:  toString(rec.Values[2]),
				RootPage: uint32(toInt64(rec.Values[3])),
				SQL:      toString(rec.Values[4]),
			}
			if schemaType == "" || entry.Type == schemaType {
				entries = append(entries, entry)
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	return entries, nil
}

// FindTable returns the schema entry for a table.
func (m *Manager) FindTable(name string) (*Entry, error) {
	// sqlite_schema is always on page 1 (bootstrap)
	if strings.ToUpper(name) == "SQLITE_SCHEMA" || strings.ToUpper(name) == "SQLITE_MASTER" {
		return &Entry{
			Type:     TypeTable,
			Name:     name,
			TblName:  name,
			RootPage: 1,
			SQL:      fmt.Sprintf("CREATE TABLE %s (type TEXT,name TEXT,tbl_name TEXT,rootpage INTEGER,sql TEXT)", name),
		}, nil
	}

	entries, err := m.GetEntries(TypeTable)
	if err != nil {
		return nil, err
	}
	upper := strings.ToUpper(name)
	for _, e := range entries {
		if strings.ToUpper(e.Name) == upper {
			return e, nil
		}
	}
	return nil, fmt.Errorf("schema: table not found: %s", name)
}

// FindView returns the schema entry for a view.
func (m *Manager) FindView(name string) (*Entry, error) {
	entries, err := m.GetEntries(TypeView)
	if err != nil {
		return nil, err
	}
	upper := strings.ToUpper(name)
	for _, e := range entries {
		if strings.ToUpper(e.Name) == upper {
			return e, nil
		}
	}
	return nil, fmt.Errorf("schema: view not found: %s", name)
}

// FindTrigger returns a trigger by name.
func (m *Manager) FindTrigger(name string) (*Entry, error) {
	entries, err := m.GetEntries(TypeTrigger)
	if err != nil {
		return nil, err
	}
	upper := strings.ToUpper(name)
	for _, e := range entries {
		if strings.ToUpper(e.Name) == upper {
			return e, nil
		}
	}
	return nil, fmt.Errorf("schema: trigger not found: %s", name)
}

// FindTriggersForTable returns all triggers for a given table.
func (m *Manager) FindTriggersForTable(tableName string) ([]*Entry, error) {
	entries, err := m.GetEntries(TypeTrigger)
	if err != nil {
		return nil, err
	}
	var result []*Entry
	upper := strings.ToUpper(tableName)
	for _, e := range entries {
		if strings.ToUpper(e.TblName) == upper {
			result = append(result, e)
		}
	}
	return result, nil
}

// FindIndex returns the schema entry for an index.
func (m *Manager) FindIndex(name string) (*Entry, error) {
	entries, err := m.GetEntries(TypeIndex)
	if err != nil {
		return nil, err
	}
	upper := strings.ToUpper(name)
	for _, e := range entries {
		if strings.ToUpper(e.Name) == upper {
			return e, nil
		}
	}
	return nil, fmt.Errorf("schema: index not found: %s", name)
}

// RemoveEntry removes a schema entry by name.
func (m *Manager) RemoveEntry(name string) error {
	tree := btree.NewBTree(m.pager, 1, true)
	_, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return false
		}
		if len(rec.Values) >= 2 {
			return strings.EqualFold(toString(rec.Values[1]), name)
		}
		return false
	})
	return err
}

// nextRowID generates a new rowid.
func (m *Manager) nextRowID() int64 {
	entries, _ := m.GetEntries("")
	var maxID int64
	for _, e := range entries {
		tree := btree.NewBTree(m.pager, e.RootPage, true)
		cursor, _ := tree.OpenCursor()
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
