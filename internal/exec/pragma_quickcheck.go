package exec

import (
	"encoding/binary"
	"fmt"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
	"strconv"
	"strings"
)

// firstRef records the b-tree / page / cell location of the FIRST
// reference to a page across all b-trees in a database. checkTreePage
// uses this to emit "2nd reference to page N" diagnostics that name
// the original reference (matching SQLite's checkTree semantics).
type firstRef struct {
	tree, page uint32
	cell       int
}

func (e *Engine) execPragmaForeignKeyList(tableName string) *Result {
	cols := []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}
	entry, _, err := e.findTable(tableName)
	if err != nil {
		return &Result{Columns: cols}
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	fks := e.constraints.TableFKConstraints(entry, colDefs)
	var rows [][]interface{}
	for id, fk := range fks {
		// For an implicit "REFERENCES t" the parent column is not named;
		// SQLite reports NULL in the "to" column.
		parentCols := fk.ParentCols
		for seq, childCol := range fk.ChildCols {
			to := ""
			if seq < len(parentCols) {
				to = parentCols[seq]
			}
			upd := fk.OnUpdate
			if upd == "" {
				upd = "NO ACTION"
			}
			del := fk.OnDelete
			if del == "" {
				del = "NO ACTION"
			}
			rows = append(rows, []interface{}{int64(id), int64(seq), fk.ParentRef, childCol, to, upd, del, "NONE"})
		}
	}
	return &Result{Columns: cols, Rows: rows}
}

// execQuickCheck implements PRAGMA quick_check / integrity_check. Without an
// argument it checks all tables; an integer argument limits the number of
// errors reported; a table-name argument restricts the check to one table.
// For each table row (in rowid order) it verifies NOT NULL columns, STRICT
// types, CHECK constraints, and UNIQUE index keys, reporting violations as
// separate rows (SQLite pragma.c integrityCheck):
//
//	"NULL value in T.C"            — NOT NULL column holds NULL
//	"non-unique entry in index X"  — UNIQUE index key repeats
//	"CHECK constraint failed in T" — stored row violates a CHECK
//
// The engine does not maintain secondary index btrees, so index uniqueness is
// verified by grouping the table rows by index key: a key repeated across
// multiple rows (with no NULL in a nullable key column) is a violation,
// reported once per duplicate row.
// unknownIndexCollation returns the first unregistered collation among the
// key collations of every schema index (optionally restricted to the indexes
// of one table), or "" when all resolve. integrity_check opens every index
// and resolves each key's collation at prepare time (pragma.c
// integrityCheck → build.c sqlite3LocateCollSeq), so an index whose keys use
// an unregistered collation fails the check outright.
func (e *Engine) unknownIndexCollation(tableFilter string) string {
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if tableFilter != "" && !strings.EqualFold(ent.TblName, tableFilter) {
				continue
			}
			if name := e.indexCollationError(ctx, ent); name != "" {
				return name
			}
		}
	}
	return ""
}

// indexCollationError returns the first unregistered key collation of one
// index entry, or "".
func (e *Engine) indexCollationError(ctx *DatabaseContext, ent *schema.Entry) string {
	colDefs := e.indexTableColumnDefs(ctx, ent.TblName)
	for _, name := range execdml.IndexKeyCollations(ent.SQL, colDefs) {
		if name != "" && !e.collationExists(name) {
			return name
		}
	}
	return ""
}

// indexTableColumnDefs parses the column defs of the table an index belongs
// to (nil when the table cannot be resolved).
func (e *Engine) indexTableColumnDefs(ctx *DatabaseContext, tblName string) []sql.ColumnDef {
	tbl, err := ctx.Schema.FindTable(tblName)
	if err != nil || tbl == nil {
		return nil
	}
	return e.ParseColumnDefs(tbl.Name, tbl.SQL)
}

// quickCheckPreempt handles degenerate images before the structural walk:
// a deserialized/corrupt image (bad magic) fails with SQLITE_NOTADB "file
// is not a database" (memdb1.test 510); an empty image (0 pages after `db
// deserialize {}`) is a valid empty database reporting ok (memdb1.test 400).
// Returns nil when no preemption applies.
func (e *Engine) quickCheckPreempt(colName string) *Result {
	ctx := e.GetDB("main")
	if ctx == nil || ctx.Pager == nil {
		return nil
	}
	if ctx.Pager.IsHeaderCorrupt() {
		return &Result{Error: fmt.Errorf("file is not a database")}
	}
	if ctx.Pager.NumPages() == 0 {
		return &Result{Columns: []string{colName}, Rows: [][]interface{}{{"ok"}}}
	}
	return nil
}

