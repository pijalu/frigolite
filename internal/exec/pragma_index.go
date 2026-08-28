package exec

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"strconv"
	"strings"
)

func (e *Engine) execPragmaLockStatus() *Result {
	var rows [][]interface{}
	tempHasTables := e.hasTempTables()
	for _, dbCtx := range e.dbList {
		if strings.EqualFold(dbCtx.Name, "TEMP") || strings.EqualFold(dbCtx.Name, "TEMPORARY") {
			if !tempHasTables {
				rows = append(rows, []interface{}{dbCtx.Name, "closed"})
			} else {
				rows = append(rows, []interface{}{dbCtx.Name, "unknown"})
			}
			continue
		}
		rows = append(rows, []interface{}{dbCtx.Name, e.lockStatusFor(dbCtx)})
	}
	return &Result{Columns: []string{"database", "status"}, Rows: rows}
}

// assignPragmaEncoding sets the database text encoding, persisting it to the
// header unless the database already has a different non-default encoding.
func (e *Engine) assignPragmaEncoding(ctx *DatabaseContext, value string) *Result {
	var encNum uint32
	switch strings.ToUpper(value) {
	case "UTF-8", "UTF8":
		e.encoding = "UTF-8"
		encNum = 1
	case "UTF-16LE", "UTF16LE", "UTF-16", "UTF16":
		e.encoding = "UTF-16le"
		encNum = 2
	case "UTF-16BE", "UTF16BE":
		e.encoding = "UTF-16be"
		encNum = 3
	default:
		return &Result{Error: fmt.Errorf("unsupported encoding: %s", value)}
	}
	if dh := e.headerFor(ctx); dh != nil && dh.TextEncoding != 0 && dh.TextEncoding != encNum && !e.schemaIsEmpty(ctx) {
		e.encoding = encodingName(dh.TextEncoding)
	} else if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
		h.TextEncoding = encNum
	}); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// autoindexDef describes one implicit autoindex of a WITHOUT ROWID table.
type autoindexDef struct {
	name   string
	cols   []string
	origin string // "pk" or "u"
}

// execPragmaIndexList implements PRAGMA index_list(table). Columns:
// (seq, name, unique, origin, partial). Explicit indexes are listed first in
// creation order; implicit PRIMARY KEY/UNIQUE autoindexes of a WITHOUT ROWID
// table follow in reverse order, matching SQLite.
func (e *Engine) execPragmaIndexList(arg string) *Result {
	cols := []string{"seq", "name", "unique", "origin", "partial"}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return &Result{Columns: cols}
	}
	tableEntry, ctx, err := e.findTable(arg)
	if err != nil {
		return &Result{Columns: cols} // unknown table → zero rows
	}
	var rows [][]interface{}
	seq := 0

	// Explicit indexes on the table.
	indexes, _ := ctx.Schema.FindIndexesForTable(tableEntry.Name)
	for _, idx := range indexes {
		unique := int64(0)
		if uniqueIndexColsRe.MatchString(idx.SQL) {
			unique = 1
		}
		partial := int64(0)
		if indexWhereRe.MatchString(idx.SQL) {
			partial = 1
		}
		rows = append(rows, []interface{}{int64(seq), idx.Name, unique, "c", partial})
		seq++
	}

	// Implicit autoindexes for WITHOUT ROWID tables, in reverse creation order.
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		defs := e.withoutRowidAutoindexes(tableEntry.Name, tableEntry)
		for i := len(defs) - 1; i >= 0; i-- {
			rows = append(rows, []interface{}{int64(seq), defs[i].name, int64(1), defs[i].origin, int64(0)})
			seq++
		}
	}
	return &Result{Columns: cols, Rows: rows}
}

