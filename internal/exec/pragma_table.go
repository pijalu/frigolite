// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package exec implements query execution.

package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
	"github.com/pijalu/frigolite/internal/vtab"
)

// isPragmaTableFunc reports whether name refers to a table-valued pragma
// function, e.g. pragma_table_info. Schema qualifiers (temp.pragma_...) are
// stripped before the check.
func isPragmaTableFunc(name string) bool {
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	return strings.HasPrefix(lower, "pragma_")
}

// isNoSuchVtabErr reports whether err is a "no such module" error from
// materializeVtabTableFunc (meaning the FROM name is an ordinary table, not a
// table-valued function).
func isNoSuchVtabErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such module")
}

// materializeVtabTableFunc materializes a table-valued virtual-table function
// reference (e.g. FROM generate_series(1,256)) into column definitions and
// rows. It returns an error wrapping "no such module" when the name is not a
// registered vtab module, so callers can fall back to ordinary table lookup.
func (e *Engine) materializeVtabTableFunc(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	// Evaluate the argument expressions to strings.
	args := make([]string, 0, len(ref.Args))
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, nil)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, fmt.Sprintf("%v", util.UnwrapColumnValue(v)))
	}
	vt, err := module.Create(args)
	if err != nil {
		return nil, nil, err
	}
	cur, err := vt.Open()
	if err != nil {
		return nil, nil, err
	}
	defer cur.Close()
	var rows [][]interface{}
	for cur.Next() {
		var row []interface{}
		for i := 0; ; i++ {
			val, err := cur.Column(i)
			if err != nil {
				break
			}
			row = append(row, val)
		}
		rows = append(rows, row)
	}
	// Build column defs from the vtab's declared columns.
	var colDefs []sql.ColumnDef
	if ci, ok := vt.(vtab.ColumnInfo); ok {
		for _, c := range ci.Columns() {
			colDefs = append(colDefs, sql.ColumnDef{Name: c})
		}
	}
	if len(colDefs) == 0 && len(rows) > 0 {
		for i := range rows[0] {
			colDefs = append(colDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d", i)})
		}
	}
	return colDefs, rows, nil
}

// execPragmaTableValued executes a SELECT whose FROM clause is a table-valued
// pragma function. The pragma is materialized into column definitions and rows
// and the outer SELECT pipeline runs over them.
func (e *Engine) execPragmaTableValued(s *sql.SelectStmt) *Result {
	colDefs, rows, err := e.materializePragmaTable(s.From)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execSelectOverMaterialized(s, colDefs, rows)
}

// materializePragmaTable converts a table-valued pragma reference into column
// definitions and rows, mirroring SQLite's pragma table-valued functions.
func (e *Engine) materializePragmaTable(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializePragmaTableWithRowImpl(ref, nil)
}

