// Package schema manages the database schema (sqlite_schema table).
package schema

import (
	"encoding/binary"

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
	TypeTable   SchemaType = "table"
	TypeIndex   SchemaType = "index"
	TypeView    SchemaType = "view"
	TypeTrigger SchemaType = "trigger"
)

// Entry represents a row in sqlite_schema.
type Entry struct {
	Type     SchemaType
	Name     string
	TblName  string
	RootPage uint32
	SQL      string
	Columns  []ColumnDef // cached column definitions (tables only)
	RowID    int64       // sqlite_schema rowid (set when read from the b-tree)
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

	// lastFileMod tracks the attached file's modification time so an external
	// connection's writes are detected and the pager cache is dropped before
	// the next schema read (schema-reload tests). Zero for in-memory dbs.
	lastFileMod  int64
	lastFileSize int64

	// trackExternalMod enables external-modification detection. Only ATTACHed
	// databases set this (their files may be written by other connections);
	// the main database's file changes come from the engine's own writes and
	// must not invalidate the pager cache.
	trackExternalMod bool

	// lastOwnCounter is the database change counter (header offset 24) that
	// this connection last wrote. checkExternalMod compares the file's
	// current counter against it to distinguish own commits (no cache drop)
	// from other connections' commits (drop and re-read). Zero means no
	// counter has been recorded yet.
	lastOwnCounter uint32

	// externalInvalidated is set by checkExternalMod when it drops the pager
	// cache (an external connection committed). The engine consumes it to
	// clear its own derived caches (tableCache, rowid sequences).
	externalInvalidated bool

	// entriesCache caches GetEntries results to avoid repeated schema scans.
	// Invalidated by AddEntry. Not thread-safe — callers must ensure single-
	// goroutine access, which holds for the current architecture (each DB has
	// its own Manager, and operations on a single DB are sequential).
	entriesCache map[SchemaType][]*Entry
	cacheValid   bool

	// headerValidated records that the pager header's freelist/root-page
	// fields have been checked against the page count (a corrupt image is
	// rejected once, on first schema read).
	headerValidated bool

	// checkedThisStmt records that checkExternalMod already ran once this
	// statement. The engine resets it at the start of each outermost
	// statement (Engine.execResetExternalChecks) so a connection observes
	// external commits made between statements while the FTS flush's repeated
	// schema reads inside one statement skip the redundant FileChangeCounter
	// Pread (the dominant cost of per-row FTS builds).
	checkedThisStmt bool
}

// NewManager creates a new schema manager.
func NewManager(pg *pager.Pager) *Manager {
	return &Manager{pager: pg}
}

// ResetStatementCheck clears the per-statement external-mod check flag so the
// next checkExternalMod re-reads the file counter (called by the engine at the
// start of each outermost statement).
func (m *Manager) ResetStatementCheck() {
	m.checkedThisStmt = false
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

	// Zero out the rest of the header (firstFree, cellCount, fragFree).
	for i := coff + 1; i < coff+5; i++ {
		pg.Data[i] = 0
	}
	// SQLite sets the cell-content pointer of an empty leaf to the page's
	// usable end (an empty page whose pointer is 0 looks like a crash-written
	// page — "free space corruption").
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(m.pager.PageSize()))
	pg.Data[coff+7] = 0 // fragmented free bytes

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

	tree := btree.NewSchemaBTree(m.pager)
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

	tree := btree.NewSchemaBTree(m.pager)
	err = tree.InsertCell(cell)

	return err
}

// SetTrackExternalMod enables or disables external-modification detection.
// ATTACHed databases enable it; the main database keeps it off so the engine's
// own file writes do not invalidate the pager cache.
func (m *Manager) SetTrackExternalMod(enabled bool) {
	m.trackExternalMod = enabled
}

// NoteOwnWrite records the database change counter that THIS connection just
// wrote to the file. checkExternalMod compares the file's current counter
// against it: a matching counter means the change was our own (no cache
// invalidation); a differing counter means another connection committed.
func (m *Manager) NoteOwnWrite(counter uint32) {
	m.lastOwnCounter = counter
}

// FileStamp returns the last-recorded file size+modtime stamp used to detect
// external modification.
func (m *Manager) FileStamp() int64 {
	return m.lastFileMod + m.lastFileSize
}

// CaptureFileStamp records the current file size+modtime as the baseline for
// external-modification detection. Called after attaching/opening a database
// so later external writes are detected.
func (m *Manager) CaptureFileStamp() {
	m.lastFileMod = 0
	m.lastFileSize = 0
	if c, ok := m.pager.FileChangeCounter(); ok {
		m.lastOwnCounter = c
	}
	m.checkExternalMod()
}

// CheckExternalMod is the exported form of checkExternalMod.
func (m *Manager) CheckExternalMod() {
	m.checkExternalMod()
}