func (e *Engine) execQuickCheck(tableName string) *Result {
	limit, arg := quickCheckParseArg(tableName)

	var rows [][]interface{}
	colName := "integrity_check"
	if early := e.quickCheckPreempt(colName); early != nil {
		return early
	}
	// integrity_check opens every index and resolves each index key's
	// collation at prepare time (pragma.c integrityCheck → build.c
	// sqlite3LocateCollSeq): an index whose key collation is not registered
	// fails the check with "no such collation sequence: NAME"
	// (collate3-1.6.3/1.7.3/3.8). Tables whose declared collations appear in
	// no index keep passing (verified against SQLite 3.53).
	if unknown := e.unknownIndexCollation(arg); unknown != "" {
		return &Result{Error: fmt.Errorf("no such collation sequence: %s", unknown)}
	}
	emit := func(msg string) {
		if limit > 0 && len(rows) >= limit {
			return
		}
		rows = append(rows, []interface{}{msg})
	}
	// PRAGMA integrity_check(<fts-table>): run the FTS integrity check.
	// A clean FTS index emits "ok"; a drifted one reports "malformed inverted
	// index for FTS4 table main.<t>" (fts3.c sqlite3Fts3IntegrityCheck via
	// PRAGMA; fts4intck1/fts4check). A missing validation capability reports
	// "unable to validate the inverted index for FTS4 table main.<t>: ...".
	if e.quickCheckFTSResult(arg, emit) {
		return &Result{Columns: []string{colName}, Rows: rows}
	}
	// Structural pass first: SQLite aborts the integrity check with
	// SQLITE_CORRUPT when any reachable b-tree page is malformed.
	ok := e.btreeStructureOK()
	if !ok {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	// Multi-line per-page diagnostics (Tree N page M cell K: 2nd reference
	// to page X / Page Y: never used). Mirrors btree.c::checkTree /
	// checkTreePage. corrupt2-5.1 asserts the "Tree 2 page 2 cell 0:
	// 2nd reference to page 10 / Page 4: never used" diagnostic format.
	// btree.c gates the page-usage audit behind !bPartial: a TABLE-SCOPED
	// check (quick_check('t1')) skips it entirely — shared-root images from
	// writable_schema experiments report orphan pages only on the full scan
	// (strict2-1.2: scoped check is "ok", full check reports "Page 3/4:
	// never used").
	if arg == "" {
		e.checkTreePage(emit)
	}
	if msg := e.checkFreelistCount(emit); msg != "" {
		emit(msg)
	}
	// Index key shape vs the stored CREATE INDEX definition (corruptL-19.4:
	// an index whose SQL was narrowed through writable_schema while its
	// btree still holds the old-width keys). SQLite's integrity check reads
	// every index entry through the index's column count and aborts with
	// SQLITE_CORRUPT on a field-count mismatch.
	if err := e.checkIndexKeyShape(); err != nil {
		return &Result{Error: err}
	}
	e.quickCheckTables(arg, emit)

	if len(rows) == 0 {
		rows = append(rows, []interface{}{"ok"})
	}
	return &Result{Columns: []string{colName}, Rows: rows}
}

// quickCheckParseArg classifies the PRAGMA argument: a non-negative integer
// limits the number of reported errors (and clears the table filter);
// anything else names the table to check.
func quickCheckParseArg(tableName string) (limit int, arg string) {
	limit = 0 // 0 = unlimited
	arg = strings.Trim(tableName, "'\"")
	if n, err := strconv.Atoi(arg); err == nil && n >= 0 {
		limit = n
		arg = ""
	}
	return limit, arg
}

// quickCheckFTSResult runs the scoped integrity_check's FTS fast path for
// PRAGMA integrity_check(<fts-table>). It reports whether the argument was
// handled as an FTS table (the caller then returns the accumulated rows);
// false means the argument is not an FTS table and the structural walk
// continues. Findings are emitted via emit (including the clean "ok" row).
func (e *Engine) quickCheckFTSResult(arg string, emit func(string)) bool {
	if arg == "" {
		return false
	}
	// findTable re-hydrates the FTS table state on a reopened connection
	// (ensureFTSForTable), so the ftsTables lookup below sees it.
	entry, _, ferr := e.findTable(arg)
	if ferr != nil || entry == nil || entry.RootPage != 0 {
		return false
	}
	if ftsTable, ok := e.ftsTables[arg]; !ok || ftsTable == nil {
		return false
	}
	res := e.RunFTSIntegrityCheck(arg)
	if res == nil || res.Error == nil {
		emit("ok")
		return true
	}
	if strings.Contains(res.Error.Error(), "database disk image is malformed") {
		emit("malformed inverted index for FTS4 table main." + arg)
	} else {
		// SQLite maps the underlying error code to its error
		// string: SQLITE_ERROR → "SQL logic error"
		// (fts3.c fts3IntegrityMethod uses sqlite3_errstr).
		emit("unable to validate the inverted index for FTS4 table main." + arg + ": SQL logic error")
	}
	return true
}

// btreeStructureOK walks every database's table and index b-trees verifying
// that each reachable page is a readable b-tree page of a valid type.
// SQLite's integrity check aborts with SQLITE_CORRUPT ("database disk image
// is malformed") when any page fails this test — e.g. raw page corruption
// written through sqlite_dbpage (dbpage 3.x).
func (e *Engine) btreeStructureOK() bool {
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil {
			continue
		}
		if !btreeStructureOKCtx(ctx) {
			return false
		}
	}
	return true
}

