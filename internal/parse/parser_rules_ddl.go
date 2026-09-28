// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Code in this file is ORGANIZED BY GRAMMAR FUNCTION, one file per family;
// rule numbers are go-lemon table indices (see parser_ruleids.go).
//
// DDL commands: CREATE/DROP TABLE, VIEW, INDEX; ALTER TABLE;
// ATTACH/DETACH; VACUUM; ANALYZE; REINDEX; virtual-table creation.

package parse

import (
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 13: create_table ::= createkw temp TABLE ifnotexists nm dbnm
func ruleCreateTableCreatekwTempTableIfnotexistsNmDbnm(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
	if schema != "" {
		name = name + "." + schema
	}
	return &sql.CreateTableStmt{
		Name:        name,
		IfNotExists: getBool(getRHS(p, ruleNo, 4)),
		Temporary:   getBool(getRHS(p, ruleNo, 2)),
		Columns:     nil, // will be filled by create_table_args
	}

}

// Rule 19: create_table_args ::= LP columnlist conslist_opt RP table_option_set
func ruleCreateTableArgsLpColumnlistConslistOptRpTableOptionSet(ruleNo int, p *Parser) interface{} {
	// This rule produces columns from a column definition list plus
	// table-level constraints (conslist_opt) and table options
	// (table_option_set). The create_table value isn't available here;
	// rule 359 combines them into the CreateTableStmt.
	cols := getColumnList(getRHS(p, ruleNo, 2))
	cons := getTableConstraints(getRHS(p, ruleNo, 3))
	opts := getTableOptions(getRHS(p, ruleNo, 5))
	return &createTableArgs{
		columns:      cols,
		constraints:  cons,
		withoutRowid: opts.withoutRowid,
		strict:       opts.strict,
	}

}

// Rule 20: create_table_args ::= AS select
func ruleCreateTableArgsAsSelect(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 2))
	if sel != nil {
		// Wrap in CreateTableStmt with AS SELECT
		createStmt := &sql.CreateTableStmt{
			AsSelect: sel,
		}
		return createStmt
	}
	return nil

}

// Rule 79: cmd ::= DROP TABLE ifexists fullname
func ruleCmdDropTableIfexistsFullname(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropTableStmt{Name: name, IfExists: ifExists}

}

