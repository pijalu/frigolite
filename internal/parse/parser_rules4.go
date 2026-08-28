package parse

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

func rule281(ruleNo int, p *Parser) interface{} {
	return "ABORT"
}

func rule282(ruleNo int, p *Parser) interface{} {
	return "FAIL"

}

// Rule 283: cmd ::= DROP TRIGGER ifexists fullname
func rule283(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropTriggerStmt{Name: name, IfExists: ifExists}

}

// Rule 284: cmd ::= ATTACH database_kw_opt expr AS expr key_opt
func rule284(ruleNo int, p *Parser) interface{} {
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
func rule285(ruleNo int, p *Parser) interface{} {
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
func rule288(ruleNo int, p *Parser) interface{} {
	return &sql.ReindexStmt{}

}

func rule289(ruleNo int, p *Parser) interface{} {
	return &sql.ReindexStmt{}

}

// Rule 290: cmd ::= ANALYZE
func rule290(ruleNo int, p *Parser) interface{} {
	return &sql.AnalyzeStmt{}

}

// Rule 291: cmd ::= ANALYZE nm dbnm
// ANALYZE with a table/index name (and optional schema qualifier). In
// SQLite's grammar nm is the FIRST identifier (the schema part for a
// dotted name, e.g. "main" in "ANALYZE main.t1") and dbnm is the
// SECOND (the table/index part, "t1"). Build "schema.table" so
// execAnalyze's dot-splitting resolves the table in its schema.
func rule291(ruleNo int, p *Parser) interface{} {
	nm := getString(getRHS(p, ruleNo, 2))
	dbnm := getString(getRHS(p, ruleNo, 3))
	name := nm
	if dbnm != "" {
		name = nm + "." + dbnm
	}
	return &sql.AnalyzeStmt{Name: name}

}

// Rule 292: cmd ::= ALTER TABLE fullname RENAME TO nm
func rule292(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:   getString(getRHS(p, ruleNo, 3)),
		Action:  "RENAME",
		NewName: getString(getRHS(p, ruleNo, 6)),
	}

}

// Rule 293: cmd ::= alter_add carglist
// ALTER TABLE ... ADD COLUMN: combine the column name/type from alter_add
// with the constraints from carglist into a full ColumnDef.
func rule293(ruleNo int, p *Parser) interface{} {
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
func rule294(ruleNo int, p *Parser) interface{} {
	return &alterAddInfo{
		table: getString(getRHS(p, ruleNo, 3)),
		name:  getString(getRHS(p, ruleNo, 6)),
		typ:   getString(getRHS(p, ruleNo, 7)),
	}

}

// Rule 295: cmd ::= ALTER TABLE fullname DROP kwcolumn_opt nm
func rule295(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:  getString(getRHS(p, ruleNo, 3)),
		Action: "DROP",
		Column: getString(getRHS(p, ruleNo, 6)),
	}

}

// Rule 296: cmd ::= ALTER TABLE fullname RENAME kwcolumn_opt nm TO nm
func rule296(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:   getString(getRHS(p, ruleNo, 3)),
		Action:  "RENAME",
		Column:  getString(getRHS(p, ruleNo, 6)),
		NewName: getString(getRHS(p, ruleNo, 8)),
	}

}

func rule297(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:   getString(getRHS(p, ruleNo, 3)),
		Action:  "DROP",
		Column:  "CONSTRAINT",
		NewName: getString(getRHS(p, ruleNo, 6)),
	}

}

// Rules 298-299: ALTER COLUMN DROP/SET NOT NULL
func rule298(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:          getString(getRHS(p, ruleNo, 3)),
		Action:         "ALTER",
		Column:         getString(getRHS(p, ruleNo, 6)),
		AlterColAction: "DROP NOT NULL",
	}
}

func rule299(ruleNo int, p *Parser) interface{} {
	return &sql.AlterTableStmt{
		Table:          getString(getRHS(p, ruleNo, 3)),
		Action:         "ALTER",
		Column:         getString(getRHS(p, ruleNo, 6)),
		AlterColAction: "SET NOT NULL",
	}

	// Rules 300-301: ALTER TABLE ADD [CONSTRAINT nm] CHECK(expr)
}

