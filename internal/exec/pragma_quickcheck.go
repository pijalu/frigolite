package exec

import (
	"encoding/binary"
	"fmt"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
	"strconv"
	"strings"
)

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
func (e *Engine) execQuickCheck(tableName string) *Result {
	limit := 0 // 0 = unlimited
	arg := strings.Trim(tableName, "'\"")
	if n, err := strconv.Atoi(arg); err == nil && n >= 0 {
		limit = n
		arg = ""
	}

	var rows [][]interface{}
	colName := "integrity_check"
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
	if arg != "" {
		// findTable re-hydrates the FTS table state on a reopened connection
		// (ensureFTSForTable), so the ftsTables lookup below sees it.
		entry, _, ferr := e.findTable(arg)
		if ferr == nil && entry != nil && entry.RootPage == 0 {
			if ftsTable, ok := e.ftsTables[arg]; ok && ftsTable != nil {
				res := e.RunFTSIntegrityCheck(arg)
				if res != nil && res.Error != nil {
					if strings.Contains(res.Error.Error(), "database disk image is malformed") {
						emit("malformed inverted index for FTS4 table main." + arg)
					} else {
						// SQLite maps the underlying error code to its error
						// string: SQLITE_ERROR → "SQL logic error"
						// (fts3.c fts3IntegrityMethod uses sqlite3_errstr).
						emit("unable to validate the inverted index for FTS4 table main." + arg + ": SQL logic error")
					}
					return &Result{Columns: []string{colName}, Rows: rows}
				}
				rows = append(rows, []interface{}{"ok"})
				return &Result{Columns: []string{colName}, Rows: rows}
			}
		}
	}
	// Structural pass first: SQLite aborts the integrity check with
		// SQLITE_CORRUPT when any reachable b-tree page is malformed.
		if !e.btreeStructureOK() {
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		if msg := e.checkFreelistCount(emit); msg != "" {
			emit(msg)
		}
		e.quickCheckTables(arg, emit)

	if len(rows) == 0 {
		rows = append(rows, []interface{}{"ok"})
	}
	return &Result{Columns: []string{colName}, Rows: rows}
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
	checkFTS := func(te *schema.Entry) {
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
	if arg == "" {
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
					checkFTS(te)
					continue
				}
				e.quickCheckTable(te, dbCtx, emit)
			}
		}
		return
	}
	te, dbCtx, err := e.findTable(arg)
	if err != nil {
		return
	}
	if te.RootPage == 0 {
		checkFTS(te)
		return
	}
	e.quickCheckTable(te, dbCtx, emit)
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
		row := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
		if e.quickCheckRow(te, colDefs, uniqIdx, row, rec.Values, seenKeys, emit) {
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
func (e *Engine) quickCheckRow(te *schema.Entry, colDefs []sql.ColumnDef, uniqIdx []uniqueIndexDef, row RowMap, values []interface{}, seenKeys map[string]int, emit func(string)) bool {
	e.quickCheckUnique(te, colDefs, uniqIdx, row, values, seenKeys, emit)
	e.quickCheckNotNull(te, colDefs, values, emit)
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
func (e *Engine) quickCheckNotNull(te *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, emit func(string)) {
	for i, cd := range colDefs {
		if cd.Generated != nil {
			continue
		}
		if (cd.NotNull || cd.PrimaryKey) && i < len(values) && values[i] == nil {
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
// should be M" (mirrors btree.c checkerWalkFreelist / btreeIntegrityCheckpoint).
// corrupt2.test 14.2/14.3/14.5: write "size=2" to header byte 36 while the
// chain still carries 3 free pages; the integrity_check must surface the
// mismatch.
func (e *Engine) checkFreelistCount(emit func(string)) string {
	if len(e.dbList) == 0 {
		return ""
	}
	ctx := e.dbList[0]
	if ctx == nil || ctx.Pager == nil {
		return ""
	}
	hdr := ctx.Pager.Header()
	if len(hdr) < 40 {
		return ""
	}
	trunk := binary.BigEndian.Uint32(hdr[32:36])
	headerCount := int(binary.BigEndian.Uint32(hdr[36:40]))
	if trunk == 0 || headerCount == 0 {
		return ""
	}
	actual := 0
	const maxIter = 100000
	seen := make(map[uint32]bool)
	for iter := 0; trunk != 0 && iter < maxIter; iter++ {
		if seen[trunk] {
			return "database disk image is malformed"
		}
		seen[trunk] = true
		actual++
		pg, err := ctx.Pager.ReadPage(trunk)
		if err != nil {
			return "database disk image is malformed"
		}
		coff := 0
		if trunk == 1 {
			coff = 100
		}
		data := pg.Data
		if coff+4 > len(data) {
			return "database disk image is malformed"
		}
		nextTrunk := binary.BigEndian.Uint32(data[coff : coff+4])
		pageSize := ctx.Pager.PageSize()
		for off := coff + 4; off+4 <= int(pageSize); off += 4 {
			leaf := binary.BigEndian.Uint32(data[off : off+4])
			if leaf == 0 {
				break
			}
			if seen[leaf] {
				return "database disk image is malformed"
			}
			seen[leaf] = true
			actual++
		}
		trunk = nextTrunk
	}
	if actual != headerCount {
		return fmt.Sprintf("*** in database main ***\nFreelist: size is %d but should be %d", headerCount, actual)
	}
	return ""
}