// Rule 80: ifexists ::= IF EXISTS
func ruleIfexistsIfExists(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 81: ifexists ::=
func ruleIfexists(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 82: cmd ::= createkw temp VIEW ifnotexists nm dbnm eidlist_opt AS select
func ruleCmdCreateView(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
	if schema != "" {
		name = name + "." + schema
	}
	sel := getSelectStmt(getRHS(p, ruleNo, 9))
	cols := getStringList(getRHS(p, ruleNo, 7))
	return &sql.CreateViewStmt{
		Name:        name,
		Columns:     cols,
		Select:      sel,
		Temporary:   getBool(getRHS(p, ruleNo, 2)), // temp nonterminal: TEMP/TEMPORARY
		IfNotExists: getBool(getRHS(p, ruleNo, 4)), // ifnotexists nonterminal
	}

}

// Rule 83: cmd ::= DROP VIEW ifexists fullname
func ruleCmdDropViewIfexistsFullname(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropViewStmt{Name: name, IfExists: ifExists}

}

// Rule 239: cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt
func ruleCmdCreateIndex(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	dbnm := getString(getRHS(p, ruleNo, 6))
	if dbnm != "" {
		// The full index name is "nm.dbnm" (e.g. CREATE INDEX aux.i2:
		// nm="aux", dbnm="i2"). The exec layer splits the schema prefix
		// back off.
		name = name + "." + dbnm
	}
	table := getString(getRHS(p, ruleNo, 8))
	sortlist := getOrderByList(getRHS(p, ruleNo, 10))
	// NULLS FIRST/LAST is only valid in ORDER BY, not in index key
	// definitions (SQLite: "unsupported use of NULLS FIRST/LAST").
	if err := rejectNullsInSortlist(sortlist); err != nil {
		p.SemanticErr = err
		return nil
	}
	where := getExpr(getRHS(p, ruleNo, 12))
	// uniqueflag is RHS[2]: empty or "UNIQUE".
	unique := strings.EqualFold(strings.TrimSpace(getString(getRHS(p, ruleNo, 2))), "UNIQUE")
	// The sortlist is []OrderByTerm; convert to []IndexColumn for the
	// engine's key population, while retaining the full term expressions
	// (Terms) for DDL validation and ALTER DROP COLUMN checks.
	// A plain identifier becomes a column reference. A numeric literal
	// is a 1-based column position (SQLite allows "CREATE INDEX ON
	// t1(1)" meaning "on the first column"); record it by its numeric
	// text so the engine can resolve it against the table columns.
	// A bare string literal index key is converted to an identifier
	// (SQLite sqlite3StringToId: CREATE INDEX t1('b') indexes column b).
	// Other expressions (e.g. "a+b") are not supported as index keys.
	var cols []sql.IndexColumn
	for _, term := range sortlist {
		switch ex := term.Expr.(type) {
		case *sql.ColumnRef:
			cols = append(cols, sql.IndexColumn{Name: ex.Name, Desc: term.Desc})
		case *sql.StringLit:
			cols = append(cols, sql.IndexColumn{Name: ex.Value, Desc: term.Desc})
		case *sql.NumericLit:
			cols = append(cols, sql.IndexColumn{Name: ex.Value, Desc: term.Desc})
		}
	}
	return &sql.CreateIndexStmt{
		Name:        name,
		Table:       table,
		Columns:     cols,
		Terms:       sortlist,
		Unique:      unique,
		Where:       where,
		IfNotExists: getBool(getRHS(p, ruleNo, 4)),
	}

}

// Rule 248: cmd ::= DROP INDEX ifexists fullname
func ruleCmdDropIndexIfexistsFullname(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropIndexStmt{Name: name, IfExists: ifExists}

}

// Rule 249: cmd ::= VACUUM into_opt
// VACUUM with an optional INTO <expr> clause (into_opt: empty, rule 252,
// or "INTO expr", rule 251). Only a string literal fills Into; any other
// expression node is carried in IntoExpr for the exec-side checks
// (vacuum.c: NULL → "non-text filename", column → resolved first).
func ruleCmdVacuumIntoOpt(ruleNo int, p *Parser) interface{} {
	return vacuumStmtFromInto("", getRHS(p, ruleNo, 2))
}

// Rule 250: cmd ::= VACUUM nm vinto
// Schema-qualified VACUUM ("VACUUM main", "VACUUM aux INTO 'f'"): nm is the
// schema name, vinto the optional INTO target (empty when absent).
func ruleCmdVacuumNmVinto(ruleNo int, p *Parser) interface{} {
	return vacuumStmtFromInto(getString(getRHS(p, ruleNo, 2)), getRHS(p, ruleNo, 3))
}

// Rule 251: into_opt ::= INTO expr — the VACUUM INTO target. The raw value
// is returned (string for a literal, expression node otherwise).
func ruleIntoOpt(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 2)
}

// Rule 283: cmd ::= DROP TRIGGER ifexists fullname
func ruleCmdDropTriggerIfexistsFullname(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropTriggerStmt{Name: name, IfExists: ifExists}

}

// Rule 284: cmd ::= ATTACH database_kw_opt expr AS expr key_opt
func ruleCmdAttachDatabaseKwOptExprAsExprKeyOpt(ruleNo int, p *Parser) interface{} {
	pathExpr := getExpr(getRHS(p, ruleNo, 3))
	schemaExpr := getExpr(getRHS(p, ruleNo, 5))
	path := ""
	if lit, ok := pathExpr.(*sql.StringLit); ok {
		path = lit.Value
	}
	schema := ""
	if lit, ok := schemaExpr.(*sql.StringLit); ok {
		schema = lit.Value
	} else if ref, ok := schemaExpr.(*sql.ColumnRef); ok {
		schema = ref.Name
	}
	return &sql.AttachStmt{Path: path, PathExpr: pathExpr, Schema: schema}

}

// Rule 285: cmd ::= DETACH database_kw_opt expr
// DETACH is a separate production from ATTACH (rule 284); the optional
// DATABASE keyword is database_kw_opt and the database name arrives as an
// expr (rule 180 yields a *sql.ColumnRef for a bare name). SQLite treats
// the DETACH argument as a scalar expression (DETACH 1+2 detaches "3");
// a multi-column row value in the argument is "row value misused". The
// raw expr is kept in SchemaExpr so execDetach can evaluate it.
func ruleCmdDetachDatabaseKwOptExpr(ruleNo int, p *Parser) interface{} {
	schema := ""
	rhs := getRHS(p, ruleNo, 3)
	switch v := rhs.(type) {
	case *sql.ColumnRef:
		schema = v.Name
	case *sql.StringLit:
		schema = v.Value
	case *sql.ParameterExpr, *sql.NullLit:
		// Unbound parameter / NULL resolves to an empty schema name,
		// matching SQLite's sqlite3Detach behavior for NULL names
		// (the attach3-12.x tests rely on this).
	case *sql.NumericLit:
		schema = getString(rhs)
	default:
		// Keep the original expression; execDetach evaluates it and
		// reports "row value misused" for multi-column row values.
	}
	return &sql.AttachStmt{IsDetach: true, Schema: schema, SchemaExpr: getExpr(rhs)}

}

// Rule 288: cmd ::= REINDEX
func ruleCmdReindex(ruleNo int, p *Parser) interface{} {
	return &sql.ReindexStmt{}

}

// Rule 289: cmd ::= REINDEX nm dbnm — REINDEX with an optional schema
// qualifier (dbnm) over the object name (nm), built "nm.dbnm" (schema
// dot object; "REINDEX main.t1" targets t1 in main).
func ruleCmdReindexNmDbnmReindexWithAnOptionalSchema(ruleNo int, p *Parser) interface{} {
	nm := getString(getRHS(p, ruleNo, 2))
	dbnm := getString(getRHS(p, ruleNo, 3))
	name := nm
	if dbnm != "" {
		name = nm + "." + dbnm
	}
	return &sql.ReindexStmt{Target: name}

}

// Rule 290: cmd ::= ANALYZE
func ruleCmdAnalyze(ruleNo int, p *Parser) interface{} {
	return &sql.AnalyzeStmt{}

}

// Rule 291: cmd ::= ANALYZE nm dbnm
// ANALYZE with a table/index name (and optional schema qualifier). In
// SQLite's grammar nm is the FIRST identifier (the schema part for a
// dotted name, e.g. "main" in "ANALYZE main.t1") and dbnm is the
// SECOND (the table/index part, "t1"). Build "schema.table" so
// execAnalyze's dot-splitting resolves the table in its schema.
func ruleCmdAnalyzeNmDbnm(ruleNo int, p *Parser) interface{} {
	nm := getString(getRHS(p, ruleNo, 2))
	dbnm := getString(getRHS(p, ruleNo, 3))
	name := nm
	if dbnm != "" {
		name = nm + "." + dbnm
	}
	return &sql.AnalyzeStmt{Name: name}

}

// Rule 292: cmd ::= ALTER TABLE fullname RENAME TO nm
func ruleCmdAlterTableFullnameRenameToNm(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:   getString(getRHS(p, ruleNo, 3)),
		Action:  "RENAME",
		NewName: getString(getRHS(p, ruleNo, 6)),
	}

}

