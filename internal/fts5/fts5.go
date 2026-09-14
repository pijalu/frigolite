// Package fts5 implements the SQLite fts5 full-text search virtual table
// module (ext/fts5/): module lifecycle (fts5_main.c), configuration parsing
// (fts5_config.c), tokenizers (fts5_tokenize.c), shadow-table storage
// (fts5_storage.c), the inverted index (fts5_hash.c/fts5_index.c) and MATCH
// query evaluation (fts5_expr.c). Slices 1-3 of P6.FTS5 cover module +
// storage + tokenizers + index writes + single-term/phrase/column-filter/
// prefix MATCH; aux functions (bm25/highlight/snippet) come later.
//
// The shadow tables are real SQL tables with C's schemas (readable through
// SQL). The %_data block payload uses a Go-native encoding instead of C's
// segment format — a documented divergence in opaque bytes only (storage.go).
package fts5

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

// Module implements the vtab.Module interface for fts5 (fts5_main.c
// fts5InitModule). It owns the persistent per-table instances so every
// statement operates on the same live index.
type Module struct {
	db     vtab.Database
	tables map[string]*Table
}

// NewModule creates the fts5 module bound to a database handle (rtree-style
// Dependency Inversion: the module manages shadow tables through the
// abstraction, never the concrete engine).
func NewModule(db vtab.Database) *Module {
	return &Module{db: db, tables: make(map[string]*Table)}
}

// Create implements vtab.Module: the configuration is parsed and validated
// (xCreate's fts5ConfigParse); the instance binds its shadow tables at
// BindSchema, when the resolved table name is known.
func (m *Module) Create(args []string) (vtab.VirtualTable, error) {
	return m.Connect(args)
}

// Connect implements vtab.Module (xConnect shares xCreate's parsing). A
// tokenizer-resolution failure is DEFERRED to BindSchema, which decides the
// message: a CREATE reports C's specific text ("no such tokenizer: ..."), a
// reopen of an existing (corrupted-config) table reports the generic
// SQLITE_ERROR text the aux path pins (fts5aux.test 13.4).
func (m *Module) Connect(args []string) (vtab.VirtualTable, error) {
	cfg, err := ParseConfig("", args)
	if err != nil {
		return nil, err
	}
	tok, tokErr := tableTokenizer(cfg)
	return &vtabInstance{mod: m, cfg: cfg, tok: tok, tokErr: tokErr}, nil
}

// Bind completes a CREATE: the name-dependent checks run, the shadow family
// is created and the persistent Table instance is registered (fts5InitVtab's
// xCreate tail). Called with the resolved schema + table name. Re-binding a
// table that already exists is idempotent: the prepare-time plan instance
// re-runs BindSchema (VtabPlanInstance), which must not re-create the shadow
// family or replace the live table (rtree's fresh/re-bind distinction).
func (m *Module) Bind(dbName, tableName string, cfg *Config, tok Tokenizer) (*Table, error) {
	if err := checkTableName(tableName); err != nil {
		return nil, err
	}
	if len(cfg.Columns) == 0 {
		// C's declare_vtab builds "CREATE TABLE x()" for a zero-column table
		// and the failure surfaces as SQLite's generic vtab constructor error.
		return nil, fmt.Errorf("vtable constructor failed: %s", tableName)
	}
	if t, ok := m.tables[strings.ToLower(tableName)]; ok {
		// Re-bind (the prepare-time plan instance re-runs BindSchema):
		// return the live table without re-creating the shadow family.
		return t, nil
	}
	t := newTable(m.db, dbName, tableName, cfg, tok)
	// xConnect parity: when the shadow family already exists in the schema,
	// this Bind is a re-open of a persisted table (the prepare-time plan
	// instance / EQP / write-path resolutions all Bind) — load its state
	// instead of re-creating the family (whose seed would collide with the
	// persisted %_config rows).
	if t.familyExists() {
		m.tables[strings.ToLower(tableName)] = t
		if err := t.loadFromShadow(); err != nil {
			delete(m.tables, strings.ToLower(tableName))
			return nil, err
		}
		return t, nil
	}
	if err := t.createShadowTables(); err != nil {
		return nil, err
	}
	t.ix = NewInvertedIndex(len(cfg.Columns))
	m.tables[strings.ToLower(tableName)] = t
	return t, nil
}