// ConsumeExternalInvalidation reports and clears whether checkExternalMod
// invalidated the pager cache since the last call.
func (m *Manager) ConsumeExternalInvalidation() bool {
	v := m.externalInvalidated
	m.externalInvalidated = false
	return v
}

// checkExternalMod detects when the underlying database file was modified by
// an external connection (an attached database written from another engine)
// and drops the pager page cache so the schema btree is re-read from disk.
// It compares the file's change counter (header offset 24) against the counter
// this connection last wrote (NoteOwnWrite): own commits do not invalidate the
// cache; commits by other connections (a NEWER counter) do.
func (m *Manager) checkExternalMod() {
	if m.pager == nil || !m.trackExternalMod {
		return
	}
	// The engine resets checkedThisStmt at the start of each outermost
	// statement; within a statement no other connection can commit in this
	// engine's model, so a single external-mod check at statement start is
	// sufficient. Skipping the repeated Pread (FileChangeCounter) inside the
	// FTS segment flush / shadow-table writes is the dominant per-row FTS
	// build cost (fts3_build_db_2: 3 preads per flush over 20k flushes).
	if m.checkedThisStmt {
		return
	}
	// A write is in progress (unflushed dirty pages): do not invalidate the
	// cache, which would discard the in-flight changes. The external-mod
	// check only applies between statements, after the engine has flushed.
	if m.pager.HasDirtyPages() {
		return
	}
	m.checkedThisStmt = true
	counter, ok := m.pager.FileChangeCounter()
	if !ok {
		return
	}
	// Only a strictly NEWER file counter means another connection committed.
	// An equal or older counter is this connection's own unflushed write.
	// Note: lastOwnCounter may legitimately be 0 (a fresh database captured
	// at open has a zero change counter), so a bare counter comparison is
	// used — no "unset" sentinel is needed because CaptureFileStamp always
	// records the file's counter before tracking is enabled.
	if counter > m.lastOwnCounter {
		m.pager.InvalidateCache()
		m.lastOwnCounter = counter
		m.externalInvalidated = true
	}
}

// GetEntries returns all schema entries of the given type.
func (m *Manager) GetEntries(schemaType SchemaType) ([]*Entry, error) {
	// Detect external file modification (an attached database written by
	// another connection): drop the pager page cache so the schema btree is
	// re-read from disk.
	m.checkExternalMod()
	// Validate the database header once: a corrupt image (freelist or root
	// page beyond the file) must report "database disk image is malformed"
	// rather than silently serving a broken schema (altercorrupt-1.1). The
	// flag is set only on success so a corrupt image is rejected on every
	// schema read (a caller that swallows the first error must not see a
	// valid schema on the next read).
	if !m.headerValidated {
		if err := m.pager.ValidateHeader(); err != nil {
			return nil, err
		}
		m.headerValidated = true
	}
	// NOTE: the schema cache is intentionally disabled (always read fresh).
	// The schema btree lives on page 1 of each database; a stale cache here
	// diverges from the btree after DDL + pager restore cycles, causing
	// "table X already exists" / "no such table" errors in the FK torture
	// tests. The btree is small, so a fresh read per call is cheap.
	var entries []*Entry
	tree := btree.NewSchemaBTree(m.pager)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}

	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			// "cursor at end" is the normal EOF; any other error is a corrupt
			// schema btree (fts3corrupt4 14.2: a crash-written page 1 whose
			// sqlite_schema table is damaged). SQLite reports it as
			// "database disk image is malformed".
			if strings.Contains(err.Error(), "cursor at end") {
				break
			}
			return nil, fmt.Errorf("database disk image is malformed")
		}

		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return nil, fmt.Errorf("database disk image is malformed")
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

	return entries, nil
}

