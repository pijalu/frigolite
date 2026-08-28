package exec

import (
	"github.com/pijalu/frigolite/internal/execconstraint"
)

// This file implements the execconstraint.ConstraintContext capability
// interface on the Engine. The constraint enforcer (internal/execconstraint)
// depends on this interface rather than on the concrete Engine type
// (Dependency Inversion).

// Compile-time probe: Engine implements execconstraint.ConstraintContext.
var _ execconstraint.ConstraintContext = (*Engine)(nil)

// FireBeforeDeleteTriggers fires the BEFORE DELETE triggers of a table
// (delegated to the DML executor's trigger machinery).
func (e *Engine) FireBeforeDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireBeforeDeleteTriggers(tableName, oldRow)
}

// FireAfterDeleteTriggers fires the AFTER DELETE triggers of a table
// (delegated to the DML executor's trigger machinery).
func (e *Engine) FireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireAfterDeleteTriggers(tableName, oldRow)
}

// HasTriggersForTable reports whether any trigger fires on the given table.
func (e *Engine) HasTriggersForTable(tableName string) bool {
	return e.hasTriggersForTable(tableName)
}
