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
			_, rankVal := fts5HiddenValues(t5, values)
			handled, err := t5.SpecialCommand(s, []interface{}{util.UnwrapColumnValue(rankVal)})
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
	// Resolve the rowid: explicit, or auto-allocated (max existing + 1).
	var rowid int64
	if fixedRowID != nil {
		rowid = *fixedRowID
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
	if ferr := t5.FlushShadowIfDirty(); ferr != nil {
		return &Result{Error: ferr}
	}
	return &Result{Changes: deleted}
}

// execFTS5Update implements UPDATE on an fts5 table.
func (e *DMLExecutor) execFTS5Update(t5 *fts5.Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result {
	cfg := t5.Config()
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
			// Only unindexed columns changed: the index is untouched and the
			// content table holds nothing to update (contentless) — report
			// the matched row count with no index work.
			rowids, err := fts5MatchedRowids(e, t5, colDefs, s.Where)
			if err != nil {
				return &Result{Error: err}
			}
			return &Result{Changes: int64(len(rowids))}
		}
	}
	rowMaps, rowids, err := e.fts5MatchedRows(t5, colDefs, s.Where, nil, nil)
	if err != nil {
		return &Result{Error: err}
	}
	updated := int64(0)
	for i, rowid := range rowids {
		newRowid := rowid
		newVals := make([]interface{}, len(t5.ColumnNames()))
		copy(newVals, fts5RowValues(t5, rowMaps[i]))
		changed := false
		for _, a := range s.Assignments {
			lower := strings.ToLower(a.Column)
			if lower == "rowid" || lower == "_rowid_" || lower == "oid" {
				v, verr := e.ctx.EvalExpr(a.Value, rowMaps[i])
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
			v, verr := e.ctx.EvalExpr(a.Value, rowMaps[i])
			if verr != nil {
				return &Result{Error: verr}
			}
			newVals[idx] = v
			changed = true
		}
		if !changed {
			continue
		}
		if _, derr := t5.Delete(rowid); derr != nil {
			return &Result{Error: derr}
		}
		if err := t5.Insert(newRowid, newVals); err != nil {
			return &Result{Error: err}
		}
		updated++
	}
	if ferr := t5.FlushShadowIfDirty(); ferr != nil {
		return &Result{Error: ferr}
	}
	return &Result{Changes: updated}
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
	rowMaps := fts5DMLRowMaps(rowids, values, t5)
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
