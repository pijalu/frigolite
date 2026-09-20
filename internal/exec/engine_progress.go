// Package exec: engine registration surface — the progress callback,
// statement-interrupt counter, DQS switches, user function and collation
// registration (with the collation_needed hook), and the authorizer
// callback invocation. Split from engine.go; behavior unchanged.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// SetProgressHandler registers a progress callback invoked after every n
// engine operations (n <= 0 disables it). A true return interrupts the
// running statement with an "interrupted" error, matching SQLite's
// sqlite3_progress_handler.
func (e *Engine) SetProgressHandler(n int, fn func() bool) {
	e.progress.period = n
	e.progress.callback = fn
	e.progress.counter = 0
}

// SetInterruptCount arms the SQLITE_TEST interrupt countdown
// (::sqlite_interrupt_count, src/vdbe.c:68): n > 0 interrupts the connection
// after n engine operations; n <= 0 disables it. The engine mirrors the
// counter decrement-per-op of sqlite3VdbeExec's SQLITE_TEST block.
func (e *Engine) SetInterruptCount(n int) {
	e.interruptCount = n
}

// InterruptCount returns the leftover countdown (the TCL harness reads
// ::sqlite_interrupt_count after a statement to learn how many ops ran).
func (e *Engine) InterruptCount() int {
	return e.interruptCount
}

// Interrupt sets the connection's interrupt flag (sqlite3_interrupt). The
// flag is consumed (cleared) by the next statement executed on this
// connection, which fails with an "interrupted" error.
func (e *Engine) Interrupt() {
	e.interrupted = true
}

// IsInterrupted reports whether the interrupt flag is currently set
// (sqlite3_is_interrupted).
func (e *Engine) IsInterrupted() bool {
	return e.interrupted
}

// ClearInterrupt clears the connection's interrupt flag without running a
// statement (used by the TCL harness after a db-eval callback aborts).
func (e *Engine) ClearInterrupt() {
	e.interrupted = false
}

// checkProgress counts engine operations and, every progressPeriod calls,
// runs the registered callback. It also decrements the SQLITE_TEST
// interrupt countdown (src/vdbe.c sqlite3VdbeExec): when the countdown
// reaches zero the connection is interrupted and the running statement
// aborts with SQLITE_INTERRUPT. A nil callback and zero countdown are a
// no-op fast path. Returns a non-nil "interrupted" error when the callback
// requests an abort or the countdown fires.
func (e *Engine) checkProgress() error {
	if e.progress.callback == nil && e.interruptCount <= 0 {
		return nil
	}
	if e.interruptCount > 0 {
		e.interruptCount--
		if e.interruptCount == 0 {
			// sqlite3_interrupt(db): the running statement fails immediately
			// with SQLITE_INTERRUPT and the flag stays set until consumed
			// (vdbeapi.c clears it when the last statement finishes).
			e.interrupted = true
			return fmt.Errorf("interrupted")
		}
	}
	if e.progress.callback == nil || e.progress.period <= 0 {
		return nil
	}
	e.progress.counter++
	if e.progress.counter >= e.progress.period {
		e.progress.counter = 0
		if e.progress.callback() {
			return fmt.Errorf("interrupted")
		}
	}
	return nil
}

// SetDQS configures SQLite's double-quoted-string (DQS) behavior.
// ddl=true allows double-quoted strings in DDL statements (CREATE TABLE
// CHECK/DEFAULT expressions, CREATE INDEX keys); dml=true allows them in DML
// (SELECT/INSERT/UPDATE expressions). Both default to true, matching SQLite.
// When disabled, an unresolved double-quoted identifier is an error
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (e *Engine) SetDQS(ddl, dml bool) {
	e.settings.dqsDDL = ddl
	e.settings.dqsDML = dml
}

// SetMainFilePath records the filesystem path of the main database, reported
// by PRAGMA database_list (SQLite reports the path passed to sqlite3_open).
func (e *Engine) SetMainFilePath(path string) {
	if e.mainDB != nil {
		e.mainDB.FilePath = path
	}
}

// SetTrackExternalModForMain enables external-modification detection for the
// main database (a second connection to the same file observes writes made by
// this one). Called by frigolite.Open for file-based databases.
func (e *Engine) SetTrackExternalModForMain(enabled bool) {
	if e.mainDB != nil && e.mainDB.Schema != nil {
		e.mainDB.Schema.SetTrackExternalMod(enabled)
		e.mainDB.Schema.CaptureFileStamp()
	}
}

// SetDefensive mirrors SQLITE_DBCONFIG_DEFENSIVE: when enabled, certain
// write operations (e.g. PRAGMA schema_version=...) are ignored.
func (e *Engine) SetDefensive(enabled bool) {
	e.settings.defensive = enabled
}

// SetQPSG mirrors SQLITE_DBCONFIG_ENABLE_QPSG (the query planner stability
// guarantee): when enabled, planning must not depend on runtime values, so
// bound-parameter LIKE patterns are not examined for the prefix-range
// optimization (whereexpr.c isLikeOrGlob's TK_VARIABLE branch).
func (e *Engine) SetQPSG(enabled bool) {
	e.settings.qpsg = enabled
}