// btreeStructureOKCtx verifies one database's table and index b-trees;
// false when any reachable page is unreadable or of an invalid type.
func btreeStructureOKCtx(ctx *DatabaseContext) bool {
	entries, err := ctx.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return false
	}
	idxEntries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return false
	}
	seen := make(map[uint32]bool)
	for _, te := range append(entries, idxEntries...) {
		if te.RootPage <= 1 {
			continue // virtual tables have no page; page 1 is walked once via the schema tree
		}
		if !walkBTreePages(ctx.Pager, te.RootPage, seen) {
			return false
		}
	}
	return true
}

// walkBTreePages visits a b-tree depth-first, validating each page's type
// byte. Interior pages recurse into every left-child pointer plus the
// rightmost pointer; overflow pages are never visited directly.
func walkBTreePages(pg *pager.Pager, root uint32, seen map[uint32]bool) bool {
	if seen[root] {
		return true // shared root (e.g. schema tree) or defensive cycle stop
	}
	seen[root] = true
	p, err := pg.ReadPage(root)
	if err != nil {
		return false
	}
	coff := 0
	if root == 1 {
		coff = 100 // database file header occupies the first 100 bytes
	}
	if p.Data[coff] == 0 {
		return false
	}
	switch p.Data[coff] {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		return true
	case storage.PageTypeInteriorTable, storage.PageTypeInteriorIndex:
	default:
		return false // zeroed or garbage page header
	}
	bp, err := storage.ParsePage(p.Data, int(pg.PageSize()), coff)
	if err != nil {
		return false
	}
	cellType := storage.CellTableInterior
	if p.Data[coff] == storage.PageTypeInteriorIndex {
		cellType = storage.CellIndexInterior
	}
	for i := 0; i < int(bp.CellCount); i++ {
		// Interior pages have a 12-byte header: the cell pointer array sits
		// right after the rightmost-pointer field (storage.CellPointer
		// assumes the leaf layout's 8-byte header).
		ptrOff := coff + 12 + i*2
		off := int(binary.BigEndian.Uint16(p.Data[ptrOff : ptrOff+2]))
		cell, derr := storage.DecodeCell(p.Data, off, cellType, int(pg.PageSize()))
		if derr != nil {
			return false
		}
		if !walkBTreePages(pg, cell.LeftPtr, seen) {
			return false
		}
	}
	return walkBTreePages(pg, bp.RightmostPtr, seen)
}

// isReservedStatName reports whether name is one of SQLite's reserved
// statistics tables (sqlite_stat1..4), which integrity_check never subjects to
// the FTS inverted-index cross-check even when the name is shadowed by a
// virtual table.
func isReservedStatName(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "SQLITE_STAT1", "SQLITE_STAT2", "SQLITE_STAT3", "SQLITE_STAT4":
		return true
	}
	return false
}

// quickCheckTables runs the integrity scan over all tables (arg=="") or a
// single named table, emitting findings via emit. For an FTS virtual table
// (or the unnamed scan's FTS tables) it runs the FTS integrity check and
// reports a drifted index as "malformed inverted index for FTS4 table
// main.<t>" (fts3.c fts3IntegrityMethod via PRAGMA integrity_check;
// fts4check 1.2.3).
func (e *Engine) quickCheckTables(arg string, emit func(string)) {
	if arg == "" {
		e.quickCheckAllTables(emit)
		return
	}
	te, dbCtx, err := e.findTable(arg)
	if err != nil {
		return
	}
	if te.RootPage == 0 {
		e.quickCheckFTSReport(te, emit)
		return
	}
	e.quickCheckTable(te, dbCtx, emit)
}

// quickCheckAllTables runs the integrity scan over every database's tables
// (the unnamed quick_check/integrity_check form).
func (e *Engine) quickCheckAllTables(emit func(string)) {
	for _, dbCtx := range e.databases {
		entries, err := dbCtx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, te := range entries {
			if isSchemaTable(te.Name) {
				continue
			}
			if te.RootPage == 0 {
				e.quickCheckFTSReport(te, emit)
				continue
			}
			e.quickCheckTable(te, dbCtx, emit)
		}
	}
}