// withoutRowidAutoindexes computes the implicit UNIQUE/PRIMARY KEY autoindexes
// of a WITHOUT ROWID table and names them sqlite_autoindex_<table>_<N>.
// Autoindexes are numbered sequentially in creation order: column-level UNIQUE
// and PRIMARY KEY constraints first (in column order), then table-level UNIQUE
// and PRIMARY KEY constraints (in declaration order). An index whose column
// set already exists is merged into that existing index; a PRIMARY KEY wins
// the "pk" origin.
func (e *Engine) withoutRowidAutoindexes(tableName string, tableEntry *schema.Entry) []autoindexDef {
	colDefs := e.parseColumnDefs(tableName, tableEntry.SQL)
	colIndex := buildColumnIndex(colDefs)
	defs := columnAutoindexes(colDefs)
	for _, tc := range e.tableConstraints(tableName, tableEntry.SQL) {
		defs = tableConstraintAutoindex(defs, tc, colIndex, colDefs)
	}
	return nameAutoindexes(defs, tableName)
}

// columnAutoindexes collects column-level UNIQUE/PRIMARY KEY autoindexes, in
// column order.
func columnAutoindexes(colDefs []sql.ColumnDef) []autoindexDef {
	var defs []autoindexDef
	for _, cd := range colDefs {
		if cd.Unique {
			defs = addAutoindex(defs, []string{cd.Name}, "u")
		}
		if cd.PrimaryKey {
			defs = addAutoindex(defs, []string{cd.Name}, "pk")
		}
	}
	return defs
}

// tableConstraintAutoindex adds a table-level UNIQUE/PRIMARY KEY constraint's
// autoindex.
func tableConstraintAutoindex(defs []autoindexDef, tc sql.TableConstraint, colIndex map[string]int, colDefs []sql.ColumnDef) []autoindexDef {
	switch tc.Type {
	case sql.ConstraintUnique:
		return addAutoindex(defs, constraintColumnNames(tc, colIndex, colDefs), "u")
	case sql.ConstraintPrimaryKey:
		return addAutoindex(defs, constraintColumnNames(tc, colIndex, colDefs), "pk")
	}
	return defs
}

// addAutoindex appends an autoindex, merging into an existing one with the
// same column set (a PRIMARY KEY wins the "pk" origin).
func addAutoindex(defs []autoindexDef, cols []string, origin string) []autoindexDef {
	for i := range defs {
		if sameColumnSet(defs[i].cols, cols) {
			if origin == "pk" {
				defs[i].origin = "pk"
			}
			return defs
		}
	}
	return append(defs, autoindexDef{cols: cols, origin: origin})
}

// nameAutoindexes names the autoindexes sqlite_autoindex_<table>_<N>.
func nameAutoindexes(defs []autoindexDef, tableName string) []autoindexDef {
	for i := range defs {
		defs[i].name = fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, i+1)
	}
	return defs
}

// sameColumnSet reports whether two lists name the same columns in the same
// order (case-insensitively).
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// constraintColumnNames resolves a table-level UNIQUE/PRIMARY KEY constraint's
// indexed columns to their names, honoring integer column positions.
func constraintColumnNames(tc sql.TableConstraint, colIndex map[string]int, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, ic := range tc.Columns {
		if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
			names = append(names, colDefs[n-1].Name)
			continue
		}
		if idx, ok := colIndex[ic.Name]; ok {
			names = append(names, colDefs[idx].Name)
		} else {
			names = append(names, ic.Name)
		}
	}
	return names
}

// indexPragmaColumn is aliased from execquery (see engine.go).

// execPragmaIndexInfo implements PRAGMA index_info(name) and
// PRAGMA index_xinfo(name). The argument may name an explicit index or, for a
// WITHOUT ROWID table, the table itself (its implicit PRIMARY KEY index).
// Mirrors SQLite's output:
//
//	index_info:  (seqno, cid, name)
//	index_xinfo: (seqno, cid, name, desc, coll, key)
func (e *Engine) execPragmaIndexInfo(arg string, xinfo bool) *Result {
	cols := []string{"seqno", "cid", "name"}
	if xinfo {
		cols = []string{"seqno", "cid", "name", "desc", "coll", "key"}
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return &Result{Columns: cols}
	}

	columns := e.indexInfoColumns(arg, xinfo)
	if columns == nil {
		// Unknown index/table: SQLite returns zero rows.
		return &Result{Columns: cols}
	}
	return &Result{Columns: cols, Rows: indexInfoRows(columns, xinfo)}
}

