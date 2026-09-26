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

// SchemaDeclared implements vtab.SchemaDeclaredMarker: fts5's constructor
// always declares the schema (fts5ConfigParse + declare), even for an empty
// column list.
func (v *vtabInstance) SchemaDeclared() bool { return true }

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
	tok, tokErr := tableTokenizer(cfg)
	t := newTable(m.db, dbName, tableName, cfg, tok)
	t.tokErr = tokErr
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
		// A stored tokenize= spec naming an unresolvable tokenizer fails
		// xConnect with the constructor error; every use of the table
		// reports it (fts5tokenizer 10.2/10.3/10.5/10.6, oracle 3.51.0).
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
	// pendingSecureUpgrade records a secure delete made while the format
	// version is still 4; the engine applies the 'version'=5 write at the
	// flush point (xSavepoint / COMMIT) or drops it on rollback.
	pendingSecureUpgrade bool
	// maxRowid tracks the largest allocated rowid for auto rowid allocation.
	maxRowid int64
	// tokErr defers a failed tokenizer resolution on REOPEN: C constructs
	// the tokenizer lazily at first use (sqlite3Fts5Tokenize's
	// LoadTokenizer call), so the index still loads and fts5vocab still
	// reads a table whose stored tokenize= spec is unresolvable — but every
	// operation that tokenizes reports the constructor error
	// (fts5tokenizer 10.x).
	tokErr error

	// Segment/pending state (fts5Index): the structure record, the pending
	// (unflushed) document rowids with C's pending-hash byte accounting, and
	// the contentless-delete operation counter. Pending docs flush at sync
	// points (autocommit statement ends, COMMIT) or when the pending byte
	// estimate crosses 'hashsize' (fts5IndexBeginWrite's overflow flush).
	structRec          *StructRec
	nextSegid          int64
	pendingRowids      []int64
	pendingBytes       int64
	pendingTermState   map[string]*pendingTerm
	nContentlessDelete int64
	// docOrigins mirrors the %_docsize origin column of contentless_delete
	// tables (the origin value each deleted rowid tombstones against).
	docOrigins map[int64]uint64
	// dirtySegments flags segments whose persisted payload no longer
	// matches the in-memory index (plain deletes rewrite them at sync).
	dirtySegments map[*Segment]bool
	// writeActive counts in-flight write operations; a query arriving while
	// it is nonzero re-enters the table mid-write and fails with C's corrupt
	// error (fts5circref: triggers on shadow tables reading the table being
	// written).
	writeActive int
	// scanGuard counts in-flight external-content scans of this table (C's
	// pConfig->bLock, held while the %_content read statements prepare and
	// step): a query plan arriving while it is nonzero is a content-table
	// recursion and fails with C's "recursively defined fts5 content table"
	// (fts5_main.c fts5BestIndexMethod's bLock check).
	scanGuard int
}