// materializePragmaTableWithRowImpl converts a table-valued pragma reference
// into column definitions and rows. When row is non-nil, column-reference
// arguments are evaluated against it (correlated table-valued pragmas).
func (e *Engine) materializePragmaTableWithRowImpl(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	lower := strings.ToLower(ref.Name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	pragma := strings.TrimPrefix(lower, "pragma_")
	switch pragma {
	case "table_info", "table_xinfo":
		return e.materializeTableInfo(ref)
	case "foreign_key_check":
		return e.materializeForeignKeyCheckWithRow(ref, row)
	case "table_list":
		return e.materializeTableList(ref)
	case "cache_size":
		// pragma_cache_size is a table-valued form of PRAGMA cache_size:
		// a single row with the setting value.
		return []sql.ColumnDef{{Name: "cache_size"}}, [][]interface{}{{int64(2000)}}, nil
	default:
		return nil, nil, fmt.Errorf("no such table-valued pragma: %s", ref.Name)
	}
}

// materializeTableInfo builds the rows of pragma_table_info / pragma_table_xinfo
// for the table or view named by the first function argument. The result has
// columns (cid, name, type, notnull, dflt_value, pk), one row per column.
func (e *Engine) materializeTableInfo(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "cid"},
		{Name: "name"},
		{Name: "type"},
		{Name: "notnull"},
		{Name: "dflt_value"},
		{Name: "pk"},
	}
	if len(ref.Args) == 0 {
		return nil, nil, fmt.Errorf("wrong number of arguments to function %s()", ref.Name)
	}
	argVal, err := e.evalExpr(ref.Args[0], nil)
	if err != nil {
		return nil, nil, err
	}
	tableName, ok := argVal.(string)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}

	var colDefs []sql.ColumnDef
	if te, _, err := e.findTable(tableName); err == nil {
		colDefs = e.parseColumnDefs(te.Name, te.SQL)
	} else if ve, _, err := e.findView(tableName); err == nil {
		colDefs, err = e.viewColumnDefs(ve)
		if err != nil {
			return nil, nil, err
		}
	} else {
		// Unknown table or view: pragma_table_info returns zero rows.
		return cols, nil, nil
	}

	rows := make([][]interface{}, 0, len(colDefs))
	cid := int64(0)
	for _, cd := range colDefs {
		// Skip dropped columns (removed via ALTER TABLE DROP COLUMN).
		if cd.Dropped {
			continue
		}
		notnull := int64(0)
		if cd.NotNull {
			notnull = 1
		}
		pk := int64(0)
		if cd.PrimaryKey {
			pk = 1
		}
		// The NONE-affinity sentinel (an expression-derived view column with
		// no declared type) renders as an empty type, matching SQLite.
		typeName := cd.Type
		if typeName == util.AffinityNone {
			typeName = ""
		}
		rows = append(rows, []interface{}{cid, cd.Name, typeName, notnull, nil, pk})
		cid++
	}
	return cols, rows, nil
}

// pragmaArgsCorrelated reports whether a table-valued pragma reference has an
// argument that is a bare column reference (an outer-row correlation, e.g.
// pragma_foreign_key_check(name) joined against sqlite_schema).
func pragmaArgsCorrelated(ref sql.TableRef) bool {
	for _, a := range ref.Args {
		if _, ok := a.(*sql.ColumnRef); ok {
			return true
		}
	}
	return false
}

// materializeCorrelatedPragma materializes a table-valued pragma once per left
// row, evaluating column-reference arguments against that row (SQLite
// correlation for table-valued pragma functions). It returns the pragma column
// definitions, the materialized row maps, and for each row map the index of the
// left row it was materialized for (so the join pairs each right row with its
// own left row instead of cross-joining).
func (e *Engine) materializeCorrelatedPragma(ref sql.TableRef, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error) {
	var colDefs []sql.ColumnDef
	var allMaps []RowMap
	var leftIdx []int
	for li, left := range leftRows {
		defs, rows, err := e.materializePragmaTableWithRow(ref, left)
		if err != nil {
			return nil, nil, nil, err
		}
		if colDefs == nil {
			colDefs = defs
		}
		for _, row := range rows {
			m := make(RowMap)
			for i, val := range row {
				if i < len(defs) {
					m[defs[i].Name] = val
				}
			}
			allMaps = append(allMaps, m)
			leftIdx = append(leftIdx, li)
		}
	}
	return colDefs, allMaps, leftIdx, nil
}

// materializePragmaTableWithRow is materializePragmaTable with a row context
// for column-reference arguments.
func (e *Engine) materializePragmaTableWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializePragmaTableWithRowImpl(ref, row)
}

// materializeForeignKeyCheck builds the rows of pragma_foreign_key_check, the
// table-valued form of PRAGMA foreign_key_check. Arguments are the optional
// child table name and optional schema name; the schema argument restricts the
// child lookup to that schema (and errors when the table is not found there,
// matching SQLite).
func (e *Engine) materializeForeignKeyCheck(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializeForeignKeyCheckWithRow(ref, nil)
}