// familyExists reports whether a table's shadow family is already in the
// schema (the %_data table is present for every fts5 configuration).
func (m *Module) familyExists(dbName, tableName string) bool {
	rows, err := m.db.ExecSQL(fmt.Sprintf(
		"SELECT 1 FROM %s WHERE type='table' AND name=%s",
		qual(dbName, "sqlite_schema"), sqlLiteral(tableName+"_data")))
	if err != nil {
		return false
	}
	return len(rows) > 0
}

// familyExists reports whether this table's shadow family is already in the
// schema (the %_data table is present for every fts5 configuration).
func (t *Table) familyExists() bool {
	rows, err := t.db.ExecSQL(fmt.Sprintf(
		"SELECT 1 FROM %s WHERE type='table' AND name=%s",
		qual(t.dbName, "sqlite_schema"), sqlLiteral(t.cfg.Name+"_data")))
	if err != nil {
		return false
	}
	return len(rows) > 0
}

// Load restores a persisted table at connection time (xConnect): the
// configuration is re-parsed from the stored CREATE VIRTUAL TABLE SQL and the
// index is rebuilt from the shadow tables. No DDL runs.
func (m *Module) Load(dbName, tableName string, args []string) (*Table, error) {
	if t, ok := m.tables[strings.ToLower(tableName)]; ok {
		return t, nil
	}
	cfg, err := ParseConfig(tableName, args)
	if err != nil {
		return nil, err
	}
	tok, err := tableTokenizer(cfg)
	if err != nil {
		return nil, err
	}
	t := newTable(m.db, dbName, tableName, cfg, tok)
	// Register BEFORE loadFromShadow: its schema reads re-enter
	// EnsureFTS5ForTable on this connection (schema-load recursion), and the
	// sentinel must be present for the re-entry to observe the table exists.
	// A failed load removes the sentinel.
	m.tables[strings.ToLower(tableName)] = t
	if err := t.loadFromShadow(); err != nil {
		delete(m.tables, strings.ToLower(tableName))
		return nil, err
	}
	return t, nil
}

// GetTable returns the persistent instance for a table name.
func (m *Module) GetTable(name string) (*Table, bool) {
	t, ok := m.tables[strings.ToLower(name)]
	return t, ok
}

// DropTable forgets a dropped table's instance (the shadow DDL runs through
// the DROP TABLE glue).
func (m *Module) DropTable(name string) {
	delete(m.tables, strings.ToLower(name))
}

// tableTokenizer resolves the table's tokenizer, defaulting to unicode61
// (fts5InitVtab loads the tokenizer named by the tokenize= directive; with no
// directive C's default is unicode61).
func tableTokenizer(cfg *Config) (Tokenizer, error) {
	spec := cfg.TokSpec
	if len(spec) == 0 {
		spec = []string{"unicode61"}
	}
	return NewTokenizer(spec)
}

// checkTableName rejects the reserved table name (fts5ConfigParse).
func checkTableName(name string) error {
	if strings.EqualFold(name, "rank") {
		return fmt.Errorf("reserved fts5 table name: %s", name)
	}
	return nil
}

// vtabInstance is the virtual-table instance handed to the generic vtab
// machinery (fts5_main.c Fts5Table). Scans and writes take the engine's
// dedicated fts5 paths, so Open serves no rows.
type vtabInstance struct {
	mod    *Module
	cfg    *Config
	tok    Tokenizer
	tokErr error // deferred tokenizer-resolution failure (surfaced at BindSchema)
}

// BestIndex implements vtab.VirtualTable (fts5BestIndexMethod accepts every
// plan; the engine's dedicated scan path does not use the output).
func (v *vtabInstance) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open implements vtab.VirtualTable (no rows: dedicated scan path).
func (v *vtabInstance) Open() (vtab.Cursor, error) { return emptyCursor{}, nil }

// emptyCursor is the inert cursor of a vtabInstance.
type emptyCursor struct{}

func (emptyCursor) Next() bool                      { return false }
func (emptyCursor) Column(int) (interface{}, error) { return nil, nil }
func (emptyCursor) Close() error                    { return nil }

// BindSchema implements vtab.SchemaBoundVTab: the resolved schema + table
// name complete the CREATE (name checks, shadow-table creation, persistent
// registration). An error aborts the owning CREATE statement.
func (v *vtabInstance) BindSchema(dbName, tableName string) error {
	if v.tokErr != nil {
		// A shadow family already in the schema means this bind is a REOPEN
		// of a table whose stored config names an unresolvable tokenizer:
		// C's xConnect failure carries no message there (SQLITE_ERROR).
		if v.mod.familyExists(dbName, tableName) {
			return fmt.Errorf("SQL logic error")
		}
		return v.tokErr
	}
	if err := checkTableName(tableName); err != nil {
		return err
	}
	_, err := v.mod.Bind(dbName, tableName, v.cfg, v.tok)
	return err
}

