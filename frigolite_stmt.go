package frigolite

// Prepared-statement parameter binding and the Exec/Query fast path: a
// prepared statement is parsed once at Prepare time; every execution
// substitutes the bound values into a copy-on-write clone of the parsed AST
// (internal/exec.BindStmtValues, the template cache's walker in bind mode)
// and runs it — no re-lex, no re-parse, no SQL normalization on the repeat
// path. Statements whose shape the clone walker does not cover (and
// parameterless statements share the AST directly) keep the historical
// rendered-SQL path as a fallback, so results are identical either way.

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/exec"
)

// Exec executes the prepared statement once and returns the complete
// result. With arguments, the i-th argument binds to parameter slot i+1 for
// this call (the count must match BindParameterCount). Without arguments the
// statement's Bind-API bindings apply, and any parameter left unbound
// evaluates to SQL NULL — the same prepare-once / bind / reset-and-step
// cycle sqlite3 applications use. Every call re-executes from the start,
// reusing the parsed statement.
//
// The Result mirrors DB.Exec for the same SQL and values: Columns, Rows,
// Changes, LastInsertRowID and Error; SQL carries the prepared text.
//
// A Stmt is NOT safe for concurrent use by multiple goroutines (bind state
// and step state are unsynchronized, like sqlite3_stmt without the
// serialized threading mode); the DB's own methods remain safe. Prepare
// rejects multi-statement SQL.
func (s *Stmt) Exec(args ...interface{}) *Result {
	if s == nil || s.closed {
		return &Result{Error: fmt.Errorf("statement is closed")}
	}
	s.vmState = vmReady
	r := s.execute(args)
	out := execResult(r)
	out.SQL = s.sql
	s.result = out
	s.row = len(out.Rows)
	if out.Error != nil {
		s.lastErr = out.Error
		s.vmState = vmPoisoned
		// sqlite3_step reports the generic SQLITE_ERROR for constraint
		// halts (vdbe.c OP_Halt) — wrap so ErrorCodeFor-classified readers
		// of this result see the step-level code, while s.lastErr keeps
		// the unwrapped error for the finalize path (capi2-3.23).
		halted := stepHaltView(out.Error, s.db.ErrorCodeFor)
		code := s.db.ErrorCodeFor(halted)
		out.Error = halted
		s.db.engine.SetLastErr(out.Error.Error(), code)
	} else {
		s.lastErr = nil
		s.vmState = vmDone
	}
	return out
}

// Query executes the prepared statement and returns its rows. Arguments
// bind positionally exactly like Exec. The Result mirrors DB.Query for the
// same SQL and values (Columns, Rows, SQL; a zero-row result carries a nil
// Rows). A Stmt is NOT safe for concurrent use by multiple goroutines.
func (s *Stmt) Query(args ...interface{}) *Result {
	if s == nil || s.closed {
		return &Result{Error: fmt.Errorf("statement is closed")}
	}
	r := s.execute(args)
	if r.Error != nil {
		out := execResult(r)
		out.SQL = s.sql
		return out
	}
	rows := r.Rows
	if len(rows) == 0 {
		rows = nil // DB.Query single-statement semantics
	}
	return &Result{Columns: r.Columns, Rows: rows, SQL: s.sql}
}

// bindArgs installs one call's positional arguments: the i-th argument
// lands in parameter slot i+1. An empty args list keeps the statement's
// existing Bind-API bindings (sqlite3_reset retains bindings too).
func (s *Stmt) bindArgs(args []interface{}) error {
	if len(args) == 0 {
		return nil
	}
	want := len(s.paramNames)
	if len(args) != want {
		return fmt.Errorf("frigolite: expected %d arguments, got %d", want, len(args))
	}
	for i, a := range args {
		if n, ok := bindValueLen(a); ok && n > int64(s.db.Limit("SQLITE_LIMIT_LENGTH")) {
			return fmt.Errorf("string or blob too big")
		}
		s.args[i+1] = a
		s.bound[i] = a
	}
	return nil
}

// boundValues returns the slot-indexed binding table (slot i+1 → index i;
// missing entries read as Go nil, SQL NULL). The table is maintained
// incrementally by Bind/BindNamed/ClearBindings and bindArgs, so projection
// is allocation-free; BindStmtValues copies every substituted value into a
// fresh literal node and retains nothing.
func (s *Stmt) boundValues() []interface{} {
	return s.bound
}

// execute runs the statement once: through the prepared-AST clone when the
// bind plan covers it (no lexing/parsing/normalizing), otherwise through the
// rendered-SQL path. Both shapes apply the same connection-level statement
// pipeline (trace hooks, zero-blob expansion, last-error state).
func (s *Stmt) execute(args []interface{}) *exec.Result {
	if err := s.bindArgs(args); err != nil {
		s.db.engine.SetLastErr(err.Error(), s.db.errorCode(err))
		return &exec.Result{Error: err}
	}
	if s.plan != nil {
		if cloned, ok := s.db.engine.BindStmtValuesScratch(s.ast, s.plan, s.boundValues()); ok {
			return s.db.runSingleStmt(cloned[0], s.sql)
		}
	}
	return s.db.runSQLText(s.renderBoundSQL())
}