// Rule 293: cmd ::= alter_add carglist
// ALTER TABLE ... ADD COLUMN: combine the column name/type from alter_add
// with the constraints from carglist into a full ColumnDef.
func ruleCmdAlterAddCarglist(ruleNo int, p *Parser) interface{} {
	ai := getAlterAddInfo(getRHS(p, ruleNo, 1))
	cols := getColumnList(getRHS(p, ruleNo, 2))
	cd := sql.ColumnDef{Name: ai.name, Type: ai.typ}
	for _, c := range cols {
		mergeColumnDef(&cd, c)
	}
	return &sql.AlterTableStmt{
		Table:  ai.table,
		Action: "ADD",
		ColDef: cd,
	}

}

// Rule 294: alter_add ::= ALTER TABLE fullname ADD kwcolumn_opt nm typetoken
func ruleAlterAddAlterTableFullnameAddKwcolumnOptNmTypetoken(ruleNo int, p *Parser) interface{} {
	return &alterAddInfo{
		table: getString(getRHS(p, ruleNo, 3)),
		name:  getString(getRHS(p, ruleNo, 6)),
		typ:   getString(getRHS(p, ruleNo, 7)),
	}

}

// Rule 295: cmd ::= ALTER TABLE fullname DROP kwcolumn_opt nm
func ruleCmdAlterTableFullnameDropKwcolumnOptNm(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:  getString(getRHS(p, ruleNo, 3)),
		Action: "DROP",
		Column: getString(getRHS(p, ruleNo, 6)),
	}

}

// Rule 296: cmd ::= ALTER TABLE fullname RENAME kwcolumn_opt nm TO nm
func ruleCmdAlterTableFullnameRenameKwcolumnOptNmToNm(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:   getString(getRHS(p, ruleNo, 3)),
		Action:  "RENAME",
		Column:  getString(getRHS(p, ruleNo, 6)),
		NewName: getString(getRHS(p, ruleNo, 8)),
	}

}

// Rule 297: cmd ::= ALTER TABLE fullname DROP CONSTRAINT nm
func ruleCmdAlterDropConstraint(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:   getString(getRHS(p, ruleNo, 3)),
		Action:  "DROP",
		Column:  "CONSTRAINT",
		NewName: getString(getRHS(p, ruleNo, 6)),
	}

}

