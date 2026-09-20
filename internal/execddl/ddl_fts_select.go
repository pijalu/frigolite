// Package execddl: FTS SELECT execution — the phases of execFTSSelect
// (preconditions, row materialization, language filter) plus the MATCH query
// string resolution and snippet/matchinfo content validation they share.
// Split from ddl_trigger_tail.go; behavior unchanged.
package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

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

	ftsTable, res := e.validateFTSSelectPreconditions(s, tableEntry, ftsTable)
	if res != nil {
		return res
	}

	allRowMaps, res := e.materializeFTSRowMaps(s, tableEntry, ftsTable, colDefs)
	if res != nil {
		return res
	}

	// A languageid=<col> table searches ONE language: the WHERE's lang_id
	// constraint when present, else language 0 (fts3.c fts3EvalNext: the
	// cursor's iLangid comes from the langid= constraint or defaults to 0 —
	// fts4langid 1.14: MATCH 'b' with docs in languages 0 and 1 returns only
	// the language-0 doc). Filter the row set before the WHERE applies, but
	// only for a MATCH query — a plain SELECT returns all languages.
	allRowMaps = e.filterFTSRowsByLanguage(s, tableEntry, ftsTable, allRowMaps)

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

// validateFTSSelectPreconditions runs every pre-read check of an FTS SELECT
// (orphan indexes, malformed MATCH, lazy table load, segment validation,
// uncompress safety, empty-index segment load, per-term corruption) and
// returns the possibly-reloaded FTS table.
func (e *DDLExecutor) validateFTSSelectPreconditions(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table) (*fts.FTS3Table, *Result) {
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
			return ftsTable, &Result{Error: err}
		}
	}

	if err := e.validateFTSSelectMatchParse(s, tableEntry, ftsTable); err != nil {
		return ftsTable, &Result{Error: err}
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
		return ftsTable, res
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
		return ftsTable, &Result{Error: fmt.Errorf("SQL logic error")}
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
			return ftsTable, mres
		}
	}

	return ftsTable, nil
}

