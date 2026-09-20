// SPDX-License-Identifier: GPL-3.0-or-later

package execdml

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// viewDeclaredColumns returns the explicit column list from a CREATE VIEW
// declaration (CREATE VIEW v(a,b) AS ...). Returns nil when the view has no
// declared column list.
// execInsertView handles INSERT statements whose target is a view. SQLite
// routes such statements through INSTEAD OF triggers; resolving the view's
// columns (which validates collations in its SELECT) happens first.
func (e *DMLExecutor) execInsertView(s *sql.InsertStmt, viewEntry *schema.Entry) *Result {
	// Qualified view column references (main.v5.b) must resolve against the
	// view row during trigger NEW-row evaluation.
	prevDML := e.currentDMLTable
	e.currentDMLTable = viewEntry.Name
	defer func() { e.currentDMLTable = prevDML }()

	// Resolve the view definition: parse and validate its SELECT expressions.
	// This surfaces errors like "no such collation sequence: X" at insert time.
	viewSelect, err := e.viewSelectFromEntry(viewEntry)
	if err != nil {
		return &Result{Error: err}
	}
	if viewSelect != nil {
		if err := e.validateCollationsInSelect(viewSelect); err != nil {
			return &Result{Error: err}
		}
	}

	// Views only accept INSERT when an INSTEAD OF INSERT trigger exists.
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}

	// Build the NEW row: map the INSERTed values to the view's output column
	// names so trigger bodies can reference NEW.col. Column names come from
	// the view's SELECT (aliases when present, else expression text), with
	// bare "*" expanded through the view's FROM source.
	viewCols := e.viewColumnNames(viewSelect)
	// A declared column list (CREATE VIEW v(a,b) AS ...) overrides the
	// SELECT-derived names for NEW-row mapping.
	if decl := e.viewDeclaredColumns(viewEntry); len(decl) > 0 {
		viewCols = decl
	}

	// INSERT ... SELECT into a view: each produced row fires the trigger.
	if s.Select != nil {
		return e.insertViewSelect(s, viewEntry, viewCols)
	}

	// Multi-row VALUES: fire the trigger once per tuple.
	if len(s.Values) > 0 {
		return e.insertViewValues(s, viewEntry, viewCols)
	}

	// DEFAULT VALUES into a view: fire once with an empty NEW row.
	if res := e.fireViewInsertRow(viewEntry, viewNewRow(nil, s.Columns, viewCols)); res != nil {
		return res
	}
	return &Result{Changes: 1}
}

// insertViewSelect fires INSTEAD OF INSERT triggers for each row produced by
// an INSERT ... SELECT whose target is a view. The view insert itself counts
// 0 changes (SQLite: "Changes to a view that are intercepted by INSTEAD OF
// triggers are not counted"); the trigger body's DML counts via its own Exec.
func (e *DMLExecutor) insertViewSelect(s *sql.InsertStmt, viewEntry *schema.Entry, viewCols []string) *Result {
	selResult := e.ctx.ExecSelect(s.Select)
	if selResult.Error != nil {
		return selResult
	}
	for _, rrow := range selResult.Rows {
		if res := e.fireViewInsertRow(viewEntry, viewNewRow(rrow, s.Columns, viewCols)); res != nil {
			return res
		}
	}
	return &Result{}
}

// insertViewValues fires INSTEAD OF INSERT triggers for each tuple of an
// INSERT ... VALUES whose target is a view. The view insert itself counts 0
// changes (the trigger body's DML counts via its own Exec).
func (e *DMLExecutor) insertViewValues(s *sql.InsertStmt, viewEntry *schema.Entry, viewCols []string) *Result {
	for _, tuple := range s.Values {
		values, evalErr := e.evalTuple(viewEntry.Name, tuple, s.Columns, nil)
		if evalErr != nil {
			return &Result{Error: evalErr}
		}
		if res := e.fireViewInsertRow(viewEntry, viewNewRow(values, s.Columns, viewCols)); res != nil {
			return res
		}
	}
	return &Result{}
}

// viewSelectFromEntry parses a view's stored SQL and returns its SELECT.
func (e *DMLExecutor) viewSelectFromEntry(viewEntry *schema.Entry) (*sql.SelectStmt, error) {
	stmts, perr := parse.ParseSQL(viewEntry.SQL)
	if perr != nil {
		return nil, perr
	}
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateViewStmt); ok {
			return c.Select, nil
		}
	}
	return nil, nil
}

// fireViewInsertRow fires INSTEAD OF INSERT triggers for one NEW row.
func (e *DMLExecutor) fireViewInsertRow(viewEntry *schema.Entry, row RowMap) *Result {
	if res := e.fireTriggers(viewEntry.Name, "INSERT", "INSTEAD", row, nil); res != nil && res.Error != nil {
		return res
	}
	return nil
}

// viewNewRow builds the NEW row map for a view INSERT from a value tuple,
// mapping by explicit column list when given, else by view column order.
// An explicit rowid/_rowid_/oid column in the INSERT list exposes its value
// through NEW.rowid to the INSTEAD OF trigger (fts5connect 4.x: REPLACE INTO
// v4(rowid, a, b) fires t4_ai with NEW.rowid=1).
func viewNewRow(values []interface{}, columns []string, viewCols []string) RowMap {
	row := make(RowMap)
	row["rowid"] = nil
	if len(columns) > 0 {
		for i, col := range columns {
			if i < len(values) {
				if execquery.IsRowIDName(col) {
					row["rowid"] = values[i]
				}
				row[col] = values[i]
			}
		}
	} else {
		for i, val := range values {
			if i < len(viewCols) {
				row[viewCols[i]] = val
			}
		}
	}
	return row
}

// viewColumnNames returns the output column names of a view's SELECT: the
// explicit alias when present, otherwise the column reference name or the
// expression text. A bare "*" is expanded through the FROM source (SQLite
// resolves view output columns the same way as the result columns of a plain
// SELECT). For a compound SELECT the head member determines the output names.

// validateCollationsInExpr verifies COLLATE operators in an expression tree.
