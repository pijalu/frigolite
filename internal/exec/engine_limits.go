package exec

// Engine limit plumbing (SQLITE_LIMIT_*): sqlite3_limit get/set semantics,
// clamps, and the compile-time defaults exercised by sqllimits1.test.
// Split from engine.go for file-size hygiene.

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execddl"
)

// SetTriggerDepthLimit sets the maximum trigger nesting depth
// (SQLITE_LIMIT_TRIGGER_DEPTH). A negative value queries the current limit.
// The limit is stored on the trigger manager and used by fireTrigger to abort
// recursive trigger chains.
func (e *Engine) SetTriggerDepthLimit(n int) int {
	return e.triggers.SetDepthLimit(n)
}

// SetLimit sets a named SQLite runtime limit (SQLITE_LIMIT_COLUMN,
// SQLITE_LIMIT_LENGTH). A negative value queries the current limit without
// changing it. SQLITE_LIMIT_EXPR_DEPTH / TRIGGER_DEPTH use their dedicated
// setters. A raise above the compile-time default is capped at the default
// (SQLite: "it is not possible to raise the column limit above its default
func (e *Engine) SetLimit(name string, n int) int {
	if n < 0 {
		return e.Limit(name)
	}
	// sqlite3_limit returns the PRIOR limit (R-53341-35419); the call
	// installs the clamped new value. LENGTH has a floor of
	// SQLITE_MIN_LENGTH 30 (main.c sqlite3_limit).
	prior := e.Limit(name)
	switch strings.ToUpper(name) {
	case "SQLITE_LIMIT_EXPR_DEPTH":
		// Raises above the compile-time default are capped (main.c
		// sqlite3_limit: newLimit > aHardLimit → hard max). sqllimits1-4.4
		// sets 0x7fffffff and reads back SQLITE_MAX_EXPR_DEPTH=1000.
		e.settings.exprDepthLimit = clampHardMax(n, 1000)
	case "SQLITE_LIMIT_TRIGGER_DEPTH":
		e.triggers.SetDepthLimit(clampHardMax(n, 1000))
	case "SQLITE_LIMIT_ATTACHED":
		// SQLite: db->aLimit[] is writable both ways; raises cap at the
		// hard max (main.c: newLimit > aHardLimit → hard max).
		// sqllimits1-2.8 lowers to 5; 4.8.1 sets 0x7fffffff → reads
		// back SQLITE_MAX_ATTACHED=10. A LOWERED limit stays lowered
		// until raised again (test order: 2.8 halves db's limit to 5,
		// then 4.8 raises it back to 10).
		e.settings.attachedLimit = clampHardMax(n, execddl.MaxAttachedDatabases)
	case "SQLITE_LIMIT_LENGTH":
		e.settings.lengthLimit = clampMin(clampHardMax(n, sqliteMaxLengthDefault), 30)
	case "SQLITE_LIMIT_SCHEMA":
		e.settings.schemaLimit = n
	default:
		if !e.setClampedLimit(name, n) {
			return e.Limit(name)
		}
	}
	return prior
}

// setClampedLimit installs a plain cap-at-hard-max limit: the LIMIT_* names
// whose only rule is newLimit > aHardLimit → hard max (src/limit.h defaults
// exercised by sqllimits1.test). Reports whether name is a known limit.
func (e *Engine) setClampedLimit(name string, n int) bool {
	switch strings.ToUpper(name) {
	case "SQLITE_LIMIT_COLUMN":
		e.settings.columnLimit = clampHardMax(n, sqliteMaxColumnDefault)
	case "SQLITE_LIMIT_SQL_LENGTH":
		e.settings.sqlLengthLimit = clampHardMax(n, sqliteMaxLengthDefault)
	case "SQLITE_LIMIT_COMPOUND_SELECT":
		e.settings.compoundSelectLimit = clampHardMax(n, sqliteMaxCompoundSelectDefault)
	case "SQLITE_LIMIT_VDBE_OP":
		e.settings.vdbeOpLimit = clampHardMax(n, sqliteMaxVDBEOpDefault)
	case "SQLITE_LIMIT_FUNCTION_ARG":
		e.settings.functionArgLimit = clampHardMax(n, sqliteMaxFunctionArgDefault)
	case "SQLITE_LIMIT_LIKE_PATTERN_LENGTH":
		e.settings.likePatternLimit = clampHardMax(n, sqliteMaxLikePatternDefault)
	case "SQLITE_LIMIT_VARIABLE_NUMBER":
		e.settings.variableNumberLimit = clampHardMax(n, sqliteMaxVariableNumberDefault)
	case "SQLITE_LIMIT_WORKER_THREADS":
		e.settings.workerThreadsLimit = clampHardMax(n, sqliteMaxWorkerThreadsDefault)
	default:
		return false
	}
	return true
}

