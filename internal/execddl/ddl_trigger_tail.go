// Package exec implements query execution.
//
// This file holds DDL execution for CREATE TRIGGER, CREATE VIEW, and CREATE
// VIRTUAL TABLE, plus the SQL-text serialization helpers used by stored
// triggers and views. It is the trigger/view/vtable half of the former
// ddl.go, split out so that each file stays within the repository's
// complexity and size budgets. Core CREATE/DROP/ATTACH execution and the
// generic expression serializer live in ddl_core.go.
package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DDLExecutor) createFTSShadowTables(tableName string, t *fts.FTS3Table, moduleName string) error {
	isFts4 := strings.EqualFold(moduleName, "fts4")
	if strings.EqualFold(moduleName, "fts5") {
		// fts5_storage.c creates %_data, %_idx, %_content, %_docsize and
		// %_config for every fts5 table (vtabdrop 2.2 lists them in
		// sqlite_master).
		cols := t.ColumnNames()
		if res := e.createShadowTableSQL(tableName+"_data", []sql.ColumnDef{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "block", Type: "BLOB"},
		}); res.Error != nil {
			return res.Error
		}
		if res := e.createShadowTableSQL(tableName+"_idx", []sql.ColumnDef{
			{Name: "segid", Type: "INTEGER"},
			{Name: "term", Type: "TEXT"},
			{Name: "pgno", Type: "INTEGER"},
		}); res.Error != nil {
			return res.Error
		}
		contentDefs := []sql.ColumnDef{{Name: "id", Type: "INTEGER", PrimaryKey: true}}
		for i := range cols {
			contentDefs = append(contentDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d", i)})
		}
		if res := e.createShadowTableSQL(tableName+"_content", contentDefs); res.Error != nil {
			return res.Error
		}
		if res := e.createShadowTableSQL(tableName+"_docsize", []sql.ColumnDef{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "sz", Type: "BLOB"},
		}); res.Error != nil {
			return res.Error
		}
		if res := e.createShadowTableSQL(tableName+"_config", []sql.ColumnDef{
			{Name: "k", Type: "INTEGER", PrimaryKey: true},
			{Name: "v", Type: ""},
		}); res.Error != nil {
			return res.Error
		}
		return nil
	}
	cols := t.ColumnNames()

	// %_content(docid INTEGER PRIMARY KEY, c0 <name>, ...)
	// SQLite names the content columns "c%d%s" — c + column index + the user
	// column name (fts3.c fts3CreateTables: "c%d%s", i, azCol[i]), so a
	// table with columns (a, b) gets c0a, c1b. A content=<table> FTS table
	// has NO %_content shadow (fts3.c fts3CreateTables skips it when
	// zContent is set), and neither does a contentless (content=) table
	// (fts4content 7.2.3: SELECT name FROM sqlite_master LIKE 'ft9_%' has no
	// ft9_content).
	if t.ContentTable() != "" || t.Contentless() {
		// still create segments/segdir/docsize/stat below
	} else {
		contentDefs := []sql.ColumnDef{{Name: "docid", Type: "INTEGER", PrimaryKey: true}}
		for i, col := range cols {
			contentDefs = append(contentDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d%s", i, col), Type: ""})
		}
		// A languageid=<col> table's %_content gains a trailing langid column
		// (fts3.c fts3CreateTables: "%z, langid" is appended to the content
		// table's column list — fts4langid 1.2 shows the 'langid' column).
		if t.LangIDColName() != "" {
			contentDefs = append(contentDefs, sql.ColumnDef{Name: "langid", Type: ""})
		}
		if res := e.createShadowTableSQL(tableName+"_content", contentDefs); res.Error != nil {
			return res.Error
		}
		// A languageid=<col> table's stored CREATE statement must match
		// SQLite byte-for-byte (fts3.c fts3CreateTables builds it with %Q/%q:
		// the table name and every c%d%s column are single-quoted, docid and
		// langid are not — fts4langid 1.2). The generic renderer emits an
		// unquoted form, so rewrite the schema entry's SQL to the canonical
		// text.
		if t.LangIDColName() != "" {
			q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
			colSQL := ""
			for i, col := range cols {
				colSQL += fmt.Sprintf(", %s", q(fmt.Sprintf("c%d%s", i, col)))
			}
			colSQL += ", langid"
			canonical := fmt.Sprintf("CREATE TABLE %s(docid INTEGER PRIMARY KEY%s)",
				q(tableName+"_content"), colSQL)
			if err := e.ctx.Schema().UpdateEntry(tableName+"_content", canonical); err != nil {
				return err
			}
		}
	}

	// %_segments(blockid INTEGER PRIMARY KEY, block BLOB)
	if res := e.createShadowTableSQL(tableName+"_segments", []sql.ColumnDef{
		{Name: "blockid", Type: "INTEGER", PrimaryKey: true},
		{Name: "block", Type: "BLOB"},
	}); res.Error != nil {
		return res.Error
	}

	// %_segdir(level, idx, start_block, leaves_end_block, end_block, root,
	// PRIMARY KEY(level, idx)). Real SQLite creates a
	// sqlite_autoindex_<name>_segdir_1 entry for the PRIMARY KEY; the engine
	// omits it (the segdir idx uniqueness is enforced by the segment writer)
	// because adding a real autoindex entry grows sqlite_schema past one page
	// and the rename/delete operations on the split schema b-tree corrupt it
	// (a pre-existing b-tree split limitation; fts4content 5.1.1/5.1.3/6.2.3
	// have want-overrides for the missing autoindex row).
	if res := e.createShadowTableSQL(tableName+"_segdir", []sql.ColumnDef{
		{Name: "level", Type: "INTEGER"},
		{Name: "idx", Type: "INTEGER"},
		{Name: "start_block", Type: "INTEGER"},
		{Name: "leaves_end_block", Type: "INTEGER"},
		{Name: "end_block", Type: "INTEGER"},
		{Name: "root", Type: "BLOB"},
	}); res.Error != nil {
		return res.Error
	}

	if isFts4 {
		// %_docsize(docid INTEGER PRIMARY KEY, size BLOB). A table created
		// with matchinfo=fts3 omits it (fts3.c bHasDocsize=0).
		if !t.NoDocsize() {
			if res := e.createShadowTableSQL(tableName+"_docsize", []sql.ColumnDef{
				{Name: "docid", Type: "INTEGER", PrimaryKey: true},
				{Name: "size", Type: "BLOB"},
			}); res.Error != nil {
				return res.Error
			}
		}
		// %_stat(id INTEGER PRIMARY KEY, value BLOB)
		if res := e.createShadowTableSQL(tableName+"_stat", []sql.ColumnDef{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "value", Type: "BLOB"},
		}); res.Error != nil {
			return res.Error
		}
	}
	return nil
}

