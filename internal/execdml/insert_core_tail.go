// Package execdml implements DML execution.
package execdml

import (
	"fmt"

	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *DMLExecutor) isTempTrigger(t *schema.Entry) bool {
	if t == nil {
		return false
	}
	tc := e.ctx.GetDB("temp")
	if tc == nil {
		return false
	}
	if _, err := tc.Schema.FindTrigger(t.Name); err == nil {
		return true
	}
	return false
}

// unsafeTriggerFunc returns the name of the first unsafe function call in a
// trigger body, or "" when safe. The trigger SQL is parsed and every body
// statement's expressions are checked against the trusted_schema setting.
func (e *DMLExecutor) unsafeTriggerFunc(triggerSQL string) string {
	stmts, perr := parse.ParseSQL(triggerSQL)
	if perr != nil || len(stmts) == 0 {
		return ""
	}
	for _, st := range stmts {
		ct, ok := st.(*sql.CreateTriggerStmt)
		if !ok {
			continue
		}
		for _, bodyStmt := range ct.Statements {
			if name := e.unsafeSchemaFuncInStmt(bodyStmt); name != "" {
				return name
			}
		}
	}
	return ""
}

// unsafeSchemaFuncInStmt returns the first unsafe function name in a statement
// by walking its expressions via the statement's AST.
func (e *DMLExecutor) unsafeSchemaFuncInStmt(stmt sql.Stmt) string {
	var unsafe string
	visitExprsInStmt(stmt, func(expr sql.Expr) {
		if unsafe != "" || expr == nil {
			return
		}
		execquery.WalkExprFull(expr, func(n sql.Expr) {
			if unsafe != "" {
				return
			}
			fc, ok := n.(*sql.FuncCall)
			if !ok {
				return
			}
			if !e.ctx.SchemaFunctionSafe(fc.Name) {
				unsafe = fc.Name
			}
		})
	})
	return unsafe
}

// checkTriggerStmtRefs validates one trigger-body statement's table references
// against the trigger's schema context.

// checkTriggerStmtRefs validates one trigger-body statement's table references
// against the trigger's schema context.