// FindTable returns the schema entry for a table.
func (m *Manager) FindTable(name string) (*Entry, error) {
	// Detect external file modification before reading the schema btree.
	m.checkExternalMod()
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
		if entry, ok := m.systemTableEntry(name, searchUpper); ok {
			return entry, nil
		}

		// Search in B-tree
		entries, err := m.GetEntries(TypeTable)
		if err != nil {
			// A corrupt database must report the corruption, not "no such
			// table" (altercorrupt: the header advertises a freelist/root
			// page beyond the file).
			if strings.Contains(err.Error(), "database disk image is malformed") {
				return nil, err
			}
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

// systemTableEntry returns a synthetic Entry for the SQLite system tables
// (sqlite_schema/sqlite_master, sqlite_sequence, pragma_*) when the name
// matches one, plus whether the name is a system table.
func (m *Manager) systemTableEntry(name, searchUpper string) (*Entry, bool) {
	// sqlite_schema is always on page 1 (bootstrap); sqlite_temp_master is an
	// alias for sqlite_master.
	if searchUpper == "SQLITE_SCHEMA" || searchUpper == "SQLITE_MASTER" ||
		searchUpper == "SQLITE_TEMP_MASTER" || searchUpper == "SQLITE_TEMP_SCHEMA" {
		return &Entry{
			Type:     TypeTable,
			Name:     name,
			TblName:  name,
			RootPage: 1,
			SQL:      fmt.Sprintf("CREATE TABLE %s (type TEXT,name TEXT,tbl_name TEXT,rootpage INTEGER,sql TEXT)", name),
		}, true
	}
	// sqlite_sequence is a system table for AUTOINCREMENT tracking. It only
	// exists as a real table when a user creates it (SQLite allows CREATE
	// TABLE sqlite_sequence via PRAGMA writable_schema); prefer a real schema
	// entry over the synthetic fallback.
	if searchUpper == "SQLITE_SEQUENCE" {
		return m.sequenceTableEntry(name), true
	}
	// Pragma table-valued functions
	if strings.HasPrefix(searchUpper, "PRAGMA_") {
		return &Entry{
			Type:     TypeTable,
			Name:     name,
			TblName:  name,
			RootPage: 1,
			SQL:      fmt.Sprintf("CREATE TABLE %s (name TEXT)", name),
		}, true
	}
	return nil, false
}

// sequenceTableEntry prefers a real sqlite_sequence schema entry over the
// synthetic fallback.
func (m *Manager) sequenceTableEntry(name string) *Entry {
	entries, gerr := m.GetEntries(TypeTable)
	if gerr == nil {
		for _, e := range entries {
			if strings.ToUpper(e.Name) == "SQLITE_SEQUENCE" || strings.ToUpper(e.TblName) == "SQLITE_SEQUENCE" {
				return e
			}
		}
	}
	return &Entry{
		Type:     TypeTable,
		Name:     name,
		TblName:  name,
		RootPage: 1,
		SQL:      fmt.Sprintf("CREATE TABLE %s (name TEXT,seq INTEGER)", name),
	}
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
// RenameTableEntryWithSQL renames only the schema entry of the given type
// (used by ALTER TABLE RENAME so a TRIGGER/VIEW named the same as the table
// is not renamed). Falls back to RenameEntryWithSQL when type is empty.
func (m *Manager) RenameTableEntryWithSQL(oldName, newName, newSQL string, wantType SchemaType) error {
	if wantType == "" {
		return m.RenameEntryWithSQL(oldName, newName, newSQL)
	}
	entries, err := m.GetEntries("")
	if err != nil {
		return err
	}
	shortName := oldName
	if dotIdx := strings.Index(oldName, "."); dotIdx >= 0 {
		shortName = oldName[dotIdx+1:]
	}
	var oldEntry *Entry
	for _, e := range entries {
		if strings.EqualFold(e.Name, oldName) || strings.EqualFold(e.Name, shortName) {
			if e.Type == wantType {
				oldEntry = e
				break
			}
		}
	}
	if oldEntry == nil {
		return fmt.Errorf("no such %s: %s", wantType, oldName)
	}
	// Reuse RenameEntryWithSQL's SQL-computation logic by renaming through a
	// temporary: simplest correct approach is to call the same code path via
	// the generic rename after removing the type ambiguity — instead, delegate
	// to a shared implementation below.
	return m.renameEntryWithSQL(oldEntry, oldName, newName, newSQL)
}

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
	return m.renameEntryWithSQL(oldEntry, oldName, newName, newSQL)
}

// renameEntryWithSQL performs the schema-entry rename for an already-resolved
// entry (shared by RenameEntryWithSQL and RenameTableEntryWithSQL).
func (m *Manager) renameEntryWithSQL(oldEntry *Entry, oldName, newName, newSQL string) error {

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

	// Remove old entry — by TYPE so a TRIGGER/VIEW named the same as the
	// renamed table survives (SQLite keeps these in separate namespaces).
	if err := m.RemoveEntryOfType(oldName, oldEntry.Type); err != nil {
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

// UpdateEntryRoot updates an existing schema entry's root page in place,
// preserving its rowid, type, tbl_name, and SQL. Used when a table b-tree
// split moves the root page so sqlite_schema stays correct across reopens.
func (m *Manager) UpdateEntryRoot(name string, newRoot uint32) error {
	m.cacheValid = false
	m.entriesCache = nil

	searchName := name
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		searchName = name[dotIdx+1:]
	}

	tree := btree.NewSchemaBTree(m.pager)

	var foundRowID int64 = -1
	var foundType, foundTbl, foundSQL interface{}
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
		foundSQL = rec.Values[4]
		return true
	}); err != nil {
		return err
	}
	if foundRowID < 0 {
		return fmt.Errorf("no such table: %s", name)
	}

	values := []interface{}{foundType, name, foundTbl, int64(newRoot), foundSQL}
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

// UpdateEntryFull updates an existing schema entry in place, preserving its
// rowid and original type/rootpage. Used by ALTER TABLE operations that must
// not reorder sqlite_schema rows (e.g. RENAME COLUMN, DROP COLUMN).