// materializeForeignKeyCheckWithRow is materializeForeignKeyCheck with a row
// context for column-reference arguments (correlated pragma_foreign_key_check).
func (e *Engine) materializeForeignKeyCheckWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "table"},
		{Name: "rowid"},
		{Name: "parent"},
		{Name: "fkid"},
	}
	var tableName, schemaName string
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, row)
		if err != nil {
			return nil, nil, err
		}
		s, ok := util.UnwrapColumnValue(v).(string)
		if !ok {
			return nil, nil, fmt.Errorf("wrong type for argument of pragma_foreign_key_check(): expected string")
		}
		if tableName == "" {
			tableName = s
		} else if schemaName == "" {
			schemaName = s
		}
	}
	viols, err := e.findFKViolations(tableName, schemaName)
	if err != nil {
		// In a correlated join (pragma_foreign_key_check(name) against
		// sqlite_schema) a non-table name (an index, a dropped object) yields
		// no rows rather than an error; the standalone call with a schema
		// qualifier still errors (fkey5-13.1).
		if pragmaArgsCorrelated(ref) && strings.Contains(err.Error(), "no such table") {
			return cols, nil, nil
		}
		return nil, nil, err
	}
	rows := make([][]interface{}, 0, len(viols))
	for _, v := range viols {
		rows = append(rows, []interface{}{v.childTable, v.rowID, v.parentTable, int64(v.fkID)})
	}
	return cols, rows, nil
}

// materializeTableList builds the rows of pragma_table_list. When called with
// a table name argument, it returns one row for that table. Without an
// argument, it returns one row for every table in the schema.
// Columns: schema, name, type, ncol, wr (without rowid), strict.
func (e *Engine) materializeTableList(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	// Note: "strict" is a SQL keyword, so the LALR parser uppercases it.
	// But with case-restoration for TK_ID fallback, the original case is
	// preserved. We use lowercase to match SQLite's pragma_table_list output.
	cols := []sql.ColumnDef{
		{Name: "schema"},
		{Name: "name"},
		{Name: "type"},
		{Name: "ncol"},
		{Name: "wr"},
		{Name: "strict"},
	}

	entries, err := e.mainDB.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return nil, nil, err
	}

	// If an argument is provided, filter to that table name
	var filterName string
	if len(ref.Args) > 0 {
		argVal, err := e.evalExpr(ref.Args[0], nil)
		if err != nil {
			return nil, nil, err
		}
		if s, ok := argVal.(string); ok {
			filterName = s
		}
	}

	var rows [][]interface{}
	for _, entry := range entries {
		if entry.Type != schema.TypeTable {
			continue
		}
		if filterName != "" && entry.Name != filterName {
			continue
		}
		colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
		wr := int64(0)
		if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
			wr = 1
		}
		strict := int64(0)
		upperSQL := strings.ToUpper(entry.SQL)
		if hasStrictKeyword(upperSQL) {
			strict = 1
		}
		rows = append(rows, []interface{}{
			"main", entry.Name, string(entry.Type),
			int64(len(colDefs)), wr, strict,
		})
	}

	// Views appear in pragma_table_list with type "view" and their column
	// count. Their ncol is the number of result columns of the view SELECT.
	viewEntries, err := e.mainDB.Schema.GetEntries(schema.TypeView)
	if err == nil {
		for _, entry := range viewEntries {
			if filterName != "" && entry.Name != filterName {
				continue
			}
			defs, derr := e.viewColumnDefs(entry)
			ncol := 0
			if derr == nil {
				ncol = len(defs)
			}
			rows = append(rows, []interface{}{
				"main", entry.Name, "view", int64(ncol), int64(0), int64(0),
			})
		}
	}
	return cols, rows, nil
}