// checkLoadedTableRefCtx verifies a table reference in a loaded trigger
// resolves in the trigger's schema context.
// fireTrigger fires a single trigger matching the given event and timing.
// Returns a Result with an error if execution fails, or nil on success
// (including when the trigger does not match or its WHEN clause is false).
func (e *DMLExecutor) fireTrigger(t *schema.Entry, event, timing string, newRow, oldRow RowMap) *Result {
	// trusted_schema=OFF blocks non-innocuous user functions in trigger
	// bodies (trustschema1-3.110/3.130); TEMP triggers are always trusted.
	if !e.isTempTrigger(t) {
		if name := e.unsafeTriggerFunc(t.SQL); name != "" {
			return &Result{Error: fmt.Errorf("unsafe use of %s()", name)}
		}
	}
	// Enforce SQLite's trigger nesting limit. Recursive trigger chains (with
	// recursive_triggers ON) abort with "triggers nested too deep" once the
	// nesting exceeds the limit (the message matches the SQLite TCL suite;
	// newer SQLite CLI versions phrase it "too many levels of trigger
	// recursion"). The limit can be lowered via SQLITE_LIMIT_TRIGGER_DEPTH.
	if e.triggerDepthExceeded() {
		return &Result{Error: fmt.Errorf("triggers nested too deep")}
	}

	// Extract the declared timing and event from the trigger header. This is
	// whitespace-robust (the declaration can have arbitrary spaces between
	// the timing, event and ON keywords) unlike a naive " BEFORE INSERT ON "
	// substring match. Triggers without an explicit timing default to BEFORE.
	declTiming, declEvent := parseTriggerHeader(t.SQL)
	if declTiming == "" {
		declTiming = "BEFORE"
	}
	if declTiming != timing {
		return nil
	}
	if declEvent != event {
		return nil
	}

	// A trigger loaded from sqlite_master may reference objects that no
	// longer exist or live in a different schema after a reopen (SQLite
	// reports "malformed database schema (NAME)" at schema load). Validate
	// the trigger's body references against the current schema.
	if e.ctx.TriggerDepth() == 0 {
		if err := e.validateLoadedTriggerSchemaCtx(t, e.currentDMLCtx); err != nil {
			return &Result{Error: err}
		}
	}

	// UPDATE OF <cols> selectivity: an UPDATE trigger declared with an OF
	// column list fires only when the triggering UPDATE statement assigns at
	// least one of those columns. SQLite keys this on the SET clause, not on
	// whether the value actually changed (setting a column to its current
	// value still fires).
	if declEvent == "UPDATE" && !e.triggerMatchesUpdateOf(t) {
		return nil
	}

	// Increment trigger depth to track recursion nesting
	e.ctx.SetTriggerDepth(e.ctx.TriggerDepth() + 1)
	defer func() { e.ctx.SetTriggerDepth(e.ctx.TriggerDepth() - 1) }()

	// Push this trigger's key onto the recursion chain so a nested statement
	// on the same table does not re-fire the SAME trigger (recursive_triggers
	// OFF); OTHER triggers on the table still fire. The key is the trigger's
	// owning schema + name (a temp trigger keyed by its stored schema).
	owning := e.triggerOwningCtx(t)
	chainKey := owning.Name + "." + t.Name
	e.ctx.SetTriggerTables(append(e.ctx.TriggerTables(), chainKey))
	defer func() { tt := e.ctx.TriggerTables(); e.ctx.SetTriggerTables(tt[:len(tt)-1]) }()

	// Record the trigger's owning database so body DML resolves unqualified
	// references correctly (temp triggers may reference any database).
	prevTriggerCtx := e.currentTriggerCtx
	e.currentTriggerCtx = e.triggerOwningCtx(t)
	defer func() { e.currentTriggerCtx = prevTriggerCtx }()

	// Set NEW and OLD row values for trigger body execution
	prevNewRow := e.ctx.TriggerNewRow()
	prevOldRow := e.ctx.TriggerOldRow()

	e.ctx.SetTriggerNewRow(newRow)
	e.ctx.SetTriggerOldRow(oldRow)
	defer func() {
		e.ctx.SetTriggerNewRow(prevNewRow)
		e.ctx.SetTriggerOldRow(prevOldRow)
	}()

	// Evaluate the WHEN clause if present. The clause sits between the
	// "ON <table>" header and the BEGIN keyword.
	whenOK, err := e.triggerWhenPasses(t)
	if err != nil {
		return &Result{Error: err}
	}
	if !whenOK {
		return nil
	}

	// Extract statements between BEGIN and END
	stmts, ok := parseTriggerBody(t)
	if !ok {
		return nil
	}

	// SQLite saves sqlite3_changes() at trigger entry and restores it when the
	// trigger program exits (e_changes R-32918-61474). Statements inside the
	// trigger update the counter normally; on exit the caller's value is
	// restored so the outer statement's changes() is unaffected.
	savedChanges := e.ctx.LastChanges()
	defer e.ctx.SetLastChanges(savedChanges)

	return e.execTriggerBody(stmts, timing)
}

// triggerDepthExceeded reports whether the trigger nesting limit is reached.

// triggerDepthExceeded reports whether the trigger nesting limit is reached.

