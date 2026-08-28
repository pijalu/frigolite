package execddl

import (
	"errors"
	"fmt"

	"regexp"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

// --- DROP VIEW ---

// --- DROP VIEW ---

func (e *DDLExecutor) execDropView(s *sql.DropViewStmt) *Result {
	if err := e.ctx.Authorize(auth.ActionDropView, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Find the view to get its database context
	_, ctx, err := e.ctx.FindView(s.Name)
	if err != nil {
		// The name is not a view. If a table or other object with that name
		// exists, SQLite rejects the statement rather than deleting the
		// wrong object ("use DROP TABLE to delete table t1").
		if te, _, terr := e.ctx.FindTable(s.Name); terr == nil {
			_ = te
			if !s.IfExists {
				return &Result{Error: fmt.Errorf("use DROP TABLE to delete table %s", s.Name)}
			}
			return &Result{}
		}
		// Not a view and not a table (e.g. no such view).
		if !s.IfExists {
			return &Result{Error: fmt.Errorf("no such view: %s", s.Name)}
		}
		return &Result{}
	}
	// Remove from schema — by TYPE so a TRIGGER named the same as the view
	// survives.
	if err := ctx.Schema.RemoveEntryOfType(s.Name, schema.TypeView); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- DROP TRIGGER ---

func (e *DDLExecutor) execDropTrigger(s *sql.DropTriggerStmt) *Result {
	e.ctx.InvalidateTableCaches()
	if err := e.ctx.Authorize(auth.ActionDropTrigger, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	entry, ctx, err := e.ctx.FindTrigger(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	if err := ctx.Schema.RemoveEntryOfType(s.Name, schema.TypeTrigger); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	// Invalidate trigger existence cache
	e.ctx.ResetHasTriggersCache()
	// If in a transaction, buffer the undo operation (re-add the entry on rollback)
	if e.ctx.InTransaction() {
		entryCopy := *entry
		ctxCopy := ctx
		e.ctx.AppendDDLBuffer(func() {
			_ = ctxCopy.Schema.AddEntry(&entryCopy)
		})
	}
	return &Result{}
}

// --- DROP INDEX ---

func (e *DDLExecutor) execDropIndex(s *sql.DropIndexStmt) *Result {
	e.ctx.InvalidateTableCaches()
	if err := e.ctx.Authorize(auth.ActionDropIndex, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Find the index to get its database context
	entry, ctx, err := e.ctx.FindIndex(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	// Auto-generated indexes (sqlite_autoindex_*) may not be dropped
	// explicitly (SQLite: "index associated with UNIQUE or PRIMARY KEY
	// constraint cannot be dropped").
	if entry != nil && strings.HasPrefix(entry.Name, "sqlite_autoindex_") {
		return &Result{Error: fmt.Errorf("index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped")}
	}
	// Remove from schema — by TYPE so a TRIGGER named the same as the index
	// survives.
	if err := ctx.Schema.RemoveEntryOfType(s.Name, schema.TypeIndex); err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	return &Result{}
}

// stripIfNotExists removes an "IF NOT EXISTS" clause that follows the
// CREATE keyword, matching SQLite's behavior of storing object-creation SQL
// without the redundant clause ("CREATE TABLE IF NOT EXISTS t(a)" is stored
// as "CREATE TABLE t(a)").
func stripIfNotExists(sqlStr string) string {
	re := regexp.MustCompile(`(?i)(CREATE\s+(?:TEMP\s+|TEMPORARY\s+)?(?:VIRTUAL\s+TABLE|TABLE|VIEW|INDEX|TRIGGER)\s+)IF\s+NOT\s+EXISTS\s+`)
	return re.ReplaceAllString(sqlStr, "$1")
}

// stripViewSchemaPrefix removes a "<schema>." prefix from the view name in a
// CREATE VIEW statement ("CREATE VIEW temp.ttt AS ..." → "CREATE VIEW ttt
// AS ..."). Used because SQLite stores temp-schema view SQL without the
// schema qualifier.
func stripViewSchemaPrefix(sqlStr, schemaPrefix string) string {
	quoted := regexp.QuoteMeta(schemaPrefix)
	re := regexp.MustCompile(`(?i)(CREATE\s+VIEW\s+)` + quoted + `\.`)
	return re.ReplaceAllString(sqlStr, "$1")
}

// --- CREATE TRIGGER ---

// hasBindParameter reports whether a SQL statement contains a bound
// parameter placeholder (?NNN, ?name, :name, @name, $name) outside string
// literals. SQLite rejects these in trigger bodies at CREATE time with
// "trigger cannot use variables".
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isPlainIdentifier reports whether name is a bare SQL identifier (letter or
// underscore start, then letters/digits/underscores) that needs no quoting.
func isPlainIdentifier(name string) bool {
	if name == "" || !isIdentStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !isIdentStart(c) && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// validateTriggerSchemaRefs enforces SQLite's schema-scoping rules for
// non-temp trigger bodies: they may not reference objects in an attached
// database, and their DML statements may not use qualified table names.
// checkQualifiedDMLTable rejects qualified table names in trigger DML.
func checkQualifiedDMLTable(table string) error {
	schemaName, _ := execdml.ParseSchemaName(table)
	if schemaName != "" {
		return fmt.Errorf("qualified table names are not allowed on INSERT, UPDATE, and DELETE statements within triggers")
	}
	return nil
}

// checkTriggerSchemaRef rejects a table reference in an attached database
// (any schema other than main/temp).
func (e *DDLExecutor) checkTriggerSchemaRef(trigName, table string, trigCtx *DatabaseContext) error {
	schemaName, _ := execdml.ParseSchemaName(table)
	if schemaName == "" {
		return nil
	}
	upper := strings.ToUpper(schemaName)
	if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
		return nil
	}
	if e.ctx.GetDB(schemaName) != nil {
		return fmt.Errorf("trigger %s cannot reference objects in database %s", trigName, schemaName)
	}
	return nil
}

// buildTriggerSQL constructs the full CREATE TRIGGER SQL text including the body.
func buildTriggerSQL(name, time, event, table string, when sql.Expr, statements []sql.Stmt) string {
	var b strings.Builder
	b.WriteString("CREATE TRIGGER ")
	b.WriteString(name)
	if time != "" {
		b.WriteString(" ")
		b.WriteString(time)
	}
	b.WriteString(" ")
	b.WriteString(event)
	b.WriteString(" ON ")
	b.WriteString(table)

	// WHEN clause
	if when != nil {
		b.WriteString(" WHEN ")
		b.WriteString(sql.ExprString(when))
	}

	b.WriteString(" BEGIN")
	for _, stmt := range statements {
		b.WriteString("\n    ")
		b.WriteString(stmtToString(stmt))
		b.WriteString(";")
	}
	b.WriteString("\nEND")
	return b.String()
}

// stmtToString converts a statement back to SQL text for trigger body serialization.
func stmtToString(stmt sql.Stmt) string {
	switch s := stmt.(type) {
	case *sql.UpdateStmt:
		return updateStmtToString(s)
	case *sql.InsertStmt:
		return insertStmtToString(s)
	case *sql.DeleteStmt:
		return deleteStmtToString(s)
	case *sql.SelectStmt:
		return selectStmtToString(s)
	default:
		return ""
	}
}

// deleteStmtToString converts a DELETE statement to SQL text.
func deleteStmtToString(s *sql.DeleteStmt) string {
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	b.WriteString(s.Table)
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(sql.ExprString(s.Where))
	}
	return b.String()
}

// selectStmtToString converts a SELECT statement to SQL text (used for views).

// --- CREATE VIRTUAL TABLE ---

// echoVTabSource resolves the underlying table name of an echo virtual table
// reference ("t1e" for CREATE VIRTUAL TABLE t1e USING echo(t1)). It returns
// ok=false when the name is not an echo vtab. The source is resolved through
// the schema so a qualified reference (main.e) works like any table lookup.
func (e *DDLExecutor) echoVTabSource(name string) (string, bool) {
	entry, _, err := e.ctx.FindTable(name)
	if err != nil {
		return "", false
	}
	moduleName, args, perr := parseVTabSQL(entry.SQL)
	if perr != nil || !strings.EqualFold(moduleName, "echo") || len(args) == 0 {
		return "", false
	}
	src := strings.Trim(args[0], "'\"")
	// A pattern argument (echo(t1*)) is not a concrete source table.
	if strings.ContainsAny(src, "*?") {
		return "", false
	}
	return src, true
}

// rewriteEchoInsert rewrites an INSERT INTO <echo vtab> statement to target
// the vtab's source table. The echo module declares the source table's schema
// (with HIDDEN columns flagged), so a no-column-list INSERT (INSERT INTO e
// VALUES(...)) supplies values for the source's non-hidden columns — the
// hidden column is skipped, exactly as SQLite's vtab INSERT does (vtabA-1.4:
// INSERT INTO t1e VALUES('a','c') on t1(a, b HIDDEN, c) stores a='a', c='c').
func (e *DDLExecutor) rewriteEchoInsert(s *sql.InsertStmt, srcName string) {
	// The vtab's column definitions (with HIDDEN flags applied) define the
	// writable column set: a no-column-list INSERT supplies values for the
	// non-hidden columns. The source table's raw defs still have "HIDDEN" in
	// the type text, so resolve through the echo vtab's defs.
	if len(s.Columns) == 0 {
		if vtabEntry, _, ferr := e.ctx.FindTable(s.Table); ferr == nil {
			for _, cd := range e.ctx.ParseColumnDefs(vtabEntry.Name, vtabEntry.SQL) {
				if cd.Dropped || execquery.IsHiddenColumnDef(cd) {
					continue
				}
				s.Columns = append(s.Columns, cd.Name)
			}
		}
	}
	s.Table = srcName
}

func parseVTabSQL(sql string) (moduleName string, args []string, err error) {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return "", nil, fmt.Errorf("vtab: invalid virtual table SQL: %s", sql)
	}
	rest := sql[idx+7:]
	parts := strings.SplitN(rest, "(", 2)
	moduleName = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		args = vtab.SplitModuleArgs(parts[1])
	}
	return moduleName, args, nil
}

// vtabUpperBound extracts an inclusive upper bound from a virtual-table
// WHERE clause of the forms "value < N", "value <= N", "N > value",
// "N >= value", or "value BETWEEN 1 AND N". Returns ok=false when no
// usable bound is found (the table falls back to its default range).
// validateIndexedBy enforces an INDEXED BY name clause on a single-table
// query. SQLite raises "no such index: X" when the named index does not
// exist, and "no query solution" when the forced index cannot serve the
// query (notably a partial index whose WHERE predicate is not implied by
// the query's own WHERE clause, or an index that does not cover the query's
// ORDER BY). The engine does not actually use the forced index for the
// scan (results are correct via a table scan), but the error contract is
// observable and tested (indexedby.test).
// whereImplies reports whether a query WHERE expression textually implies a
// partial-index predicate. It is a conservative syntactic check: the
// predicate matches when the WHERE contains an identical conjunct (e.g.
// WHERE z=1 implies a predicate "z=1"). Used only for the INDEXED BY
// "no query solution" error contract.
func (e *DDLExecutor) whereImplies(where sql.Expr, pred string) bool {
	if where == nil {
		return false
	}
	predNorm := normalizeSQLText(pred)
	var found bool
	execquery.WalkExprFull(where, func(n sql.Expr) {
		if be, ok := n.(*sql.BinaryOp); ok {
			if normalizeSQLText(sql.ExprString(be)) == predNorm {
				found = true
			}
		}
	})
	return found
}

// normalizeSQLText normalizes SQL text for comparison by removing all
// whitespace, so "b='abc' AND i=5" matches the AST rendering "b = 'abc' AND
// i = 5". Used only for the INDEXED BY partial-index predicate check.
func normalizeSQLText(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// ensureFTSForTable lazily re-creates the in-memory FTS table for a virtual
// table entry whose module is an FTS module. On a fresh connection the FTS
// tables map is empty even though the schema (sqlite_schema) still contains
// the CREATE VIRTUAL TABLE entry; this restores the mapping so that
// INSERT/SELECT/DELETE route to FTS storage instead of treating the table as
// storageless (which would make writes no-ops and RETURNING project NULLs).
func (e *DDLExecutor) ensureFTSForTable(entry *schema.Entry) {
	if entry == nil || !strings.HasPrefix(strings.ToUpper(entry.SQL), "CREATE VIRTUAL TABLE") {
		return
	}
	if _, ok := e.ctx.FTSTables()[entry.Name]; ok {
		return
	}
	moduleName, args, err := parseVTabSQL(entry.SQL)
	if err != nil {
		return
	}
	ftsMod := e.getFTSModule(moduleName)
	if ftsMod == nil {
		return
	}
	ftsTable, err := ftsMod.GetOrCreateTable(entry.Name, moduleName, args)
	if err != nil {
		return
	}
	// A content=<table> table with no explicit columns derives its column
	// names from the content table's CURRENT schema at connection time
	// (fts3.c fts3ContentColumns; fts4content 6.2.5: after DROP TABLE t7 +
	// CREATE TABLE t7(x, y), a reopened connection's SELECT * FROM ft7
	// returns the x/y values because the columns are re-derived from the new
	// t7). registerFTSVTab does the same at CREATE; a reopened connection
	// must re-derive too.
	if ct := ftsTable.ContentTable(); ct != "" && len(ftsTable.ColumnNames()) == 0 {
		ctEntry, _, cerr := e.ctx.FindTable(ct)
		if cerr == nil && ctEntry != nil {
			ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
			var names []string
			for _, cd := range ctDefs {
				if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
					continue
				}
				names = append(names, cd.Name)
			}
			ftsTable.SetColumnNames(names)
		}
	}
	e.ctx.FTSTables()[entry.Name] = ftsTable
	// Rebuild the in-memory index from the %_content shadow table so a
	// reopened connection (db_restore_and_reopen, sqlite3 db test.db) finds
	// the documents that were stored before the close (fts3conf 1.x.y, the
	// engine persists FTS document text in %_content; see writeFTSContentRow).
	e.rebuildFTSFromContent(entry.Name, ftsTable)
	// Load the %_segdir/%_segments index too: for a database written by real
	// SQLite (fts3corrupt4 deserialize/crash tests) the FTS data lives there
	// and MATCH/matchinfo must work against it; for the engine's own tables
	// the segment postings duplicate the content rebuild and are deduplicated
	// by addPosting. A corrupt segment records a load error that surfaces as
	// "database disk image is malformed" at the next FTS operation.
	e.loadFTSSegments(entry.Name, ftsTable)
}

// loadFTSSegments populates an FTS table's in-memory index from its %_segdir
// rows and %_segments blocks (real SQLite segment format). This lets the
// engine query databases whose FTS index was written by SQLite itself.
// loadFTSSegments loads every %_segdir segment into ftsTable's in-memory
// index (the reopen/reload path, where all bands share one key space).
func (e *DDLExecutor) loadFTSSegments(tableName string, ftsTable *fts.FTS3Table) {
	e.loadFTSSegmentsForIndex(tableName, ftsTable, -1)
}

// loadFTSSegmentsForIndex loads only the segments of ONE index band:
// iIndex -1 loads every band; iIndex >= 0 loads only rows whose absolute
// level satisfies (level/1024)%nIndex == iIndex — the main index plus each
// prefix truncation are INDEPENDENT indexes (fts3_write.c allocates one
// segdir range per index: absolute level = base + iIndex*1024 + rel), and
// the integrity check must compare each band against its own expectation
// exactly as fts3ChecksumIndex runs once per iIndex. Loading every band
// into one key space lets a delete-marker applied while reading a main-band
// segment erase a prefix band's contribution to the same string key.
func (e *DDLExecutor) loadFTSSegmentsForIndex(tableName string, ftsTable *fts.FTS3Table, iIndex int) {
	nIndex := 1 + len(ftsTable.PrefixLengths())
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return
	}
	ftsTable.ClearLoadErr()
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		ftsTable.SetLoadErr(fmt.Errorf("database disk image is malformed"))
		return
	}
	// Collect every row first, then apply segments in AGE order — oldest
	// first, newest last. The segment's age is its (level, idx) key: a higher
	// absolute level means older data (merges sink outputs upward) and a
	// higher idx within one level means newer. Physical %_segdir rowid order
	// is NOT age order once user SQL rewrites the table (prepare_for_optimize
	// re-inserts rows ordered by level,idx), and applying delete-marker
	// tombstones in the wrong order resurrects/kills the wrong documents
	// (fts4opt 2.x: integrity-check missing-term failures after prepare).
	type segRowInfo struct {
		level, idx     int64
		leavesEndBlock int64
		root           []byte
	}
	var rows []segRowInfo
	loadSeen := 0
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil {
			if !strings.Contains(rerr.Error(), "cursor at end") {
				ftsTable.SetLoadErr(fmt.Errorf("database disk image is malformed"))
			}
			break
		}
		if cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) == 0 {
			break
		}
		loadSeen++
		// Band filter: skip rows belonging to other indexes.
		if iIndex >= 0 {
			lvRaw, _ := rec.Values[0].(int64)
			if lvRaw < 0 || (lvRaw/1024)%int64(nIndex) != int64(iIndex) {
				if ok, nerr := cursor.Next(); nerr != nil || !ok {
					break
				}
				continue
			}
		}
		// %_segdir(level, idx, start_block, leaves_end_block, end_block, root).
		// Values are [level, idx, start_block, leaves_end_block, end_block, root].
		var levelVal, idxVal int64
		if lv, ok := rec.Values[0].(int64); ok {
			levelVal = lv
		}
		if iv, ok := rec.Values[1].(int64); ok {
			idxVal = iv
		}
		var leavesEndBlock int64
		if len(rec.Values) >= 4 {
			switch lb := rec.Values[3].(type) {
			case int64:
				leavesEndBlock = lb
			case float64:
				leavesEndBlock = int64(lb)
			case []byte:
				fmt.Sscanf(string(lb), "%d", &leavesEndBlock)
			case string:
				fmt.Sscanf(lb, "%d", &leavesEndBlock)
			}
		}
		// start_block==0 marks a root-only segment (fts3_write.c
		// fts3SegReaderNew: iStartLeaf==0 → rootOnly=1); %_segments is never
		// read, so a non-zero leaves_end_block on such a row must not send
		// the loader block-hunting (fts3corrupt7 1.1 crafted segdir).
		if len(rec.Values) >= 3 {
			if sb, ok := rec.Values[2].(int64); ok && sb == 0 {
				leavesEndBlock = 0
			}
		}
		rootVal := rec.Values[len(rec.Values)-1]
		var root []byte
		switch rv := rootVal.(type) {
		case []byte:
			root = rv
		case string:
			root = []byte(rv)
		}
		rows = append(rows, segRowInfo{level: levelVal, idx: idxVal, leavesEndBlock: leavesEndBlock, root: root})
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	// Oldest first: higher level = older; within one level, lower idx = older.
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].level != rows[b].level {
			return rows[a].level > rows[b].level
		}
		return rows[a].idx < rows[b].idx
	})
	for _, row := range rows {
		if len(row.root) > 0 {
			reader := func(blockID int) ([]byte, error) {
				blk, res := e.readFTSBlock(tableName, blockID)
				if res != nil {
					return nil, fmt.Errorf("corrupt segment root")
				}
				if blk == nil {
					return nil, fmt.Errorf("corrupt segment root")
				}
				return blk, nil
			}
			if lerr := ftsTable.LoadSegment(row.root, int(row.leavesEndBlock), reader); lerr != nil {
				// A segment that fails to load (corrupt term structure) makes
				// any SELECT/MATCH fail with "database disk image is
				// malformed" (fts3corrupt4 12.1: a corrupt segdir root).
				// UPDATE/DELETE skip the loadErr check (they use the
				// in-memory index; fts3corrupt4 25.x succeeds). A STRUCTURAL
				// break (interior chain) defeats every term lookup, so it is
				// recorded as such (fts3corrupt7 3.x).
				if errors.Is(lerr, fts.ErrSegmentStructure) {
					ftsTable.SetStructuralLoadErr()
				}
				ftsTable.SetLoadErr(fmt.Errorf("database disk image is malformed"))
			}
		}
	}
}

