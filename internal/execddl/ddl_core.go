// Package exec implements query execution.
//
// This file holds core DDL execution: CREATE TABLE (with AS SELECT), DROP
// TABLE/VIEW/INDEX, ATTACH/DETACH, auto-index creation, and the generic
// SELECT/expression serializers used by stored objects. It is the
// CREATE/DROP/ATTACH half of the former ddl.go, split out so that each file
// stays within the repository's complexity and size budgets. Trigger, view,
// and virtual-table creation lives in ddl_trigger.go.
package execddl

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/execdml"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// authorizeActionOrSkip runs the SQL authorizer for an action and returns a
// terminal Result when the authorizer DENIES (error) or IGNOREs (silently skip
// the operation). Returns nil to continue when the action is allowed. SQLite's
// authorizer IGNORE result means "skip this operation without error" for
// DDL (DROP/ALTER/ATTACH/DETACH), matching auth-1.23.1/1.255/1.353.
func (e *DDLExecutor) authorizeActionOrSkip(action auth.Action, arg1, arg2, arg3, arg4 string) *Result {
	switch e.ctx.AuthorizeResult(action, arg1, arg2, arg3, arg4) {
	case auth.ResultDeny:
		return &Result{Error: fmt.Errorf("not authorized")}
	case auth.ResultIgnore:
		return &Result{}
	default:
		return nil
	}
}

// execAttach implements ATTACH DATABASE.
func (e *DDLExecutor) execAttach(s *sql.AttachStmt) *Result {
	// SQLITE_IGNORE on SQLITE_ATTACH silently skips the attach (auth-1.255
	// returns IGNORE and the database is not attached); DENY errors.
	if res := e.authorizeActionOrSkip(auth.ActionAttach, s.Path, s.Schema, "", ""); res != nil {
		return res
	}
	schemaUpper := strings.ToUpper(s.Schema)

	// Check reserved names: "main", "temp", "temporary" are always in use
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}
	// Check for duplicate attachment
	if _, ok := e.ctx.Databases()[schemaUpper]; ok {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}
	if e.attachedDBCount() >= MaxAttachedDatabases {
		return &Result{Error: fmt.Errorf("too many attached databases - max %d", MaxAttachedDatabases)}
	}
	if schemaUpper == "SQLITE_MASTER" || schemaUpper == "SQLITE_SCHEMA" {
		return &Result{Error: fmt.Errorf("reserved schema name: %s", s.Schema)}
	}

	path, isMemory := resolveAttachPath(e, s)
	pg, res := openAttachPager(path, isMemory)
	if res != nil {
		return res
	}
	// Initialize schema for the attached database
	sch := schema.NewManager(pg)
	if err := sch.Init(); err != nil {
		pg.Close()
		return &Result{Error: fmt.Errorf("cannot initialize schema for attached database: %w", err)}
	}
	// Record the file state at attach time so later external writes (from
	// another connection) are detected and the schema re-read.
	sch.SetTrackExternalMod(true)
	sch.CaptureFileStamp()

	// Check text encoding compatibility
	if res := e.checkAttachEncoding(pg); res != nil {
		pg.Close()
		return res
	}

	ctx := &DatabaseContext{
		Name:     s.Schema,
		Pager:    pg,
		Schema:   sch,
		FilePath: path,
		IsMemory: isMemory,
		IsTemp:   false,
	}

	e.ctx.Databases()[schemaUpper] = ctx
	e.ctx.AppendDBList(ctx)
	// Inherit MAIN's current secure_delete value for the new Btree
	// (mirrors src/attach.c:207-208 sqlite3BtreeSecureDelete inheritance
	// from db->aDb[0].pBt).
	e.inheritSecureDeleteOnAttach(schemaUpper)
	return &Result{}
}

// inheritSecureDeleteOnAttach records the secure_delete value inherited from
// MAIN for a newly attached DB. Mirrors src/attach.c:207-208 where the new
// Btree inherits the secure_delete setting of db->aDb[0].pBt. Called from
// execAttach after the DBContext is registered.
func (e *DDLExecutor) inheritSecureDeleteOnAttach(schemaUpper string) {
	v := e.ctx.MainSecureDelete()
	e.ctx.SetPerSchemaSecureDelete(schemaUpper, v)
}