// parseTriggerWhen extracts and parses the WHEN expression of a trigger's
// CREATE TRIGGER SQL text. Returns nil when the trigger has no WHEN clause.
// parseTriggerWhen extracts and parses the WHEN expression of a trigger's
// CREATE TRIGGER SQL text. Returns nil when the trigger has no WHEN clause.
func (e *DMLExecutor) evalTuple(tableName string, tuple []sql.Expr, columns []string, colDefs []sql.ColumnDef) ([]interface{}, error) {
	values := make([]interface{}, len(tuple))
	for i, expr := range tuple {
		v, err := e.ctx.EvalExpr(expr, nil)
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	if len(columns) > 0 {
		// The VALUES list must supply exactly one value per named column.
		if len(values) != len(columns) {
			return nil, fmt.Errorf("table %s has %d values for %d columns",
				tableName, len(values), len(columns))
		}
		if colDefs == nil {
			// No column definitions (e.g. view INSERT): return the values
			// as-is; the caller maps them to the view's output columns by
			// the column names.
			return values, nil
		}
		return e.mapNamedTupleValues(values, columns, colDefs)
	}
	if colDefs == nil {
		// No column definitions available (e.g. view INSERT): return the
		// values as-is; the caller maps them to the view's output columns.
		return values, nil
	}
	return e.mapPositionalTupleValues(tableName, values, colDefs)
}

// mapNamedTupleValues starts with each column's DEFAULT and overrides with the
// provided values by column name. Duplicate column names use the FIRST
// occurrence (SQLite ignores later duplicates).

// mapNamedTupleValues starts with each column's DEFAULT and overrides with the
// provided values by column name. Duplicate column names use the FIRST
// occurrence (SQLite ignores later duplicates).

// evalReturningStrict evaluates RETURNING expressions with strict column
// resolution: unknown columns and invalid qualifiers produce "no such column"
// errors (SQLite semantics), and table-qualified wildcards are rejected.
// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline
//
// evalReturningStrict evaluates RETURNING expressions with strict column
// resolution: unknown columns and invalid qualifiers produce "no such column"
// errors (SQLite semantics), and table-qualified wildcards are rejected.
// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline
func (e *DMLExecutor) evalReturningExprs(ret sql.SelectColumn, row Row, colDefs []sql.ColumnDef) ([]interface{}, error) {
	switch expr := ret.Expr.(type) {
	case *sql.ColumnRef:
		if expr.Name == "*" && expr.Table == "" {
			// RETURNING * — expand to all column values
			return returningAllColumnValues(row, colDefs), nil
		}
		// Single column reference
		return e.evalReturningSingle(expr, row)

	case *sql.RowValue:
		// Multi-expression RETURNING — evaluate each sub-expression
		var values []interface{}
		for _, subExpr := range expr.Values {
			if ref, ok := subExpr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
				// Expand * to all column values inline
				values = append(values, returningAllColumnValues(row, colDefs)...)
			} else {
				vals, err := e.evalReturningSingle(subExpr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, vals...)
			}
		}
		return values, nil

	default:
		// Single expression not * and not a row value
		return e.evalReturningSingle(ret.Expr, row)
	}
}

// evalReturningSingle evaluates one RETURNING expression and unwraps its value.

// evalReturningSingle evaluates one RETURNING expression and unwraps its value.

// viewDeclaredColumns returns the explicit column list from a CREATE VIEW
// declaration (CREATE VIEW v(a,b) AS ...). Returns nil when the view has no
// declared column list.
// execInsertView handles INSERT statements whose target is a view. SQLite
// routes such statements through INSTEAD OF triggers; resolving the view's
// columns (which validates collations in its SELECT) happens first.
// viewColumnNames returns the output column names of a view's SELECT: the
// explicit alias when present, otherwise the column reference name or the
// expression text. A bare "*" is expanded through the FROM source (SQLite
// resolves view output columns the same way as the result columns of a plain
// SELECT). For a compound SELECT the head member determines the output names.
// validateCollationsInExpr verifies COLLATE operators in an expression tree.
func (e *DMLExecutor) validateCollationsInExpr(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "COLLATE") {
			return e.checkCollationName(v.Right)
		}
		return e.validateCollationsIn(v.Left, v.Right)
	case *sql.CaseExpr:
		if err := e.validateCollationsIn(v.Operand, v.Else); err != nil {
			return err
		}
		for _, w := range v.Whens {
			if err := e.validateCollationsIn(w.When, w.Then); err != nil {
				return err
			}
		}
		return nil
	case *sql.Between:
		return e.validateCollationsIn(v.Operand, v.Low, v.High)
	case *sql.InList:
		if err := e.validateCollationsInExpr(v.Operand); err != nil {
			return err
		}
		return e.validateCollationsInExprs(v.List)
	case *sql.Subquery:
		return e.validateCollationsInSelect(v.Select)
	case *sql.ExistsExpr:
		return e.validateCollationsInSelect(v.Select)
	default:
		return e.validateCollationsInWrapper(expr)
	}
}

// validateCollationsInWrapper handles the remaining expression types: unary
// wrappers, pairwise operators, and expression lists.

// validateCollationsInWrapper handles the remaining expression types: unary
// wrappers, pairwise operators, and expression lists.