// rebuildFTSFromContent repopulates an FTS table's in-memory index from the
// rows of its %_content shadow table. The content table holds the original
// document text (docid + one column per user column) written by
// writeFTSContentRow on every insert; reading it back after a reopen restores
// the MATCH-able document set.
func (e *DDLExecutor) rebuildFTSFromContent(tableName string, ftsTable *fts.FTS3Table) {
	// An FTS4 content=<table> table reads its document text from the external
	// content table instead of the %_content shadow (fts3.c fts3DoRebuild
	// prepares "SELECT %s" over zReadExprlist).
	content := tableName + "_content"
	if ct := ftsTable.ContentTable(); ct != "" {
		content = ct
	}
	contentEntry, _, err := e.ctx.FindTable(content)
	if err != nil || contentEntry == nil {
		return
	}
	// A content=<table> table whose content source is itself a virtual table
	// (fts4content 9.x: content=e1 where e1 is an echo module mirroring a
	// real table) has no b-tree: read its rows through the vtab machinery
	// (fts3.c fts3DoRebuild prepares "SELECT %s" over the content source).
	if contentEntry.RootPage == 0 {
		e.rebuildFTSFromVTabContent(tableName, ftsTable, contentEntry)
		return
	}
	tree := e.ctx.TableBTreeForName(contentEntry.Name, contentEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		// A %_content btree that cannot be navigated means the document text
		// is unavailable, but the segment index may still serve MATCH
		// queries (fts3corrupt4 28.1: the content page is corrupt yet MATCH
		// 'h' returns 0 rows). Per-row decode failures are recorded
		// separately (corruptContentDocIDs); a read that needs the text
		// fails at that point. The unreadable-btree state is sticky: a query
		// that READS content columns fails with "database disk image is
		// malformed" (fts3corrupt4 52.1: SELECT * FROM t1, t2 — SQLite's
		// full scan steps the corrupt content table and reports the damage).
		if strings.Contains(cerr.Error(), "malformed") {
			ftsTable.SetContentBtreeUnreadable()
		}
		return
	}
	colDefs := e.ctx.ParseColumnDefs(contentEntry.Name, contentEntry.SQL)
	// Map the content columns to FTS column indices: content has docid then
	// c0a/c1b... (one per user column). The FTS table's columns are the same
	// count, so values[i] maps to user column i. For a content=<table> table
	// the external table's columns are matched by NAME against the FTS
	// columns (fts3.c fts3ContentColumns / fts3ReadExprList).
	isContentExternal := ftsTable.ContentTable() != ""
	// For content= tables, find each FTS column's position in the external
	// table's columns (0-based over the non-rowid columns).
	ftsCols := ftsTable.ColumnNames()
	contentValIdx := make([]int, len(ftsCols))
	for fi, fname := range ftsCols {
		contentValIdx[fi] = -1
		vi := 0
		for _, cd := range colDefs {
			if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
				continue
			}
			if strings.EqualFold(cd.Name, fname) {
				contentValIdx[fi] = vi
				break
			}
			vi++
		}
	}
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil {
			// A cell that fails to decode mid-scan is skipped: SQLite reads
			// %_content lazily per matched row (fts3Column), so an unreadable
			// non-matched row never surfaces; poisoning the whole table would
			// fail queries over intact rows (fts3corrupt7 1.1).
			break
		}
		if cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			// A content record that fails to decode is corrupt. Record its
			// rowid so a query that matches it fails with "database disk
			// image is malformed" (fts3corrupt4 11.1), while queries that
			// never read the row succeed (9.1: an unmatched corrupt row).
			if cell != nil {
				ftsTable.RecordCorruptContentDocID(cell.RowID)
			}
			break
		}
		var docID int64
		var vals []interface{}
		uncompressFn := ftsTable.UncompressFn()
		if isContentExternal {
			// The external content table's record has no docid column (the
			// rowid is the cell rowid); build the FTS value list by
			// contentValIdx directly over rec.Values.
			docID = cell.RowID
			for _, vi := range contentValIdx {
				var v interface{}
				if vi >= 0 && vi < len(rec.Values) {
					v = rec.Values[vi]
				}
				if uncompressFn != "" && v != nil {
					if uv, uerr := e.ctx.EvalExpr(&sql.FuncCall{
						Name: uncompressFn,
						Args: []sql.Expr{&sql.StringLit{Value: fmt.Sprintf("%v", v)}},
					}, nil); uerr == nil && uv != nil {
						v = uv
					}
				}
				vals = append(vals, v)
			}
		} else {
			for i, v := range rec.Values {
				if i == 0 {
					if iv, ok := v.(int64); ok {
						docID = iv
					}
					continue
				}
				if _, ok := e.ctx.FTSTables()[tableName]; !ok {
					return
				}
				if uncompressFn != "" {
					// FTS4 uncompress= restores the original text from the
					// compressed content value (fts3.c fts3ReadNextRow).
					if uv, uerr := e.ctx.EvalExpr(&sql.FuncCall{
						Name: uncompressFn,
						Args: []sql.Expr{&sql.StringLit{Value: fmt.Sprintf("%v", v)}},
					}, nil); uerr == nil && uv != nil {
						v = uv
					}
				}
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			vals = make([]interface{}, 0)
		}
		ftsTable.InsertWithID(docID, vals)
		// Record the docid as pending so the next COMMIT flushes the rebuilt
		// index to %_segdir (SQLite's fts3RebuildMethod writes segments
		// immediately; the engine defers to the commit-time flush, which
		// requires the pending list — fts4check/fts4intck1's integrity check
		// reads the index from the segments).
		ftsTable.RecordPending(docID)
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
}