// attachedDBCount counts attached databases (excluding main/temp/temporary).
func (e *DDLExecutor) attachedDBCount() int {
	count := 0
	for _, ctx := range e.ctx.Databases() {
		upper := strings.ToUpper(ctx.Name)
		if upper != "MAIN" && upper != "TEMP" && upper != "TEMPORARY" {
			count++
		}
	}
	return count
}

// resolveAttachPath resolves the database path for ATTACH, evaluating a path
// expression when given, and detects in-memory databases (including
// mode=memory URI filenames).
func resolveAttachPath(e *DDLExecutor, s *sql.AttachStmt) (string, bool) {
	path := s.Path
	if path == "" && s.PathExpr != nil {
		evalPath, err := e.ctx.EvalExpr(s.PathExpr, RowMap{})
		if err == nil {
			if sp, ok := evalPath.(string); ok {
				path = sp
			} else if evalPath != nil {
				path = fmt.Sprintf("%v", evalPath)
			}
		}
	}
	isMemory := path == "" || path == ":memory:"
	// SQLite URI filenames: `file:PATH?mode=memory` (and `?mode=memory&...`)
	// open an in-memory database regardless of the PATH component (attach
	// test 11.1 uses printf('file:%09000x/x.db?mode=memory&cache=shared',1)
	// to build a long URI). Treat mode=memory URIs as in-memory.
	if strings.Contains(path, "?mode=memory") || strings.Contains(path, "&mode=memory") {
		isMemory = true
	}
	// Strip the URI query string from a non-memory file path: SQLite's
	// `file:path?param=value` URIs carry VFS hints (e.g. 8_3_names=1) that
	// are not part of the file name (8_3_names-4.0 ATTACHes
	// 'file:./test2.db?8_3_names=1').
	if !isMemory {
		if q := strings.Index(path, "?"); q >= 0 {
			path = path[:q]
		}
		// Strip the `file:` URI scheme prefix (SQLite URI filenames): the
		// remaining string is a plain file path.
		path = strings.TrimPrefix(path, "file:")
	}
	return path, isMemory
}

// openAttachPager opens the pager for an ATTACHed database.
func openAttachPager(path string, isMemory bool) (*pager.Pager, *Result) {
	if isMemory {
		return pager.OpenInMemory(pager.DefaultPageSize), nil
	}
	pg, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		return nil, &Result{Error: fmt.Errorf("unable to open database: %s", path)}
	}
	return pg, nil
}

// checkAttachEncoding verifies the attached database uses the same text
// encoding as the main database (SQLite requires this). A brand-new attached
// file whose header is still zeroed (TextEncoding 0 = unspecified) adopts the
// main database's encoding, so it is not rejected.
func (e *DDLExecutor) checkAttachEncoding(pg *pager.Pager) *Result {
	hdr := pg.Header()
	if hdr == nil {
		return nil
	}
	dh, err := storage.ParseHeader(hdr)
	if err != nil {
		return nil
	}
	mainEncNum := encodingNum(e.ctx.TextEncoding())
	// A fresh file (never written) has TextEncoding 0 — it adopts main's
	// encoding rather than conflicting with it.
	if dh.TextEncoding == 0 {
		return nil
	}
	if dh.TextEncoding != mainEncNum {
		return &Result{Error: fmt.Errorf("attached databases must use the same text encoding as main database")}
	}
	return nil
}

// encodingNum converts an encoding name to its numeric value.
func encodingNum(enc string) uint32 {
	switch enc {
	case "UTF-8":
		return 1
	case "UTF-16le":
		return 2
	case "UTF-16be":
		return 3
	}
	return 0
}

// execDetach implements DETACH DATABASE.
func (e *DDLExecutor) execDetach(s *sql.AttachStmt) *Result {
	// SQLITE_IGNORE on SQLITE_DETACH silently skips the detach (auth-1.259
	// returns IGNORE and the database stays attached); DENY errors.
	if res := e.authorizeActionOrSkip(auth.ActionDetach, "", s.Schema, "", ""); res != nil {
		return res
	}
	schemaName, res := detachSchemaName(e, s)
	if res != nil {
		return res
	}
	schemaUpper := strings.ToUpper(schemaName)

	// Validate: cannot detach main or temp
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("cannot detach database %s", schemaName)}
	}

	ctx, ok := e.ctx.Databases()[schemaUpper]
	if !ok {
		return &Result{Error: fmt.Errorf("no such database: %s", schemaName)}
	}

	// A database that is the destination of an active backup is locked and
	// cannot be detached (SQLite: "database %s is locked").
	if e.ctx.BackupLocked(schemaName) {
		return &Result{Error: fmt.Errorf("database %s is locked", schemaName)}
	}

	// Close the pager and remove from map
	if err := ctx.Pager.Close(); err != nil {
		return &Result{Error: fmt.Errorf("error closing database %s: %w", s.Schema, err)}
	}

	delete(e.ctx.Databases(), schemaUpper)
	// Remove from the ordered attach list.
	for i, c := range e.ctx.DBList() {
		if c == ctx {
			e.ctx.RemoveDBListIndex(i)
			break
		}
	}
	return &Result{}
}

