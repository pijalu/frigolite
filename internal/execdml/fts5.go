package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// fts5 DML execution: INSERT/DELETE/UPDATE against an fts5 virtual table
// (fts5_main.c fts5UpdateMethod). The table instance owns the index and
// shadow IO; these helpers map statement values, enforce the contentless
// rules and drive rowid allocation.

// flushFTS5Shadow persists the table's pending index blob without disturbing
// the connection's last_insert_rowid: the %_data id=11 write goes through the
// SQL layer, which would otherwise clobber lastRowID (C's shadow writes run
// through the storage API and never touch db->lastRowid — the same
// preserve-and-restore the FTS3 segment flush applies).
func (e *DMLExecutor) flushFTS5Shadow(t5 *fts5.Table) error {
	saved := e.ctx.LastRowID()
	if err := t5.FlushShadowIfDirty(); err != nil {
		return err
	}
	e.ctx.SetLastRowID(saved)
	return nil
}

// fts5UserValues maps an insert's values tuple (indexed by the colDefs order:
// user columns then the hidden table-name and rank columns) onto the user
// columns.
func fts5UserValues(t5 *fts5.Table, values []interface{}) []interface{} {
	n := len(t5.ColumnNames())
	out := make([]interface{}, n)
	for i := 0; i < n && i < len(values); i++ {
		out[i] = values[i]
	}
	return out
}

// fts5HiddenValues extracts the hidden (table-name, rank) column values.
func fts5HiddenValues(t5 *fts5.Table, values []interface{}) (cmd interface{}, rank interface{}) {
	n := len(t5.ColumnNames())
	if n < len(values) {
		cmd = values[n]
	}
	if n+1 < len(values) {
		rank = values[n+1]
	}
	return cmd, rank
}

// insertSourceIsVocabOver reports whether the INSERT...SELECT sources a
// fts5vocab virtual table defined over the named fts5 table (fts5vocab2.test
// 5.1/5.2's conflicting write aborts with SQLITE_ABORT).
func insertSourceIsVocabOver(ctx DMLContext, sel *sql.SelectStmt, target string) bool {
	if sel == nil || sel.From.Name == "" {
		return false
	}
	entry, _, err := ctx.FindTable(sel.From.Name)
	if err != nil || entry == nil || entry.RootPage != 0 {
		return false
	}
	modName, args, ok := splitVtabSQLHead(entry.SQL)
	if !ok || !strings.EqualFold(modName, "fts5vocab") || len(args) == 0 {
		return false
	}
	return strings.EqualFold(dequoteFirstArg(args[0]), target)
}

// splitVtabSQLHead extracts the module name and argument texts from a stored
// CREATE VIRTUAL TABLE statement.
func splitVtabSQLHead(sqlStr string) (module string, args []string, ok bool) {
	up := strings.ToUpper(sqlStr)
	idx := strings.Index(up, " USING ")
	if idx < 0 {
		return "", nil, false
	}
	rest := strings.TrimSpace(sqlStr[idx+len(" USING "):])
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rest), ";")), nil, true
	}
	module = strings.ToLower(strings.TrimSpace(rest[:open]))
	inner := rest[open+1:]
	if close := strings.LastIndexByte(inner, ')'); close >= 0 {
		inner = inner[:close]
	}
	for _, a := range strings.Split(inner, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			args = append(args, a)
		}
	}
	return module, args, true
}

// dequoteFirstArg removes one level of SQL quoting from a module argument.
func dequoteFirstArg(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"' || s[0] == '`' || s[0] == '[') {
		q := s[0]
		if q == '[' {
			q = ']'
		}
		if s[len(s)-1] == q {
			return strings.ReplaceAll(s[1:len(s)-1], string(q)+string(q), string(q))
		}
	}
	return s
}

// rankValue returns the unwrapped value of the hidden rank column.
func rankValue(t5 *fts5.Table, values []interface{}) interface{} {
	_, rankVal := fts5HiddenValues(t5, values)
	return util.UnwrapColumnValue(rankVal)
}

// fts5SpecialCommandArgs builds the argument vector of one special-insert
// directive: most commands take their value from the rank slot (C's
// apVal[2+nCol]); 'delete' reads apVal[1] — the explicit rowid — and
// apVal[2..] — the supplied user column values (fts5SpecialDelete).
func fts5SpecialCommandArgs(cmd string, userVals []interface{}, rankVal interface{}, fixedRowID *int64) []interface{} {
	if fixedRowID != nil && strings.EqualFold(cmd, "delete") {
		args := make([]interface{}, 0, len(userVals)+1)
		args = append(args, *fixedRowID)
		for _, uv := range userVals {
			args = append(args, util.UnwrapColumnValue(uv))
		}
		return args
	}
	return []interface{}{rankVal}
}