// quickCheckFTSReport cross-checks one FTS table's inverted index against
// its content, and lets rtree-family tables report through the shared
// checker. Non-FTS, non-rtree tables are silently skipped.
func (e *Engine) quickCheckFTSReport(te *schema.Entry, emit func(string)) {
	if te.RootPage != 0 {
		return
	}
	// Reserved sqlite_statN names are statistics tables, never a legitimate
	// FTS integrity target. A hostile schema may shadow one with an fts5
	// virtual table (vtabK); fts5's xIntegrity then validates only its own
	// (consistent) shadow tables, so integrity_check reports "ok" — the
	// inverted-index-vs-content cross-check does not apply to a table whose
	// name collides with the reserved stats name.
	if isReservedStatName(te.Name) {
		return
	}
	ftsTable, ok := e.ftsTables[te.Name]
	if !ok || ftsTable == nil {
		// Not FTS: rtree-family tables report their problems through the
		// shared checker ("In RTree main.<name>:" + lines, sqlite3 parity
		// with integrity_check's aggregate report).
		if vtab.RTreeFamilyModuleOf(te.SQL) {
			if rep := vtab.RTreeIntegrityReport(e.Database(), te.Name); rep != "" {
				emit(rep)
			}
		}
		return
	}
	// fts5 tables use the %_data/%_idx storage layout, not the fts3/4
	// %_segdir/%_content model this cross-check implements. SQLite's fts5
	// xIntegrity (fts5StorageIntegrity) compares %_idx rowids against
	// %_data and reports "ok" for a healthy table (vtabK-170); running the
	// fts3/4 algorithm here would misread the fts5 layout as corrupt.
	if ftsTable.IsFTS5() {
		return
	}
	res := e.RunFTSIntegrityCheck(te.Name)
	if res != nil && res.Error != nil && strings.Contains(res.Error.Error(), "database disk image is malformed") {
		emit("malformed inverted index for FTS4 table main." + te.Name)
	}
}

// quickCheckTable scans a single table's rows and runs the per-row integrity
// checks.
func (e *Engine) quickCheckTable(te *schema.Entry, dbCtx *DatabaseContext, emit func(string)) {
	colDefs := e.parseColumnDefs(te.Name, te.SQL)
	uniqIdx := e.uniqueIndexColumns(te.Name)
	var seenKeys = map[string]int{}
	tree := e.tableBTreePg(dbCtx.Pager, te.Name, te.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		// WITHOUT ROWID rows live in an index btree whose records are stored
		// PK-first (index_xinfo iField layout, see execdml/wr_order.go); the
		// SELECT scan path permutes them back to declared order at decode time
		// (execquery.RemapWRRecordToDeclared). The integrity scan must do the
		// same or it misreads storage slot 0 as declared column 0 — e.g. for
		// t1(b UNIQUE, a INT PRIMARY KEY) the on-disk record [1, NULL] (a=1,
		// b=NULL) would be read as b=1, a=NULL and falsely report
		// "NULL value in t1.a" (upsert1-600/610). Rowid tables need no remap.
		if hasWithoutRowidKeyword(strings.ToUpper(te.SQL)) {
			e.selectEngine.RemapWRRecordToDeclared(rec, te.SQL, colDefs)
		}
		row := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
		if e.quickCheckRow(te, colDefs, uniqIdx, row, rec.Values, cell.RowID, seenKeys, emit) {
			break
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
}

// quickCheckRow runs the per-row integrity checks for execQuickCheck: UNIQUE
// index key grouping, NOT NULL, STRICT types, and CHECK constraints.
func (e *Engine) quickCheckRow(te *schema.Entry, colDefs []sql.ColumnDef, uniqIdx []uniqueIndexDef, row RowMap, values []interface{}, rowID int64, seenKeys map[string]int, emit func(string)) bool {
	e.quickCheckUnique(te, colDefs, uniqIdx, row, values, seenKeys, emit)
	e.quickCheckNotNull(te, colDefs, values, rowID, emit)
	e.quickCheckStrict(te, colDefs, values, emit)
	e.quickCheckConstraints(te, colDefs, row, emit)
	return false
}

// quickCheckUnique reports UNIQUE index key duplicates. A key with any NULL
// is unique UNLESS the key column is declared NOT NULL. The first occurrence
// of a key is fine; each subsequent occurrence is a violation.
func (e *Engine) quickCheckUnique(te *schema.Entry, colDefs []sql.ColumnDef, uniqIdx []uniqueIndexDef, row RowMap, values []interface{}, seenKeys map[string]int, emit func(string)) {
	rowKeys := e.quickCheckRowKeys(te, colDefs, uniqIdx, row, values)
	for _, kr := range rowKeys {
		if kr.hasNull && !kr.notNull {
			continue
		}
		keyName := kr.idxName + "\x00" + kr.key
		seenKeys[keyName]++
		if seenKeys[keyName] > 1 {
			emit(fmt.Sprintf("non-unique entry in index %s", kr.idxName))
		}
	}
}

// quickCheckNotNull reports NULL values in NOT NULL or PRIMARY KEY columns.
// INTEGER PRIMARY KEY columns of rowid tables are stored as NULL in the record
// (the value lives in the rowid); treat a stored NULL as the rowid for the
// NOT NULL check.
func (e *Engine) quickCheckNotNull(te *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64, emit func(string)) {
	for i, cd := range colDefs {
		if cd.Generated != nil {
			continue
		}
		if (cd.NotNull || cd.PrimaryKey) && i < len(values) && values[i] == nil {
			// IPK rowid-alias substitution: NULL in the IPK slot means rowid.
			if cd.PrimaryKey && !cd.PKDesc && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") {
				continue
			}
			emit(fmt.Sprintf("NULL value in %s.%s", te.Name, cd.Name))
		}
	}
}

// quickCheckStrict reports STRICT type violations for STRICT tables.
func (e *Engine) quickCheckStrict(te *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, emit func(string)) {
	if !hasStrictKeyword(strings.ToUpper(te.SQL)) {
		return
	}
	for i, cd := range colDefs {
		if cd.Generated != nil {
			continue
		}
		if i < len(values) {
			if err := checkStrictValueForQuickCheck(te.Name, cd.Name, cd.Type, values[i]); err != nil {
				emit(err.Error())
			}
		}
	}
}

// quickCheckConstraints reports failing CHECK constraints (column-level and
// table-level).
func (e *Engine) quickCheckConstraints(te *schema.Entry, colDefs []sql.ColumnDef, row RowMap, emit func(string)) {
	for _, cd := range colDefs {
		if cd.Check == nil {
			continue
		}
		if cv, err := e.evalExpr(cd.Check, row); err == nil && cv != nil && !execexpr.ToBool(cv) {
			emit(fmt.Sprintf("CHECK constraint failed in %s", te.Name))
		}
	}
	for _, tc := range e.tableConstraints(te.Name, te.SQL) {
		if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
			continue
		}
		if cv, err := e.evalExpr(tc.Expr, row); err == nil && cv != nil && !execexpr.ToBool(cv) {
			emit(fmt.Sprintf("CHECK constraint failed in %s", te.Name))
		}
	}
}