// detachSchemaName evaluates the DETACH argument as a scalar expression when
// a schema expression is given (SQLite treats it as an expression: DETACH
// (SELECT 1)+2 detaches "3"). Returns a Result error when the argument is a
// multi-column row value or a subquery returning more than one column.
//
// The parser already extracts literal names (bare identifiers, strings,
// numbers) into s.Schema; use that directly and skip expression evaluation to
// avoid treating a bare schema name as a column reference. Complex
// expressions (e.g. DETACH (SELECT 1)+2) leave s.Schema empty and are
// evaluated here.
func detachSchemaName(e *DDLExecutor, s *sql.AttachStmt) (string, *Result) {
	if s.Schema != "" {
		return s.Schema, nil
	}
	if s.SchemaExpr == nil {
		return "", nil
	}
	// SQLite rejects multi-column row values and subqueries returning more
	// than one column in the DETACH argument.
	if subq, ok := s.SchemaExpr.(*sql.Subquery); ok {
		if n := e.ctx.SubqueryColumnCount(subq.Select); n != 1 {
			return "", &Result{Error: fmt.Errorf("sub-select returns %d columns - expected 1", n)}
		}
	}
	if err := e.ctx.ValidateRowValueUse(s.SchemaExpr, false); err != nil {
		return "", &Result{Error: err}
	}
	v, err := e.ctx.EvalExpr(s.SchemaExpr, RowMap{})
	if err != nil {
		return "", &Result{Error: err}
	}
	if v == nil {
		return "", nil
	}
	if str, ok := util.UnwrapColumnValue(v).(string); ok {
		return str, nil
	}
	return fmt.Sprintf("%v", util.UnwrapColumnValue(v)), nil
}

// hasBindParameter reports whether a SQL statement contains a bound
// parameter placeholder (?NNN, ?name, :name, @name, $name) outside string
// literals. SQLite rejects these in trigger bodies at CREATE time with
// "trigger cannot use variables".
func hasBindParameter(sqlStr string) bool {
	i := 0
	for i < len(sqlStr) {
		switch sqlStr[i] {
		case '\'', '"', '`':
			// Skip string literals (and quoted identifiers) so ? inside
			// them is not counted.
			i = skipQuoted(sqlStr, i, sqlStr[i])
		case '[':
			// Skip bracket-quoted identifiers.
			i = skipBracketIdent(sqlStr, i)
		case '-':
			// Skip line comments.
			i = skipMaybeComment(sqlStr, i)
		case '?':
			// Any ? outside a string literal is a bind parameter placeholder
			// (bare ? or ?NNN/?name). SQLite rejects all of them in triggers.
			return true
		case ':', '@', '$':
			// Named parameters :name, @name, $name (also :1, @1, $1).
			if isNamedParamStart(sqlStr, i) {
				return true
			}
			i++
		default:
			i++
		}
	}
	return false
}

// isNamedParamStart reports whether sqlStr[i:] begins a named parameter
// (:name, @name, $name, :1, ...).
func isNamedParamStart(sqlStr string, i int) bool {
	return i+1 < len(sqlStr) && (isIdentStart(sqlStr[i+1]) || isDigit(sqlStr[i+1]))
}

// skipBracketIdent advances past a bracket-quoted identifier.
func skipBracketIdent(sqlStr string, i int) int {
	for i < len(sqlStr) && sqlStr[i] != ']' {
		i++
	}
	return i + 1
}

