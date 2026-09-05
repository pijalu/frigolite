// Package exec implements query execution.
package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// execPragma dispatches a PRAGMA statement to the execpragma registry, which
// owns the pragma handler map. The registry returns a minimal result which is
// converted to the engine result type.
func (e *Engine) execPragma(s *sql.PragmaStmt) *Result {
	// SQLite opens the lazily-created temp database (aDb[1].pBt) the first
	// time anything addresses it — including PRAGMA temp.<...> — so
	// PRAGMA lock_status then reports "temp unknown" rather than
	// "temp closed" (pragma2-4.1/4.4: PRAGMA temp.cache_size=2000).
	if up := strings.ToUpper(s.Schema); up == "TEMP" || up == "TEMPORARY" {
		e.tempBtreeOpen = true
	}
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