// Table is one fts5 virtual table instance: configuration, tokenizer,
// in-memory index and shadow-table IO (fts5FullTable + Fts5Storage + Fts5Index).
type Table struct {
	db     vtab.Database
	dbName string
	cfg    *Config
	tok    Tokenizer
	ix     *InvertedIndex
	// contentValues mirrors the %_content rows of normal-content tables.
	contentValues map[int64][]interface{}
	// version bumps on every index mutation, invalidating the match cache.
	version uint64
	cache   *matchCacheEntry
	// shadowDirty records pending %_data blob writes (flushed at the
	// statement boundary by FlushShadowIfDirty).
	shadowDirty bool
	// maxRowid tracks the largest allocated rowid for auto rowid allocation.
	maxRowid int64
}

// newTable builds a Table with its mirrors initialized.
func newTable(db vtab.Database, dbName, tableName string, cfg *Config, tok Tokenizer) *Table {
	cfg.Name = tableName
	return &Table{
		db:            db,
		dbName:        dbName,
		cfg:           cfg,
		tok:           tok,
		contentValues: make(map[int64][]interface{}),
	}
}

// NextRowid returns the next auto-assigned rowid (max existing + 1).
func (t *Table) NextRowid() int64 { return t.maxRowid + 1 }

// noteRowid extends the max-rowid watermark.
func (t *Table) noteRowid(rowid int64) {
	if rowid > t.maxRowid {
		t.maxRowid = rowid
	}
}

// Config returns the table's parsed configuration.
func (t *Table) Config() *Config { return t.cfg }

// Name returns the virtual table name.
func (t *Table) Name() string { return t.cfg.Name }

// ColumnNames returns the user column names in declared order.
func (t *Table) ColumnNames() []string { return t.cfg.Columns }

// HasDoc reports whether a rowid is indexed.
func (t *Table) HasDoc(rowid int64) bool { return t.ix.HasDoc(rowid) }

// ColumnIndex resolves a user column name to its index (-1 when unknown).
func (t *Table) ColumnIndex(name string) int {
	for i, c := range t.cfg.Columns {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// tokenizeValues tokenizes one document's values into per-column token
// streams (fts5StorageInsert: each value is coerced to text and tokenized;
// unindexed columns yield no tokens).
func (t *Table) tokenizeValues(values []interface{}) [][]string {
	cols := make([][]string, len(t.cfg.Columns))
	for i := range t.cfg.Columns {
		if t.cfg.Unindexed[i] {
			continue
		}
		var text string
		if i < len(values) && values[i] != nil {
			switch x := values[i].(type) {
			case string:
				text = x
			case []byte:
				text = string(x)
			default:
				text = fmt.Sprintf("%v", x)
			}
		}
		for _, tok := range t.tok.Tokenize(text) {
			cols[i] = append(cols[i], tok.Term)
		}
	}
	return cols
}

// Insert adds a document (fts5UpdateMethod's insert path + fts5StorageInsert).
func (t *Table) Insert(rowid int64, values []interface{}) error {
	cols := t.tokenizeValues(values)
	defer t.bumpVersion()
	t.noteRowid(rowid)
	t.ix.AddDoc(rowid, nil, cols)
	if t.cfg.EContent == ContentNormal {
		t.contentValues[rowid] = append([]interface{}(nil), values...)
	}
	if err := t.insertContentRow(rowid, values); err != nil {
		return err
	}
	if err := t.insertDocsizeRow(rowid); err != nil {
		return err
	}
	t.markShadowDirty()
	return nil
}

// Delete removes a document (fts5StorageDelete). It reports whether the
// rowid existed.
func (t *Table) Delete(rowid int64) (bool, error) {
	if !t.ix.RemoveDoc(rowid) {
		return false, nil
	}
	defer t.bumpVersion()
	delete(t.contentValues, rowid)
	if err := t.deleteContentRow(rowid); err != nil {
		return true, err
	}
	if err := t.deleteDocsizeRow(rowid); err != nil {
		return true, err
	}
	t.markShadowDirty()
	return true, nil
}

// markShadowDirty records that the in-memory index has diverged from the
// %_data id=11 blob (the pending-terms state; C flushes pending terms at
// sync points, i.e. statement ends).
func (t *Table) markShadowDirty() { t.shadowDirty = true }

// FlushShadowIfDirty persists the index blob when the index changed since
// the last flush (sqlite3Fts5StorageSync at the statement boundary).
func (t *Table) FlushShadowIfDirty() error {
	if !t.shadowDirty {
		return nil
	}
	t.shadowDirty = false
	return t.flushShadowIndex()
}

// DeleteAll clears the whole index (the 'delete-all' special command and a
// WHERE-less DELETE on a contentless table: fts5SpecialDelete).
func (t *Table) DeleteAll() error {
	defer t.bumpVersion()
	t.maxRowid = 0
	t.ix = NewInvertedIndex(len(t.cfg.Columns))
	t.contentValues = make(map[int64][]interface{})
	if t.cfg.EContent == ContentNormal || t.cfg.EContent == ContentUnindexed {
		if _, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s",
			qual(t.dbName, t.cfg.Name+"_content"))); err != nil {
			return err
		}
	}
	if t.cfg.ColumnSize {
		if _, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s",
			qual(t.dbName, t.cfg.Name+"_docsize"))); err != nil {
			return err
		}
	}
	return t.flushShadowIndex()
}