// quickCheckRowKeys records this row's index keys for uniqueness grouping.
// Rows that do not satisfy a partial index's WHERE clause are not in the
// index and must not be checked for uniqueness.
func (e *Engine) quickCheckRowKeys(te *schema.Entry, colDefs []sql.ColumnDef, uniqIdx []uniqueIndexDef, row RowMap, values []interface{}) []quickCheckKeyRec {
	var rowKeys []quickCheckKeyRec
	for _, def := range uniqIdx {
		if len(def.Cols) == 0 {
			continue
		}
		if def.Where != "" {
			if wv, werr := e.evalWhereForRow(def.Where, row); werr != nil || wv == nil || !execexpr.ToBool(wv) {
				continue
			}
		}
		key, hasNull, notNullCols := e.quickCheckIndexKeyForRow(def.Cols, values, colDefs, row)
		rowKeys = append(rowKeys, quickCheckKeyRec{idxName: def.Name, key: key, hasNull: hasNull, notNull: notNullCols})
	}
	return rowKeys
}

// quickCheckKeyRec carries a row's index-key grouping info for quick_check.
type quickCheckKeyRec struct {
	idxName string
	key     string
	hasNull bool
	notNull bool // all key columns declared NOT NULL
}

// quickCheckIndexKeyForRow builds the composite index-key string for a row
// and reports whether any key column value is NULL and whether all key
// columns are declared NOT NULL (used by integrity_check's uniqueness rule).
// Each key column may be a plain column name, a 1-based column position, or
// an expression (e.g. "substr(b,2,4) COLLATE rtrim", "abs(d)") — expression
// keys are evaluated against the row the same way index maintenance does, so
// integrity_check groups by the actual index key values.
func (e *Engine) quickCheckIndexKeyForRow(cols []string, values []interface{}, colDefs []sql.ColumnDef, row RowMap) (key string, hasNull bool, notNull bool) {
	idx := buildColumnIndex(colDefs)
	notNull = true
	var parts []string
	for _, cn := range cols {
		v, ok, colNotNull := e.quickCheckIndexKeyValue(cn, idx, values, colDefs, row)
		if !ok {
			hasNull = true
		}
		if !colNotNull {
			notNull = false
		}
		parts = append(parts, fmt.Sprintf("%v", execexpr.UnwrapCollatedValue(v)))
	}
	return strings.Join(parts, "\x00"), hasNull, notNull
}

// quickCheckIndexKeyValue resolves one index key column for a row: a plain
// column name or 1-based position reads the stored value; anything else is
// parsed as an expression (SELECT <expr>) and evaluated against the row map.
// The ok result is false when the value is NULL or cannot be computed.
func (e *Engine) quickCheckIndexKeyValue(cn string, idx map[string]int, values []interface{}, colDefs []sql.ColumnDef, row RowMap) (interface{}, bool, bool) {
	ci := quickCheckKeyColumnIndex(cn, idx, colDefs)
	if ci >= 0 {
		colNotNull := ci < len(colDefs) && (colDefs[ci].NotNull || colDefs[ci].PrimaryKey)
		var v interface{} = nil
		if ci < len(values) {
			v = values[ci]
		}
		if v == nil {
			return nil, false, colNotNull
		}
		return v, true, colNotNull
	}
	// Expression index key: evaluate SELECT <expr> against the row.
	expr := parseWhereExpr(cn)
	if expr == nil {
		return nil, false, false
	}
	v, err := e.evalExpr(expr, row)
	if err != nil || v == nil {
		return nil, false, false
	}
	return v, true, false
}