// clampHardMax caps a new limit at its compile-time hard max (main.c
// sqlite3_limit: newLimit > aHardLimit → hard max).
func clampHardMax(n, hardMax int) int {
	if n > hardMax {
		return hardMax
	}
	return n
}

// clampMin floors a limit (LENGTH's SQLITE_MIN_LENGTH 30 floor).
func clampMin(n, min int) int {
	if n < min {
		return min
	}
	return n
}

// sqliteMaxColumnDefault is the SQLite compile-time default SQLITE_MAX_COLUMN.
const sqliteMaxColumnDefault = 2000

// sqliteMaxLengthDefault is the SQLite compile-time default SQLITE_MAX_LENGTH.
const sqliteMaxLengthDefault = 1000000000

// Additional compile-time limit defaults (src/limit.h) exercised by
// sqllimits1.test.
const (
	sqliteMaxCompoundSelectDefault = 500
	sqliteMaxVDBEOpDefault         = 250000000
	sqliteMaxFunctionArgDefault    = 127
	sqliteMaxLikePatternDefault    = 50000
	sqliteMaxVariableNumberDefault = 32766
	sqliteMaxWorkerThreadsDefault  = 8
)

// Limit returns the current value of a named SQLite compile-time/run-time
// limit (e.g. "SQLITE_LIMIT_ATTACHED", "SQLITE_LIMIT_EXPR_DEPTH").
// Unknown limits return 0. Used by the test harness to query the engine's
// configured limits (attach4-1.1 checks SQLITE_LIMIT_ATTACHED).
func (e *Engine) Limit(name string) int {
	switch strings.ToUpper(name) {
	case "SQLITE_LIMIT_ATTACHED":
		if e.settings.attachedLimit > 0 {
			return e.settings.attachedLimit
		}
		return execddl.MaxAttachedDatabases
	case "SQLITE_LIMIT_EXPR_DEPTH":
		return e.settings.exprDepthLimit
	case "SQLITE_LIMIT_TRIGGER_DEPTH":
		return e.triggers.DepthLimit()
	case "SQLITE_LIMIT_SCHEMA":
		return e.settings.schemaLimit
	default:
		if v, ok := e.plainLimitSetting(name); ok {
			return v
		}
		// Out-of-range limit ids (sqllimits1-1.20..1.23
		// SQLITE_LIMIT_TOOSMALL/TOOBIG): sqlite3_limit returns -1
		// without touching state (src/main.c sqlite3_limit default).
		return -1
	}
}

// plainLimitSetting reads the settings-backed limits whose value is stored
// verbatim (no compile-time fallback). ok is false for names outside the set.
func (e *Engine) plainLimitSetting(name string) (int, bool) {
	switch strings.ToUpper(name) {
	case "SQLITE_LIMIT_COLUMN":
		return e.settings.columnLimit, true
	case "SQLITE_LIMIT_LENGTH":
		return e.settings.lengthLimit, true
	case "SQLITE_LIMIT_SQL_LENGTH":
		return e.settings.sqlLengthLimit, true
	case "SQLITE_LIMIT_COMPOUND_SELECT":
		return e.settings.compoundSelectLimit, true
	case "SQLITE_LIMIT_VDBE_OP":
		return e.settings.vdbeOpLimit, true
	case "SQLITE_LIMIT_FUNCTION_ARG":
		return e.settings.functionArgLimit, true
	case "SQLITE_LIMIT_LIKE_PATTERN_LENGTH":
		return e.settings.likePatternLimit, true
	case "SQLITE_LIMIT_VARIABLE_NUMBER":
		return e.settings.variableNumberLimit, true
	case "SQLITE_LIMIT_WORKER_THREADS":
		return e.settings.workerThreadsLimit, true
	}
	return 0, false
}
