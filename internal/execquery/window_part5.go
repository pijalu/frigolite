package execquery

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// validateWindowFuncArity validates the built-in window functions' argument
// counts (and nth_value's constant second argument).
func (e *SelectEngine) validateWindowFuncArity(fn *sql.FuncCall, name string) error {
	switch name {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST", "NTILE":
		return e.validateRankingFuncArity(fn, name)
	case "LEAD", "LAG", "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE":
		return e.validateValueFuncArity(fn, name)
	}
	return nil
}

// validateRankingFuncArity validates the ranking window functions' argument
// counts (ROW_NUMBER family and ntile).
func (e *SelectEngine) validateRankingFuncArity(fn *sql.FuncCall, name string) error {
	switch name {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST":
		if len(fn.Args) != 0 {
			return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
		}
	case "NTILE":
		if len(fn.Args) != 1 {
			return fmt.Errorf("wrong number of arguments to function ntile()")
		}
		if n, ok := e.ntileConstArg(fn); ok && n <= 0 {
			return fmt.Errorf("argument of ntile must be a positive integer")
		}
	}
	return nil
}

// validateValueFuncArity validates LEAD/LAG and the value window functions'
// argument counts.
func (e *SelectEngine) validateValueFuncArity(fn *sql.FuncCall, name string) error {
	switch name {
	case "LEAD", "LAG":
		if len(fn.Args) < 1 || len(fn.Args) > 3 {
			return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
		}
	case "FIRST_VALUE", "LAST_VALUE":
		if len(fn.Args) != 1 {
			return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
		}
	case "NTH_VALUE":
		if len(fn.Args) != 2 {
			return fmt.Errorf("wrong number of arguments to function nth_value()")
		}
		if err := e.validateNthValueArg(fn.Args[1]); err != nil {
			return err
		}
	}
	return nil
}

// checkWindowRefOverride validates an OVER clause's named-window reference:
// the base must exist and must not be illegally overridden. An inline OVER
// (no reference name) passes.
func (e *SelectEngine) checkWindowRefOverride(over *sql.WindowDef, windows []sql.WindowDef) error {
	refName := over.Name
	if refName == "" {
		refName = over.BaseName
	}
	if refName == "" {
		return nil
	}
	return e.checkWindowOverride(over, refName, windows)
}

// windowFuncOverError returns the resolve.c "may not be used as a window
// function" error when name is not a window-capable aggregate (unknown
// functions and classic application aggregates both reject OVER); nil when
// the function may be used with OVER.
func (e *SelectEngine) windowFuncOverError(name string) error {
	reg, found := e.ctx.Functions().Find(name)
	if !found || reg.Type != function.TypeAggregate || reg.ClassicAggregate {
		return fmt.Errorf("%s() may not be used as a window function", strings.ToLower(name))
	}
	return nil
}

// validateNthValueArg validates the second argument of nth_value(): it must
// be a positive integer. A constant argument that is not a positive integer
// (0, negative, a non-numeric string, NULL, or a non-integral real) is
// rejected at prepare time; non-constant arguments are checked per row at
// execution.
func (e *SelectEngine) validateNthValueArg(arg sql.Expr) error {
	constErr := func() error {
		return fmt.Errorf("second argument to nth_value must be a positive integer")
	}
	v, err := e.ctx.EvalExpr(arg, RowMap{})
	if err != nil || e.exprHasColumnRef(arg) {
		// Non-constant expression (needs a row, e.g. nth_value(b, c) or
		// nth_value(b, b+1)): the runtime check handles it per row.
		return nil
	}
	nv := util.UnwrapColumnValue(unwrapCollatedValue(v))
	switch x := nv.(type) {
	case int64:
		if x < 1 {
			return constErr()
		}
	case float64:
		if x < 1 || math.Trunc(x) != x {
			return constErr()
		}
	case string:
		// SQLite converts the string to a number first ('2' and '2.0' are
		// both the integer 2; '4ab' is not a number).
		fv, convErr := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if convErr != nil || fv < 1 || math.Trunc(fv) != fv {
			return constErr()
		}
	default:
		// NULL (or an unhandled constant type) is not a positive integer.
		return constErr()
	}
	return nil
}

// checkWindowOverride validates that an OVER clause referencing base window
// refName does not illegally override the base's clauses.
func (e *SelectEngine) checkWindowOverride(over *sql.WindowDef, refName string, windows []sql.WindowDef) error {
	base, ok := e.findNamedWindow(refName, windows)
	if !ok {
		return fmt.Errorf("no such window: %s", refName)
	}
	if over.Frame != nil && base.Frame != nil {
		return fmt.Errorf("cannot override frame specification of window: %s", refName)
	}
	// SQLite rejects adding a PARTITION BY clause to ANY named window
	// reference, even when the base window has no partitions (window.c
	// sqlite3WindowRewrite).
	if len(over.Partitions) > 0 {
		return fmt.Errorf("cannot override PARTITION clause of window: %s", refName)
	}
	if len(over.OrderBy) > 0 && len(base.OrderBy) > 0 {
		return fmt.Errorf("cannot override ORDER BY clause of window: %s", refName)
	}
	// Adding an ORDER BY to a base window that already has a frame is
	// rejected: the frame's ORDER BY dependency cannot be re-established
	// (window.c sqlite3WindowAssemble).
	if len(over.OrderBy) > 0 && base.Frame != nil {
		return fmt.Errorf("cannot override frame specification of window: %s", refName)
	}
	return nil
}

// findNamedWindow returns the WINDOW-clause definition for name, if present.
func (e *SelectEngine) findNamedWindow(name string, windows []sql.WindowDef) (sql.WindowDef, bool) {
	for _, w := range windows {
		if w.Name == name {
			return w, true
		}
	}
	return sql.WindowDef{}, false
}

// isAggregateWindowFunc reports whether fn is an aggregate function used as a
// window (e.g. sum() OVER ...).