// quickCheckKeyColumnIndex resolves an index key column name or 1-based
// position to a column index, or -1 when cn is an expression.
func quickCheckKeyColumnIndex(cn string, idx map[string]int, colDefs []sql.ColumnDef) int {
	if n, err := strconv.Atoi(cn); err == nil && n >= 1 && n <= len(colDefs) {
		return n - 1
	}
	if i, ok := idx[cn]; ok {
		return i
	}
	return -1
}

// evalWhereForRow parses a WHERE predicate string (e.g. a partial index's
// WHERE clause) and evaluates it against a row map, returning the boolean
// result (nil for NULL).
func (e *Engine) evalWhereForRow(whereSQL string, row Row) (interface{}, error) {
	return e.evalExpr(parseWhereExpr(whereSQL), row)
}

// checkStrictValueForQuickCheck validates a value against a STRICT column type
// using SQLite's quick_check error format: "non-DECLARED value in table.column".
// The "non-X" is the DECLARED type name (sqlite3StdType[eCType-1]), not the
// actual value type. Allowed actual types follow pragma.c's aStdTypeMask:
//
//	ANY:     any type
//	BLOB:    BLOB only
//	INT:     INT only
//	INTEGER: INT only
//	REAL:    INT or REAL
//	TEXT:    TEXT only
//
// Unlike enforceStrictType, this does NOT apply affinity — it checks the raw
// stored value type (used for detecting corruption).
func checkStrictValueForQuickCheck(tableName, colName, declaredType string, v interface{}) error {
	if v == nil {
		return nil
	}
	upper := strings.ToUpper(strings.TrimSpace(declaredType))
	// The declared type in the error message uses the canonical STRICT name
	// (e.g. "INT", "INTEGER", "REAL", "TEXT", "BLOB"). sqlite3StdType is
	// {"ANY","BLOB","INT","INTEGER","REAL","TEXT"} indexed by eCType-1.
	if !strictDeclaredType(upper) {
		return nil
	}
	actualType := strictActualType(util.UnwrapColumnValue(v))
	if actualType == "" {
		return nil
	}
	if strictTypeAllowed(upper, actualType) {
		return nil
	}
	return fmt.Errorf("non-%s value in %s.%s", upper, tableName, colName)
}

// strictDeclaredType reports whether declaredType is one of the canonical
// STRICT type names.
func strictDeclaredType(declaredType string) bool {
	switch declaredType {
	case "INT", "INTEGER", "REAL", "TEXT", "BLOB", "ANY":
		return true
	}
	return false
}

// strictActualType maps a stored value to its storage-class name (INT, REAL,
// TEXT, BLOB), or "" for unsupported types.
func strictActualType(v interface{}) string {
	switch v.(type) {
	case int64:
		return "INT"
	case float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	}
	return ""
}

// strictTypeAllowed reports whether an actual storage class satisfies a
// STRICT declared type: ANY accepts everything, BLOB/INT/TEXT only their own
// class, and REAL accepts INT or REAL.
func strictTypeAllowed(declared, actual string) bool {
	switch declared {
	case "ANY":
		return true
	case "BLOB":
		return actual == "BLOB"
	case "INT", "INTEGER":
		return actual == "INT"
	case "REAL":
		return actual == "REAL" || actual == "INT"
	case "TEXT":
		return actual == "TEXT"
	}
	return true
}

// hasTempTables reports whether the TEMP schema has any tables.
func (e *Engine) hasTempTables() bool {
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && (strings.EqualFold(dbCtx.Name, "TEMP") || strings.EqualFold(dbCtx.Name, "TEMPORARY")) {
			entries, err := dbCtx.Schema.GetEntries(schema.TypeTable)
			if err == nil && len(entries) > 0 {
				return true
			}
		}
	}
	return false
}

// checkFreelistCount validates the on-disk freelist: counts pages reachable
// from the header-declared trunk chain and compares against the header-
// declared count. A mismatch is reported as "Freelist: size is N but
// should be M" (mirrors btree.c checkList's trailing "size is %u but
// should be %u"). corrupt2.test 14.2/14.3/14.5: write "size=2" to header
// byte 36 while the chain still carries 3 free pages; integrity_check
// must surface the mismatch.
//
// Per-problem messages mirror btree.c checkList + checkRef (run with
// zPfx="Freelist: ") exactly:
//
//	"Freelist: invalid page number N"              — checkRef: page 0 or beyond the file
//	"Freelist: 2nd reference to page N"            — checkRef: page revisited
//	"Freelist: failed to get page N"               — sqlite3PagerGet failed
//	"Freelist: freelist leaf count too big on page N" — k > usableSize/4-2
//
// Unlike SQLite's single accumulator row, each message is emitted as its
// own row; the TCL flatten comparison renders the two forms identically.
// The walk stops at the first invalid trunk (checkRef → break) but keeps
// scanning leaves after a bad leaf, exactly like the C loop.
func (e *Engine) checkFreelistCount(emit func(string)) string {
	ctx, iPage, headerCount, ok := e.freelistHeaderState()
	if !ok {
		return ""
	}
	numPages := ctx.Pager.NumPages()
	used := make(map[uint32]bool)
	consumed, errored := e.walkFreelistTrunks(ctx, iPage, numPages, used, emit)
	if consumed != headerCount && !errored {
		return fmt.Sprintf("*** in database main ***\nFreelist: size is %d but should be %d", consumed, headerCount)
	}
	return ""
}