// skipMaybeComment advances past a "--" line comment, or one character when
// the dash is not the start of a comment.
func skipMaybeComment(sqlStr string, i int) int {
	if i+1 < len(sqlStr) && sqlStr[i+1] == '-' {
		for i < len(sqlStr) && sqlStr[i] != '\n' {
			i++
		}
		return i
	}
	return i + 1
}

// skipQuoted advances past a quoted string literal (or quoted identifier),
// honoring doubled quotes and backslash escapes.
func skipQuoted(sqlStr string, i int, quote byte) int {
	i++
	for i < len(sqlStr) {
		if sqlStr[i] == quote {
			if i+1 < len(sqlStr) && sqlStr[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		if sqlStr[i] == '\\' && i+1 < len(sqlStr) {
			i += 2
			continue
		}
		i++
	}
	return i
}

// defaultContainsNonConstant reports whether a DEFAULT expression contains a
// non-constant element: a bind parameter, RAISE, or a real column reference
// (CURRENT_TIME/DATE/TIMESTAMP and TRUE/FALSE are constant literals).
func defaultContainsNonConstant(expr sql.Expr) bool {
	found := false
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if found {
			return
		}
		switch v := n.(type) {
		case *sql.ParameterExpr, *sql.RaiseExpr, *sql.Subquery, *sql.ExistsExpr:
			found = true
		case *sql.ColumnRef:
			// "*" is the star argument of an aggregate (count(*)), not a
			// real column reference; DEFAULT(count(*)) is accepted at CREATE
			// and evaluated/rejected at INSERT time, matching SQLite.
			if v.Name != "*" && !isConstantDefaultColumnRef(v) {
				found = true
			}
		}
	})
	return found
}

// isConstantDefaultColumnRef reports whether a column reference in a DEFAULT
// expression is a constant literal (CURRENT_TIME/DATE/TIMESTAMP, TRUE/FALSE)
// rather than a real column reference (which makes the DEFAULT non-constant).
func isConstantDefaultColumnRef(v *sql.ColumnRef) bool {
	return strings.EqualFold(v.Name, "CURRENT_TIME") ||
		strings.EqualFold(v.Name, "CURRENT_DATE") ||
		strings.EqualFold(v.Name, "CURRENT_TIMESTAMP") ||
		strings.EqualFold(v.Name, "TRUE") ||
		strings.EqualFold(v.Name, "FALSE")
}

// findGeneratedColumnLoop detects circular generated-column definitions:
// a generated column whose expression (transitively) references another
// generated column back to itself. Returns the name of the column where the
// loop is detected ("" when there is no cycle), matching SQLite's
// "generated column loop on \"NAME\"" error.
func findGeneratedColumnLoop(colDefs []sql.ColumnDef) string {
	isGen := generatedColumnSet(colDefs)
	for _, cd := range colDefs {
		if cd.Generated == nil {
			continue
		}
		if loopAt, found := visitGeneratedRefs(cd.Name, colDefs, isGen, make(map[string]bool)); found {
			return loopAt
		}
	}
	return ""
}

// generatedColumnSet builds the set of generated column names.
func generatedColumnSet(colDefs []sql.ColumnDef) map[string]bool {
	isGen := make(map[string]bool)
	for _, cd := range colDefs {
		if cd.Generated != nil {
			isGen[cd.Name] = true
		}
	}
	return isGen
}

// visitGeneratedRefs follows generated-column references from name, returning
// the column where a cycle closes. On a cycle, SQLite reports the generated
// column that is defined LAST in the table (the column whose definition
// closes the loop when columns are processed in order), so the cycle's
// members are collected and the max-index one returned.
func visitGeneratedRefs(name string, colDefs []sql.ColumnDef, isGen map[string]bool, path map[string]bool) (string, bool) {
	if path[name] {
		return maxIndexCycleMember(name, colDefs, path), true
	}
	path[name] = true
	defer delete(path, name)
	for _, cd := range colDefs {
		if cd.Name != name || cd.Generated == nil {
			continue
		}
		var refs []string
		collectExprRefs(cd.Generated, &refs)
		for _, r := range refs {
			if isGen[r] {
				if loopAt, found := visitGeneratedRefs(r, colDefs, isGen, path); found {
					return loopAt, true
				}
			}
		}
	}
	return "", false
}

