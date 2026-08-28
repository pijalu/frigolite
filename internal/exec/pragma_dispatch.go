// Package exec implements query execution.
package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// execPragma dispatches a PRAGMA statement to the execpragma registry, which
// owns the pragma handler map. The registry returns a minimal result which is
// converted to the engine result type.
func (e *Engine) execPragma(s *sql.PragmaStmt) *Result {
	res := e.pragmas.Handle(e, s)
	if res == nil {
		// A nil registry result means the pragma handler produced no output
		// (e.g. a successful encoding assignment that writes the header and
		// returns no rows). Return an empty result rather than nil so callers
		// never dereference a nil *Result.
		return &Result{}
	}
	return &Result{Columns: res.Columns, Rows: res.Rows, Error: res.Error}
}