// freelistHeaderState reads the header-declared freelist first-trunk page
// and page count. ok is false when there is nothing to verify: no database,
// no pager, a truncated header, or an all-zero freelist declaration.
func (e *Engine) freelistHeaderState() (ctx *DatabaseContext, firstTrunk, count uint32, ok bool) {
	if len(e.dbList) == 0 {
		return nil, 0, 0, false
	}
	ctx = e.dbList[0]
	if ctx == nil || ctx.Pager == nil {
		return nil, 0, 0, false
	}
	hdr := ctx.Pager.Header()
	if len(hdr) < 40 {
		return nil, 0, 0, false
	}
	firstTrunk = binary.BigEndian.Uint32(hdr[32:36])
	count = binary.BigEndian.Uint32(hdr[36:40])
	if firstTrunk == 0 && count == 0 {
		return nil, 0, 0, false
	}
	return ctx, firstTrunk, count, true
}

// walkFreelistTrunks walks the header-declared trunk chain, counting pages
// reachable from it (checkRef → break on the first invalid trunk reference
// but keep scanning leaves after a bad leaf, exactly like the C loop).
func (e *Engine) walkFreelistTrunks(ctx *DatabaseContext, iPage, numPages uint32, used map[uint32]bool, emit func(string)) (consumed uint32, errored bool) {
	for iter := 0; iPage != 0 && iter < 100000; iter++ {
		next, pages, errd, stop := e.scanFreelistTrunk(ctx, iPage, numPages, used, emit)
		consumed += pages
		if errd {
			errored = true
		}
		if stop {
			return consumed, true
		}
		iPage = next
	}
	return consumed, errored
}

// scanFreelistTrunk processes one freelist trunk page. stop reports a fatal
// condition that ends the walk (the trunk reference itself is unusable —
// btree.c checkRef → break); errored reports any problem found, including
// an oversized leaf count, after which the walk continues with the next
// trunk. pages counts the trunk page itself plus its reachable leaves.
func (e *Engine) scanFreelistTrunk(ctx *DatabaseContext, iPage, numPages uint32, used map[uint32]bool, emit func(string)) (nextTrunk, pages uint32, errored, stop bool) {
	// checkRef (btree.c:10633): out-of-range or duplicate page reference.
	if used[iPage] {
		emit(fmt.Sprintf("Freelist: 2nd reference to page %d", iPage))
		return 0, 0, true, true
	}
	if freelistRefCheck(iPage, numPages, emit) {
		return 0, 0, true, true
	}
	used[iPage] = true
	pages++
	pg, err := ctx.Pager.ReadPage(iPage)
	if err != nil {
		emit(fmt.Sprintf("Freelist: failed to get page %d", iPage))
		return 0, pages, true, true
	}
	data := pg.Data
	if len(data) < 8 {
		emit(fmt.Sprintf("Freelist: failed to get page %d", iPage))
		return 0, pages, true, true
	}
	// SQLite freelist trunk format (btree.c:10701):
	//   offset 0-3: next trunk page number
	//   offset 4-7: leaf count (4 bytes, not 2!)
	//   offset 8+: leaf page numbers (4 bytes each)
	nextTrunk = binary.BigEndian.Uint32(data[0:4])
	leafCount := binary.BigEndian.Uint32(data[4:8])
	// btree.c checkList: "freelist leaf count too big on page %u"
	// when k > usableSize/4 - 2. usableSize == pageSize (no reserved
	// bytes), matching maxTrunkLeaves + 2.
	if leafCount > uint32(ctx.Pager.PageSize())/4-2 {
		emit(fmt.Sprintf("Freelist: freelist leaf count too big on page %d", iPage))
		return nextTrunk, pages, true, false
	}
	leafPages, leafErrored := e.scanFreelistLeaves(data, leafCount, numPages, used, emit)
	return nextTrunk, pages + leafPages, leafErrored, false
}