// indexInfoColumns resolves the index columns for index_info/index_xinfo: a
// named index, a WITHOUT ROWID table's implicit PRIMARY KEY index, or nil when
// the argument names neither.
func (e *Engine) indexInfoColumns(arg string, xinfo bool) []indexPragmaColumn {
	// 1. Named index.
	if idxEntry, ctx, err := e.findIndex(arg); err == nil {
		var tableEntry *schema.Entry
		var colDefs []sql.ColumnDef
		if te, _, terr := e.findTable(idxEntry.TblName); terr == nil {
			tableEntry = te
			colDefs = e.parseColumnDefs(te.Name, te.SQL)
		}
		return e.indexColumnsFromSQL(idxEntry.SQL, ctx, tableEntry, colDefs)
	}
	// 2. WITHOUT ROWID table name: implicit PRIMARY KEY index.
	if tableEntry, _, terr := e.findTable(arg); terr == nil && hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
		return e.withoutRowidPKColumns(arg, tableEntry, colDefs, xinfo)
	}
	return nil
}

// indexInfoRows renders index columns as result rows for index_info/xinfo.
func indexInfoRows(columns []indexPragmaColumn, xinfo bool) [][]interface{} {
	rows := make([][]interface{}, 0, len(columns))
	for i, c := range columns {
		if !xinfo && c.Rowid {
			continue // index_info omits the trailing rowid column
		}
		if xinfo {
			var name interface{}
			if c.Name != "" {
				name = c.Name
			}
			coll := c.Coll
			if coll == "" {
				coll = "BINARY"
			}
			rows = append(rows, []interface{}{int64(i), c.Cid, name, int64(execexpr.BoolToInt(c.Desc)), coll, c.Key})
		} else {
			rows = append(rows, []interface{}{int64(i), c.Cid, c.Name})
		}
	}
	return rows
}

// indexColumnsFromSQL resolves an explicit index's columns from its CREATE
// INDEX SQL: names and DESC flags from the AST, collations from the table
// column definitions (or an explicit COLLATE in the index column list).
func (e *Engine) indexColumnsFromSQL(sqlStr string, ctx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef) []indexPragmaColumn {
	stmts, perr := parse.ParseSQL(sqlStr)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ci, ok := stmts[0].(*sql.CreateIndexStmt)
	if !ok {
		return nil
	}
	explicitColls := parseIndexColumnCollations(sqlStr)
	colIndex := buildColumnIndex(colDefs)
	var out []indexPragmaColumn
	for i, ic := range ci.Columns {
		out = append(out, indexColumnFromItem(ic, i, colDefs, colIndex, explicitColls))
	}
	if tableEntry != nil && !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		// Rowid tables store a trailing rowid in every index record.
		out = append(out, indexPragmaColumn{Cid: -1, Key: 0, Rowid: true})
	}
	return out
}

// indexColumnFromItem resolves one CREATE INDEX column item to an
// indexPragmaColumn: name, DESC flag, collation from the table column (or an
// explicit COLLATE in the index column list).
func indexColumnFromItem(ic sql.IndexColumn, i int, colDefs []sql.ColumnDef, colIndex map[string]int, explicitColls []string) indexPragmaColumn {
	cid := int64(-1)
	coll := ""
	if idx := resolveColumnIndex(ic.Name, colDefs, colIndex); idx >= 0 {
		cid = int64(idx)
		coll = colDefs[idx].Collate
	}
	if i < len(explicitColls) && explicitColls[i] != "" {
		coll = explicitColls[i]
	}
	return indexPragmaColumn{Name: ic.Name, Desc: ic.Desc, Coll: coll, Cid: cid, Key: 1}
}

