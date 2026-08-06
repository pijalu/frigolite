// Package schema manages the database schema (sqlite_schema table).
package schema

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/rename"
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
	Columns   []ColumnDef // cached column definitions (tables only)
	RowID     int64       // sqlite_schema rowid (set when read from the b-tree)
}

// ColumnDef represents a column definition (replicated from sql.ColumnDef
// to avoid importing the sql package).
type ColumnDef struct {
	Name       string
	Type       string
	NotNull    bool
	PrimaryKey bool
	AutoInc    bool
	Unique     bool
	Default    string
}

// Manager manages the database schema.
type Manager struct {
	pager *pager.Pager
	debug bool

	// entriesCache caches GetEntries results to avoid repeated schema scans.
	// Invalidated by AddEntry. Not thread-safe — callers must ensure single-
	// goroutine access, which holds for the current architecture (each DB has
	// its own Manager, and operations on a single DB are sequential).
	entriesCache map[SchemaType][]*Entry
	cacheValid   bool
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

// InvalidateCache clears the schema entries cache so the next GetEntries
// call re-reads the sqlite_schema btree. Used after direct edits to
// sqlite_schema (PRAGMA writable_schema=ON), which SQLite treats as a schema
// change: subsequent table lookups must see the updated rootpages/SQL.
func (m *Manager) InvalidateCache() {
	m.cacheValid = false
	m.entriesCache = nil
}

// AddEntry adds a new entry to the schema.
func (m *Manager) AddEntry(entry *Entry) error {
	// Invalidate schema cache since the schema has changed
	m.cacheValid = false
	m.entriesCache = nil

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
	err = tree.InsertCell(cell)
	return err
}

// addEntryWithRowID inserts a schema entry using an explicit rowid (used to
// preserve a renamed entry's position in sqlite_schema).
func (m *Manager) addEntryWithRowID(entry *Entry, rowID int64) error {
	m.cacheValid = false
	m.entriesCache = nil

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
		RowID:   rowID,
		Payload: record,
	}

	tree := btree.NewBTree(m.pager, 1, true)
	return tree.InsertCell(cell)
}

// GetEntries returns all schema entries of the given type.
func (m *Manager) GetEntries(schemaType SchemaType) ([]*Entry, error) {
	// Return cached entries if cache is valid
	if m.cacheValid && m.entriesCache != nil {
		if entries, ok := m.entriesCache[schemaType]; ok {
			return entries, nil
		}
	}

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
				RowID:    cell.RowID,
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

	// Cache the result (copy the slice to prevent caller modifications from
	// corrupting the cache)
	if !m.cacheValid {
		m.entriesCache = make(map[SchemaType][]*Entry)
		m.cacheValid = true
	}
	cached := make([]*Entry, len(entries))
	copy(cached, entries)
	m.entriesCache[schemaType] = cached

	return entries, nil
}