// maxIndexCycleMember returns the cycle member defined last in the table.
func maxIndexCycleMember(name string, colDefs []sql.ColumnDef, path map[string]bool) string {
	maxIdx := -1
	maxName := name
	for i, cd := range colDefs {
		if path[cd.Name] && i > maxIdx {
			maxIdx = i
			maxName = cd.Name
		}
	}
	return maxName
}

// execFTSDelete handles DELETE from an FTS virtual table.
func (e *DDLExecutor) execFTSDelete(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	// Set the FTS match context so the WHERE clause's MATCH expression
	// resolves to this table (execFTSSelect does the same).
	prevMatch := e.ctx.CurrentFTSMatch()
	e.ctx.SetCurrentFTSMatch(tableName)
	defer func() { e.ctx.SetCurrentFTSMatch(prevMatch) }()

	// A corrupt segment root makes the delete fail like a corrupted read.
	// %_content is not checked: DELETE matches against the in-memory index
	// and does not read the content btree (fts3corrupt4 25.1 pattern).
	if res := e.validateFTSSegmentsCheck(tableName, false, false); res != nil {
		return res
	}

	// Apply the WHERE filter first, then DELETE ... ORDER BY ... LIMIT (the
	// SQLite extension applies to the filtered rowid set).
	if s.Where == nil && s.Limit == nil && len(s.OrderBy) == 0 {
		// DELETE FROM <fts> (no WHERE/LIMIT/ORDER BY) clears the whole table
		// — SQLite's fts3DeleteAll drops the segment directory and resets the
		// in-memory index instead of deleting each document's postings one by
		// one (per-doc removal is O(total postings), making the automerge
		// test's between-scenario cleanup ~seconds for 500 40KB documents).
		// For a content=<table> table SQLite does NOT use fts3DeleteAll: the
		// DELETE goes through the per-doc xUpdate path, which skips docids
		// whose content row is missing (the delete terms cannot be computed)
		// — so a docid inserted into the index without a content row survives
		// (fts4content 3.1.4: DELETE FROM ft3 leaves docid 21 MATCH-able).
		if ftsTable.ContentTable() == "" && !ftsTable.Contentless() {
			changed := ftsTable.DocCount()
			ftsTable.Clear()
			e.clearFTSShadowIndex(tableName)
			e.clearFTSContent(tableName)
			e.writeFTSStat(tableName, ftsTable)
			return &Result{Changes: int64(changed)}
		}
	}
	matched := e.collectFTSMatched(ftsTable, colDefs, s)
	matched = e.applyFTSOrderLimit(matched, s)

	deleted := int64(0)
	for _, docID := range matched {
		// An FTS4 content=<table> table's xDelete computes the delete terms
		// from the content row; a docid whose content row is missing cannot
		// be deleted and is skipped (fts3.c fts3DeleteTerms; fts4content
		// 3.1.4: DELETE FROM ft3 leaves docid 21 indexed until its content
		// row exists).
		if ct := ftsTable.ContentTable(); ct != "" && !e.ctx.ContentRowExists(ct, docID) {
			continue
		}
		ftsTable.Delete(docID)
		// Remove the document row from the %_content shadow table so SELECT
		// FROM <name>_content reflects the deletion (fts3comp1 1.9: DELETE
		// FROM t1 WHERE docid=1 leaves only docids 3 and 4).
		e.deleteFTSContentRow(tableName, docID)
		e.deleteFTSDocsizeRow(tableName, docID)
		deleted++
	}
	if deleted > 0 {
		// The FTS4 %_stat aggregate (id=1) tracks the current document set;
		// a DELETE must refresh it (fts4aa 1.6: SELECT hex(value) FROM
		// t1_stat after a DELETE returns the updated totals).
		e.writeFTSStat(tableName, ftsTable)
	}
	return &Result{Changes: deleted}
}

// deleteFTSDocsizeRow removes one document row from an FTS table's %_docsize
// shadow table by docid (fts3.c fts3DeleteTerms/delete, SQL_DELETE_DOCSIZE).
func (e *DDLExecutor) deleteFTSDocsizeRow(tableName string, docID int64) {
	docsize := tableName + "_docsize"
	if _, _, err := e.ctx.FindTable(docsize); err != nil {
		return
	}
	_ = e.ctx.Exec(&sql.DeleteStmt{
		Table: docsize,
		Where: &sql.BinaryOp{
			Operator: "=",
			Left:     &sql.ColumnRef{Name: "docid"},
			Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", docID)},
		},
	})
}