// SpecialCommand handles the INSERT INTO t1(t1, rank) VALUES('cmd', ...)
// directives (fts5UpdateMethod's special-insert path). handled is false for
// an unknown command.
func (t *Table) SpecialCommand(cmd string, args []interface{}) (bool, error) {
	switch strings.ToLower(cmd) {
	case "delete-all":
		if t.cfg.EContent == ContentNormal {
			return true, fmt.Errorf("'delete-all' may only be used with a " +
				"contentless or external content fts5 table")
		}
		return true, t.DeleteAll()
	case "delete":
		if t.cfg.ContentlessDelete {
			return true, fmt.Errorf("'delete' may not be used with a contentless_delete=1 table")
		}
		if t.cfg.Contentless() {
			return true, fmt.Errorf("cannot use the delete command on fts5 contentless tables")
		}
		// External-content (or normal) delete of one document by rowid; a
		// rowid absent from the index fails like C's checksum mismatch.
		if len(args) == 0 {
			return true, fmt.Errorf("database disk image is malformed")
		}
		rowid, ok := asInt64(args[0])
		if !ok || !t.ix.HasDoc(rowid) {
			return true, fmt.Errorf("database disk image is malformed")
		}
		_, err := t.Delete(rowid)
		return true, err
	case "rebuild":
		if t.cfg.Contentless() {
			return true, fmt.Errorf("'rebuild' cannot be used with a contentless fts5 table")
		}
		return true, t.rebuild()
	case "rank":
		// The rank function configuration: parsed and persisted in %_config
		// (fts5SpecialInsert's sqlite3Fts5ConfigSetValue('rank') path).
		spec := ""
		if len(args) > 0 {
			if s, ok := args[0].(string); ok {
				spec = s
			}
		}
		parsed, err := ParseRankSpec(spec)
		if err != nil {
			return true, err
		}
		t.cfg.Rank = *parsed
		return true, t.storeConfigValue("rank", spec)
	case "pgsz", "hashsize", "automerge", "usermerge", "crisismerge",
		"deletemerge", "secure-delete", "insttoken":
		// Integer-valued maintenance/config directives (fts5ConfigSetValue):
		// the value must be INTEGER-typed — C's
		// sqlite3_value_numeric_type(pVal)==SQLITE_INTEGER check, so REAL
		// values (66.67) and non-numeric text fail before any range check —
		// then range-checked and persisted in %_config (the raw value is
		// stored, like C's sqlite3Fts5StorageConfigValue).
		v, ok := configIntValue(argValue(args))
		if !ok || badConfigValue(strings.ToLower(cmd), v) {
			return true, errRankLogic()
		}
		return true, t.storeConfigValue(strings.ToLower(cmd), v)
	case "merge", "integrity-check", "optimize":
		// Index maintenance directives with no SQL-observable effect at this
		// storage granularity; integrity-check on a healthy index is a no-op.
		return true, nil
	}
	return false, nil
}

// argValue returns the first special-insert argument.
func argValue(args []interface{}) interface{} {
	if len(args) > 0 {
		return args[0]
	}
	return nil
}