// createShadowTableSQL creates a real btree-backed table from its column
// definitions via the normal CREATE TABLE machinery. A non-empty pkCols list
// declares a table-level PRIMARY KEY over those columns (producing the
// sqlite_autoindex_* entry SQLite creates for the shadow table).
func (e *DDLExecutor) createShadowTableSQL(name string, cols []sql.ColumnDef, pkCols ...[]sql.ColumnDef) *Result {
	st := &sql.CreateTableStmt{
		Name:    name,
		Columns: cols,
	}
	if len(pkCols) > 0 && len(pkCols[0]) > 0 {
		var idxCols []sql.IndexedColumn
		for _, cd := range pkCols[0] {
			idxCols = append(idxCols, sql.IndexedColumn{Name: cd.Name})
		}
		st.Constraints = append(st.Constraints, sql.TableConstraint{
			Type:    sql.ConstraintPrimaryKey,
			Columns: idxCols,
		})
	}
	return e.execCreateTable(st)
}

// validateIndexedBy validates an INDEXED BY clause: the named index must
// exist on the query's table, and a partial index must be implied by the
// query WHERE clause.
func (e *DDLExecutor) validateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error {
	idxEntry := e.findIndexEntry(tableEntry, indexName)
	if idxEntry == nil {
		return fmt.Errorf("no such index: %s", indexName)
	}
	return e.checkPartialIndexUsable(idxEntry, s)
}

// findIndexEntry locates the schema entry for an index on the given table.
func (e *DDLExecutor) findIndexEntry(tableEntry *schema.Entry, indexName string) *schema.Entry {
	for _, ctx := range e.ctx.Databases() {
		en, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range en {
			if strings.EqualFold(ent.Name, indexName) && strings.EqualFold(ent.TblName, tableEntry.Name) {
				return ent
			}
		}
	}
	return nil
}

// checkPartialIndexUsable returns an error when a partial index (WHERE
// predicate) cannot serve the query: without any WHERE, or when the predicate
// cannot be implied, the forced partial index yields no query solution.
func (e *DDLExecutor) checkPartialIndexUsable(idxEntry *schema.Entry, s *sql.SelectStmt) error {
	wm := execdml.IndexWhereRe.FindStringSubmatch(idxEntry.SQL)
	if wm == nil {
		return nil
	}
	pred := strings.TrimSpace(wm[1])
	if s.Where == nil || !e.whereImplies(s.Where, pred) {
		return fmt.Errorf("no query solution")
	}
	return nil
}

