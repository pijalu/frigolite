package execdml

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// enforceStrictType enforces a STRICT table column type on a stored value.
// Moved from internal/exec so both the DML write path (INSERT/UPDATE) and
// the engine's STRICT validation share the implementation.
func enforceStrictType(tableName, colName, declaredType string, v interface{}) error {
	if v == nil {
		return nil // NULL is always allowed
	}
	upper := strings.ToUpper(strings.TrimSpace(declaredType))
	v = util.UnwrapColumnValue(v)
	// In STRICT tables, affinity is applied first (e.g., INTEGER → TEXT column
	// converts the value to text '4', which then passes the type check).
	switch upper {
	case "TEXT":
		return enforceStrictText(tableName, colName, v)
	case "INT", "INTEGER":
		return enforceStrictInt(tableName, colName, upper, v)
	case "REAL":
		return enforceStrictReal(tableName, colName, v)
	case "BLOB":
		return enforceStrictBlob(tableName, colName, v)
	}
	return nil // ANY (and unknown types) accept everything
}

// enforceStrictText enforces a STRICT TEXT column: strings, int64, and
// float64 pass (affinity converts them to text).
func enforceStrictText(tableName, colName string, v interface{}) error {
	switch v.(type) {
	case string, int64, float64:
		return nil
	default:
		return strictTypeErr(tableName, colName, "TEXT", v)
	}
}

// enforceStrictInt enforces a STRICT INT/INTEGER column.
func enforceStrictInt(tableName, colName, typeName string, v interface{}) error {
	switch v := v.(type) {
	case int64:
		return nil
	case float64:
		// SQLite accepts whole-number reals and converts to integer
		if v == float64(int64(v)) {
			return nil
		}
		return strictTypeErr(tableName, colName, typeName, v)
	case string:
		// Numeric-looking strings are accepted and converted to integer
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			return nil
		}
		// Try float — a whole-number float string is accepted
		if f, err := strconv.ParseFloat(v, 64); err == nil && f == float64(int64(f)) {
			return nil
		}
		return strictTypeErr(tableName, colName, typeName, v)
	default:
		return strictTypeErr(tableName, colName, typeName, v)
	}
}

// enforceStrictReal enforces a STRICT REAL column.
func enforceStrictReal(tableName, colName string, v interface{}) error {
	switch v := v.(type) {
	case float64:
		return nil
	case int64:
		return nil // integers are accepted and converted to real
	case string:
		// Numeric-looking strings are accepted and converted to real
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return nil
		}
		return strictTypeErr(tableName, colName, "REAL", v)
	default:
		return strictTypeErr(tableName, colName, "REAL", v)
	}
}

// enforceStrictBlob enforces a STRICT BLOB column.
func enforceStrictBlob(tableName, colName string, v interface{}) error {
	switch v.(type) {
	case []byte:
		return nil
	default:
		return strictTypeErr(tableName, colName, "BLOB", v)
	}
}

// strictTypeErr formats a STRICT type-mismatch error.
func strictTypeErr(tableName, colName, typeName string, v interface{}) error {
	return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), typeName, tableName, colName)
}

// strictStorageClass returns the storage class name for error messages.
func strictStorageClass(v interface{}) string {
	v = util.UnwrapColumnValue(v)
	switch v.(type) {
	case int64:
		return "INT"
	case float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	default:
		return "UNKNOWN"
	}
}

// stripCTASSelect returns the CREATE TABLE text up to (but not including) an
// "AS SELECT" clause. Table options such as STRICT and WITHOUT ROWID only
// appear before AS SELECT, and the closing parenthesis of the column list
// must not be confused with parentheses inside the SELECT body.
func stripCTASSelect(createSQL string) string {
	upper := strings.ToUpper(createSQL)
	idx := strings.Index(upper, " AS SELECT")
	if idx < 0 {
		// Allow "AS" and "SELECT" separated by arbitrary whitespace.
		re := regexp.MustCompile(`(?i)\s+AS\s+SELECT`)
		loc := re.FindStringIndex(createSQL)
		if loc == nil {
			return createSQL
		}
		return createSQL[:loc[0]]
	}
	return createSQL[:idx]
}

// isStrictTable returns true if the table's CREATE SQL specifies STRICT.
func isStrictTable(createSQL string) bool {
	upper := strings.ToUpper(createSQL)
	return hasStrictKeyword(upper)
}

// hasStrictKeyword checks if "STRICT" appears as a standalone keyword in the
// CREATE TABLE SQL (not inside a string literal or column name).
func hasStrictKeyword(upperSQL string) bool {
	sql := stripCTASSelect(upperSQL)
	idx := strings.LastIndex(sql, ")")
	if idx < 0 {
		return false
	}
	tail := sql[idx:]
	return strings.Contains(tail, "STRICT")
}

// hasWithoutRowidKeyword checks if "WITHOUT ROWID" appears after the closing
// parenthesis in the CREATE TABLE SQL.
func hasWithoutRowidKeyword(upperSQL string) bool {
	sql := stripCTASSelect(upperSQL)
	idx := strings.LastIndex(sql, ")")
	if idx < 0 {
		return false
	}
	tail := sql[idx:]
	return strings.Contains(tail, "WITHOUT")
}