// configIntValue coerces a special-insert argument the way C's
// sqlite3_value_numeric_type(pVal)==SQLITE_INTEGER gate does: INTEGER values
// pass, text that converts to an integer passes, and REAL values (66.67) or
// non-numeric text fail (fts5_config.c fts5ConfigSetValue).
func configIntValue(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// badConfigValue reports whether v falls outside the accepted range of the
// fts5ConfigSetValue directive (badkey → C's generic SQLITE_ERROR).
func badConfigValue(cmd string, v int64) bool {
	switch cmd {
	case "pgsz":
		return v < 32 || v > 64*1024
	case "hashsize":
		return v <= 0
	case "automerge":
		return v < 0 || v > 64
	case "usermerge":
		return v < 2 || v > 16
	case "crisismerge":
		return v < 0
	case "deletemerge", "secure-delete", "insttoken":
		return v < 0
	}
	return true
}

// rebuild re-indexes every external content row (fts5StorageRebuild).
func (t *Table) rebuild() error {
	defer t.bumpVersion()
	type doc struct {
		rowid  int64
		values []interface{}
	}
	var docs []doc
	if t.cfg.EContent == ContentExternal {
		rowids, values, err := t.scanExternal()
		if err != nil {
			return err
		}
		for i, rowid := range rowids {
			docs = append(docs, doc{rowid: rowid, values: values[i]})
		}
	} else {
		// Normal content re-reads the stored %_content mirror
		// (fts5StorageRebuild scans %_content for content= tables).
		for _, rowid := range t.ix.SortedRowids() {
			docs = append(docs, doc{rowid: rowid, values: t.contentValues[rowid]})
		}
	}
	t.ix = NewInvertedIndex(len(t.cfg.Columns))
	t.maxRowid = 0
	for _, d := range docs {
		t.ix.AddDoc(d.rowid, nil, t.tokenizeValues(d.values))
		t.noteRowid(d.rowid)
	}
	return t.flushShadowIndex()
}

// ScanDocs returns the documents a full scan visits in ascending rowid order
// with their stored values (fts5StorageScan). A contentless table without
// columnsize has no scan source and fails like C.
func (t *Table) ScanDocs() ([]int64, [][]interface{}, error) {
	if t.cfg.EContent == ContentExternal {
		return t.scanExternal()
	}
	if t.cfg.Contentless() && !t.cfg.ColumnSize {
		return nil, nil, fmt.Errorf("%s: table does not support scanning", t.cfg.Name)
	}
	rowids := t.ix.SortedRowids()
	values := make([][]interface{}, len(rowids))
	for i, rowid := range rowids {
		if t.cfg.EContent == ContentNormal {
			values[i] = t.contentValues[rowid]
		}
	}
	return rowids, values, nil
}

// DocValues returns one document's stored values in user-column order
// (fts5StorageColumn): the %_content mirror for normal content, a live read
// of the external content table, or NULLs for a contentless table.
func (t *Table) DocValues(rowid int64) ([]interface{}, error) {
	switch t.cfg.EContent {
	case ContentNormal:
		if v, ok := t.contentValues[rowid]; ok {
			return v, nil
		}
		return make([]interface{}, len(t.cfg.Columns)), nil
	case ContentExternal:
		v, err := t.readExternalValues(rowid)
		if err != nil {
			return nil, err
		}
		if v == nil {
			v = make([]interface{}, len(t.cfg.Columns))
		}
		return v, nil
	default:
		return nil, nil
	}
}

// SortedMatchRowids returns the union of rowids in the given set, ascending.
func (t *Table) SortedMatchRowids(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for rowid := range set {
		out = append(out, rowid)
	}
	sortRowids(out)
	return out
}

// Rename renames the table and its shadow family (fts5StorageRename).
func (t *Table) Rename(newName string) error {
	return t.renameShadowTables(newName)
}

// Drop removes the shadow family (fts5DestroyMethod).
func (t *Table) Drop() error { return t.dropShadowTables() }

// TableState is a statement-rollback snapshot of the table's in-memory state
// (the shadow tables themselves are covered by the pager snapshot).
type TableState struct {
	ix       *InvertedIndex
	content  map[int64][]interface{}
	maxRowid int64
}

// Snapshot captures the in-memory state.
func (t *Table) Snapshot() *TableState {
	return &TableState{ix: t.ix.Snapshot(), content: snapshotContent(t.contentValues), maxRowid: t.maxRowid}
}

// snapshotContent deep-copies the values mirror.
func snapshotContent(src map[int64][]interface{}) map[int64][]interface{} {
	out := make(map[int64][]interface{}, len(src))
	for k, v := range src {
		out[k] = append([]interface{}(nil), v...)
	}
	return out
}

// Restore rolls the in-memory state back to a snapshot.
func (t *Table) Restore(s *TableState) {
	t.ix = s.ix
	t.contentValues = s.content
	t.maxRowid = s.maxRowid
	t.bumpVersion()
}