// insertFTS5Row routes one INSERT row to an fts5 table (fts5UpdateMethod's
// insert + special-insert paths). values is indexed by the fts5 colDefs order
// (user columns, then the hidden table-name and rank columns).
func (e *DMLExecutor) insertFTS5Row(t5 *fts5.Table, tableEntry *schema.Entry, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	userVals := fts5UserValues(t5, values)
	cmdVal, _ := fts5HiddenValues(t5, values)
	// A non-NULL table-name hidden column is a special insert directive
	// (INSERT INTO t1(t1, rank) VALUES('delete-all', ...) — fts5UpdateMethod).
	if cmdVal != nil {
		cmd := util.UnwrapColumnValue(cmdVal)
		if s, ok := cmd.(string); ok {
			cmdArgs := fts5SpecialCommandArgs(s, userVals, rankValue(t5, values), fixedRowID)
			handled, err := t5.SpecialCommand(s, cmdArgs)
			if err != nil {
				return &Result{Error: err}
			}
			if handled {
				e.ctx.SetLastRowID(0)
				return &Result{Changes: 0, LastInsertRowID: 0}
			}
			// An unknown directive reaches fts5ConfigSetValue's badkey path:
			// C's generic SQLITE_ERROR.
			return &Result{Error: fmt.Errorf("SQL logic error")}
		}
	}
	// It is an error to write an fts5_locale() value to a table without the
	// locale=1 option (fts5_main.c:2005-2020, SQLITE_MISMATCH).
	if !t5.Config().Locale {
		for _, v := range userVals {
			if fts5.IsLocaleValue(util.UnwrapColumnValue(v)) {
				return &Result{Error: fmt.Errorf("fts5_locale() requires locale=1")}
			}
		}
	}
	// Resolve the rowid: explicit, or auto-allocated (max existing + 1).
	var rowid int64
	if fixedRowID != nil {
		rowid = *fixedRowID
	} else if t5.Config().EContent != fts5.ContentNormal &&
		t5.Config().EContent != fts5.ContentUnindexed && !t5.Config().ColumnSize {
		// A NONE/EXTERNAL content table without columnsize has no backing
		// store to allocate a rowid from: fts5StorageNewRowid returns
		// SQLITE_MISMATCH and the user must provide the rowid explicitly
		// (fts5columnsize 2.1: content='' inserts).
		return &Result{Error: fmt.Errorf("datatype mismatch")}
	} else {
		rowid = t5.NextRowid()
	}
	// The rowid is a PRIMARY KEY: an existing document conflicts unless the
	// statement resolved with OR REPLACE/IGNORE (fts5UpdateMethod through the
	// generic vtab conflict handling).
	if t5.HasDoc(rowid) {
		switch strings.ToUpper(orConflict) {
		case "REPLACE":
			if _, err := t5.Delete(rowid); err != nil {
				return &Result{Error: err}
			}
		case "IGNORE":
			return &Result{Changes: 0}
		default:
			return &Result{Error: fmt.Errorf("constraint failed")}
		}
	}
	if err := t5.Insert(rowid, userVals); err != nil {
		return &Result{Error: err}
	}
	e.ctx.SetLastRowID(rowid)
	return &Result{Changes: 1, LastInsertRowID: rowid}
}

// execFTS5Delete implements DELETE on an fts5 table.
func (e *DMLExecutor) execFTS5Delete(t5 *fts5.Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	cfg := t5.Config()
	// A contentless table without contentless_delete rejects every DELETE
	// with a WHERE (fts5_main.c:1992); the WHERE-less form is the 'delete-all'
	// reset and is allowed.
	if cfg.Contentless() && !cfg.ContentlessDelete {
		if s.Where != nil {
			return &Result{Error: fmt.Errorf("cannot DELETE from contentless fts5 table: %s", t5.Name())}
		}
		if err := t5.DeleteAll(); err != nil {
			return &Result{Error: err}
		}
		return &Result{Changes: 0}
	}
	rowids, err := fts5MatchedRowids(e, t5, colDefs, s.Where)
	if err != nil {
		return &Result{Error: err}
	}
	deleted := int64(0)
	for _, rowid := range rowids {
		ok, derr := t5.Delete(rowid)
		if derr != nil {
			return &Result{Error: derr}
		}
		if ok {
			deleted++
		}
	}
	if ferr := e.flushFTS5Shadow(t5); ferr != nil {
		return &Result{Error: ferr}
	}
	return &Result{Changes: deleted}
}