// deleteFTSContentRow removes one document row from an FTS table's %_content
// shadow table by docid.
func (e *DDLExecutor) deleteFTSContentRow(tableName string, docID int64) {
	content := tableName + "_content"
	if _, _, err := e.ctx.FindTable(content); err != nil {
		return
	}
	execdml.BeginInternalShadowWrite()
	defer execdml.EndInternalShadowWrite()
	_ = e.ctx.Exec(&sql.DeleteStmt{
		Table: content,
		Where: &sql.BinaryOp{
			Operator: "=",
			Left:     &sql.ColumnRef{Name: "docid"},
			Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", docID)},
		},
	})
}

// ftsRowIDEqConstraint matches a WHERE clause of the exact form
// "rowid/docid/_rowid_/oid = <integer literal>" (either operand order, a
// unary-minus literal allowed). Returns the docid and true when the clause is
// such a constraint; false otherwise. Used as a fast path so single-row FTS
// DELETE/UPDATE statements avoid the O(n) per-statement document scan
// (fts4opt 2.x, fts4onepass 3.x).
func ftsRowIDEqConstraint(where sql.Expr) (int64, bool) {
	bop, ok := where.(*sql.BinaryOp)
	if !ok || bop.Operator != "=" {
		return 0, false
	}
	isRowIDRef := func(e sql.Expr) bool {
		ref, ok := e.(*sql.ColumnRef)
		if !ok || ref.Table != "" {
			return false
		}
		return execquery.IsRowIDName(ref.Name) || strings.EqualFold(ref.Name, "docid")
	}
	constInt := func(e sql.Expr) (int64, bool) {
		if n, ok := e.(*sql.NumericLit); ok {
			iv, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(n.Value, ".0")), 10, 64)
			if err == nil {
				return iv, true
			}
		}
		if u, ok := e.(*sql.UnaryOp); ok && u.Operator == "-" {
			if n, ok := u.Operand.(*sql.NumericLit); ok {
				iv, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(n.Value, ".0")), 10, 64)
				if err == nil {
					return -iv, true
				}
			}
		}
		return 0, false
	}
	if isRowIDRef(bop.Left) {
		if v, ok := constInt(bop.Right); ok {
			return v, true
		}
	}
	if isRowIDRef(bop.Right) {
		if v, ok := constInt(bop.Left); ok {
			return v, true
		}
	}
	return 0, false
}

// collectFTSMatched returns the docIDs matching an FTS DELETE's WHERE clause
// (all rows when the WHERE is nil). The row map carries the docid aliases and
// the actual column values (like ftsRowMapForDoc) so WHERE expressions on
// both resolve correctly (fts3comp1 1.8: DELETE FROM t1 WHERE docid = 1).
func (e *DDLExecutor) collectFTSMatched(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) []int64 {
	// Fast path: "WHERE rowid/docid = <const>" resolves by direct doc lookup
	// (fts3.c fts3SpecialDelete/fts3DeleteByRowid use the docid index). The
	// generic scan below builds a full row map per document — O(n) per
	// statement — which is quadratic over fts4opt 2.x's thousands of
	// single-row DELETEs.
	if id, ok := ftsRowIDEqConstraint(s.Where); ok && ftsTable.HasDoc(id) {
		return []int64{id}
	}
	var matched []int64
	for _, docID := range ftsTable.AllRowsMap() {
		shouldDelete := true
		if s.Where != nil {
			rowMap := e.ftsRowMapForDoc(ftsTable, colDefs, docID)
			match, err := e.ctx.EvalBool(s.Where, rowMap)
			if err != nil || !match {
				shouldDelete = false
			}
		}
		if shouldDelete {
			matched = append(matched, docID)
		}
	}
	return matched
}

// applyFTSOrderLimit applies ORDER BY, LIMIT, and OFFSET to an FTS docID set.
// Only rowid/asc/desc ORDER BY forms are meaningful; other expressions fall
// back to natural order.
func (e *DDLExecutor) applyFTSOrderLimit(matched []int64, s *sql.DeleteStmt) []int64 {
	if len(s.OrderBy) > 0 {
		if ob, ok := ftsOrderByRowID(s.OrderBy); ok {
			sort.SliceStable(matched, func(i, j int) bool {
				if ob.desc {
					return matched[i] > matched[j]
				}
				return matched[i] < matched[j]
			})
		}
	}
	limit := -1
	offset := 0
	if s.Limit != nil {
		if v, err := e.ctx.EvalConstInt(s.Limit); err == nil {
			limit = int(v)
		}
	}
	if s.Offset != nil {
		if v, err := e.ctx.EvalConstInt(s.Offset); err == nil {
			offset = int(v)
		}
	}
	return applyFTSSlice(matched, limit, offset)
}