// viewColumnDefs derives the column names and declared types of a view from
// its stored SELECT statement, mirroring SQLite's view column typing
// (sqlite3SubqueryColumnTypes in select.c).
func (e *Engine) viewColumnDefs(viewEntry *schema.Entry) ([]sql.ColumnDef, error) {
	return e.viewColumnDefsGuard(viewEntry, nil)
}

// viewColumnDefsGuard is viewColumnDefs with a recursion guard: resolving is
// the set of view names currently being expanded (to break cycles such as
// CREATE VIEW v1 AS SELECT * FROM v2; CREATE VIEW v2 AS SELECT * FROM v1).
func (e *Engine) viewColumnDefsGuard(viewEntry *schema.Entry, resolving map[string]bool) ([]sql.ColumnDef, error) {
	if resolving[viewEntry.Name] {
		// Circular view reference: fall back to the declared column list when
		// present, otherwise return no defs (the execution path reports the
		// circular reference).
		return nil, fmt.Errorf("exec: circular view reference: %s", viewEntry.Name)
	}
	if resolving == nil {
		resolving = map[string]bool{}
	}
	resolving[viewEntry.Name] = true
	defer delete(resolving, viewEntry.Name)
	sqlStr := viewEntry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return nil, fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	// Skip past " AS" plus any following whitespace (the body may begin on
	// the same line or the next line).
	body := sqlStr[idx+3:]
	body = strings.TrimLeft(body, " \t\r\n")
	stmts, err := parse.ParseSQL(body)
	if err != nil || len(stmts) == 0 {
		return nil, fmt.Errorf("exec: view parse error: %v", err)
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return nil, fmt.Errorf("exec: view does not contain SELECT")
	}
	defs := e.viewColumnDefsFromSelectGuard(sel, resolving)
	// A declared column list must match the SELECT's result width; SQLite
	// raises "expected N columns for 'view' but got M" at view use time.
	if declared := viewDeclaredColumns(sqlStr); len(declared) > 0 && len(declared) != len(defs) {
		return nil, fmt.Errorf("expected %d columns for '%s' but got %d", len(declared), viewEntry.Name, len(defs))
	}
	return defs, nil
}

// viewColumnDefsFromSelect computes column definitions for a view's SELECT.
// Column names come from aliases or column references; declared types follow
// SQLite's expression-based type inference.
func (e *Engine) viewColumnDefsFromSelect(sel *sql.SelectStmt) []sql.ColumnDef {
	return e.viewColumnDefsFromSelectGuard(sel, nil)
}

// viewColumnDefsFromSelectGuard is viewColumnDefsFromSelect with a recursion
// guard threaded through nested view/subquery column resolution.
func (e *Engine) viewColumnDefsFromSelectGuard(sel *sql.SelectStmt, resolving map[string]bool) []sql.ColumnDef {
	// Resolve the FROM sources (base table plus each JOIN operand) so bare "*"
	// can expand to every column, and so column references can inherit types.
	srcDefs := e.fromSourceColumnDefsGuard(sel.From, resolving)
	for _, j := range sel.Joins {
		srcDefs = append(srcDefs, e.fromSourceColumnDefsGuard(j.Table, resolving)...)
	}
	var defs []sql.ColumnDef
	for i, col := range sel.Columns {
		// A bare * expands to every column of the FROM sources (a single
		// table, or a joined result).
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
			for _, sd := range srcDefs {
				defs = append(defs, sql.ColumnDef{Name: sd.Name, Type: sd.Type, Collate: sd.Collate})
			}
			continue
		}
		// A qualified star (t.* / alias.*) expands to the referenced source's
		// columns in order (SQLite: SELECT episode.*, files.f ...).
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table != "" {
			for _, sd := range e.qualifiedSourceColumnDefs(ref.Table, sel, srcDefs, resolving) {
				defs = append(defs, sql.ColumnDef{Name: sd.Name, Type: sd.Type, Collate: sd.Collate})
			}
			continue
		}
		name := col.As
		if name == "" {
			if ref, ok := col.Expr.(*sql.ColumnRef); ok {
				name = ref.Name
			}
		}
		defs = append(defs, sql.ColumnDef{Name: name, Type: e.viewColumnType(sel, i, srcDefs)})
	}
	return defs
}

