package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// The EXPLAIN row stream (vdbe.c's EXPLAIN contract): a synthetic VDBE
// program, one row per opcode, for the statement's resolved plan. The
// tree-walking executor has no literal opcode stream, so the rows are
// derived from the SAME plan-node tree the EXPLAIN QUERY PLAN renderer
// emits — one opcode cluster per plan decision (open / position / emit /
// advance / sort / dedup). The consuming surfaces (the fixtures'
// `db eval "EXPLAIN ..."` callbacks and the explain() table-valued
// function, ext/misc/explain.c) read the columns addr, opcode, p1..p5 and
// comment exactly as SQLite's program does.

// explainRow is one synthetic opcode row: the EXPLAIN result columns.
type explainRow struct {
	addr    int64
	opcode  string
	p1, p2  int64
	p3      int64
	p4      string
	p5      int64
	comment string
}

// explainProgram accumulates rows and resolves the forward targets (the
// halt address Init jumps to, loop bounds) after the body is laid out.
type explainProgram struct {
	rows []explainRow
}

// emit appends one row and returns its address.
func (p *explainProgram) emit(opcode string, p1, p2, p3 int64, p4 string, p5 int64, comment string) int64 {
	addr := int64(len(p.rows))
	p.rows = append(p.rows, explainRow{addr: addr, opcode: opcode, p1: p1, p2: p2, p3: p3, p4: p4, p5: p5, comment: comment})
	return addr
}

// emitExplainRows builds the EXPLAIN row stream for stmt: the program frame
// (Init ... Halt/Transaction/Goto) around the plan nodes' opcode clusters.
func (e *SelectEngine) emitExplainRows(stmt sql.Stmt) *Result {
	prog := &explainProgram{}
	// Init occupies row 0 (C's aOp[0].opcode==OP_Init invariant,
	// resolveP2Values); its jump target and comment fill in after the body
	// is laid out and Halt's address is known.
	prog.emit("", 0, 0, 0, "", 0, "")
	switch stmt.(type) {
	case *sql.InsertStmt, *sql.UpdateStmt, *sql.DeleteStmt:
		e.emitDMLRows(prog, stmt)
	default:
		nodes, ok := e.planExplainNodes(stmt)
		if !ok {
			return nil
		}
		for _, n := range nodes {
			e.emitPlanNodeRows(prog, n, stmt)
		}
	}
	halt := int64(len(prog.rows))
	prog.emit("Halt", 0, 0, 0, "", 0, "")
	nTrans := int64(1)
	switch stmt.(type) {
	case *sql.UpdateStmt, *sql.InsertStmt, *sql.DeleteStmt:
		nTrans = 2 // a write statement's Transaction row carries a journal use
	}
	prog.emit("Transaction", 0, 0, nTrans, "", 1, "usesStmtJournal=0")
	prog.emit("Goto", 0, 1, 0, "", 0, "")
	prog.rows[0] = explainRow{addr: 0, opcode: "Init", p1: 0, p2: halt, comment: fmt.Sprintf("Start at %d", halt)}
	return explainRowsResult(prog.rows)
}