// Rule 300: cmd ::= ALTER TABLE fullname ADD CONSTRAINT nm CHECK LP expr RP onconf
func rule300(ruleNo int, p *Parser) interface{} {
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
func rule301(ruleNo int, p *Parser) interface{} {
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
func rule302(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 303: cmd ::= create_vtab LP vtabarglist RP
func rule303(ruleNo int, p *Parser) interface{} {
	vt, _ := getRHS(p, ruleNo, 1).(*sql.CreateVirtualTableStmt)
	if vt != nil {
		vt.Args = getStringList(getRHS(p, ruleNo, 3))
	}
	return getRHS(p, ruleNo, 1)

}

// Rule 304: create_vtab ::= createkw VIRTUAL TABLE ifnotexists nm dbnm USING nm
func rule304(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	dbnm := getString(getRHS(p, ruleNo, 6)) // optional schema-qualified part
	if dbnm != "" {
		name = name + "." + dbnm
	}
	module := getString(getRHS(p, ruleNo, 8))
	return &sql.CreateVirtualTableStmt{
		Name:   name,
		Module: module,
	}

}

func rule305(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 306: token ::= ID (a single virtual-table argument token)
func rule306(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 309: with ::= WITH wqlist
// The wqlist value is []sql.CTEDef; propagate it as the with value so
// INSERT (rule 164) can attach the CTEs.
func rule309(ruleNo int, p *Parser) interface{} {
	return getCTEDefs(getRHS(p, ruleNo, 2))

}

// Rule 310: with ::= WITH RECURSIVE wqlist
// Mark every CTE as recursive (WITH RECURSIVE applies to the whole list).
func rule310(ruleNo int, p *Parser) interface{} {
	defs := getCTEDefs(getRHS(p, ruleNo, 3))
	for i := range defs {
		defs[i].Recursive = true
	}
	return defs

}

// Rule 311: wqas ::= AS
// The materialization hint (MATERIALIZED / NOT MATERIALIZED) is not
// modeled; pass through a marker value.
func rule311(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 314: wqitem ::= withnm eidlist_opt wqas LP select RP
func rule314(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	cols := getStringList(getRHS(p, ruleNo, 2))
	sel := getSelectStmt(getRHS(p, ruleNo, 5))
	return sql.CTEDef{Name: name, Columns: cols, Select: sel}

}

// Rule 315: withnm ::= nm
func rule315(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 316: wqlist ::= wqitem
func rule316(ruleNo int, p *Parser) interface{} {
	if d, ok := getRHS(p, ruleNo, 1).(sql.CTEDef); ok {
		return []sql.CTEDef{d}
	}
	return nil

}

func rule317(ruleNo int, p *Parser) interface{} {
	defs := getCTEDefs(getRHS(p, ruleNo, 1))
	if d, ok := getRHS(p, ruleNo, 3).(sql.CTEDef); ok {
		return append(defs, d)
	}
	return defs

}

// Rule 318: windowdefn_list ::= windowdefn_list COMMA windowdefn
func rule318(ruleNo int, p *Parser) interface{} {
	// The LALR tables reduce a single windowdefn to a *sql.WindowDef here
	// (rule 410's list shape is not used); accept both the list and the
	// single-definition shapes.
	var list []sql.WindowDef
	if l := getWindowDefList(getRHS(p, ruleNo, 1)); l != nil {
		list = l
	} else if wd := getWindowDef(getRHS(p, ruleNo, 1)); wd != nil {
		list = []sql.WindowDef{*wd}
	}
	wd := getWindowDef(getRHS(p, ruleNo, 3))
	if wd != nil {
		list = append(list, *wd)
	}
	return list

}

// Rule 319: windowdefn ::= nm AS LP window RP
func rule319(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	inner := getWindowDef(getRHS(p, ruleNo, 4))
	if inner != nil {
		inner.Name = name
		return inner
	}
	// The LALR tables fold a frame-only window body (LP frame_opt RP) into a
	// *sql.WindowFrame value at RHS 4 (rule 411's shape is not used for the
	// parenthesized form). Wrap it in a WindowDef with the frame.
	if f := getWindowFrame(getRHS(p, ruleNo, 4)); f != nil {
		return &sql.WindowDef{Name: name, FrameSpec: frameFromStruct(f), Frame: f}
	}
	return &sql.WindowDef{Name: name}

}

// Rule 320: window ::= PARTITION BY nexprlist orderby_opt frame_opt
func rule320(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		Partitions: getExprList(getRHS(p, ruleNo, 3)),
		OrderBy:    getOrderByList(getRHS(p, ruleNo, 4)),
		FrameSpec:  getFrameOptSpec(getRHS(p, ruleNo, 5)),
		Frame:      getFrameOptFrame(getRHS(p, ruleNo, 5)),
	}

}

// Rule 321: window ::= nm PARTITION BY nexprlist orderby_opt frame_opt
func rule321(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		BaseName:   getString(getRHS(p, ruleNo, 1)),
		Partitions: getExprList(getRHS(p, ruleNo, 4)),
		OrderBy:    getOrderByList(getRHS(p, ruleNo, 5)),
		FrameSpec:  getFrameOptSpec(getRHS(p, ruleNo, 6)),
		Frame:      getFrameOptFrame(getRHS(p, ruleNo, 6)),
	}

}

// Rule 322: window ::= ORDER BY sortlist frame_opt
func rule322(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		OrderBy:   getOrderByList(getRHS(p, ruleNo, 3)),
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 4)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 4)),
	}

}

// Rule 323: window ::= nm ORDER BY sortlist frame_opt
func rule323(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		BaseName:  getString(getRHS(p, ruleNo, 1)),
		OrderBy:   getOrderByList(getRHS(p, ruleNo, 4)),
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 5)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 5)),
	}

}