// QPSG reports the query planner stability guarantee flag.
func (e *Engine) QPSG() bool { return e.settings.qpsg }

// RegisterFunction registers a scalar SQL function for this engine instance.
// It is used by the test harness to reproduce SQLite's TCL-defined functions
// (e.g. `db func f f` where f returns a constant).
func (e *Engine) RegisterFunction(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	e.funcs.Register(name, fn, minArgs, maxArgs)
}

// RegisterFunctionFlags registers a scalar SQL function with SQLite
// function-safety flags (innocuous / directonly) controlling its use in
// schema objects under PRAGMA trusted_schema.
func (e *Engine) RegisterFunctionFlags(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int, innocuous, directOnly bool) {
	e.funcs.RegisterFlags(name, fn, minArgs, maxArgs, innocuous, directOnly)
}

// SchemaFunctionSafe reports whether a function may be used in a schema
// object under the current trusted_schema setting.
func (e *Engine) SchemaFunctionSafe(name string) bool {
	return e.funcs.SchemaSafe(name, e.settings.trustedSchema)
}

// FunctionExists reports whether a scalar/aggregate function of any arity is
// registered (built-ins plus connection-defined functions).
func (e *Engine) FunctionExists(name string) bool {
	_, found := e.funcs.Find(name)
	return found
}

// RegisterCollation registers a custom collation sequence for this engine
// (sqlite3_create_collation). The function compares two strings and returns
// -1/0/1. Collation names are case-insensitive; registering a name that is a
// built-in (BINARY/NOCASE/RTRIM) replaces the built-in for this connection,
// matching SQLite (the user collation shadows the built-in of the same name).
func (e *Engine) RegisterCollation(name string, fn func(a, b string) int) {
	if e == nil || fn == nil {
		return
	}
	if e.collations == nil {
		e.collations = make(map[string]func(a, b string) int)
	}
	e.collations[strings.ToUpper(name)] = fn
}

// UnregisterCollation removes a registered custom collation sequence
// (sqlite_delete_collation). It reports whether a collation was removed.
func (e *Engine) UnregisterCollation(name string) bool {
	if e == nil || e.collations == nil {
		return false
	}
	_, ok := e.collations[strings.ToUpper(name)]
	delete(e.collations, strings.ToUpper(name))
	return ok
}

// RegisterCollationNeeded sets the collation-needed callback
// (sqlite3_collation_needed). It is invoked when a statement resolves a
// collation sequence that is not registered for this connection; the callback
// typically registers the missing collation via RegisterCollation, after
// which the original lookup retries (callback.c sqlite3GetCollSeq). A nil fn
// clears the hook.
func (e *Engine) RegisterCollationNeeded(fn func(name string)) {
	if e == nil {
		return
	}
	e.collationsNeeded = fn
}

// lookupCollation returns a registered custom collation function for name
// (case-insensitive), or nil if name is not registered.
func (e *Engine) lookupCollation(name string) func(a, b string) int {
	if e == nil || e.collations == nil {
		return nil
	}
	return e.collations[strings.ToUpper(name)]
}

// RegisteredCollations returns a copy of this connection's custom collation
// registry (the VACUUM rebuild transfers it to the destination engine so the
// logical copy resolves every source collation — vacuum2-6).
func (e *Engine) RegisteredCollations() map[string]func(a, b string) int {
	if e == nil || e.collations == nil {
		return nil
	}
	out := make(map[string]func(a, b string) int, len(e.collations))
	for k, v := range e.collations {
		out[k] = v
	}
	return out
}

// compareValuesCollate compares two SQL values with a collation name,
// consulting this engine's registered custom collations in addition to the
// built-in BINARY/NOCASE/RTRIM. An empty or unknown collation falls back to
// BINARY (SQLite's default), matching util.CompareValuesCollate.
func (e *Engine) compareValuesCollate(a, b interface{}, collation string) int {
	return util.CompareValuesCollateFn(a, b, collation, func(name string) (util.CollationFunc, bool) {
		if f := e.lookupCollation(name); f != nil {
			return util.CollationFunc(f), true
		}
		return nil, false
	})
}

// CompareValuesCollate compares two SQL values with a collation name.
// Exported for the expression evaluator's collation comparison.
func (e *Engine) CompareValuesCollate(a, b interface{}, collation string) int {
	return e.compareValuesCollate(a, b, collation)
}

// LookupCollation returns a registered custom collation function for name
// (case-insensitive), or nil if name is not registered. When a
// collation-needed callback is set (RegisterCollationNeeded), a miss invokes
// it and retries once — callback.c sqlite3GetCollSeq: find → callCollNeeded →
// find again → "no such collation sequence". Exported for the expression
// evaluator's COLLATE operator and the compile-time collation validators.
func (e *Engine) LookupCollation(name string) func(a, b string) int {
	if f := e.lookupCollation(name); f != nil {
		return f
	}
	if e.collationsNeeded != nil {
		e.collationsNeeded(name)
		return e.lookupCollation(name)
	}
	return nil
}