// explainRowsResult renders the rows as the EXPLAIN result columns.
func explainRowsResult(rows []explainRow) *Result {
	out := &Result{Columns: []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment"}}
	for _, r := range rows {
		out.Rows = append(out.Rows, []interface{}{
			r.addr, r.opcode, r.p1, r.p2, r.p3, r.p4, r.p5, r.comment,
		})
	}
	return out
}

// planExplainNodes derives the plan-node tree the stream renders. It shares
// the EQP derivation: the same dispatch, so the stream and the QUERY PLAN
// text cannot drift. ok is false for statements with no plan (schema
// errors surface as results from the EQP path instead). Write statements
// render through emitDMLRows (the write-cursor stream), not the plan-node
// walk.
func (e *SelectEngine) planExplainNodes(stmt sql.Stmt) ([]planNode, bool) {
	if s, ok := stmt.(*sql.SelectStmt); ok {
		nodes, errResult := e.explainQueryPlanSelectNodes(s)
		if errResult != nil {
			return nil, false
		}
		return nodes, true
	}
	return nil, false
}

// emitPlanNodeRows renders one plan node's opcode cluster (and, depth-first,
// its children's). The detail strings are the plan's canonical serialization
// (the EQP renders exactly them), so parsing keeps the two surfaces in lock
// step.
func (e *SelectEngine) emitPlanNodeRows(prog *explainProgram, n planNode, stmt sql.Stmt) {
	detail := n.detail
	switch {
	case detail == "SCAN CONSTANT ROW", detail == "COMPOUND QUERY",
		detail == "LEFT-MOST SUBQUERY", detail == "MULTI-INDEX OR",
		strings.HasPrefix(detail, "CO-ROUTINE "),
		isCompoundBranchLabel(detail), isSubqueryLabel(detail):
		// Structural labels and constant scans emit no opcode rows of
		// their own; the children carry the decisions.
		for _, c := range n.children {
			e.emitPlanNodeRows(prog, c, stmt)
		}
	case strings.HasPrefix(detail, "USE TEMP B-TREE FOR DISTINCT"):
		e.emitDistinctRows(prog)
		for _, c := range n.children {
			e.emitPlanNodeRows(prog, c, stmt)
		}
	case strings.HasPrefix(detail, "USE TEMP B-TREE FOR ORDER BY"):
		e.emitSorterRows(prog, orderSortTerms(stmt), false)
		for _, c := range n.children {
			e.emitPlanNodeRows(prog, c, stmt)
		}
	case strings.HasPrefix(detail, "USE TEMP B-TREE FOR GROUP BY"):
		e.emitSorterRows(prog, groupSortTerms(stmt), true)
		for _, c := range n.children {
			e.emitPlanNodeRows(prog, c, stmt)
		}
	default:
		e.emitScanRows(prog, detail)
		for _, c := range n.children {
			e.emitPlanNodeRows(prog, c, stmt)
		}
	}
}

// isCompoundBranchLabel reports whether the detail is a compound SELECT's
// set-operation branch label (compoundOpLabel's vocabulary).
func isCompoundBranchLabel(detail string) bool {
	switch detail {
	case "UNION ALL", "UNION USING TEMP B-TREE", "INTERSECT USING TEMP B-TREE", "EXCEPT USING TEMP B-TREE":
		return true
	}
	return false
}

// isSubqueryLabel reports whether the detail is a subquery plan label
// (planSubqueryNodes' "<CORRELATED >{SCALAR,EXISTS} SUBQUERY <n>" shapes).
func isSubqueryLabel(detail string) bool {
	for _, prefix := range []string{"SCALAR SUBQUERY ", "CORRELATED SCALAR SUBQUERY ", "EXISTS SUBQUERY ", "CORRELATED EXISTS SUBQUERY ", "LIST SUBQUERY ", "CORRELATED LIST SUBQUERY "} {
		if strings.HasPrefix(detail, prefix) {
			return true
		}
	}
	return false
}

// emitScanRows renders one scan/seek node's opcode cluster: the cursor opens
// (table first, then the serving index), the positioning decision, the row
// emission and the advance. C's cursor numbers allocate sequentially; p1
// values are not part of the asserted contract.
func (e *SelectEngine) emitScanRows(prog *explainProgram, detail string) {
	shape := parseScanDetail(detail)
	if shape == nil {
		return
	}
	switch {
	case shape.vtab:
		// Created virtual table: VOpen + VFilter (p4 carries the idxStr the
		// module handed back through xBestIndex — bestindexF's do_vtabsorter
		// reads lindex $vm [ii+4], the VFilter p4).
		prog.emit("VOpen", 0, 0, 0, "", 0, "")
		prog.emit("VFilter", 0, 0, 1, shape.vtabIdxStr, 0, "")
		prog.emit("VNext", 0, 0, 0, "", 0, "")
		return
	case shape.index == "INTEGER PRIMARY KEY":
		root := e.tableRootPage(shape.table)
		if root < 0 {
			return
		}
		prog.emit("OpenRead", 0, root, 0, e.tableRecordShape(shape.table), 0, openComment(shape.table, root))
		prog.emit("SeekRowid", 0, 0, 1, "", 0, "intkey=r[1]")
		prog.emit("ResultRow", 1, 1, 0, "", 0, "output=r[1..]")
		return
	}
	root := e.tableRootPage(shape.table)
	if root < 0 {
		return
	}
	prog.emit("OpenRead", 0, root, 0, e.tableRecordShape(shape.table), 0, openComment(shape.table, root))
	idxRoot := int64(-1)
	idxCols := 0
	if shape.index != "" && shape.index != "AUTOMATIC COVERING INDEX" && shape.index != "AUTOMATIC PARTIAL COVERING INDEX" {
		idxRoot, idxCols = e.indexRootPage(shape.table, shape.index)
	}
	if idxRoot >= 0 {
		prog.emit("OpenRead", 1, idxRoot, 0, e.indexRecordShape(shape.index, idxCols), 0, openComment(shape.index, idxRoot))
	}
	if shape.seek {
		// The seek positioning decision: an open-interval seek over the
		// index cursor with the loop's IdxGT guard (wherecode.c's seek
		// block), or the direct rowid pin for IPK loops.
		prog.emit("SeekGE", 1, 0, 1, "", 1, "key=r[1]")
		prog.emit("IdxGT", 1, 0, 1, "", 1, "key=r[1]")
		prog.emit("DeferredSeek", 1, 0, 0, "", 0, "Move 0 to 1.rowid if needed")
	} else {
		prog.emit("Rewind", 1, 0, 0, "", 0, "")
	}
	prog.emit("ResultRow", 1, 1, 0, "", 0, "output=r[1..]")
	prog.emit("Next", 1, 0, 0, "", 1, "")
}

// emitSorterRows renders the ORDER BY / GROUP BY sorter machinery: the
// sorter cursor opens first (C opens it before the scan cursors), one key
// input row per sort term, the insert/sort/data/next loop.
func (e *SelectEngine) emitSorterRows(prog *explainProgram, terms []sql.Expr, group bool) {
	if group && e.vtabExplainOrderConsumed {
		// The GROUP BY over a consumed ORDER BY (the WHERE_GROUPBY parity):
		// C's streaming aggregate groups through the ephemeral index — no
		// sorter is opened (bestindexF's 2.11 helper errors on SorterSort;
		// the want's IdxInsert = the ephemeral index's group-key insert).
		prog.emit("OpenEphemeral", 1, 0, 0, "k(1,B)", 0, "")
		for _, term := range terms {
			if cr, ok := term.(*sql.ColumnRef); ok {
				prog.emit("Column", 0, 0, 2, "", 0, "r[2]=cursor 0 column "+cr.Name)
			}
		}
		prog.emit("MakeRecord", 0, 2, 6, "", 0, "r[6]=key")
		prog.emit("IdxInsert", 1, 6, 1, "", 16, "key=r[2]")
		prog.emit("Found", 1, 0, 0, "", 0, "")
		return
	}
	prog.emit("SorterOpen", 2, 5, 0, "k(1,B)", 0, "")
	for _, term := range terms {
		if fn := sortTermFunction(term); fn != "" {
			prog.emit("Function", 0, 0, 2, fn, 0, "r[2]=func()")
			continue
		}
		if cr, ok := term.(*sql.ColumnRef); ok {
			prog.emit("Column", 0, 0, 1, "", 0, "r[1]="+"cursor 0 column "+cr.Name)
		}
	}
	prog.emit("SorterInsert", 2, 6, 0, "", 0, "key=r[6]")
	comment := ""
	if group {
		comment = "GROUP BY sort"
	}
	prog.emit("SorterSort", 2, 0, 0, "", 0, comment)
	prog.emit("SorterData", 2, 7, 3, "", 0, "r[7]=data")
	prog.emit("ResultRow", 3, 3, 0, "", 0, "output=r[3..]")
	prog.emit("SorterNext", 2, 0, 0, "", 0, "")
}

// emitDistinctRows renders C's UNORDERED DISTINCT machinery
// (select.c codeDistinct's default arm): the ephemeral dedup cursor, the
// Found probe (the skip decision), the record and its insertion.
func (e *SelectEngine) emitDistinctRows(prog *explainProgram) {
	prog.emit("OpenEphemeral", 1, 0, 0, "k(1,B)", 8, "nColumn=0")
	prog.emit("Found", 1, 0, 1, "", 1, "key=r[1]")
	prog.emit("MakeRecord", 1, 1, 2, "", 0, "r[2]=mkrec(r[1])")
	prog.emit("IdxInsert", 1, 2, 1, "", 16, "key=r[2]")
}

// orderSortTerms returns the ORDER BY terms of a top-level SELECT (the
// sorter's key inputs). Non-SELECT statements have no sorter terms.
func orderSortTerms(stmt sql.Stmt) []sql.Expr {
	if s, ok := stmt.(*sql.SelectStmt); ok && s != nil {
		out := make([]sql.Expr, 0, len(s.OrderBy))
		for _, ob := range s.OrderBy {
			out = append(out, ob.Expr)
		}
		return out
	}
	return nil
}

// groupSortTerms returns the GROUP BY terms of a top-level SELECT.
func groupSortTerms(stmt sql.Stmt) []sql.Expr {
	if s, ok := stmt.(*sql.SelectStmt); ok && s != nil {
		out := make([]sql.Expr, 0, len(s.GroupBy))
		out = append(out, s.GroupBy...)
		return out
	}
	return nil
}

// sortTermFunction renders a non-column sort term's Function p4 (C renders
// "random(0)" — the function name with its argument count) or "" for terms
// the sorter reads as plain columns.
func sortTermFunction(term sql.Expr) string {
	fn, ok := term.(*sql.FuncCall)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s(%d)", fn.Name, len(fn.Args))
}

// allOrderTermsConstant reports whether every ORDER BY term is a constant
// expression — sqlite3ExprIsConstant(0, term) parity: no column references,
// no bound parameters, no subqueries, and functions are constant only when
// deterministic (resolve.c's EP_ConstFunc) with constant arguments. An
// integer literal in 1..nCols is a POSITIONAL term in this engine's pipeline
// (resolveOrderByOrdinalTerms rewrites it at sort time; C resolves it in
// resolve.c before the constant test runs) — never constant.
func (e *SelectEngine) allOrderTermsConstant(terms []sql.OrderByTerm, nCols int) bool {
	if len(terms) == 0 {
		return false
	}
	for _, ob := range terms {
		if pos, _ := ordinalPosition(ob.Expr, nCols); pos > 0 {
			return false
		}
		if !e.exprIsConstantExpr(ob.Expr) {
			return false
		}
	}
	return true
}

// exprIsConstantExpr is the constant-expression test of one term (see
// allOrderTermsConstant).
func (e *SelectEngine) exprIsConstantExpr(expr sql.Expr) bool {
	switch v := expr.(type) {
	case nil, *sql.NumericLit, *sql.StringLit, *sql.NullLit, *sql.BlobLit:
		return true
	case *sql.ColumnRef, *sql.ParameterExpr, *sql.Subquery, *sql.ExistsExpr, *sql.InList:
		return false
	case *sql.ParenExpr:
		return e.exprIsConstantExpr(v.Expr)
	case *sql.UnaryOp:
		return e.exprIsConstantExpr(v.Operand)
	case *sql.BinaryOp:
		return e.exprIsConstantExpr(v.Left) && e.exprIsConstantExpr(v.Right)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if !e.exprIsConstantExpr(arg) {
				return false
			}
		}
		return e.funcIsDeterministic(v.Name)
	}
	return false
}