// scanFreelistLeaves checks a trunk page's leaf entries. Per-leaf semantics
// mirror btree.c checkRef run per leaf: a revisited leaf is "2nd reference",
// a zero or out-of-range slot is "invalid page number N" (the C code does
// not skip zero slots); neither breaks the leaf loop.
func (e *Engine) scanFreelistLeaves(data []byte, leafCount, numPages uint32, used map[uint32]bool, emit func(string)) (pages uint32, errored bool) {
	for i := uint32(0); i < leafCount; i++ {
		off := 8 + i*4
		if int(off)+4 > len(data) {
			break
		}
		leaf := binary.BigEndian.Uint32(data[off : off+4])
		if used[leaf] && leaf != 0 && leaf <= numPages {
			emit(fmt.Sprintf("Freelist: 2nd reference to page %d", leaf))
			errored = true
			continue
		}
		if freelistRefCheck(leaf, numPages, emit) {
			errored = true
			continue
		}
		used[leaf] = true
		pages++
	}
	return pages, errored
}

// freelistRefCheck mirrors btree.c checkRef for the freelist walk: page 0
// or beyond the file emits "Freelist: invalid page number N" and reports
// true (a failed reference).
func freelistRefCheck(pgno, numPages uint32, emit func(string)) bool {
	if pgno == 0 || pgno > numPages {
		emit(fmt.Sprintf("Freelist: invalid page number %d", pgno))
		return true
	}
	return false
}

// checkIndexKeyShape verifies that each explicit index's b-tree key width
// matches its stored CREATE INDEX definition: a rowid-table index record
// holds index-column-count + 1 values (the trailing rowid). A mismatch means
// the schema's index SQL was rewritten (writable_schema) while the b-tree
// still stores the old-width keys — SQLite's integrity check trips
// SQLITE_CORRUPT ("database disk image is malformed") when it reads such an
// entry through the narrowed definition (corruptL-19.4, oracle-verified:
// index i1 redefined from an expression to (b,c,d) makes integrity_check
// fail while the underlying data is untouched).
func (e *Engine) checkIndexKeyShape() error {
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil || ctx.Schema == nil {
			continue
		}
		entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if mismatch, checked := e.indexKeyShapeEntry(ctx, ent); checked && mismatch {
				return fmt.Errorf("database disk image is malformed")
			}
		}
	}
	return nil
}

// indexKeyShapeEntry checks one explicit index's first b-tree key against
// its stored CREATE INDEX definition (rowid tables only: WITHOUT ROWID
// index records use the PK-first layout with no trailing rowid, so the
// width rule is not exact there). checked is false for entries the check
// does not apply to (raw SQL missing, virtual/empty root, unparseable
// definition, expression targets, unreadable or empty b-tree — the
// structural pass covers page damage).
func (e *Engine) indexKeyShapeEntry(ctx *DatabaseContext, ent *schema.Entry) (mismatch, checked bool) {
	if ent.SQL == "" || ent.RootPage <= 1 {
		return false, false
	}
	stmts, perr := parse.ParseSQL(ent.SQL)
	if perr != nil || len(stmts) == 0 {
		return false, false
	}
	ci, ok := stmts[0].(*sql.CreateIndexStmt)
	if !ok {
		return false, false
	}
	// Expression targets (memo->>'y') are dropped from the parsed
	// Columns list, so the parsed count can undercount the stored
	// key width. Only indexes whose every source target parsed as a
	// plain column have an exact width rule (corruptL-19.4's
	// narrowed (b, c, d) definition keeps the check; json102's
	// (a3, a1, memo->>'y') does not).
	if indexColumnTermCount(ent.SQL) != len(ci.Columns) {
		return false, false
	}
	if !rowidTableOf(ctx, ent.TblName) {
		return false, false
	}
	want := len(ci.Columns) + 1
	tree := btree.NewBTree(ctx.Pager, ent.RootPage, false)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, false
	}
	cell, err := cursor.ReadCell()
	if err != nil {
		// Empty index (cursor at end) or unreadable page: the
		// structural pass covers page damage; an empty index has no
		// entries to mismatch.
		return false, false
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return false, false
	}
	return len(rec.Values) != want, true
}

// rowidTableOf reports whether the named table is a rowid table (false when
// the table cannot be resolved or declares WITHOUT ROWID).
func rowidTableOf(ctx *DatabaseContext, tblName string) bool {
	te, err := ctx.Schema.FindTable(tblName)
	if err != nil || te == nil {
		return false
	}
	return !hasWithoutRowidKeyword(strings.ToUpper(te.SQL))
}

// indexColumnTermCount counts the top-level comma-separated terms inside
// the column list of a CREATE INDEX statement's raw SQL text: the terms
// between the '(' that follows the table name and the matching ')'.
// Returns 0 when the shape cannot be recognized. Used to detect expression
// targets, which the parser drops from CreateIndexStmt.Columns.
func indexColumnTermCount(sqlText string) int {
	open := strings.Index(sqlText, "(")
	if open < 0 {
		return 0
	}
	close := strings.LastIndex(sqlText, ")")
	if close <= open {
		return 0
	}
	inner := sqlText[open+1 : close]
	depth := 0
	terms := 1
	inQuote := byte(0)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inQuote = c
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				terms++
			}
		}
	}
	return terms
}