func rule324(ruleNo int, p *Parser) interface{} {
	// window ::= nm frame_opt — a bare window-name reference (optionally with
	// an added frame). The nm is the referenced base window.
	return &sql.WindowDef{
		BaseName:  getString(getRHS(p, ruleNo, 1)),
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 2)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 2)),
	}

}

// Rule 325: frame_opt ::=
func rule325(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 326: frame_opt ::= range_or_rows frame_bound_s frame_exclude_opt
func rule326(ruleNo int, p *Parser) interface{} {
	bound := getFrameBound(getRHS(p, ruleNo, 2))
	if bound == nil {
		return frameSpecFromParts(
			getString(getRHS(p, ruleNo, 1)),
			getString(getRHS(p, ruleNo, 2)),
			getString(getRHS(p, ruleNo, 3)),
		)
	}
	// Shorthand form: "ROWS 2 PRECEDING" == "ROWS BETWEEN 2 PRECEDING AND CURRENT ROW".
	// SQLite validates the shorthand boundary in the engine.
	frame := &sql.WindowFrame{
		Type:  getString(getRHS(p, ruleNo, 1)),
		Start: *bound,
	}
	if excl := getString(getRHS(p, ruleNo, 3)); excl != "" {
		frame.Exclude = excl
	}
	return frame
}

// Rule 327: frame_opt ::= range_or_rows BETWEEN frame_bound_s AND frame_bound_e frame_exclude_opt
func rule327(ruleNo int, p *Parser) interface{} {
	spec := frameSpecFromParts(
		getString(getRHS(p, ruleNo, 1)),
		"BETWEEN",
		getString(getRHS(p, ruleNo, 3)),
		"AND",
		getString(getRHS(p, ruleNo, 5)),
	)
	frame := &sql.WindowFrame{
		Type:    getString(getRHS(p, ruleNo, 1)),
		Between: true,
	}
	if b := getFrameBound(getRHS(p, ruleNo, 3)); b != nil {
		frame.Start = *b
	}
	if b := getFrameBound(getRHS(p, ruleNo, 5)); b != nil {
		frame.End = *b
	}
	if excl := getString(getRHS(p, ruleNo, 6)); excl != "" {
		frame.Exclude = excl
		spec += " EXCLUDE " + excl
	}
	return frame
}

// Rule 328: range_or_rows ::= RANGE|ROWS|GROUPS
func rule328(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 329: frame_bound_s ::= frame_bound
func rule329(ruleNo int, p *Parser) interface{} {
	return getFrameBound(getRHS(p, ruleNo, 1))

}

// Rule 330: frame_bound_s ::= UNBOUNDED PRECEDING
func rule330(ruleNo int, p *Parser) interface{} {
	return &sql.FrameBound{Kind: "UNBOUNDED PRECEDING"}

}

// Rule 331: frame_bound_e ::= frame_bound
func rule331(ruleNo int, p *Parser) interface{} {
	return getFrameBound(getRHS(p, ruleNo, 1))

}

func rule332(ruleNo int, p *Parser) interface{} {
	return &sql.FrameBound{Kind: "UNBOUNDED FOLLOWING"}

}

// Rule 333: frame_bound ::= expr PRECEDING|FOLLOWING
func rule333(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 1))
	dir := getString(getRHS(p, ruleNo, 2))
	return &sql.FrameBound{Kind: dir, Expr: expr}

}

// Rule 334: frame_bound ::= CURRENT ROW
func rule334(ruleNo int, p *Parser) interface{} {
	return &sql.FrameBound{Kind: "CURRENT ROW"}

}