// Rule 298: cmd ::= ALTER TABLE fullname ALTER COLUMN nm DROP NOT NULL
// Rules 298-299: ALTER COLUMN DROP/SET NOT NULL
func ruleCmdAlterColumnDropNotNull(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:          getString(getRHS(p, ruleNo, 3)),
		Action:         "ALTER",
		Column:         getString(getRHS(p, ruleNo, 6)),
		AlterColAction: "DROP NOT NULL",
	}
}

// Rule 299: cmd ::= ALTER TABLE fullname ALTER COLUMN nm SET NOT NULL
func ruleCmdAlterColumnSetNotNull(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:          getString(getRHS(p, ruleNo, 3)),
		Action:         "ALTER",
		Column:         getString(getRHS(p, ruleNo, 6)),
		AlterColAction: "SET NOT NULL",
	}

}

// Rule 300: cmd ::= ALTER TABLE fullname ADD CONSTRAINT nm CHECK LP expr RP onconf
func ruleCmdAlterAddConstraintCheck(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:  getString(getRHS(p, ruleNo, 3)),
		Action: "ADD",
		NewConstraint: &sql.TableConstraint{
			Type: sql.ConstraintCheck,
			Name: getString(getRHS(p, ruleNo, 6)),
			Expr: getExpr(getRHS(p, ruleNo, 9)),
		},
	}
}

// Rule 301: cmd ::= ALTER TABLE fullname ADD CHECK LP expr RP onconf
func ruleCmdAlterTableFullnameAddCheckLpExprRpOnconf(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:  getString(getRHS(p, ruleNo, 3)),
		Action: "ADD",
		NewConstraint: &sql.TableConstraint{
			Type: sql.ConstraintCheck,
			Expr: getExpr(getRHS(p, ruleNo, 7)),
		},
	}

}

// Rule 302: cmd ::= create_vtab
func ruleCmdCreateVtab(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 303: cmd ::= create_vtab LP vtabarglist RP
func ruleCmdCreateVtabLpVtabarglistRp(ruleNo int, p *Parser) interface{} {
	vt, _ := getRHS(p, ruleNo, 1).(*sql.CreateVirtualTableStmt)
	if vt != nil {
		vt.Args = getStringList(getRHS(p, ruleNo, 3))
	}
	return getRHS(p, ruleNo, 1)

}

// Rule 304: create_vtab ::= createkw VIRTUAL TABLE ifnotexists nm dbnm USING nm
func ruleCreateVtab(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	dbnm := getString(getRHS(p, ruleNo, 6)) // optional schema-qualified part
	if dbnm != "" {
		name = name + "." + dbnm
	}
	module := getString(getRHS(p, ruleNo, 8))
	return &sql.CreateVirtualTableStmt{
		Name:        name,
		Module:      module,
		IfNotExists: getBool(getRHS(p, ruleNo, 4)),
	}

}

// Rule 305: vtabarg ::= (empty)
func ruleVtabargEmpty(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 306: token ::= ID (a single virtual-table argument token)
func ruleTokenIdASingleVirtualTableArgumentToken(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 403: vtabarglist ::= vtabarg
func ruleVtabarglistVtabarg(ruleNo int, p *Parser) interface{} {
	return []string{getString(getRHS(p, ruleNo, 1))}

}

// Rule 404: vtabarglist ::= vtabarglist COMMA vtabarg
func ruleVtabarglistVtabarglistCommaVtabarg(ruleNo int, p *Parser) interface{} {
	head := getStringList(getRHS(p, ruleNo, 1))
	arg := getString(getRHS(p, ruleNo, 3))
	return append(head, arg)

}

// Rule 405: vtabarg ::= vtabarg token
func ruleVtabargVtabargToken(ruleNo int, p *Parser) interface{} {
	return strings.TrimSpace(getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2)))

}

// vacuumStmtFromInto builds a VacuumStmt from an INTO-clause RHS value: a
// string (or string-lit token) is the target filename; any other expression
// node is preserved for the exec-side error reporting.
func vacuumStmtFromInto(schema string, v interface{}) *sql.VacuumStmt {
	vs := &sql.VacuumStmt{Schema: schema}
	switch t := v.(type) {
	case nil:
	case string:
		vs.Into = t
	case *sql.StringLit:
		vs.Into = t.Value
	case sql.Token:
		vs.Into = t.Value
	default:
		if e, ok := v.(sql.Expr); ok {
			vs.IntoExpr = e
		}
	}
	return vs
}