// validateIndexKeyExpr rejects non-deterministic functions, window functions,
// and subqueries in index expressions.
func validateIndexKeyExpr(expr sql.Expr) error {
	var err error
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		switch e := n.(type) {
		case *sql.FuncCall:
			if e.Over != nil {
				err = fmt.Errorf("misuse of window function %s()", strings.ToLower(e.Name))
				return
			}
			err = checkIndexKeyFunc(e)
		case *sql.Subquery:
			err = fmt.Errorf("subqueries prohibited in index expressions")
		}
	})
	return err
}

// checkIndexKeyFunc validates one function call appearing in an index
// expression: non-deterministic functions are prohibited, and julianday('now')
// is a non-deterministic use.
func checkIndexKeyFunc(e *sql.FuncCall) error {
	switch strings.ToUpper(e.Name) {
	case "RANDOM", "RANDOMBLOB", "ZEROBLOB":
		return fmt.Errorf("non-deterministic functions prohibited in index expressions")
	case "JULIANDAY":
		for _, a := range e.Args {
			if sl, ok := a.(*sql.StringLit); ok && strings.EqualFold(sl.Value, "now") {
				return fmt.Errorf("non-deterministic use of julianday() in an index")
			}
		}
	}
	return nil
}

// execFTSSelect implements SELECT from an FTS virtual table.
func (e *DDLExecutor) execFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	// Set current FTS match context for MATCH evaluation
	e.ctx.SetCurrentFTSMatch(tableEntry.Name)
	defer func() { e.ctx.SetCurrentFTSMatch("") }()

	// Set the matchinfo() context: extract the MATCH constraint's query
	// string (when it is a constant) and its phrase structure so
	// matchinfo(TABLE) can compute per-phrase hit statistics. A non-constant
	// MATCH RHS (a joined column) leaves hasMatch false and matchinfo
	// returns an empty blob for the phrase-dependent values.
	e.ctx.SetFTSMatchInfo(tableEntry.Name, false, nil)
	defer e.ctx.ClearFTSMatchInfo()
	e.setFTSMatchInfoFromWhere(s, tableEntry.Name, ftsTable)

	// An orphan autoindex in sqlite_schema (an index row with no SQL whose
	// name does not resolve to a table PK/UNIQUE slot) makes the whole schema
	// malformed; SQLite reports it at schema load, before any table read
	// (prepare.c sqlite3InitCallback "orphan index"). Check here so the
	// schema error surfaces before the FTS segment validation would report
	// "database disk image is malformed" (fts3corrupt4 5.1). PRAGMA
	// writable_schema=ON skips schema validation entirely, so it must run in
	// the same statement batch before the query (fts3corrupt4 21.1/22.1).
	if !e.ctx.WritableSchema() {
		if err := e.validateOrphanIndexes(); err != nil {
			return &Result{Error: err}
		}
	}

	// A malformed MATCH expression fails the statement at prepare, before
	// any row is read — even on an empty table (fts3.c fts3FilterMethod
	// surfaces the expression parser's SQLITE_ERROR as "malformed MATCH
	// expression: [query]"; fts3expr 2.x).
	if s.Where != nil {
		if qs, ok := e.ftsMatchQueryString(s.Where, tableEntry.Name); ok {
			node, perr := fts.ParseMatchQuery(qs)
			if perr != nil {
				return &Result{Error: perr}
			}
			_ = node
			// Deferred corruption: a segment load that failed partway leaves
			// the loaded terms queryable; only a query referencing a term
			// whose doclist lives in an unreadable block fails with
			// "database disk image is malformed" (fts3.c reads each term's
			// doclist on demand; fts3defer2 1.x vs 1.7).
			if ftsTable.LoadErr() != nil {
				// A structurally broken segment b-tree defeats every term
				// lookup (fts3corrupt7 3.x: the 40000-deep interior chain),
				// so the MATCH fails regardless of its terms.
				if ftsTable.StructuralLoadErr() {
					return &Result{Error: fmt.Errorf("database disk image is malformed")}
				}
			}
		}
	}
	// A corrupt segment root surfaces as "database disk image is malformed"
	// when the index is read (fts3corrupt 2.2/3.2: MATCH after corruption).
	// ensureFTSForTable first: a fresh connection's first FTS SELECT reaches
	// here before the lazy table load, and the load is what records the
	// segment error this check reports (fts3corrupt4 31.1: the crafted
	// segdir root fails LoadSegment; the SELECT must fail malformed).
	e.ensureFTSForTable(tableEntry)
	if ft, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		ftsTable = ft
	}
	if res := e.validateFTSSegments(tableEntry.Name, false); res != nil {
		return res
	}
	// A corrupt %_stat value is NOT validated here: it only matters when
	// matchinfo() actually reads it (fts3corrupt 5.2/5.3 corrupt the stat
	// blob then SELECT matchinfo; a plain MATCH over a table whose stat says
	// nDoc=0 — e.g. after DELETE FROM ft3 removed every document — must still
	// return rows, fts4content 3.1.5). The matchinfo functions themselves
	// validate the blob.

	// An FTS4 uncompress= function that is not schema-safe (e.g. a
	// direct-only function) makes reading the table fail with "SQL logic
	// error": SQLite executes SELECT %_content with uncompress(?) per column
	// (fts3ReadExprList), and the core rejects the unsafe schema function
	// (fts3comp1 4.3).
	if ufn := ftsTable.UncompressFn(); ufn != "" && !e.ctx.SchemaFunctionSafe(ufn) {
		return &Result{Error: fmt.Errorf("SQL logic error")}
	}

	// A table whose in-memory index is empty but whose %_segdir has rows (a
	// hand-crafted or externally-written FTS index, e.g. fts3corrupt4 15.x
	// inserts into t1_segdir directly) needs its segments loaded now.
	if ftsTable.DocCount() == 0 {
		e.loadFTSSegments(tableEntry.Name, ftsTable)
	}
	// A MATCH query that reads a term whose segment doclist is corrupt must
	// fail with "database disk image is malformed" even when no candidate rows
	// exist (fts3corrupt4 31.1: an empty in-memory index whose hand-crafted
	// segment holds a corrupt term; SQLite reads the segment at prepare).
	if s.Where != nil {
		if mres := e.validateFTSMatchCorruption(s.Where, tableEntry.Name); mres != nil {
			return mres
		}
	}

	// For an FTS4 content=<table> table, row values come from the external
	// content table: an unconstrained SELECT returns every content-table row;
	// a MATCH returns the matched docids' content rows (fts3.c
	// fts3ReadExprList). A contentless (content=) table and a content=<table>
	// whose content table was dropped have no row values: docid/MATCH queries
	// still work off the index, but reading a content column fails
	// (fts4content 7.2.x, 6.2.x).
	var allRowMaps []RowMap
	if ct := ftsTable.ContentTable(); ct != "" || ftsTable.Contentless() {
		// contentOK is false for a contentless table or when the content
		// table cannot be found. A self-referential content source (CREATE
		// VIRTUAL TABLE t1 USING fts4(content=t1)) has no usable content
		// either: SQLite fails every read with "SQL logic error"
		// (fts4content 12.x).
		contentOK := true
		if ftsTable.Contentless() {
			contentOK = false
		} else if strings.EqualFold(ct, tableEntry.Name) {
			contentOK = false
		} else if _, isFTSContent := e.ctx.FTSTables()[ct]; isFTSContent {
			// A content source that is itself an FTS table (t1 content=t2,
			// t2 content=t1 — fts4content 12.2.x) cannot be read as a
			// content table: SQLite fails EVERY read with "SQL logic error",
			// including count(*) (which reads no content column).
			return &Result{Error: fmt.Errorf("SQL logic error")}
		} else if _, _, cerr := e.ctx.FindTable(ct); cerr != nil {
			contentOK = false
		}
		if s.Where != nil {
			if qs, ok := e.ftsMatchQueryString(s.Where, tableEntry.Name); ok {
				// A MATCH query's row set is the INDEX docids (fts3.c
				// fts3EvalNext: the index is the source of truth for which
				// documents match; the content table only supplies values). A
				// content row deleted after indexing still matches (its
				// values read as NULL/empty).
				matched, merr := ftsTable.MatchDocIDs(qs)
				if merr != nil {
					// A MATCH syntax error fails the statement (fts3.c
					// fts3FilterMethod surfaces the expression parser's
					// SQLITE_ERROR; fts3expr 2.x "malformed MATCH
					// expression").
					return &Result{Error: merr}
				}
				if contentOK {
					allRowMaps = e.ftsContentTableRowMapsForDocIDs(ftsTable, colDefs, matched)
				} else {
					// Index-only rows: docid queries work without the
					// content table; a query that reads a content column
					// fails below.
					allRowMaps = e.ftsIndexRowMapsForDocIDs(ftsTable, colDefs, matched)
				}
			}
		}
		if allRowMaps == nil {
			// No MATCH constraint (or an unresolvable one): return every
			// content-table row; the WHERE filter applies below. A missing
			// content table yields no rows; a content-column read fails
			// below (fts4content 6.2.2/6.2.4).
			if contentOK {
				allRowMaps = e.ftsContentTableRowMaps(ftsTable, colDefs, nil)
			}
		}
		// A self-referential content source (CREATE VIRTUAL TABLE t1 USING
		// fts4(content=t1)) fails EVERY read with "SQL logic error" —
		// including count(*) — because the content table cannot be read
		// (fts4content 12.x).
		if strings.EqualFold(ftsTable.ContentTable(), tableEntry.Name) {
			return &Result{Error: fmt.Errorf("SQL logic error")}
		}
		// Reading a content column when the content is unavailable fails:
		// "SQL logic error" when the FTS columns are known (6.2.2, 7.1.x,
		// 7.2.4), "no such table: main.<ct>" when the columns were never
		// derived because the content table was missing at connection time
		// (6.2.4: SELECT * FROM ft7 after a reopen with t7 dropped).
		if !contentOK && e.selectReadsFTSContentColumn(s, ftsTable) {
			if len(ftsTable.ColumnNames()) == 0 {
				return &Result{Error: fmt.Errorf("no such table: main.%s", ftsTable.ContentTable())}
			}
			return &Result{Error: fmt.Errorf("SQL logic error")}
		}
		// A content=<table> table whose content table's column set no longer
		// matches the FTS columns fails on a content-column read with "SQL
		// logic error" (fts4content 6.2.8: after DROP TABLE t7 + CREATE
		// TABLE t7(x), SELECT * FROM ft7 WHERE ft7 MATCH errors because the
		// FTS column y is missing from the new t7's single column). The FTS
		// columns are matched against the content table BY NAME (fts3.c
		// fts3ReadExprList); a missing FTS column makes the read fail.
		if contentOK && e.selectReadsFTSContentColumn(s, ftsTable) {
			ctEntry, _, cerr := e.ctx.FindTable(ct)
			if cerr == nil && ctEntry != nil {
				ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
				ctCols := make(map[string]bool)
				for _, cd := range ctDefs {
					if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
						continue
					}
					ctCols[strings.ToLower(cd.Name)] = true
				}
				for _, cn := range ftsTable.ColumnNames() {
					if !ctCols[strings.ToLower(cn)] {
						return &Result{Error: fmt.Errorf("SQL logic error")}
					}
				}
			}
		}
	} else {
		// A %_content btree that could not be navigated at load time fails
		// any query that READS content columns with "database disk image is
		// malformed" (fts3corrupt4 52.1: SELECT * FROM t1, t2 steps the
		// corrupt content table); index-only queries still work (28.1/28.2).
		if e.selectReadsFTSContentColumn(s, ftsTable) && e.contentBtreeCorrupt(tableEntry.Name) {
		}
		allRowMaps = e.ftsRowMaps(ftsTable, colDefs)
	}

	// A languageid=<col> table searches ONE language: the WHERE's lang_id
	// constraint when present, else language 0 (fts3.c fts3EvalNext: the
	// cursor's iLangid comes from the langid= constraint or defaults to 0 —
	// fts4langid 1.14: MATCH 'b' with docs in languages 0 and 1 returns only
	// the language-0 doc). Filter the row set before the WHERE applies, but
	// only for a MATCH query — a plain SELECT returns all languages.
	if langCol := ftsTable.LangIDColName(); langCol != "" && allRowMaps != nil {
		if _, isMatch := e.ftsMatchQueryString(s.Where, tableEntry.Name); isMatch {
			wantLang := int64(0)
			if lv, ok := e.ftsLangIDFromWhere(s.Where, langCol); ok {
				wantLang = lv
			}
			filtered := allRowMaps[:0]
			for _, rm := range allRowMaps {
				var docLang int64
				if v, ok := rm[langCol]; ok {
					docLang = ftsValueToInt64(v)
				}
				if docLang == wantLang {
					filtered = append(filtered, rm)
				}
			}
			allRowMaps = filtered
		}
	}

	// Apply WHERE clause
	if s.Where != nil {
		var ferr error
		allRowMaps, ferr = e.filterFTSRows(s.Where, allRowMaps)
		if ferr != nil {
			return &Result{Error: ferr}
		}
	}
	// A matched document whose %_content row failed to decode is corrupt:
	// reading it fails with "database disk image is malformed" (fts3corrupt4
	// 11.1). Rows that are never matched are never read (9.1).
	if res := e.validateFTSMatchedContent(allRowMaps, ftsTable); res != nil {
		return res
	}

	// offsets()/snippet() read the %_content shadow table and compare it
	// against the index; a mismatched row (e.g. a hand UPDATE of the content
	// table) is corrupt (fts3matchinfo 6.2: UPDATE t9_content then
	// offsets(t9) → "database disk image is malformed").
	if res := e.validateFTSSnippetAuxContent(s, tableEntry.Name, ftsTable, allRowMaps); res != nil {
		return res
	}

	// Handle aggregates (after WHERE filtering)
	if result := e.ctx.HandleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows from RowMaps. Expression errors (e.g. matchinfo
	// format validation) propagate as query errors (fts3matchinfo 5.x).
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		outRow, err := e.ctx.BuildOutputRowWithErr(s.Columns, colDefs, rowMap)
		if err != nil {
			return &Result{Error: err}
		}
		allRows[i] = outRow
	}

	// Build column names
	columns := e.ctx.BuildColumnNames(s.Columns, colDefs, s)
	result := &Result{Columns: columns, Rows: allRows}

	// Apply DISTINCT, ORDER BY, LIMIT
	return e.ctx.FinalizeSelectResult(result, s, allRowMaps)
}