// funcIsDeterministic reports whether the named function carries the
// SQLITE_DETERMINISTIC flag (resolve.c sets EP_ConstFunc for it, which is
// what makes a function call constant for the ORDER BY elimination). An
// unknown function is never constant.
func (e *SelectEngine) funcIsDeterministic(name string) bool {
	if volatileBuiltinFuncs[strings.ToLower(name)] {
		return false
	}
	f, ok := e.ctx.Functions().Find(name)
	if !ok || f == nil {
		return false
	}
	if f.Deterministic {
		return true
	}
	return f.Builtin
}

// volatileBuiltinFuncs are the builtin functions C's FuncDef table marks
// VFUNCTION — their value varies between rows of one statement, so they are
// never EP_ConstFunc (random/randomblob are the where2-2.5 vs 2.6 pair).
var volatileBuiltinFuncs = map[string]bool{
	"random":            true,
	"randomblob":        true,
	"changes":           true,
	"total_changes":     true,
	"last_insert_rowid": true,
}

// scanDetailShape is the parsed plan-node detail: the objects the node
// opens and the positioning decision it represents.
type scanDetailShape struct {
	table      string
	index      string // "" = none; "INTEGER PRIMARY KEY" = the IPK loop
	seek       bool   // SEARCH positioning (vs a full walk)
	vtab       bool
	vtabIdxStr string
}