// applyFTSSlice applies LIMIT n [OFFSET m] to a docID slice: keep the first n
// after skipping m.
func applyFTSSlice(matched []int64, limit, offset int) []int64 {
	if offset > len(matched) {
		return nil
	}
	matched = matched[offset:]
	if limit >= 0 && limit < len(matched) {
		matched = matched[:limit]
	}
	return matched
}

// execFTSUpdate handles UPDATE on an FTS virtual table. It evaluates the SET
// assignments against each matched row (using the rowid aliases rowid/docid/oid
// for docid changes and column names for content columns), then re-indexes the
// document. A docid change deletes the old document and inserts a new one under
// the new id (SQLite's FTS3 docid update semantics).
func (e *DDLExecutor) execFTSUpdate(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result {
	// Set the FTS match context so MATCH expressions in the WHERE clause
	// resolve to this table.
	prevMatch := e.ctx.CurrentFTSMatch()
	e.ctx.SetCurrentFTSMatch(tableName)
	defer func() { e.ctx.SetCurrentFTSMatch(prevMatch) }()

	// SQLite's fts3UpdateMethod writes through the vtab columns; an UPDATE
	// SET referencing a column the FTS table does not declare raises
	// "no such column: X" (fts4rename 1.2: UPDATE t1 SET Col0=1 fails).
	// Validate the SET targets against the FTS table's real columns plus
	// the docid alias (fts4rename.test).
	if bad := ftsUpdateUnknownColumn(colDefs, ftsTable, s); bad != "" {
		return &Result{Error: fmt.Errorf("no such column: %s", bad)}
	}

	// A corrupt segment root makes the update fail like a corrupted read.
	// %_content is not checked: UPDATE matches against the in-memory index
	// and does not read the content btree (fts3corrupt4 25.1 succeeds despite
	// a corrupt t1_content).
	if res := e.validateFTSSegmentsCheck(tableName, false, false); res != nil {
		return res
	}

	matched, matchRes := e.collectFTSUpdateMatched(ftsTable, colDefs, s)
	if matchRes != nil {
		return matchRes
	}
	matched = e.applyFTSUpdateOrderLimit(matched, s)

	updated := int64(0)
	for _, docID := range matched {
		res := e.updateFTSDoc(tableName, ftsTable, colDefs, s, docID)
		if res != nil {
			// UPDATE OR IGNORE: silently skip a row whose docid change
			// conflicts (SQLite ON CONFLICT IGNORE semantics).
			if strings.EqualFold(s.OnConflict, "IGNORE") && strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
				continue
			}
			return res
		}
		updated++
	}
	return &Result{Changes: updated}
}

// ftsUpdateUnknownColumn finds an FTS UPDATE SET target that does not name a
// column of the FTS table. Valid targets are the user columns plus the docid
// alias ("docid"/"rowid"). Returns the first unknown name, or "" when all are
// valid (fts4rename 1.2; SQLite fts3UpdateMethod rejects unknown vtab columns).
func ftsUpdateUnknownColumn(colDefs []sql.ColumnDef, ftsTable *fts.FTS3Table, s *sql.UpdateStmt) string {
	set := make(map[string]bool)
	for _, n := range ftsTable.ColumnNames() {
		set[strings.ToLower(n)] = true
	}
	set["docid"] = true
	set["rowid"] = true
	// The languageid=<col> hidden column is a valid UPDATE target too
	// (fts4langid 6.0: UPDATE vt0 SET lid = 1 WHERE lid=0).
	if lc := ftsTable.LangIDColName(); lc != "" {
		set[strings.ToLower(lc)] = true
	}
	for _, a := range s.Assignments {
		if !set[strings.ToLower(a.Column)] {
			return a.Column
		}
	}
	for _, c := range s.SetParenColumns {
		if !set[strings.ToLower(c)] {
			return c
		}
	}
	return ""
}