// Rule 335: frame_exclude_opt ::=
func rule335(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 336: frame_exclude_opt ::= EXCLUDE frame_exclude
func rule336(ruleNo int, p *Parser) interface{} {
	// Return the bare exclude value; the caller adds the "EXCLUDE " prefix
	// when building the frame spec text.
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 337: frame_exclude ::= NO OTHERS
func rule337(ruleNo int, p *Parser) interface{} {
	return "NO OTHERS"

}

// Rule 338: frame_exclude ::= CURRENT ROW
func rule338(ruleNo int, p *Parser) interface{} {
	return "CURRENT ROW"

}

// Rule 339: frame_exclude ::= GROUP|TIES
func rule339(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

func rule340(ruleNo int, p *Parser) interface{} {
	// window_clause ::= WINDOW windowdefn_list. The LALR tables reduce a
	// single windowdefn to a *sql.WindowDef here (the list shape is only used
	// when there are two or more, rule 318); accept both shapes.
	if list := getWindowDefList(getRHS(p, ruleNo, 2)); list != nil {
		return list
	}
	if wd := getWindowDef(getRHS(p, ruleNo, 2)); wd != nil {
		return []sql.WindowDef{*wd}
	}
	return nil

}

// Rule 341: filter_over ::= filter_clause over_clause
func rule341(ruleNo int, p *Parser) interface{} {
	return &windowFilter{
		filter: getExpr(getRHS(p, ruleNo, 1)),
		over:   getWindowDef(getRHS(p, ruleNo, 2)),
	}

}

// Rule 342: filter_over ::= over_clause
func rule342(ruleNo int, p *Parser) interface{} {
	return &windowFilter{
		over: getWindowDef(getRHS(p, ruleNo, 1)),
	}

}

// Rule 343: filter_over ::= filter_clause
func rule343(ruleNo int, p *Parser) interface{} {
	return &windowFilter{
		filter: getExpr(getRHS(p, ruleNo, 1)),
	}

}

// Rule 344: over_clause ::= OVER LP window RP
func rule344(ruleNo int, p *Parser) interface{} {
	// The LALR tables fold the empty window (OVER ()) into
	// "OVER LP frame_opt RP": rh3 is then a frame-spec string
	// rather than a *sql.WindowDef (rule 411 never reduces for the
	// empty case). Accept both shapes.
	if wd := getWindowDef(getRHS(p, ruleNo, 3)); wd != nil {
		return wd
	}
	return &sql.WindowDef{
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 3)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 3)),
	}

}

// Rule 345: over_clause ::= OVER nm
func rule345(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{Name: getString(getRHS(p, ruleNo, 2))}

}

// Rule 346: filter_clause ::= FILTER LP WHERE expr RP
func rule346(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 4))

}

