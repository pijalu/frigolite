package execexpr

import (
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// extractValue extracts the raw value and collation from a potentially
// collated value.
func extractValue(v interface{}) (interface{}, string) {
	if cv, ok := v.(*CollatedValue); ok {
		return cv.Value, cv.Collation
	}
	return v, ""
}

// ExtractValue extracts the raw value and collation from a potentially
// collated value. Exported for the execution engine's collation comparison.
func ExtractValue(v interface{}) (interface{}, string) {
	return extractValue(v)
}

// unwrapCollatedValue extracts the raw value from a CollatedValue wrapper.
// Used when a value flows to a result column, where the collation marker
// (a *CollatedValue pointer) must not leak into the output. Since a COLLATE
// expression wraps its operand (which may itself be a *util.ColumnValue),
// this also unwraps the inner ColumnValue to produce the raw scalar. A
// top-level ColumnValue (e.g. the affinity wrapper on a CAST result) is
// unwrapped too. Nested wrappers (e.g. a CollatedValue whose inner value is
// itself a CollatedValue from a double application) are peeled recursively.
func unwrapCollatedValue(v interface{}) interface{} {
	for {
		if cv, ok := v.(*CollatedValue); ok {
			v = cv.Value
			continue
		}
		if cv, ok := v.(*util.ColumnValue); ok {
			v = cv.Value
			continue
		}
		return v
	}
}

// UnwrapCollatedValue extracts the raw value from a CollatedValue wrapper.
// Exported for the execution engine's output column rendering.
func UnwrapCollatedValue(v interface{}) interface{} {
	return unwrapCollatedValue(v)
}

// isColumnValue reports whether v is a column value (a *util.ColumnValue
// wrapper, possibly wrapped in a CollatedValue marker). Used by
// compareValuesWithCollate to apply SQLite's left-operand collation rule.
func isColumnValue(v interface{}) bool {
	if cv, ok := v.(*CollatedValue); ok {
		_, isCol := cv.Value.(*util.ColumnValue)
		return isCol
	}
	_, isCol := v.(*util.ColumnValue)
	return isCol
}

// IsColumnValue reports whether v is a column value. Exported for the
// execution engine's collation comparison.
func IsColumnValue(v interface{}) bool {
	return isColumnValue(v)
}

// isRowIDName reports whether name is one of the implicit rowid aliases
// (rowid, _rowid_, oid), which SQLite treats as the row's integer key.
func isRowIDName(name string) bool {
	return strings.EqualFold(name, "rowid") || strings.EqualFold(name, "_rowid_") || strings.EqualFold(name, "oid")
}

// IsRowIDName reports whether name is one of the implicit rowid aliases.
// Exported for the execution engine's row and column handling.
func IsRowIDName(name string) bool {
	return isRowIDName(name)
}

// isSQLNull reports whether v is a SQL NULL value.
func isSQLNull(v interface{}) bool {
	if v == nil {
		return true
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		return cv.Value == nil
	}
	if cv, ok := v.(*CollatedValue); ok {
		return isSQLNull(cv.Value)
	}
	return false
}

// IsSQLNull reports whether v is a SQL NULL value. Exported for the
// execution engine's output-column rendering.
func IsSQLNull(v interface{}) bool {
	return isSQLNull(v)
}