// parseScanDetail parses the EQP scan-node grammar into its objects:
//
//	SCAN <t>
//	SCAN <t> EXISTS
//	SCAN <t> USING [COVERING] INDEX <i>
//	SCAN <t> VIRTUAL TABLE INDEX <n>:<str>     (also 0x<n>:<str>)
//	SEARCH <t> [EXISTS] USING INDEX <i> [(conds)]
//	SEARCH <t> [EXISTS] USING INTEGER PRIMARY KEY [(conds)]
//	SEARCH <t> [EXISTS] USING [AUTOMATIC [PARTIAL ]COVERING INDEX ...]
//
// Returns nil for details that are not scan nodes (the caller's switch
// filters those; this remains the guard for unrecognized text).
func parseScanDetail(detail string) *scanDetailShape {
	rest := detail
	seek := false
	switch {
	case strings.HasPrefix(rest, "SCAN "):
		rest = rest[len("SCAN "):]
	case strings.HasPrefix(rest, "SEARCH "):
		rest = rest[len("SEARCH "):]
		seek = true
	default:
		return nil
	}
	shape := &scanDetailShape{seek: seek}
	rest = strings.TrimSuffix(rest, " EXISTS")
	// The virtual-table tail: "VIRTUAL TABLE INDEX <n>:<str>".
	if i := strings.Index(rest, " VIRTUAL TABLE INDEX "); i >= 0 {
		parseVtabTail(rest, i, shape)
		return shape
	}
	// The USING tails.
	if i := strings.Index(rest, " USING "); i >= 0 {
		shape.table = rest[:i]
		parseUsingTail(rest[i+len(" USING "):], shape)
		return shape
	}
	shape.table = rest
	return shape
}