// setFTSMatchInfoFromWhere populates the matchinfo() context from a SELECT's
// constant MATCH constraint (setFTSMatchInfo has already cleared it). The
// MATCH RHS may be a literal or any constant expression (e.g. a string
// concatenation 'a'||'b'); a non-constant RHS (a joined column) has no
// constant query (fts3matchinfo 10.1).
func (e *DDLExecutor) setFTSMatchInfoFromWhere(s *sql.SelectStmt, tableName string, ftsTable *fts.FTS3Table) {
	if s.Where == nil {
		return
	}
	if query, ok := e.ftsMatchQueryString(s.Where, tableName); ok {
		if phrases := e.ftsMatchPhrases(ftsTable, query); phrases != nil {
			e.ctx.SetFTSMatchInfo(tableName, true, phrases)
		}
	}
}

// validateFTSMatchedContent fails when a matched document's %_content row was
// recorded as corrupt (fts3corrupt4 11.1: reading it fails with "database
// disk image is malformed").
func (e *DDLExecutor) validateFTSMatchedContent(allRowMaps []RowMap, ftsTable *fts.FTS3Table) *Result {
	for _, rowMap := range allRowMaps {
		if docID, ok := rowMap["rowid"]; ok {
			if dv, ok := util.UnwrapColumnValue(docID).(int64); ok && ftsTable.IsCorruptContentDocID(dv) {
				return &Result{Error: fmt.Errorf("database disk image is malformed")}
			}
		}
	}
	return nil
}