// fromSourceColumnDefsGuard returns the column definitions of a single FROM
// source (a table, view, derived-table subquery, or table-valued function),
// with a recursion guard threaded through nested view resolution.
func (e *Engine) fromSourceColumnDefsGuard(ref sql.TableRef, resolving map[string]bool) []sql.ColumnDef {
	if ref.Subquery != nil {
		return e.viewColumnDefsFromSelectGuard(ref.Subquery, resolving)
	}
	if ref.Name != "" {
		if te, _, err := e.findTable(ref.Name); err == nil {
			return e.parseColumnDefs(te.Name, te.SQL)
		}
		if ve, _, err := e.findView(ref.Name); err == nil {
			if defs, derr := e.viewColumnDefsGuard(ve, resolving); derr == nil {
				return defs
			}
		}
		if isPragmaTableFunc(ref.Name) {
			if defs, _, merr := e.materializeVtabTableFunc(ref); merr == nil {
				return defs
			}
		}
	}
	return nil
}

// qualifiedSourceColumnDefs returns the column definitions of the FROM source
// (base table/view or a JOIN operand) referenced by a qualified star
// (t.* / alias.*). The qualifier matches the table name or its alias,
// case-insensitively.
func (e *Engine) qualifiedSourceColumnDefs(qualifier string, sel *sql.SelectStmt, srcDefs []sql.ColumnDef, resolving map[string]bool) []sql.ColumnDef {
	// Base source (FROM t or FROM t AS a): match the table name or alias.
	base := sel.From
	baseName := base.Name
	if base.As != "" {
		baseName = base.As
	}
	if baseName != "" && strings.EqualFold(baseName, qualifier) {
		return e.fromSourceColumnDefsGuard(base, resolving)
	}
	// JOIN operands: match the operand's table name or alias.
	for _, j := range sel.Joins {
		jName := j.Table.Name
		if j.Table.As != "" {
			jName = j.Table.As
		}
		if jName != "" && strings.EqualFold(jName, qualifier) {
			return e.fromSourceColumnDefsGuard(j.Table, resolving)
		}
	}
	// Fall back to the flattened source defs (single-source queries without
	// an alias match, e.g. a subquery-derived source).
	return srcDefs
}

// viewColumnType computes the declared type string of view column i, following
// sqlite3SubqueryColumnTypes: affinity of the expression is refined across
// compound (UNION / multi-row VALUES) members, then mapped to a type name.
func (e *Engine) viewColumnType(sel *sql.SelectStmt, i int, srcDefs []sql.ColumnDef) string {
	first := sel
	aff := e.exprAffinity(first.Columns[i].Expr, srcDefs)
	pS2 := first
	m := 0
	// Walk the compound chain while the current member has no affinity,
	// remembering the datatypes of the members skipped. Bounds-check each
	// member: a malformed/uneven compound (e.g. an expected-error case)
	// must not panic when a later member has fewer columns.
	for aff == 0 && pS2.Union != nil {
		if i < len(pS2.Columns) {
			m |= exprDataType(pS2.Columns[i].Expr)
		}
		pS2 = pS2.Union
		if i >= len(pS2.Columns) {
			break
		}
		aff = e.exprAffinity(pS2.Columns[i].Expr, srcDefs)
	}
	if aff == 0 {
		// No affinity: the expression (e.g. a function call) has no declared
		// affinity. SQLite's view columns default to SQLITE_AFF_NONE (not
		// BLOB) in sqlite3SubqueryColumnTypes. Return the sentinel type name
		// so downstream affinity extraction yields 0 (NONE).
		return util.AffinityNone
	}
	// Compound queries refine the affinity using the datatypes of later members.
	if isTextOrNumericAff(aff) && (pS2.Union != nil || pS2 != first) {
		for p := pS2.Union; p != nil; p = p.Union {
			m |= exprDataType(p.Columns[i].Expr)
		}
		if aff == 'T' && (m&0x01) != 0 {
			aff = 'B'
		} else if isNumericAff(aff) && (m&0x02) != 0 {
			aff = 'B'
		}
		if isNumericAff(aff) && isCastExpr(first.Columns[i].Expr) {
			aff = 'F' // FLEXNUM: CAST over a compound numeric column
		}
	}
	zType := e.exprColumnType(first.Columns[i].Expr, srcDefs)
	if zType == "" || value.Affinity(zType) != aff {
		if aff == 'N' || aff == 'F' {
			return "NUM"
		}
		for _, st := range viewStdTypes {
			if st.aff == aff {
				return st.name
			}
		}
		return ""
	}
	return zType
}