// parseVtabTail fills a shape from the "VIRTUAL TABLE INDEX <n>:<str>" tail
// of a vtab scan node: the table name ends before the tail and the idxStr is
// everything after the separating colon.
func parseVtabTail(rest string, tail int, shape *scanDetailShape) {
	shape.table = rest[:tail]
	shape.vtab = true
	shape.vtabIdxStr = rest[tail+len(" VIRTUAL TABLE INDEX "):]
	if c := strings.IndexByte(shape.vtabIdxStr, ':'); c >= 0 {
		shape.vtabIdxStr = shape.vtabIdxStr[c+1:]
	}
}

// parseUsingTail fills a shape's index field from a scan node's "USING ..."
// tail (the EQP grammar's index-delivery forms).
func parseUsingTail(using string, shape *scanDetailShape) {
	switch {
	case strings.HasPrefix(using, "INTEGER PRIMARY KEY"):
		shape.index = "INTEGER PRIMARY KEY"
	case strings.HasPrefix(using, "COVERING INDEX "):
		shape.index = stripSeekConditions(strings.TrimPrefix(using, "COVERING INDEX "))
	case strings.HasPrefix(using, "INDEX "):
		shape.index = stripSeekConditions(strings.TrimPrefix(using, "INDEX "))
	case strings.HasPrefix(using, "AUTOMATIC COVERING INDEX"):
		shape.index = "AUTOMATIC COVERING INDEX"
	case strings.HasPrefix(using, "AUTOMATIC PARTIAL COVERING INDEX"):
		shape.index = "AUTOMATIC PARTIAL COVERING INDEX"
	default:
		// "USING <label> <conditions>" (skip-scan / unusual labels):
		// the label's INDEX token if recognizable, else no index rows.
		fields := strings.SplitN(using, " ", 2)
		if len(fields) > 1 && fields[0] == "INDEX" {
			shape.index = stripSeekConditions(fields[1])
		}
	}
}

// stripSeekConditions removes a SEARCH node's trailing "(<constraints>)"
// suffix from an index name: the detail renders "USING INDEX <i> (x=?)" and
// the constraint text is not part of the schema object name.
func stripSeekConditions(name string) string {
	if strings.HasSuffix(name, ")") {
		if i := strings.LastIndex(name, " ("); i >= 0 {
			return name[:i]
		}
	}
	return name
}