// rebuildFTSFromVTabContent repopulates an FTS table's in-memory index from a
// virtual-table content source (fts4content 9.x: CREATE VIRTUAL TABLE ft1
// USING fts4(content=e1) where e1 is an echo module mirroring tbl1). The rows
// come from the vtab's materialization; the vtab's rowid is the docid and its
// columns map to the FTS columns by name (the same name-matching
// rebuildFTSFromContent uses for a b-tree content table).
func (e *DDLExecutor) rebuildFTSFromVTabContent(tableName string, ftsTable *fts.FTS3Table, contentEntry *schema.Entry) {
	rows, err := e.virtualTableRows(contentEntry, 0, "", false)
	if err != nil || rows == nil {
		return
	}
	// The vtab's column definitions (echo mirrors the source table's columns;
	// ParseColumnDefs on the echo entry resolves them).
	vtDefs := e.ctx.ParseColumnDefs(contentEntry.Name, contentEntry.SQL)
	ftsCols := ftsTable.ColumnNames()
	// Map each FTS column name to its position in the vtab's column list.
	contentValIdx := make([]int, len(ftsCols))
	for fi, fname := range ftsCols {
		contentValIdx[fi] = -1
		for vi, cd := range vtDefs {
			if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
				continue
			}
			if strings.EqualFold(cd.Name, fname) {
				contentValIdx[fi] = vi
				break
			}
		}
	}
	docID := int64(1)
	for _, row := range rows {
		vals := make([]interface{}, len(ftsCols))
		for fi, vi := range contentValIdx {
			if vi >= 0 && vi < len(row) {
				vals[fi] = row[vi]
			}
		}
		ftsTable.InsertWithID(docID, vals)
		ftsTable.RecordPending(docID)
		docID++
	}
}