// viewStdTypes maps an affinity to SQLite's standard type names
// (sqlite3StdType[1..] in 3.53: BLOB, INT, INTEGER, REAL, TEXT). The first
// match wins, so INTEGER affinity yields "INT".
var viewStdTypes = []struct {
	aff  rune
	name string
}{
	{'B', "BLOB"},
	{'I', "INT"},
	{'I', "INTEGER"},
	{'R', "REAL"},
	{'T', "TEXT"},
}

// exprAffinity returns the affinity of a view column expression: CAST targets
// carry the affinity of their type name, column references inherit the
// affinity of the source column's declared type, and anything else has none.
func (e *Engine) exprAffinity(expr sql.Expr, srcDefs []sql.ColumnDef) rune {
	switch x := expr.(type) {
	case *sql.CastExpr:
		return value.Affinity(x.AsType)
	case *sql.ColumnRef:
		for _, cd := range srcDefs {
			if cd.Name == x.Name {
				return value.Affinity(cd.Type)
			}
		}
		return 0
	default:
		return 0
	}
}

// exprColumnType returns the declared type of a view column expression, or ""
// if the expression has none. Column references inherit the source column's
// declared type; CAST and other expressions have none (mirroring SQLite's
// columnType(), which only reports types for column and subselect expressions).
func (e *Engine) exprColumnType(expr sql.Expr, srcDefs []sql.ColumnDef) string {
	switch x := expr.(type) {
	case *sql.ColumnRef:
		for _, cd := range srcDefs {
			if cd.Name == x.Name {
				return cd.Type
			}
		}
		return ""
	default:
		return ""
	}
}

// exprDataType returns a bitmask of possible result datatypes for an
// expression, mirroring sqlite3ExprDataType: 0x01 numeric, 0x02 text, 0x04 blob.
func exprDataType(expr sql.Expr) int {
	switch x := expr.(type) {
	case *sql.NumericLit:
		return 0x01
	case *sql.StringLit:
		return 0x02
	case *sql.BlobLit:
		return 0x04
	case *sql.CastExpr:
		return exprDataType(x.Operand)
	default:
		return 0
	}
}

// isCastExpr reports whether expr is a CAST expression.
func isCastExpr(expr sql.Expr) bool {
	_, ok := expr.(*sql.CastExpr)
	return ok
}

// isTextOrNumericAff reports whether aff is TEXT or one of the numeric
// affinities (NUMERIC, INTEGER, REAL) — SQLite's "affinity >= SQLITE_AFF_TEXT".
func isTextOrNumericAff(aff rune) bool {
	return aff == 'T' || aff == 'N' || aff == 'I' || aff == 'R'
}

// isNumericAff reports whether aff is a numeric affinity
// (NUMERIC, INTEGER, REAL) — SQLite's "affinity >= SQLITE_AFF_NUMERIC".
func isNumericAff(aff rune) bool {
	return aff == 'N' || aff == 'I' || aff == 'R'
}