// execFTS5Update implements UPDATE on an fts5 table.
func (e *DMLExecutor) execFTS5Update(t5 *fts5.Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result {
	cfg := t5.Config()
	fromJoin := s.From.Name != "" || s.From.Subquery != nil || len(s.From.Args) > 0
	if fromJoin {
		// SQLite resolves the UPDATE ... FROM sources at prepare time: a
		// missing FROM table errors even when no target row matches
		// (fts4upfrom 1.x.4 "no such table: changes").
		if _, jerr := e.JoinUpdateFromRows(s, nil); jerr != nil {
			return &Result{Error: jerr}
		}
	}
	if cfg.Contentless() {
		// fts5ContentlessUpdate (fts5_main.c:1847): only unindexed columns
		// may change on a contentless table; with contentless_delete=1 every
		// indexed column must be assigned at once (not a subset).
		assigned := make(map[string]bool)
		for _, a := range s.Assignments {
			assigned[strings.ToLower(a.Column)] = true
		}
		if cfg.ContentlessDelete {
			missing := false
			for i, col := range t5.ColumnNames() {
				if !cfg.Unindexed[i] && !assigned[strings.ToLower(col)] {
					missing = true
					break
				}
			}
			if missing {
				return &Result{Error: fmt.Errorf("cannot UPDATE a subset of columns on fts5 contentless-delete table: %s", t5.Name())}
			}
		} else {
			indexedAssigned := false
			for i, col := range t5.ColumnNames() {
				if !cfg.Unindexed[i] && assigned[strings.ToLower(col)] {
					indexedAssigned = true
					break
				}
			}
			if indexedAssigned {
				return &Result{Error: fmt.Errorf("cannot UPDATE contentless fts5 table: %s", t5.Name())}
			}
			// Only unindexed columns changed: the index is untouched. A
			// contentless_unindexed table rewrites the stored (UNINDEXED)
			// columns of each affected row — fts5UpdateMethod's bContent
			// branch (sqlite3Fts5StorageContentInsert with bContent=1).
			if fromJoin {
				n, err := e.fts5FromMatchedCount(t5, s)
				if err != nil {
					return &Result{Error: err}
				}
				return &Result{Changes: n}
			}
			rowMaps, rowids, err := e.fts5MatchedRows(t5, colDefs, s.Where, nil, nil)
			if err != nil {
				return &Result{Error: err}
			}
			if t5.Config().EContent == fts5.ContentUnindexed {
				for i, rowid := range rowids {
					newVals := make([]interface{}, len(t5.ColumnNames()))
					copy(newVals, fts5RowValues(t5, rowMaps[i]))
					for _, a := range s.Assignments {
						idx := t5.ColumnIndex(a.Column)
						if idx < 0 {
							return &Result{Error: fmt.Errorf("no such column: %s", a.Column)}
						}
						v, verr := e.ctx.EvalExpr(a.Value, rowMaps[i])
						if verr != nil {
							return &Result{Error: verr}
						}
						newVals[idx] = v
					}
					if uerr := t5.UpdateUnindexedContent(rowid, newVals); uerr != nil {
						return &Result{Error: uerr}
					}
				}
				if ferr := e.flushFTS5Shadow(t5); ferr != nil {
					return &Result{Error: ferr}
				}
			}
			return &Result{Changes: int64(len(rowids))}
		}
	}
	var rowMaps []RowMap
	var rowids []int64
	var err error
	if fromJoin {
		// No pre-filtering by WHERE: its FROM-column terms can only be
		// evaluated per (target, FROM) pair below.
		rowMaps, rowids, err = e.fts5UniverseRows(t5, s.Where)
	} else {
		rowMaps, rowids, err = e.fts5MatchedRows(t5, colDefs, s.Where, nil, nil)
	}
	if err != nil {
		return &Result{Error: err}
	}
	updated := int64(0)
	for i, rowid := range rowids {
		evalMap := rowMaps[i]
		if fromJoin {
			joined, ok, jerr := e.fts5JoinedEvalMap(s, rowMaps[i])
			if jerr != nil {
				return &Result{Error: jerr}
			}
			if !ok {
				continue
			}
			evalMap = joined
		}
		newRowid := rowid
		newVals := make([]interface{}, len(t5.ColumnNames()))
		copy(newVals, fts5RowValues(t5, rowMaps[i]))
		changed := false
		for _, a := range s.Assignments {
			lower := strings.ToLower(a.Column)
			if lower == "rowid" || lower == "_rowid_" || lower == "oid" {
				v, verr := e.ctx.EvalExpr(a.Value, evalMap)
				if verr != nil {
					return &Result{Error: verr}
				}
				if n, ok := util.UnwrapColumnValue(v).(int64); ok {
					newRowid = n
					changed = true
				}
				continue
			}
			idx := t5.ColumnIndex(a.Column)
			if idx < 0 {
				return &Result{Error: fmt.Errorf("no such column: %s", a.Column)}
			}
			v, verr := e.ctx.EvalExpr(a.Value, evalMap)
			if verr != nil {
				return &Result{Error: verr}
			}
			// Writing an fts5_locale() value to a locale-less table is an
			// error (fts5_main.c:2005-2020; the check spans UPDATE values).
			if !t5.Config().Locale && fts5.IsLocaleValue(util.UnwrapColumnValue(v)) {
				return &Result{Error: fmt.Errorf("fts5_locale() requires locale=1")}
			}
			// Store the scalar: an expression that resolves to a joined row
			// cell (UPDATE ... FROM: SET b=o.c) yields the cell's affinity
			// wrapper, which the content write would stringify as Go source
			// ("&{apple 0}") — C binds the plain value (fts4upfrom 1.x).
			newVals[idx] = util.UnwrapColumnValue(v)
			changed = true
		}
		if !changed {
			continue
		}
		// A changed rowid colliding with another document is resolved per
		// the statement's OR action (fts5_main.c fts5UpdateMethod: REPLACE
		// deletes the conflicting document first, IGNORE skips the row).
		if newRowid != rowid && t5.HasDoc(newRowid) {
			switch strings.ToUpper(s.OnConflict) {
			case "REPLACE":
				if _, rerr := t5.Delete(newRowid); rerr != nil {
					return &Result{Error: rerr}
				}
			case "IGNORE":
				continue
			}
		}
		if _, derr := t5.Delete(rowid); derr != nil {
			return &Result{Error: derr}
		}
		if err := t5.Insert(newRowid, newVals); err != nil {
			return &Result{Error: err}
		}
		updated++
	}
	if ferr := e.flushFTS5Shadow(t5); ferr != nil {
		return &Result{Error: ferr}
	}
	return &Result{Changes: updated}
}

// fts5JoinedEvalMap builds the evaluation row map for one target document in
// an UPDATE ... FROM: the document's row map merged with the FIRST joined
// FROM row whose WHERE is satisfied (fts4upfrom 1.x: UPDATE ft SET b=o.c
// FROM ft AS o WHERE ft.a == ...). ok is false when no FROM row matches (the
// document is not updated). With no WHERE, the first joined row applies.
func (e *DMLExecutor) fts5JoinedEvalMap(s *sql.UpdateStmt, base RowMap) (RowMap, bool, error) {
	joined, jerr := e.JoinUpdateFromRows(s, base)
	if jerr != nil {
		return nil, false, jerr
	}
	if len(joined) == 0 {
		return base, true, nil
	}
	if s.Where != nil {
		for _, jrow := range joined {
			match, merr := e.ctx.EvalBool(s.Where, jrow)
			if merr == nil && match {
				return jrow, true, nil
			}
		}
		return base, false, nil
	}
	return joined[0], true, nil
}

// fts5FromMatchedCount counts the documents an UPDATE ... FROM would update
// (the contentless path's change count): the WHERE is evaluated per joined
// (target, FROM) pair.
func (e *DMLExecutor) fts5FromMatchedCount(t5 *fts5.Table, s *sql.UpdateStmt) (int64, error) {
	rowMaps, _, err := e.fts5UniverseRows(t5, s.Where)
	if err != nil {
		return 0, err
	}
	n := int64(0)
	for i := range rowMaps {
		_, ok, jerr := e.fts5JoinedEvalMap(s, rowMaps[i])
		if jerr != nil {
			return 0, jerr
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// fts5RowValues extracts a row map's user-column values in declared order.
func fts5RowValues(t5 *fts5.Table, rowMap RowMap) []interface{} {
	out := make([]interface{}, len(t5.ColumnNames()))
	for i, col := range t5.ColumnNames() {
		if v, ok := rowMap[col]; ok {
			out[i] = util.UnwrapColumnValue(v)
		}
	}
	return out
}

// fts5MatchedRowids evaluates the WHERE clause (with MATCH) and returns the
// matching rowids.
func fts5MatchedRowids(e *DMLExecutor, t5 *fts5.Table, colDefs []sql.ColumnDef, where sql.Expr) ([]int64, error) {
	rowMaps, rowids, err := e.fts5MatchedRows(t5, colDefs, where, nil, nil)
	if err != nil {
		return nil, err
	}
	_ = rowMaps
	return rowids, nil
}

// fts5MatchedRows evaluates the WHERE clause (with MATCH) over the table's
// documents and returns the surviving row maps and rowids. A top-level MATCH
// conjunct drives the scan universe from the index (xFilter parity), so
// index-only documents of external-content tables are visible.
func (e *DMLExecutor) fts5MatchedRows(t5 *fts5.Table, colDefs []sql.ColumnDef, where sql.Expr, orderBy []sql.OrderByTerm, limit sql.Expr) ([]RowMap, []int64, error) {
	_ = orderBy
	_ = limit
	rowMaps, rowids, err := e.fts5UniverseRows(t5, where)
	if err != nil {
		return nil, nil, err
	}
	var matched []RowMap
	var matchedIDs []int64
	for i, rowMap := range rowMaps {
		if where == nil {
			matched = append(matched, rowMap)
			matchedIDs = append(matchedIDs, rowids[i])
			continue
		}
		pass, perr := e.ctx.RowPassesWhere(where, rowMap, nil)
		if perr != nil {
			return nil, nil, perr
		}
		if pass {
			matched = append(matched, rowMap)
			matchedIDs = append(matchedIDs, rowids[i])
		}
	}
	return matched, matchedIDs, nil
}

// fts5UniverseRows resolves the UPDATE/DELETE scan universe (the MATCH
// conjunct's index hit, else every document) WITHOUT applying the WHERE
// filter: the UPDATE ... FROM executor re-evaluates the WHERE per joined
// (target, FROM) row pair, so filtering here against the bare document row
// would drop rows whose WHERE references FROM columns.
func (e *DMLExecutor) fts5UniverseRows(t5 *fts5.Table, where sql.Expr) ([]RowMap, []int64, error) {
	set, err := t5.MatchUniverse(where, func(expr sql.Expr) (interface{}, error) {
		return e.ctx.EvalExpr(expr, nil)
	})
	if err != nil {
		return nil, nil, err
	}
	var rowids []int64
	var values [][]interface{}
	if set != nil {
		rowids = t5.SortedMatchRowids(set)
		for _, rowid := range rowids {
			vals, verr := t5.DocValues(rowid)
			if verr != nil {
				return nil, nil, verr
			}
			values = append(values, vals)
		}
	} else {
		rowids, values, err = t5.ScanDocs()
		if err != nil {
			return nil, nil, err
		}
	}
	return fts5DMLRowMaps(rowids, values, t5), rowids, nil
}

// fts5DMLRowMaps builds WHERE-evaluation row maps for fts5 documents, with
// the qualified <table>.col keys the join-style resolution expects.
func fts5DMLRowMaps(rowids []int64, values [][]interface{}, t5 *fts5.Table) []RowMap {
	nUser := len(t5.ColumnNames())
	out := make([]RowMap, 0, len(rowids))
	for i, rowid := range rowids {
		rowMap := make(RowMap)
		rowMap["rowid"] = &util.ColumnValue{Value: rowid, Affinity: 'I'}
		for c := 0; c < nUser; c++ {
			var v interface{}
			if i < len(values) && c < len(values[i]) {
				v = values[i][c]
			}
			cv := &util.ColumnValue{Value: v, Affinity: 'T'}
			rowMap[t5.ColumnNames()[c]] = cv
			rowMap[t5.Name()+"."+t5.ColumnNames()[c]] = cv
		}
		rowMap[t5.Name()] = &util.ColumnValue{Value: rowid, Affinity: 'I'}
		rowMap["rank"] = &util.ColumnValue{Value: nil, Affinity: 'F'}
		out = append(out, rowMap)
	}
	return out
}

// insertSelectIntoFTS5 inserts SELECT result rows as fts5 documents
// (INSERT INTO t5 SELECT ... — the SELECT path of fts5UpdateMethod's insert).
func (e *DMLExecutor) insertSelectIntoFTS5(t5 *fts5.Table, tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, selectResult *Result) *Result {
	colMapping := buildInsertColumnMapping(s.Columns, colDefs)
	var changes int64
	for _, row := range selectResult.Rows {
		if err := e.ctx.CheckProgress(); err != nil {
			return &Result{Error: err}
		}
		values, explicitRowID, hasExplicitRowID := e.buildInsertSelectValues(row, s.Columns, colMapping, colDefs)
		var fixed *int64
		if hasExplicitRowID {
			fixed = &explicitRowID
		}
		res := e.insertFTS5Row(t5, tableEntry, values, fixed, s.OrConflict)
		if res.Error != nil {
			return res
		}
		changes += res.Changes
	}
	return &Result{Changes: changes}
}