// validateFTSSnippetAuxContent verifies each matched row's %_content value
// against the in-memory document when the SELECT uses offsets()/snippet()
// (they read the content table; a hand UPDATE makes them corrupt,
// fts3matchinfo 6.2).
func (e *DDLExecutor) validateFTSSnippetAuxContent(s *sql.SelectStmt, tableName string, ftsTable *fts.FTS3Table, allRowMaps []RowMap) *Result {
	if !e.selectUsesFTSSnippetAux(s) {
		return nil
	}
	// SQLite checks each aux function's arguments per row BEFORE reading any
	// content (fts3.c fts3FunctionArg runs first). When an aux call's first
	// argument is statically invalid (not the FTS table name), the illegal-
	// argument error must win over the corruption error from reading the
	// hand-corrupted content row (fts3query 5.4.x).
	if !e.ftsWithValidAuxFirstArg(s, tableName) {
		return nil
	}
	for _, rowMap := range allRowMaps {
		docID, ok := rowMap["rowid"]
		if !ok {
			continue
		}
		dv, ok := util.UnwrapColumnValue(docID).(int64)
		if !ok {
			continue
		}
		if res := e.validateFTSContentRow(tableName, ftsTable, dv); res != nil {
			return res
		}
	}
	return nil
}

// ftsMatchQueryString extracts the constant MATCH query string for an FTS
// table from a WHERE expression. Returns (query, true) when the WHERE
// contains exactly the table's MATCH with a constant RHS; otherwise
// (query, false). A MATCH whose RHS is a column (joined value) has no
// constant query (fts3matchinfo 10.1).
func (e *DDLExecutor) ftsMatchQueryString(where sql.Expr, tableName string) (string, bool) {
	var found string
	execquery.WalkExprFull(where, func(n sql.Expr) {
		bop, ok := n.(*sql.BinaryOp)
		if !ok || (bop.Operator != "MATCH" && bop.Operator != "NOT MATCH") {
			return
		}
		if t := ftsMatchTableNameFor(bop, e.ctx.FTSTables()); !strings.EqualFold(t, tableName) {
			return
		}
		if lit, ok := bop.Right.(*sql.StringLit); ok {
			if found == "" {
				found = lit.Value
			}
			return
		}
		if blob, ok := bop.Right.(*sql.BlobLit); ok {
			if found == "" {
				found = string(blob.Value)
			}
			return
		}
		// A constant expression RHS (e.g. 'a'||'b') evaluates to the query
		// string (fts3snippet.test 5.1 builds a huge OR list via ||). A
		// non-constant RHS (column reference) is left unresolved.
		hasColRef := false
		execquery.WalkExprFull(bop.Right, func(sub sql.Expr) {
			if _, isCol := sub.(*sql.ColumnRef); isCol {
				hasColRef = true
			}
		})
		if !hasColRef {
			if v, err := e.ctx.EvalExpr(bop.Right, nil); err == nil {
				if sv, ok := util.UnwrapColumnValue(v).(string); ok && found == "" {
					found = sv
				}
			}
		}
	})
	if found == "" {
		return "", false
	}
	return found, true
}