// ftsOrderByRowID reports whether an FTS DELETE/UPDATE ORDER BY is a simple
// rowid (or integer column) ordering that can be applied to the docID set.
// Returns the ascending/descending direction.
func ftsOrderByRowID(orderBy []sql.OrderByTerm) (struct{ desc bool }, bool) {
	if len(orderBy) != 1 {
		return struct{ desc bool }{}, false
	}
	ob := orderBy[0]
	if ref, ok := ob.Expr.(*sql.ColumnRef); ok && strings.EqualFold(ref.Name, "rowid") {
		return struct{ desc bool }{desc: ob.Desc}, true
	}
	return struct{ desc bool }{}, false
}

// isValidStrictType returns true if the type name is allowed in a STRICT table.
// Allowed types: INT, INTEGER, TEXT, REAL, BLOB, ANY (case-insensitive).
func isValidStrictType(typeName string) bool {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	switch upper {
	case "INT", "INTEGER", "TEXT", "REAL", "BLOB", "ANY":
		return true
	}
	return false
}

// validateWithoutOption validates the CREATE TABLE trailing options (SQLite
// supports "STRICT" and "WITHOUT ROWID"; any other trailing token is an
// "unknown table option" error).
func validateWithoutOption(createSQL string) error {
	sql := execdml.StripCTASSelect(createSQL)
	idx := strings.LastIndex(sql, ")")
	if idx < 0 {
		return nil
	}
	tail := strings.TrimSpace(sql[idx+1:])
	if tail == "" {
		return nil
	}
	for _, opt := range strings.Split(tail, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		upper := strings.ToUpper(opt)
		if upper == "STRICT" {
			continue
		}
		if strings.HasPrefix(upper, "WITHOUT") {
			rest := strings.TrimSpace(opt[len("WITHOUT"):])
			if rest != "" && strings.EqualFold(rest, "ROWID") {
				continue
			}
			return fmt.Errorf("unknown table option: %s", rest)
		}
		return fmt.Errorf("unknown table option: %s", opt)
	}
	return nil
}