// tableRootPage resolves a table's (or view-less name's) root page through
// the multi-schema lookup. -1 = unresolvable (schema rows, aliases of
// materialized subqueries): the stream renders no open for it.
func (e *SelectEngine) tableRootPage(name string) int64 {
	entry, _, err := e.ctx.FindTable(name)
	if err != nil || entry == nil {
		return -1
	}
	if entry.RootPage <= 0 {
		return -1
	}
	return int64(entry.RootPage)
}

// indexRootPage resolves a named index's root page and key-column count on
// the given table. -1 = not found (automatic indexes have no schema row).
func (e *SelectEngine) indexRootPage(tableName, indexName string) (int64, int) {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return -1, 0
	}
	for _, entry := range entries {
		if entry.Type == "index" && strings.EqualFold(entry.Name, indexName) && strings.EqualFold(entry.TblName, tableName) {
			cols := len(e.indexColumns(indexName))
			return int64(entry.RootPage), cols
		}
	}
	return -1, 0
}

// tableRecordShape renders the OpenRead p4 of a table cursor: C's
// sqlite3VdbeAddOp4Int carries the column count (vdbe.c OP_OpenRead's
// "root=.. iDb=0; <name>" comment and the integer p4).
func (e *SelectEngine) tableRecordShape(name string) string {
	entry, _, err := e.ctx.FindTable(name)
	if err != nil || entry == nil {
		return "0"
	}
	return fmt.Sprintf("%d", len(e.ctx.ParseColumnDefs(entry.Name, entry.SQL)))
}

// indexRecordShape renders the OpenRead p4 of an index cursor: C's KeyInfo
// rendering "k(<nKeyCol>,<collations>)" — BINARY columns render empty, other
// collations as their first letter.
func (e *SelectEngine) indexRecordShape(indexName string, nCols int) string {
	if indexName == "" || nCols <= 0 {
		return "k(1,B)"
	}
	colls := make([]string, 0, nCols)
	for _, col := range e.indexColumns(indexName) {
		c := ""
		if coll := e.indexColumnCollation("", indexName, col); coll != "" {
			c = coll[:1]
		}
		colls = append(colls, c)
	}
	return "k(" + fmt.Sprint(len(colls)) + "," + strings.Join(colls, ",") + ")"
}

// openComment renders C's OpenRead/OpenWrite comment: "root=<p2> iDb=0; <name>".
func openComment(name string, root int64) string {
	return fmt.Sprintf("root=%d iDb=0; %s", root, name)
}

// emitDMLRows renders a write statement's program frame: the target table
// opens for writing (OpenWrite), the positioning cluster rides the write
// cursor, the maintained indexes open (update.c aToOpen), and the row writes
// follow. INSERT ... SELECT renders the driving query's plan first (C
// compiles the sub-program before the insert machinery).
func (e *SelectEngine) emitDMLRows(prog *explainProgram, stmt sql.Stmt) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		e.emitInsertRows(prog, s)
	case *sql.DeleteStmt:
		e.emitDeleteRows(prog, s)
	case *sql.UpdateStmt:
		e.emitUpdateRows(prog, s)
	}
}

// emitInsertRows renders INSERT's stream: the driving query (if any), the
// write cursor, and one open + insertion per index (insert.c opens every
// index: aToOpen is all-ones on insert).
func (e *SelectEngine) emitInsertRows(prog *explainProgram, s *sql.InsertStmt) {
	if s.Select != nil {
		driveNodes, errResult := e.explainQueryPlanSelectNodes(s.Select)
		if errResult == nil {
			for _, n := range driveNodes {
				e.emitPlanNodeRows(prog, n, s.Select)
			}
		}
	}
	e.emitTableWriteOpen(prog, s.Table)
	for _, idx := range e.tableIndexNames(s.Table) {
		e.emitIndexWriteOpen(prog, s.Table, idx)
		prog.emit("IdxInsert", 0, 0, 1, "", 0, "key=r[1]")
	}
	prog.emit("IdxInsert", 0, 0, 1, "", 0, "key=r[1]")
}