// withoutRowidPKColumns builds the implicit PRIMARY KEY index columns of a
// WITHOUT ROWID table. Key columns come from the PRIMARY KEY constraint; the
// remaining table columns appear as payload (key=0) columns in index_xinfo.
func (e *Engine) withoutRowidPKColumns(tableName string, tableEntry *schema.Entry, colDefs []sql.ColumnDef, xinfo bool) []indexPragmaColumn {
	colIndex := buildColumnIndex(colDefs)
	inPK := make(map[int]bool)
	out := pkColumnLevelColumns(colDefs, inPK)
	for _, tc := range e.tableConstraints(tableName, tableEntry.SQL) {
		out = pkTableLevelColumns(out, tc, colDefs, colIndex, inPK)
	}
	if xinfo {
		out = pkPayloadColumns(out, colDefs, inPK)
	}
	return out
}

// pkColumnLevelColumns collects column-level PRIMARY KEY constraints (e.g.
// "b PRIMARY KEY") as single-column key entries, in column order.
func pkColumnLevelColumns(colDefs []sql.ColumnDef, inPK map[int]bool) []indexPragmaColumn {
	var out []indexPragmaColumn
	for i, cd := range colDefs {
		if !cd.PrimaryKey || inPK[i] {
			continue
		}
		inPK[i] = true
		out = append(out, indexPragmaColumn{Name: cd.Name, Cid: int64(i), Coll: cd.Collate, Key: 1})
	}
	return out
}

// pkTableLevelColumns appends a table-level PRIMARY KEY constraint's columns
// as key entries.
func pkTableLevelColumns(out []indexPragmaColumn, tc sql.TableConstraint, colDefs []sql.ColumnDef, colIndex map[string]int, inPK map[int]bool) []indexPragmaColumn {
	if tc.Type != sql.ConstraintPrimaryKey {
		return out
	}
	for _, ic := range tc.Columns {
		idx := resolveColumnIndex(ic.Name, colDefs, colIndex)
		if idx < 0 {
			continue
		}
		inPK[idx] = true
		coll := ic.Collate
		if coll == "" {
			coll = colDefs[idx].Collate
		}
		out = append(out, indexPragmaColumn{Name: colDefs[idx].Name, Desc: ic.Desc, Coll: coll, Cid: int64(idx), Key: 1})
	}
	return out
}

// pkPayloadColumns appends the remaining (non-PK) columns as payload (key=0)
// columns for index_xinfo.
func pkPayloadColumns(out []indexPragmaColumn, colDefs []sql.ColumnDef, inPK map[int]bool) []indexPragmaColumn {
	for i, cd := range colDefs {
		if inPK[i] {
			continue
		}
		out = append(out, indexPragmaColumn{Name: cd.Name, Cid: int64(i), Coll: cd.Collate, Key: 0})
	}
	return out
}

// resolveColumnIndex maps an index-column name (or 1-based integer position)
// to a table column ordinal, or -1 when the column is unknown.
func resolveColumnIndex(name string, colDefs []sql.ColumnDef, colIndex map[string]int) int {
	if n, err := strconv.Atoi(name); err == nil && n >= 1 && n <= len(colDefs) {
		return n - 1
	}
	if i, ok := colIndex[name]; ok {
		return i
	}
	return -1
}

// parseIndexColumnCollations extracts per-column COLLATE names from a CREATE
// INDEX column list ("CREATE INDEX i ON t(a, b COLLATE rtrim)" -> ["", "rtrim"]).
func parseIndexColumnCollations(sqlStr string) []string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return nil
	}
	colsStr := sqlStr[start+1 : end]
	var colls []string
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		cu := strings.ToUpper(col)
		if idx := strings.Index(cu, " COLLATE "); idx >= 0 {
			rest := strings.TrimSpace(col[idx+len(" COLLATE "):])
			// Strip trailing ASC/DESC.
			ru := strings.ToUpper(rest)
			if di := strings.Index(ru, " DESC"); di >= 0 {
				rest = strings.TrimSpace(rest[:di])
			} else if ai := strings.Index(ru, " ASC"); ai >= 0 {
				rest = strings.TrimSpace(rest[:ai])
			}
			colls = append(colls, rest)
		} else {
			colls = append(colls, "")
		}
	}
	return colls
}

// execPragmaForeignKeyList implements PRAGMA foreign_key_list(table),
// reporting each FK of the table as rows (id, seq, table, from, to,
// on_update, on_delete, match).