// FindTable returns the schema entry for a table.
func (m *Manager) FindTable(name string) (*Entry, error) {
	// If the name has a schema prefix (e.g. "aux.t4"), try the full name first
	// to support tables in attached databases, then fall back to the short name.
	searchNames := []string{name}
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		shortName := name[dotIdx+1:]
		searchNames = []string{name, shortName}
	}

	// Check each search name in order (full name first, then short name)
	for _, searchName := range searchNames {
		searchUpper := strings.ToUpper(searchName)

		// sqlite_schema is always on page 1 (bootstrap)
		if searchUpper == "SQLITE_SCHEMA" || searchUpper == "SQLITE_MASTER" {
			return &Entry{
				Type:     TypeTable,
				Name:     name,
				TblName:  name,
				RootPage: 1,
				SQL:      fmt.Sprintf("CREATE TABLE %s (type TEXT,name TEXT,tbl_name TEXT,rootpage INTEGER,sql TEXT)", name),
			}, nil
		}

		// sqlite_temp_master is an alias for sqlite_master
		if searchUpper == "SQLITE_TEMP_MASTER" || searchUpper == "SQLITE_TEMP_SCHEMA" {
			return &Entry{
				Type:     TypeTable,
				Name:     name,
				TblName:  name,
				RootPage: 1,
				SQL:      fmt.Sprintf("CREATE TABLE %s (type TEXT,name TEXT,tbl_name TEXT,rootpage INTEGER,sql TEXT)", name),
			}, nil
		}

		// sqlite_sequence is a system table for AUTOINCREMENT tracking. It
		// only exists as a real table when a user creates it (SQLite allows
		// CREATE TABLE sqlite_sequence via PRAGMA writable_schema); prefer a
		// real schema entry over the synthetic fallback.
		if searchUpper == "SQLITE_SEQUENCE" {
			entries, gerr := m.GetEntries(TypeTable)
			if gerr == nil {
				for _, e := range entries {
					if strings.ToUpper(e.Name) == searchUpper || strings.ToUpper(e.TblName) == searchUpper {
						return e, nil
					}
				}
			}
			return &Entry{
				Type:     TypeTable,
				Name:     name,
				TblName:  name,
				RootPage: 1,
				SQL:      fmt.Sprintf("CREATE TABLE %s (name TEXT,seq INTEGER)", name),
			}, nil
		}

		// Pragma table-valued functions
		if strings.HasPrefix(searchUpper, "PRAGMA_") {
			return &Entry{
				Type:     TypeTable,
				Name:     name,
				TblName:  name,
				RootPage: 1,
				SQL:      fmt.Sprintf("CREATE TABLE %s (name TEXT)", name),
			}, nil
		}

		// Search in B-tree
		entries, err := m.GetEntries(TypeTable)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.ToUpper(e.Name) == searchUpper || strings.ToUpper(e.TblName) == searchUpper {
				return e, nil
			}
		}
	}

	return nil, fmt.Errorf("no such table: %s", name)
}