// emitDeleteRows renders DELETE's stream: the write cursor, the positioning
// cluster, and one open + deletion per index (delete.c opens every index —
// every deleted row's entries must go).
func (e *SelectEngine) emitDeleteRows(prog *explainProgram, s *sql.DeleteStmt) {
	e.emitTableWriteOpen(prog, s.Table)
	for _, idx := range e.tableIndexNames(s.Table) {
		e.emitIndexWriteOpen(prog, s.Table, idx)
	}
	e.emitDMLScanRows(prog, s.Table, s.Where, 1)
	prog.emit("IdxDelete", 0, 0, 1, s.Table, 0, "key=r[1]")
}

// emitUpdateRows renders UPDATE's stream: the write cursor, the positioning
// cluster, and one maintenance cluster per index the update maintains
// (update.c's compile-time aToOpen decision) — the old entry deletes and the
// new one inserts. The OpenWrite rows carry the maintained index's root page;
// the explain() TVF's rootpage=explain.p2 join (indexexpr2-4.2xx) reads
// exactly them.
func (e *SelectEngine) emitUpdateRows(prog *explainProgram, s *sql.UpdateStmt) {
	e.emitTableWriteOpen(prog, s.Table)
	for _, idx := range e.updateMaintainedIndexes(s) {
		e.emitIndexWriteOpen(prog, s.Table, idx)
		prog.emit("IdxDelete", 0, 0, 1, idx, 0, "key=r[1]")
		prog.emit("IdxInsert", 0, 0, 1, "", 0, "key=r[1]")
	}
	e.emitDMLScanRows(prog, s.Table, s.Where, 2)
}

// emitDMLScanRows renders the statement's driving scan (the plan node the EQP
// renders — explainDMLPlanNodes' first node) positioned on the write cursor,
// then the FK child-check scans as read-cursor clusters (C compiles the FK
// scans against their own cursors).
func (e *SelectEngine) emitDMLScanRows(prog *explainProgram, table string, where sql.Expr, childScans int) {
	nodes := e.explainDMLPlanNodes(table, where, childScans)
	if len(nodes) == 0 {
		return
	}
	e.emitWriteScanCluster(prog, table, nodes[0].detail)
	for _, n := range nodes[1:] {
		e.emitScanRows(prog, n.detail)
	}
}

// emitWriteScanCluster renders the positioning decision of the driving scan
// on the already-opened write cursor (the scan detail analysis shared with
// the EQP).
func (e *SelectEngine) emitWriteScanCluster(prog *explainProgram, tableName, detail string) {
	shape := parseScanDetail(detail)
	if shape == nil {
		return
	}
	if shape.index == "INTEGER PRIMARY KEY" {
		prog.emit("SeekRowid", 0, 0, 1, "", 0, "intkey=r[1]")
		return
	}
	if shape.seek {
		prog.emit("SeekGE", 0, 0, 1, "", 1, "key=r[1]")
		prog.emit("IdxGT", 0, 0, 1, "", 1, "key=r[1]")
		prog.emit("DeferredSeek", 0, 0, 0, "", 0, "Move 0 to 0.rowid if needed")
		return
	}
	prog.emit("Rewind", 0, 0, 0, "", 0, "")
	prog.emit("Next", 0, 0, 0, "", 1, "")
}

// tableIndexNames returns the index names of the table in schema order (the
// maintenance-cursor set of insert.c/delete.c).
func (e *SelectEngine) tableIndexNames(tableName string) []string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		if ent.Type == "index" && strings.EqualFold(ent.TblName, tableName) {
			out = append(out, ent.Name)
		}
	}
	return out
}

// emitIndexWriteOpen renders one index write cursor's OpenWrite.
func (e *SelectEngine) emitIndexWriteOpen(prog *explainProgram, tableName, indexName string) {
	root, cols := e.indexRootPage(tableName, indexName)
	if root < 0 {
		return
	}
	prog.emit("OpenWrite", prog.nextCursor(), root, 0, e.indexRecordShape(indexName, cols), 0, openComment(indexName, root))
}

// nextCursor returns the next synthetic cursor number (the row count so far
// is a stable per-program sequence).
func (p *explainProgram) nextCursor() int64 { return int64(len(p.rows)) }