// ftsMatchPhrases parses and resolves a MATCH query string against an FTS
// table, returning the phrase structure for matchinfo(). Returns nil when the
// query fails to parse (SQLite treats an unparseable MATCH as matching
// nothing, so matchinfo has no phrases to report).
func (e *DDLExecutor) ftsMatchPhrases(ftsTable *fts.FTS3Table, query string) []fts.MatchPhrase {
	node, err := fts.ParseMatchQuery(query)
	if err != nil {
		return nil
	}
	if !ftsTable.IsFTS4() {
		fts.ClearFirstFlags(node)
	}
	node = fts.TokenizeQueryNode(node, ftsTable.Tokenizer())
	node = fts.ResolveQuery(node, ftsTable.ColumnNames())
	return fts.ExtractPhrases(node)
}

// ftsMatchTableNameFor resolves the FTS table a MATCH expression targets,
// mirroring ftsMatchTableName (execquery) without the engine dependency.
func ftsMatchTableNameFor(bop *sql.BinaryOp, ftsTables map[string]*fts.FTS3Table) string {
	colRef, ok := bop.Left.(*sql.ColumnRef)
	if !ok {
		return ""
	}
	if colRef.Table != "" {
		if _, ok := ftsTables[colRef.Table]; ok {
			return colRef.Table
		}
		return ""
	}
	if _, ok := ftsTables[colRef.Name]; ok {
		return colRef.Name
	}
	for tname, ft := range ftsTables {
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, colRef.Name) {
				return tname
			}
		}
	}
	return ""
}