// hasPrimaryKey returns true if the CREATE TABLE statement has any PRIMARY KEY
// constraint (column-level or table-level).
// The LALR parser doesn't propagate table-level constraints, so we also
// check the raw SQL for "PRIMARY KEY".
func hasPrimaryKey(s *sql.CreateTableStmt) bool {
	for _, col := range s.Columns {
		if col.PrimaryKey {
			return true
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintPrimaryKey {
			return true
		}
	}
	// Fallback: check raw SQL for table-level PRIMARY KEY constraint.
	// The LALR parser may not populate s.Constraints for table-level
	// constraints like PRIMARY KEY(a,b).
	if s.RawSQL != "" {
		upper := strings.ToUpper(s.RawSQL)
		if strings.Contains(upper, "PRIMARY KEY") || strings.Contains(upper, "PRIMARY  KEY") {
			return true
		}
	}
	return false
}

// enforceStrictType checks if a value is compatible with a STRICT column type.
// Returns an error if the value's storage class does not match the declared type.
// STRICT rules (SQLite src/vdbeaux.c):
//   - TEXT: value must be text (string)
//   - INTEGER/INT: value must be an integer; numeric strings are accepted
//   - REAL: value must be real (or integer, converted to real); numeric strings accepted
//   - BLOB: value must be a blob
//   - ANY: any value accepted
func TrimGenerationType(t string) string {
	upper := strings.ToUpper(t)
	trimmed := strings.TrimSpace(t)
	upperTrimmed := strings.ToUpper(trimmed)
	// The type may START with a generation keyword (no declared type), e.g.
	// "Generated Always AS (...)" → empty.
	for _, kw := range []string{"GENERATED", "ALWAYS", "AS"} {
		if upperTrimmed == kw || strings.HasPrefix(upperTrimmed, kw+" ") {
			return ""
		}
	}
	idx := -1
	for _, kw := range []string{" GENERATED", " ALWAYS", " AS "} {
		if i := strings.Index(upper, kw); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		return t
	}
	return strings.TrimSpace(t[:idx])
}