// updateMaintainedIndexes returns the names of the indexes an UPDATE
// maintains, by update.c's compile-time decision (update.c:564-585): every
// index is maintained when the statement changes a key column (chngKey); an
// individual index is maintained when its partial WHERE might change values
// (indexWhereClauseMightChange) or when any of its indexed keys — a plain
// column or an expression — references a changed column
// (indexColumnIsBeingUpdated). This is the plan-side twin of the executor's
// per-row maintenance decision (execdml maintainedUpdateIndexes); the EXPLAIN
// stream renders the compile-time set the C program opens.
func (e *SelectEngine) updateMaintainedIndexes(s *sql.UpdateStmt) []string {
	entry, _, err := e.ctx.FindTable(s.Table)
	if err != nil || entry == nil {
		return nil
	}
	changed := map[string]bool{}
	for _, a := range s.Assignments {
		changed[strings.ToLower(a.Column)] = true
	}
	entries, err2 := e.ctx.Schema().GetEntries("")
	if err2 != nil {
		return nil
	}
	keyChanged := e.updateChangesKey(entry, changed)
	var out []string
	for _, ent := range entries {
		if ent.Type != "index" || !strings.EqualFold(ent.TblName, s.Table) {
			continue
		}
		if keyChanged || e.indexWhereMightChange(ent, changed) || updateIndexKeyTouched(e.indexEntryColumns(ent), changed) {
			out = append(out, ent.Name)
		}
	}
	return out
}

// updateChangesKey reports the statement's chngKey flag (update.c): the rowid
// changed, or a PRIMARY KEY column changed (the rowid alias for rowid tables,
// the declared PK for WITHOUT ROWID tables). chngKey maintains every index.
func (e *SelectEngine) updateChangesKey(entry *schema.Entry, changed map[string]bool) bool {
	if changed["rowid"] {
		return true
	}
	for _, def := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if def.PrimaryKey && changed[strings.ToLower(def.Name)] {
			return true
		}
	}
	return false
}

// indexWhereMightChange reports whether the index's partial WHERE clause
// might change values due to the UPDATE (update.c
// indexWhereClauseMightChange): the predicate references a changed column.
func (e *SelectEngine) indexWhereMightChange(ent *schema.Entry, changed map[string]bool) bool {
	if ent.SQL == "" {
		return false // automatic indexes carry no partial predicate
	}
	wm := indexWhereRe.FindStringSubmatch(ent.SQL)
	if wm == nil {
		return false
	}
	return exprReferencesChangedCols(wm[1], changed)
}

// updateIndexKeyTouched reports whether any indexed key of the UPDATE target
// is a changed column or an expression referencing one (update.c
// indexColumnIsBeingUpdated).
func updateIndexKeyTouched(keys []string, changed map[string]bool) bool {
	for _, key := range keys {
		if isIdentifier(key) {
			if changed[strings.ToLower(key)] {
				return true
			}
			continue
		}
		if exprReferencesChangedCols(key, changed) {
			return true
		}
	}
	return false
}

// exprReferencesChangedCols reports whether the index expression text
// references any changed column (update.c sqlite3ExprReferencesUpdatedColumn's
// TK_COLUMN walk, at the tokenizer level): an identifier token matching a
// changed column name. Function names (identifier followed by "(") and
// COLLATE arguments are not column references.
func exprReferencesChangedCols(exprText string, changed map[string]bool) bool {
	tok := sql.NewTokenizer(exprText)
	afterCollate := false
	for {
		t := tok.Next()
		switch t.Type {
		case sql.TokenEOF, sql.TokenError:
			return false
		case sql.TokenKeyword:
			afterCollate = strings.EqualFold(t.Value, "COLLATE")
			continue
		case sql.TokenIdentifier:
			if afterCollate {
				afterCollate = false
				continue // a collation name, never a column reference
			}
			afterCollate = false
			if !changed[strings.ToLower(t.Value)] {
				continue
			}
			if t.End < len(exprText) && exprText[t.End] == '(' {
				continue // a function name, not a column reference
			}
			return true
		}
	}
}

// emitTableWriteOpen renders the write cursor's OpenWrite for a DML target
// table (C opens DELETE/UPDATE/INSERT targets for writing: vdbeaux.c
// OP_OpenWrite).
func (e *SelectEngine) emitTableWriteOpen(prog *explainProgram, tableName string) {
	root := e.tableRootPage(tableName)
	if root < 0 {
		return
	}
	prog.emit("OpenWrite", 0, root, 0, e.tableRecordShape(tableName), 0, openComment(tableName, root))
}