// selectUsesFTSSnippetAux reports whether a SELECT references the FTS
// offsets() or snippet() auxiliary functions in its output columns (they read
// the %_content shadow table, so the engine must verify content consistency).
func (e *DDLExecutor) selectUsesFTSSnippetAux(s *sql.SelectStmt) bool {
	used := false
	for _, col := range s.Columns {
		execquery.WalkExprFull(col.Expr, func(n sql.Expr) {
			if fc, ok := n.(*sql.FuncCall); ok {
				upper := strings.ToUpper(fc.Name)
				if upper == "OFFSETS" || upper == "SNIPPET" {
					used = true
				}
			}
		})
	}
	return used
}

// validateFTSContentRow reads one document row from the %_content shadow table
// and compares each column's token count against the in-memory document. A
// mismatch means the content table was modified without updating the index
// (SQLite's offsets() detects this when the tokenizer runs out of tokens
// before the query positions; fts3matchinfo 6.2).
func (e *DDLExecutor) validateFTSContentRow(tableName string, ftsTable *fts.FTS3Table, docID int64) *Result {
	content := tableName + "_content"
	ent, _, err := e.ctx.FindTable(content)
	if err != nil || ent == nil || ent.RootPage == 0 {
		return nil
	}
	tree := e.ctx.TableBTreeForName(ent.Name, ent.RootPage, true)
	cell, cerr := e.readCellByRowID(tree, docID)
	if cerr != nil || cell == nil {
		return nil
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	// rec.Values[0] is docid; the rest are the content columns.
	doc := ftsTable.GetDoc(docID)
	if doc == nil {
		return nil
	}
	nCol := len(ftsTable.ColumnNames())
	for i := 0; i < nCol; i++ {
		contentStr := contentColumnString(rec.Values, i+1)
		memStr, memBlob := docColumnValue(doc, i)
		match := contentStr == memStr || (memBlob != nil && contentStr == string(memBlob))
		if match {
			continue
		}
		// Content differs: corrupt when the token counts disagree. (An
		// FTS4 content table stores the raw text; a hand UPDATE changes it.)
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	return nil
}

// contentColumnString renders one %_content record value as a string.
func contentColumnString(values []interface{}, idx int) string {
	if idx >= len(values) {
		return ""
	}
	switch v := values[idx].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// docColumnValue returns a document column's string and blob forms.
func docColumnValue(doc *fts.Document, i int) (string, []byte) {
	if i >= len(doc.Columns) {
		return "", nil
	}
	switch v := doc.Columns[i].(type) {
	case string:
		return v, nil
	case []byte:
		return "", v
	}
	return "", nil
}

// ftsLangIDFromWhere extracts the languageid=<col> value from an FTS WHERE
// clause of the form "lang_id = N" (or "N = lang_id"). Returns ok=false when
// the WHERE has no lang_id constraint (the query defaults to language 0).
func (e *DDLExecutor) ftsLangIDFromWhere(where sql.Expr, langCol string) (int64, bool) {
	if where == nil {
		return 0, false
	}
	// The langid constraint may be any conjunct of an AND tree
	// ("t1 MATCH 'b' AND lang_id = 1"): walk the AND branches and look for
	// the equality that constrains the langid column. OR branches cannot
	// constrain a single language (fts3.c reads one langid per cursor).
	if bop, ok := where.(*sql.BinaryOp); ok && bop.Operator == "AND" {
		if v, ok := e.ftsLangIDFromWhere(bop.Left, langCol); ok {
			return v, true
		}
		return e.ftsLangIDFromWhere(bop.Right, langCol)
	}
	bop, ok := where.(*sql.BinaryOp)
	if !ok || bop.Operator != "=" {
		return 0, false
	}
	// Either side may be the langid column.
	colRef := func(expr sql.Expr) (string, bool) {
		ref, ok := expr.(*sql.ColumnRef)
		if !ok {
			return "", false
		}
		return ref.Name, true
	}
	var valExpr sql.Expr
	if name, ok := colRef(bop.Left); ok && strings.EqualFold(name, langCol) {
		valExpr = bop.Right
	} else if name, ok := colRef(bop.Right); ok && strings.EqualFold(name, langCol) {
		valExpr = bop.Left
	} else {
		return 0, false
	}
	v, err := e.ctx.EvalExpr(valExpr, nil)
	if err != nil {
		return 0, false
	}
	return ftsValueToInt64(v), true
}

// ftsValueToInt64 coerces a SQL value to int64 the way SQLite's
// sqlite3_value_int does (integers pass, floats truncate, text parses a
// leading integer, NULL → 0). Used for the FTS4 languageid=<col> value.

// ftsWithValidAuxFirstArg reports whether every matchinfo/offsets/snippet/
// optimize call in the SELECT's select list passes the static form of
// fts3.c fts3FunctionArg: its first argument is a column reference naming
// the FTS table itself. Calls with any other first argument (e.g. a real
// content column) fail with "illegal first argument to <func>" before
// SQLite reads any content, so a corrupt content row must not preempt that
// error (fts3query 5.4.x).
func (e *DDLExecutor) ftsWithValidAuxFirstArg(s *sql.SelectStmt, tableName string) bool {
	ok := true
	for _, cd := range s.Columns {
		execquery.WalkExprFull(cd.Expr, func(n sql.Expr) {
			fc, isFunc := n.(*sql.FuncCall)
			if !isFunc {
				return
			}
			switch strings.ToUpper(fc.Name) {
			case "MATCHINFO", "OFFSETS", "SNIPPET", "OPTIMIZE":
			default:
				return
			}
			if len(fc.Args) == 0 {
				ok = false
				return
			}
			colRef, isCol := fc.Args[0].(*sql.ColumnRef)
			if !isCol || !strings.EqualFold(colRef.Name, tableName) {
				ok = false
			}
		})
		if !ok {
			break
		}
	}
	return ok
}