// Rule 348: input ::= cmdlist
func rule348(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 349: cmdlist ::= cmdlist ecmd
func rule349(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 350: cmdlist ::= ecmd
func rule350(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

func rule351(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 352: ecmd ::= cmdx SEMI
func rule352(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 353: ecmd ::= explain cmdx SEMI (EXPLAIN)
func rule353(ruleNo int, p *Parser) interface{} {
	queryPlan := false
	if b, ok := getRHS(p, ruleNo, 1).(bool); ok {
		queryPlan = b
	}
	return &sql.ExplainStmt{
		Statement: getStmt(getRHS(p, ruleNo, 2)),
		QueryPlan: queryPlan,
	}

}

// Rule 354: trans_opt ::=
func rule354(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 355: trans_opt ::= TRANSACTION
func rule355(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 359: cmd ::= create_table create_table_args
func rule359(ruleNo int, p *Parser) interface{} {
	ct, _ := getRHS(p, ruleNo, 1).(*sql.CreateTableStmt)
	args := getRHS(p, ruleNo, 2)
	if ct != nil {
		if cta, ok := args.(*createTableArgs); ok {
			ct.Columns = cta.columns
			ct.Constraints = cta.constraints
			ct.WithoutRowid = cta.withoutRowid
			ct.Strict = cta.strict
		} else if cols, ok := args.([]sql.ColumnDef); ok {
			ct.Columns = cols
		} else if ct2, ok := args.(*sql.CreateTableStmt); ok && ct2 != nil {
			ct.Columns = ct2.Columns
			ct.AsSelect = ct2.AsSelect
		}
	}
	if ct != nil {
		return ct
	}
	return getRHS(p, ruleNo, 1)

}

// Rule 360: table_option_set ::= table_option
func rule360(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 361: columnlist ::= columnlist COMMA columnname carglist
func rule361(ruleNo int, p *Parser) interface{} {
	acc := getColumnList(getRHS(p, ruleNo, 1))
	col := getColumnDef(getRHS(p, ruleNo, 3))
	mergeColumnConstraints(&col, getColumnList(getRHS(p, ruleNo, 4)))
	return append(acc, col)

}

func rule362(ruleNo int, p *Parser) interface{} {
	col := getColumnDef(getRHS(p, ruleNo, 1))
	mergeColumnConstraints(&col, getColumnList(getRHS(p, ruleNo, 2)))
	return []sql.ColumnDef{col}

}

// Rule 363: nm ::= ID|INDEXED|JOIN_KW
func rule363(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 364: nm ::= STRING
func rule364(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 365: typetoken ::= typename
func rule365(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 366: typename ::= ID|STRING
func rule366(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 369: carglist ::= carglist ccons
func rule369(ruleNo int, p *Parser) interface{} {
	acc := getColumnList(getRHS(p, ruleNo, 1))
	if c, ok := getRHS(p, ruleNo, 2).(sql.ColumnDef); ok {
		acc = append(acc, c)
	}
	return acc

}

// Rule 370: carglist ::=
func rule370(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 371: ccons ::= AS generated
func rule371(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 372: ccons ::= GENERATED ALWAYS AS generated
func rule372(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 4))}

}

// Rule 373: ccons ::= AS generated
func rule373(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 374: conslist_opt ::= COMMA conslist
func rule374(ruleNo int, p *Parser) interface{} {
	return getConstraintSlice(getRHS(p, ruleNo, 2))

}

func rule375(ruleNo int, p *Parser) interface{} {
	acc := getConstraintSlice(getRHS(p, ruleNo, 1))
	tc, _ := getRHS(p, ruleNo, 3).(sql.TableConstraint)
	// Attach a preceding CONSTRAINT-name marker.
	if len(acc) > 0 && acc[len(acc)-1].Type == "" && tc.Type != "" {
		tc.Name = acc[len(acc)-1].Name
		acc = acc[:len(acc)-1]
	}
	if tc.Type != "" || tc.Name != "" {
		acc = append(acc, tc)
	}
	return acc

}

// Rule 376: conslist ::= tcons
func rule376(ruleNo int, p *Parser) interface{} {
	return getConstraintSlice(getRHS(p, ruleNo, 1))

}

// Rule 377: tconscomma ::= (empty)
func rule377(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 379: resolvel ::= ROLLBACK|ABORT|FAIL
func rule379(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 380: selectnowith ::= oneselect (already handled, but keep for pass-through)
func rule380(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 381: oneselect ::= values
func rule381(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 1))
	if sel != nil {
		sel.ValuesChain = true
	}
	return sel

}

// Rule 383: as ::= ID|STRING
func rule383(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

func rule385(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 386: expr ::= term
func rule386(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

	// Rule 387: likeop ::= LIKE_KW|MATCH
}

func rule387(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		switch strings.ToUpper(tok.Value) {
		case "MATCH":
			return "MATCH"
		case "GLOB":
			return "GLOB"
		case "REGEXP":
			return "REGEXP"
		}
	}
	return "LIKE"

}

func rule389(ruleNo int, p *Parser) interface{} {
	return getExprList(getRHS(p, ruleNo, 1))

}

// Rule 395: plus_num ::= INTEGER|FLOAT
func rule395(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return &sql.NumericLit{Value: tok.Value}
	}
	return getRHS(p, ruleNo, 1)

}

// Rule 403: vtabarglist ::= vtabarg
func rule403(ruleNo int, p *Parser) interface{} {
	return []string{getString(getRHS(p, ruleNo, 1))}

}

// Rule 404: vtabarglist ::= vtabarglist COMMA vtabarg
func rule404(ruleNo int, p *Parser) interface{} {
	head := getStringList(getRHS(p, ruleNo, 1))
	arg := getString(getRHS(p, ruleNo, 3))
	return append(head, arg)

}

// Rule 405: vtabarg ::= vtabarg token
func rule405(ruleNo int, p *Parser) interface{} {
	return strings.TrimSpace(getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2)))

}

// Rule 409: with ::=
func rule409(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 410: windowdefn_list ::= windowdefn
func rule410(ruleNo int, p *Parser) interface{} {
	wd := getWindowDef(getRHS(p, ruleNo, 1))
	if wd != nil {
		return []sql.WindowDef{*wd}
	}
	return nil

}

func rule411(ruleNo int, p *Parser) interface{} {
	// window ::= frame_opt — a bare frame spec with no PARTITION BY / ORDER BY.
	// The frame_opt value may be a *sql.WindowFrame (structured) or a string
	// (empty frame_opt, rule 325).
	if f := getWindowFrame(getRHS(p, ruleNo, 1)); f != nil {
		return &sql.WindowDef{FrameSpec: frameFromStruct(f), Frame: f}
	}
	return &sql.WindowDef{FrameSpec: getString(getRHS(p, ruleNo, 1))}

}
