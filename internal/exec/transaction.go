// Package exec implements query execution.
package exec

// --- COMMIT ---

func (e *Engine) execCommit() *Result {
	e.inTransaction = false
	e.ddlBuffer = nil
	if err := e.pager.Flush(); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- BEGIN TRANSACTION ---

func (e *Engine) execBegin() *Result {
	e.inTransaction = true
	e.ddlBuffer = nil
	return &Result{}
}

// --- ROLLBACK ---

func (e *Engine) execRollback() *Result {
	e.inTransaction = false
	// Undo all DDL operations that were performed during the transaction
	for i := len(e.ddlBuffer) - 1; i >= 0; i-- {
		e.ddlBuffer[i]()
	}
	e.ddlBuffer = nil
	return &Result{}
}