// collectFTSUpdateMatched returns the docIDs matching an FTS UPDATE's WHERE
// clause (all rows when the WHERE is nil). For an FTS4 content=<table> table
// the WHERE clause is evaluated against the content table's column values
// (SQLite's xUpdate reads the old row from the content table; fts3.c
// fts3DeleteTerms), and a docid whose content row is missing is skipped
// entirely — the update cannot compute the old terms without it
// (fts4content 3.3.x: UPDATE ft3 SET x=y, y=x reindexes only the docids whose
// content rows exist).
func (e *DDLExecutor) collectFTSUpdateMatched(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) ([]int64, *Result) {
	// Fast path for a rowid/docid equality WHERE (see collectFTSMatched;
	// fts4onepass 3.x chains thousands of single-doc UPDATEs).
	if s.From.Name == "" && s.From.Subquery == nil {
		if id, ok := ftsRowIDEqConstraint(s.Where); ok {
			if ftsTable.HasDoc(id) {
				return []int64{id}, nil
			}
			return nil, nil
		}
	}
	var matched []int64
	for _, docID := range ftsTable.AllRowsMap() {
		shouldUpdate := true
		if s.Where != nil {
			rowMap, matchedRow, jerr := e.ftsUpdateJoinedRowMap(ftsTable, colDefs, s, docID)
			if jerr != nil {
				// A missing FROM table fails the whole statement (fts4upfrom
				// 1.x: UPDATE ft SET c=v FROM changes → "no such table:
				// changes").
				return nil, &Result{Error: jerr}
			}
			if !matchedRow {
				shouldUpdate = false
			} else {
				match, err := e.ctx.EvalBool(s.Where, rowMap)
				if err != nil || !match {
					shouldUpdate = false
				}
			}
		}
		if !shouldUpdate {
			continue
		}
		// A content=<table> table's xUpdate computes the delete terms from
		// the content row; a docid whose content row is missing cannot be
		// updated and is skipped (fts3.c fts3DeleteTerms; fts4content 3.1.4
		// / 3.3.x).
		if ct := ftsTable.ContentTable(); ct != "" && !e.ctx.ContentRowExists(ct, docID) {
			continue
		}
		matched = append(matched, docID)
	}
	return matched, nil
}

// applyFTSUpdateOrderLimit applies ORDER BY, LIMIT, and OFFSET to an FTS UPDATE
// docID set (only rowid/asc/desc ORDER BY forms are meaningful).
func (e *DDLExecutor) applyFTSUpdateOrderLimit(matched []int64, s *sql.UpdateStmt) []int64 {
	if len(s.OrderBy) > 0 {
		if ob, ok := ftsOrderByRowID(s.OrderBy); ok {
			sort.SliceStable(matched, func(i, j int) bool {
				if ob.desc {
					return matched[i] > matched[j]
				}
				return matched[i] < matched[j]
			})
		}
	}
	limit := -1
	offset := 0
	if s.Limit != nil {
		if v, err := e.ctx.EvalConstInt(s.Limit); err == nil {
			limit = int(v)
		}
	}
	if s.Offset != nil {
		if v, err := e.ctx.EvalConstInt(s.Offset); err == nil {
			offset = int(v)
		}
	}
	return applyFTSSlice(matched, limit, offset)
}

// validateOrphanIndexes mirrors SQLite's schema-load check (prepare.c
// sqlite3InitCallback): an index row whose SQL text is empty must be a
// sqlite_autoindex_* entry that backs a PRIMARY KEY / UNIQUE constraint of an
// existing table. A bare index row that does not resolve is an "orphan index"
// and makes the whole schema malformed. The engine returns the same message
// SQLite does, so corrupt databases report it at query time instead of a
// later, less specific FTS "database disk image is malformed".
func (e *DDLExecutor) validateOrphanIndexes() error {
	entries, err := e.ctx.Schema().GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	for _, ent := range entries {
		if ent.SQL != "" {
			continue // a real CREATE INDEX statement parses against its table
		}
		if !e.autoindexNameResolves(ent) {
			return fmt.Errorf("malformed database schema (%s) - orphan index", ent.Name)
		}
	}
	return nil
}

// autoindexNameResolves reports whether an autoindex entry (empty SQL) is a
// sqlite_autoindex_<table>_<N> row matching a PRIMARY KEY / UNIQUE slot of
// its table. SQLite numbers autoindex slots starting at 1.