// pendingTerm is one term's state in the pending-hash byte accounting
// mirror (the fields of C's Fts5HashEntry that nPendingData accumulates).
type pendingTerm struct {
	lastRowid int64
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
// unindexed columns yield no tokens). A table whose tokenizer failed to
// construct reports the deferred error here (sqlite3Fts5Tokenize's lazy
// LoadTokenizer).
func (t *Table) tokenizeValues(values []interface{}) ([][]string, error) {
	if t.tokErr != nil {
		return nil, t.tokErr
	}
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
	return cols, nil
}

// tokenizeFor tokenizes text with the table's tokenizer, yielding no tokens
// when the tokenizer failed to construct (the calling aux functions then
// produce empty results; the error itself surfaces on the DML/MATCH paths).
func (t *Table) tokenizeFor(text string) []Token {
	if t.tokErr != nil || t.tok == nil {
		return nil
	}
	return t.tok.Tokenize(text)
}

// Insert adds a document (fts5UpdateMethod's insert path + fts5StorageInsert).
func (t *Table) Insert(rowid int64, values []interface{}) error {
	defer t.beginWrite()()
	cols, err := t.tokenizeValues(values)
	if err != nil {
		return err
	}
	defer t.bumpVersion()
	t.noteRowid(rowid)
	t.ix.AddDoc(rowid, nil, cols)
	t.storeContentCopy(rowid, values)
	if err := t.insertContentRow(rowid, values); err != nil {
		return err
	}
	t.noteContentlessOrigin(rowid)
	if err := t.insertDocsizeRow(rowid); err != nil {
		return err
	}
	// The document joins the pending hash; the flush happens at the sync
	// point or when 'hashsize' overflows (sqlite3Fts5IndexBeginWrite).
	return t.AddPendingRow(rowid, cols)
}

// storeContentCopy mirrors the stored content subset for scans and
// DocValues: ContentNormal stores every column; UNINDEXED content stores
// only the UNINDEXED ones (fts5StorageInsert's content-table writes).
func (t *Table) storeContentCopy(rowid int64, values []interface{}) {
	if t.cfg.EContent != ContentNormal && t.cfg.EContent != ContentUnindexed {
		return
	}
	stored := make([]interface{}, len(values))
	copy(stored, values)
	if t.cfg.EContent == ContentUnindexed {
		for i := range stored {
			if i < len(t.cfg.Unindexed) && !t.cfg.Unindexed[i] {
				stored[i] = nil
			}
		}
	}
	t.contentValues[rowid] = stored
}

// noteContentlessOrigin records the origin value a contentless_delete
// docsize row carries for the document (sqlite3Fts5IndexGetOrigin: the
// origin the NEXT flush will assign its segment).
func (t *Table) noteContentlessOrigin(rowid int64) {
	if !t.cfg.ContentlessDelete {
		return
	}
	if t.docOrigins == nil {
		t.docOrigins = make(map[int64]uint64)
	}
	t.docOrigins[rowid] = t.structRec.NOriginCntr
}

// commandRebuild handles the 'rebuild' directive: contentless tables cannot
// re-read their documents (fts5UpdateMethod's rebuild branch).
func (t *Table) commandRebuild() (bool, error) {
	if t.cfg.Contentless() {
		return true, fmt.Errorf("'rebuild' may not be used with a contentless fts5 table")
	}
	return true, t.rebuild()
}

// Delete removes a document (fts5StorageDelete). It reports whether the
// rowid existed.
func (t *Table) Delete(rowid int64) (bool, error) {
	if !t.ix.RemoveDoc(rowid) {
		return false, nil
	}
	defer t.beginWrite()()
	defer t.bumpVersion()
	// A secure delete requests the one-time format upgrade (fts5_index.c
	// fts5FlushSecureDelete's REPLACE INTO %_config when
	// iVersion!=FTS5_CURRENT_VERSION_SECUREDELETE). The write itself is
	// deferred to the flush point — xSavepoint flush, or COMMIT for the
	// deletes still pending at commit — so a savepoint-scoped DELETE
	// (fts5version 2.1-2.3) never observes the upgrade before it is
	// committed. The engine applies/drops the request via
	// ApplySecureUpgrade/DiscardSecureUpgrade.
	if t.cfg.SecureDelete && t.cfg.FormatVersion != 5 {
		t.pendingSecureUpgrade = true
	}
	if t.cfg.ContentlessDelete {
		if err := t.contentlessDeleteTombstone(rowid); err != nil {
			return true, err
		}
	}
	if err := t.deleteShadowRows(rowid); err != nil {
		return true, err
	}
	if !t.cfg.ContentlessDelete {
		// A plain (or secure) delete rewrites the containing segments'
		// payloads at the next sync (fts5IndexDelete's in-place segment
		// edits; contentless_delete records tombstones instead).
		t.markSegmentsDirty(rowid)
	}
	return true, nil
}

// contentlessDeleteTombstone tombstones the rowid in every segment whose
// origin range covers the document's origin; the FIRST such segment (highest
// level first) also bumps nEntryTombstone
// (sqlite3Fts5IndexContentlessDelete).
func (t *Table) contentlessDeleteTombstone(rowid int64) error {
	origin := t.docOrigins[rowid]
	found := false
	for lvl := len(t.structRec.Levels) - 1; lvl >= 0 && !found; lvl-- {
		for i := len(t.structRec.Levels[lvl]) - 1; i >= 0; i-- {
			seg := t.structRec.Levels[lvl][i]
			if seg.Origin1 > origin || seg.Origin2 < origin {
				continue
			}
			if !found {
				seg.NEntryTombstone++
				found = true
			}
			seg.Tombs[rowid] = true
			if err := t.tombstoneAdd(seg, rowid); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteShadowRows drops the document's mirrors: the in-memory content copy,
// the %_content/%_docsize rows and the origin bookkeeping. An emptied table
// restarts auto rowid allocation at 1: the shadow %_content rowid table is
// empty, and OP_NewRowid (no AUTOINCREMENT) picks 1 for an empty b-tree.
func (t *Table) deleteShadowRows(rowid int64) error {
	delete(t.contentValues, rowid)
	if err := t.deleteContentRow(rowid); err != nil {
		return err
	}
	if err := t.deleteDocsizeRow(rowid); err != nil {
		return err
	}
	delete(t.docOrigins, rowid)
	if len(t.ix.SortedRowids()) == 0 {
		t.maxRowid = 0
	}
	return nil
}

// markSegmentsDirty flags every segment holding rowid whose payload needs a
// rewrite at the next sync.
func (t *Table) markSegmentsDirty(rowid int64) {
	if t.dirtySegments == nil {
		t.dirtySegments = make(map[*Segment]bool)
	}
	for _, seg := range t.structRec.allSegments() {
		for _, rid := range seg.Rowids {
			if rid == rowid {
				t.dirtySegments[seg] = true
				break
			}
		}
	}
}

// ApplySecureUpgrade persists the deferred secure-delete format upgrade:
// REPLACE 'version'=5 into %_config (fts5FlushSecureDelete's one-time
// REPLACE, run at the flush point). No-op when no secure delete is pending
// or the version already reflects the upgrade.
func (t *Table) ApplySecureUpgrade() error {
	if !t.pendingSecureUpgrade {
		return nil
	}
	t.pendingSecureUpgrade = false
	if !t.cfg.SecureDelete || t.cfg.FormatVersion == 5 {
		return nil
	}
	if err := t.storeConfigValue("version", 5); err != nil {
		return err
	}
	t.cfg.FormatVersion = 5
	return nil
}

// DiscardSecureUpgrade drops a pending secure-delete upgrade request: the
// deletes that requested it were rolled back (fts5RollbackToMethod /
// sqlite3Fts5StorageRollback discard the pending data).
func (t *Table) DiscardSecureUpgrade() { t.pendingSecureUpgrade = false }

// DeleteAll clears the whole index (the 'delete-all' special command and a
// WHERE-less DELETE on a contentless table: fts5SpecialDelete).
func (t *Table) DeleteAll() error {
	defer t.beginWrite()()
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
	// fts5StorageDeleteAll empties %_data and re-seeds the averages and
	// structure records (sqlite3Fts5IndexReinit).
	return t.resetIndexStructure()
}

// SpecialCommand handles the INSERT INTO t1(t1, rank) VALUES('cmd', ...)
// directives (fts5UpdateMethod's special-insert path). handled is false for
// an unknown command.
func (t *Table) SpecialCommand(cmd string, args []interface{}) (bool, error) {
	defer t.beginWrite()()
	switch strings.ToLower(cmd) {
	case "delete-all":
		return t.specialDeleteAll()
	case "delete":
		return true, t.specialDelete(args)
	case "rebuild":
		return t.commandRebuild()
	case "rank":
		return t.specialRank(args)
	case "pgsz", "hashsize", "automerge", "usermerge", "crisismerge",
		"deletemerge", "secure-delete", "insttoken":
		return t.specialConfigValue(strings.ToLower(cmd), args)
	case "merge":
		n := int64(-1)
		if len(args) > 0 {
			if v, ok := asInt64(args[0]); ok {
				n = v
			}
		}
		return true, t.mergeCommand(n)
	case "optimize":
		return true, t.optimizeCommand()
	case "integrity-check":
		// On a healthy index a no-op (the mirror model validates on load).
		return true, nil
	case "flush":
		// sqlite3Fts5FlushToDisk: write any pending in-memory index data to
		// the shadow tables now (the engine otherwise flushes at statement
		// boundaries).
		return true, t.FlushShadowIfDirty()
	}
	return false, nil
}

// specialDeleteAll applies the 'delete-all' command (fts5SpecialDelete's
// gate: only contentless or external-content tables may delete all).
func (t *Table) specialDeleteAll() (bool, error) {
	if t.cfg.EContent == ContentNormal {
		return true, fmt.Errorf("'delete-all' may only be used with a " +
			"contentless or external content fts5 table")
	}
	return true, t.DeleteAll()
}

// specialRank applies the 'rank' command: the rank function configuration is
// parsed and persisted in %_config (fts5SpecialInsert's
// sqlite3Fts5ConfigSetValue('rank') path).
func (t *Table) specialRank(args []interface{}) (bool, error) {
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
}

// specialConfigValue applies an integer-valued maintenance/config directive
// (fts5ConfigSetValue): the value must be INTEGER-typed — C's
// sqlite3_value_numeric_type(pVal)==SQLITE_INTEGER check, so REAL values
// (66.67) and non-numeric text fail before any range check — then
// range-checked and persisted in %_config (the raw value is stored, like C's
// sqlite3Fts5StorageConfigValue).
func (t *Table) specialConfigValue(cmd string, args []interface{}) (bool, error) {
	v, ok := configIntValue(argValue(args))
	if !ok || badConfigValue(cmd, v) {
		return true, errRankLogic()
	}
	// C keeps bSecureDelete in memory (fts5_config.c fts5ConfigSetValue);
	// the format version upgrade happens lazily on the first secure
	// delete.
	switch cmd {
	case "secure-delete":
		t.cfg.SecureDelete = v != 0
	case "pgsz":
		t.cfg.Pgsz = v
	case "hashsize":
		t.cfg.HashSize = v
	case "automerge":
		t.cfg.Automerge = v
	case "usermerge":
		t.cfg.Usermerge = v
	case "crisismerge":
		t.cfg.CrisisMerge = v
	case "deletemerge":
		t.cfg.DeleteMerge = v
	}
	return true, t.storeConfigValue(cmd, v)
}

// specialDelete implements the 'delete' special command (fts5SpecialDelete +
// fts5StorageDeleteFromIndex). The command is allowed on every content mode
// except contentless_delete=1 (fts5_main.c fts5UpdateMethod: only the
// bContentlessDelete gate precedes fts5SpecialDelete) — it removes the index
// entries for one rowid using the SUPPLIED values, so it works with no
// content table at all.
func (t *Table) specialDelete(args []interface{}) error {
	if t.cfg.ContentlessDelete {
		return fmt.Errorf("'delete' may not be used with a contentless_delete=1 table")
	}
	if len(args) == 0 {
		return fmt.Errorf("database disk image is malformed")
	}
	rowid, ok := asInt64(args[0])
	if !ok {
		// fts5SpecialDelete: a non-INTEGER rowid slot deletes nothing.
		return nil
	}
	// fts5StorageDeleteFromIndex subtracts the supplied values' token counts
	// from the column totals; a negative total, or a delete from a table
	// with no rows (p->nTotalRow<1), is FTS5_CORRUPT. Row existence is NOT
	// checked — deleting a rowid the index never saw is a silent no-op
	// (fts5secure4 1.1).
	supplied, err := t.tokenizeValues(args[1:])
	if err != nil {
		return err
	}
	if t.ix.NumDocs() < 1 {
		return fmt.Errorf("database disk image is malformed")
	}
	for i, toks := range supplied {
		if t.ix.ColTotal(i) < int64(len(toks)) {
			return fmt.Errorf("database disk image is malformed")
		}
	}
	if !t.ix.HasDoc(rowid) {
		return nil
	}
	_, err = t.Delete(rowid)
	return err
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