// FindView returns the schema entry for a view.
func (m *Manager) FindView(name string) (*Entry, error) {
	entries, err := m.GetEntries(TypeView)
	if err != nil {
		return nil, err
	}

	// Try original name first, then without schema prefix
	searchNames := []string{name}
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		shortName := name[dotIdx+1:]
		searchNames = []string{name, shortName}
	}

	for _, searchName := range searchNames {
		searchUpper := strings.ToUpper(searchName)
		for _, e := range entries {
			eUpper := strings.ToUpper(e.Name)
			if eUpper == searchUpper {
				return e, nil
			}
			// If stored entry has a schema prefix, try matching the short name
			if dotIdx := strings.Index(e.Name, "."); dotIdx >= 0 {
				shortUpper := strings.ToUpper(e.Name[dotIdx+1:])
				if shortUpper == searchUpper {
					return e, nil
				}
			}
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

	// Try original name first, then without schema prefix
	searchNames := []string{name}
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		shortName := name[dotIdx+1:]
		searchNames = []string{name, shortName}
	}

	for _, searchName := range searchNames {
		searchUpper := strings.ToUpper(searchName)
		for _, e := range entries {
			eUpper := strings.ToUpper(e.Name)
			if eUpper == searchUpper {
				return e, nil
			}
			// If stored entry has a schema prefix, try matching the short name
			if dotIdx := strings.Index(e.Name, "."); dotIdx >= 0 {
				shortUpper := strings.ToUpper(e.Name[dotIdx+1:])
				if shortUpper == searchUpper {
					return e, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no such trigger: %s", name)
}

// FindTriggersForTable returns all triggers for a given table.
// Matches by both qualified name (e.g., "aux.t4") and unqualified name ("t4").
func (m *Manager) FindTriggersForTable(tableName string) ([]*Entry, error) {
	entries, err := m.GetEntries(TypeTrigger)
	if err != nil {
		return nil, err
	}
	var result []*Entry
	upper := strings.ToUpper(tableName)
	for _, e := range entries {
		eUpper := strings.ToUpper(e.TblName)
		if eUpper == upper || strings.HasSuffix(eUpper, "."+upper) {
			result = append(result, e)
		}
	}
	return result, nil
}

// FindIndexesForTable returns all indexes associated with a given table.
// This is used by DROP TABLE to cascade-drop indexes (SQLite semantics:
// dropping a table removes all its indexes).
func (m *Manager) FindIndexesForTable(tableName string) ([]*Entry, error) {
	entries, err := m.GetEntries(TypeIndex)
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

	// Try original name first, then without schema prefix
	searchNames := []string{name}
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		shortName := name[dotIdx+1:]
		searchNames = []string{name, shortName}
	}

	for _, searchName := range searchNames {
		searchUpper := strings.ToUpper(searchName)
		for _, e := range entries {
			eUpper := strings.ToUpper(e.Name)
			if eUpper == searchUpper {
				return e, nil
			}
			// If stored entry has a schema prefix, try matching the short name
			if dotIdx := strings.Index(e.Name, "."); dotIdx >= 0 {
				shortUpper := strings.ToUpper(e.Name[dotIdx+1:])
				if shortUpper == searchUpper {
					return e, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("schema: index not found: %s", name)
}

// RenameEntry renames a schema entry (used by ALTER TABLE RENAME TO).
// If newSQL is non-empty, it is used directly as the entry's new SQL text.
// Otherwise, the SQL is computed using token-level rename via FindRenameTokens+ApplyRenames,
// with a fallback to simple string replacement.
func (m *Manager) RenameEntry(oldName, newName string) error {
	return m.RenameEntryWithSQL(oldName, newName, "")
}

// RenameEntryWithSQL renames a schema entry using the provided SQL text.
// If newSQL is empty, the SQL is computed using token-level rename
// via FindRenameTokens+ApplyRenames, with a fallback to simple string replacement.
func (m *Manager) RenameEntryWithSQL(oldName, newName, newSQL string) error {
	// Try the full name first, then the short name (schema prefix stripped)
	searchNames := []string{oldName}
	if dotIdx := strings.Index(oldName, "."); dotIdx >= 0 {
		shortName := oldName[dotIdx+1:]
		searchNames = []string{oldName, shortName}
	}

	entries, err := m.GetEntries("")
	if err != nil {
		return err
	}

	// Find the old entry
	var oldEntry *Entry
	for _, searchName := range searchNames {
		oldUpper := strings.ToUpper(searchName)
		for _, e := range entries {
			if strings.ToUpper(e.Name) == oldUpper {
				oldEntry = e
				break
			}
		}
		if oldEntry != nil {
			break
		}
	}
	if oldEntry == nil {
		return fmt.Errorf("no such table: %s", oldName)
	}

	// Determine the new SQL text
	finalSQL := newSQL
	if finalSQL == "" {
		// Use token-level rename
		ctx := &rename.RenameContext{
			OldName:   oldName,
			NewName:   newName,
			QuotedNew: `"` + newName + `"`,
			IsTable:   true,
		}
		ranges, rErr := rename.FindRenameTokens(oldEntry.SQL, ctx)
		if rErr == nil && len(ranges) > 0 {
			finalSQL = rename.ApplyRenames(oldEntry.SQL, ranges, `"`+newName+`"`)
		} else {
			// Fallback to simple string replacement
			finalSQL = strings.Replace(oldEntry.SQL, oldName, `"`+newName+`"`, 1)
		}
	}

	// Remove old entry
	if err := m.RemoveEntry(oldName); err != nil {
		return err
	}

	// Add new entry with updated name/tbl_name. Reuse the old entry's rowid
	// so the renamed object keeps its position in sqlite_schema (SQLite
	// rewrites the row in place; a fresh rowid would move it to the end).
	newEntry := &Entry{
		Type:     oldEntry.Type,
		Name:     newName,
		TblName:  newName,
		RootPage: oldEntry.RootPage,
		SQL:      finalSQL,
	}

	return m.addEntryWithRowID(newEntry, oldEntry.RowID)
}

// UpdateEntry replaces the SQL text of an existing schema entry WITHOUT
// moving its row in sqlite_schema. In SQLite, ALTER TABLE RENAME COLUMN
// rewrites the schema row in place, so the entry keeps its original position
// when sqlite_schema is scanned in rowid order. Replacements are matched by
// name. Returns an error if no matching entry is found.
func (m *Manager) UpdateEntry(name, newSQL string) error {
	return m.UpdateEntryFull(name, name, newSQL)
}

// UpdateEntryFull updates an existing schema entry in place, preserving its
// rowid and original type/rootpage. Used by ALTER TABLE operations that must
// not reorder sqlite_schema rows (e.g. RENAME COLUMN, DROP COLUMN).
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
		if len(rec.Values) >= 2 {
			return strings.EqualFold(toString(rec.Values[1]), searchName)
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