// validateFTSSelectMatchParse fails a SELECT whose constant MATCH expression
// does not parse — at prepare, before any row is read, even on an empty table
// (fts3.c fts3FilterMethod surfaces the expression parser's SQLITE_ERROR as
// "malformed MATCH expression: [query]"; fts3expr 2.x). A structurally broken
// segment b-tree defeats every term lookup (fts3corrupt7 3.x), so the MATCH
// fails regardless of its terms; a part-failed load stays queryable
// (fts3defer2 1.x vs 1.7).
func (e *DDLExecutor) validateFTSSelectMatchParse(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table) error {
	if s.Where == nil {
		return nil
	}
	qs, ok := e.ftsMatchQueryString(s.Where, tableEntry.Name)
	if !ok {
		return nil
	}
	node, perr := fts.ParseMatchQuery(qs)
	if perr != nil {
		return perr
	}
	_ = node
	// Deferred corruption: a segment load that failed partway leaves
	// the loaded terms queryable; only a query referencing a term
	// whose doclist lives in an unreadable block fails with
	// "database disk image is malformed" (fts3.c reads each term's
	// doclist on demand; fts3defer2 1.x vs 1.7).
	if ftsTable.LoadErr() != nil && ftsTable.StructuralLoadErr() {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}

// materializeFTSRowMaps builds the SELECT's candidate row maps: content-table
// rows for a content=<table> table, index-only rows for a contentless table,
// and the in-memory %_content-backed rows otherwise.
func (e *DDLExecutor) materializeFTSRowMaps(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) ([]RowMap, *Result) {
	// For an FTS4 content=<table> table, row values come from the external
	// content table: an unconstrained SELECT returns every content-table row;
	// a MATCH returns the matched docids' content rows (fts3.c
	// fts3ReadExprList). A contentless (content=) table and a content=<table>
	// whose content table was dropped have no row values: docid/MATCH queries
	// still work off the index, but reading a content column fails
	// (fts4content 7.2.x, 6.2.x).
	var allRowMaps []RowMap
	if ct := ftsTable.ContentTable(); ct != "" || ftsTable.Contentless() {
		contentOK, res := e.resolveFTSContentAvailability(tableEntry, ftsTable, ct)
		if res != nil {
			return nil, res
		}
		allRowMaps = e.loadFTSContentRows(s, tableEntry, ftsTable, colDefs, ct, contentOK)
		if res := e.validateFTSContentColumnRead(s, tableEntry, ftsTable, ct, contentOK); res != nil {
			return nil, res
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
	return allRowMaps, nil
}

// resolveFTSContentAvailability determines whether an FTS table's content
// source can supply row values. contentOK is false for a contentless table or
// when the content table cannot be found. A self-referential content source
// (CREATE VIRTUAL TABLE t1 USING fts4(content=t1)) has no usable content
// either: SQLite fails every read with "SQL logic error" (fts4content 12.x).
func (e *DDLExecutor) resolveFTSContentAvailability(tableEntry *schema.Entry, ftsTable *fts.FTS3Table, ct string) (bool, *Result) {
	contentOK := true
	switch {
	case ftsTable.Contentless():
		contentOK = false
	case strings.EqualFold(ct, tableEntry.Name):
		contentOK = false
	default:
		if _, isFTSContent := e.ctx.FTSTables()[ct]; isFTSContent {
			// A content source that is itself an FTS table (t1 content=t2,
			// t2 content=t1 — fts4content 12.2.x) cannot be read as a
			// content table: SQLite fails EVERY read with "SQL logic error",
			// including count(*) (which reads no content column).
			return false, &Result{Error: fmt.Errorf("SQL logic error")}
		}
		if _, _, cerr := e.ctx.FindTable(ct); cerr != nil {
			contentOK = false
		}
	}
	return contentOK, nil
}

// loadFTSContentRows builds the SELECT's candidate row maps: the MATCH
// query's index docids joined to content (or index-only) rows, else every
// content-table row; see materializeFTSRowMaps.
func (e *DDLExecutor) loadFTSContentRows(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, ct string, contentOK bool) []RowMap {
	var allRowMaps []RowMap
	if s.Where != nil {
		if qs, ok := e.ftsMatchQueryString(s.Where, tableEntry.Name); ok {
			// A MATCH query's row set is the INDEX docids (fts3.c
			// fts3EvalNext: the index is the source of truth for which
			// documents match; the content table only supplies values). A
			// content row deleted after indexing still matches (its
			// values read as NULL/empty).
			matched, merr := ftsTable.MatchDocIDs(qs)
			if merr != nil {
				return nil
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
	if allRowMaps == nil && contentOK {
		// No MATCH constraint (or an unresolvable one): return every
		// content-table row; the WHERE filter applies below. A missing
		// content table yields no rows; a content-column read fails
		// below (fts4content 6.2.2/6.2.4).
		allRowMaps = e.ftsContentTableRowMaps(ftsTable, colDefs, nil)
	}
	return allRowMaps
}

// validateFTSContentColumnRead rejects content-column reads that the content
// source cannot serve; see materializeFTSRowMaps.
func (e *DDLExecutor) validateFTSContentColumnRead(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, ct string, contentOK bool) *Result {
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
	if !contentOK || !e.selectReadsFTSContentColumn(s, ftsTable) {
		return nil
	}
	return e.contentColumnsMatchFTS(ftsTable, ct)
}

// contentColumnsMatchFTS verifies every FTS column still exists (by name) in
// the content table's current schema; see validateFTSContentColumnRead.
func (e *DDLExecutor) contentColumnsMatchFTS(ftsTable *fts.FTS3Table, ct string) *Result {
	ctEntry, _, cerr := e.ctx.FindTable(ct)
	if cerr != nil || ctEntry == nil {
		return nil
	}
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
	return nil
}

// filterFTSRowsByLanguage narrows a MATCH query's row set to ONE language for
// a languageid=<col> table: the WHERE's lang_id constraint when present, else
// language 0 (fts3.c fts3EvalNext: the cursor's iLangid comes from the
// langid= constraint or defaults to 0 — fts4langid 1.14: MATCH 'b' with docs
// in languages 0 and 1 returns only the language-0 doc). Filter the row set
// before the WHERE applies, but only for a MATCH query — a plain SELECT
// returns all languages.
func (e *DDLExecutor) filterFTSRowsByLanguage(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, allRowMaps []RowMap) []RowMap {
	langCol := ftsTable.LangIDColName()
	if langCol == "" || allRowMaps == nil {
		return allRowMaps
	}
	if _, isMatch := e.ftsMatchQueryString(s.Where, tableEntry.Name); !isMatch {
		return allRowMaps
	}
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
	return filtered
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
		e.resolveMatchQueryRHS(bop, &found)
	})
	if found == "" {
		return "", false
	}
	return found, true
}

// resolveMatchQueryRHS extracts the constant query string from one MATCH
// operator's right-hand side into found; see ftsMatchQueryString.
func (e *DDLExecutor) resolveMatchQueryRHS(bop *sql.BinaryOp, found *string) {
	if lit, ok := bop.Right.(*sql.StringLit); ok {
		if *found == "" {
			*found = lit.Value
		}
		return
	}
	if blob, ok := bop.Right.(*sql.BlobLit); ok {
		if *found == "" {
			*found = string(blob.Value)
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
			if sv, ok := util.UnwrapColumnValue(v).(string); ok && *found == "" {
				*found = sv
			}
		}
	}
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
		if !ftsContentColumnMatches(rec.Values, doc, i) {
			// Content differs: corrupt when the token counts disagree. (An
			// FTS4 content table stores the raw text; a hand UPDATE changes it.)
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
	}
	return nil
}

// ftsContentColumnMatches compares one %_content column value against the
// in-memory document's stored value (text or blob form); see
// validateFTSContentRow.
func ftsContentColumnMatches(recValues []interface{}, doc *fts.Document, i int) bool {
	contentStr := contentColumnString(recValues, i+1)
	memStr, memBlob := docColumnValue(doc, i)
	return contentStr == memStr || (memBlob != nil && contentStr == string(memBlob))
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
	valExpr := langEqualityValue(bop, langCol, colRef)
	if valExpr == nil {
		return 0, false
	}
	v, err := e.ctx.EvalExpr(valExpr, nil)
	if err != nil {
		return 0, false
	}
	return ftsValueToInt64(v), true
}

// langEqualityValue returns the value side of a `<langid col> = <expr>`
// equality (either operand order), or nil when neither side is the langid
// column; see ftsLangIDFromWhere.
func langEqualityValue(bop *sql.BinaryOp, langCol string, colRef func(sql.Expr) (string, bool)) sql.Expr {
	if name, ok := colRef(bop.Left); ok && strings.EqualFold(name, langCol) {
		return bop.Right
	}
	if name, ok := colRef(bop.Right); ok && strings.EqualFold(name, langCol) {
		return bop.Left
	}
	return nil
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
	for _, cd := range s.Columns {
		if !auxFirstArgValidIn(cd.Expr, tableName) {
			return false
		}
	}
	return true
}

// auxFirstArgValidIn reports whether every aux call (matchinfo/offsets/
// snippet/optimize) in the expression names the FTS table as its first
// argument; see ftsWithValidAuxFirstArg.
func auxFirstArgValidIn(expr sql.Expr, tableName string) bool {
	ok := true
	execquery.WalkExprFull(expr, func(n sql.Expr) {
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
	return ok
}
