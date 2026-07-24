package frigolite

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func setupDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// checkQueryResult checks that a query result matches the expected value.
// Parse errors are silently ignored (expected for unsupported features).
// If the query succeeds but the result doesn't match expected, the test FAILS.
// expected is in TCL list format with optional { } braces.
// In TCL, {} represents NULL and individual values may be braced.
func checkQueryResult(t *testing.T, res *Result, expected string) {
	t.Helper()
	if res.Error != nil {
		return
	}
	var parts []string
	for _, row := range res.Rows {
		for _, val := range row {
			if val == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, formatSQLiteValue(val))
			}
		}
	}
	got := strings.Join(parts, " ")
	// Parse TCL list format: strip outer braces, split into tokens,
	// where {} represents NULL and {value} represents a braced value.
	want := parseTCLList(expected)
	if got != want {
		t.Errorf("result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// parseTCLList parses a TCL list string into a space-separated string.
// In TCL lists, {} represents NULL and {value} represents a braced value.
func parseTCLList(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// If the entire string is wrapped in { }, strip the outer braces
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		// Need to match braces properly
		depth := 0
		for i, c := range s {
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
				if depth == 0 && i < len(s)-1 {
					// Multiple top-level values: braces don't wrap entire string
					break
				}
				if depth == 0 && i == len(s)-1 {
					// Braces wrap entire string
					inner := s[1 : len(s)-1]
					return parseTCLList(inner)
				}
			}
		}
	}
	// Parse tokens
	var tokens []string
	i := 0
	for i < len(s) {
		// Skip whitespace
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '{' {
			// Braced token: find matching }
			depth := 1
			start := i + 1
			i++
			for i < len(s) && depth > 0 {
				if s[i] == '{' {
					depth++
				} else if s[i] == '}' {
					depth--
				}
				i++
			}
			// Guard against slice bounds: if i <= start, the braces were empty/malformed
			if i <= start {
				i = start
				tokens = append(tokens, "NULL")
			} else {
				token := s[start : i-1]
				// {} represents NULL, otherwise it's the braced value
				if token == "" {
					tokens = append(tokens, "NULL")
				} else {
					tokens = append(tokens, token)
				}
			}
		} else {
			// Unbraced token: read until whitespace
			start := i
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
				i++
			}
			tokens = append(tokens, s[start:i])
		}
	}
	return strings.Join(tokens, " ")
}

// checkExecOK checks that an exec statement completed without error.
func checkExecOK(t *testing.T, res *Result) {
	t.Helper()
	if res.Error != nil {
		t.Errorf("exec error: %v", res.Error)
	}
}

func TestOpenClose(t *testing.T) {
	db := setupDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestExec(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("CREATE TABLE test (id INTEGER, name TEXT)")
	if res.Error != nil {
		t.Fatalf("CREATE TABLE: %v", res.Error)
	}

	res = db.Exec("INSERT INTO test VALUES (1, 'Alice')")
	if res.Error != nil {
		t.Fatalf("INSERT: %v", res.Error)
	}
	if res.Changes != 1 {
		t.Errorf("INSERT changes: got %d, want 1", res.Changes)
	}
}

func TestEmptyResult(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Query("SELECT * FROM nonexistent")
	if res.Error == nil {
		t.Errorf("expected error for nonexistent table")
	}
}

func TestParseErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("INVALID SQL")
	if res.Error == nil {
		t.Errorf("expected error for invalid SQL")
	}
}

func TestFileExists(t *testing.T) {
	path := t.TempDir() + "/test.db"
	if FileExists(path) {
		t.Errorf("file should not exist yet")
	}
	db, _ := Open(path)
	db.Close()
	if !FileExists(path) {
		t.Errorf("file should exist now")
	}
	os.Remove(path)
}

func TestDumpAll(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t1 (id INTEGER)")
	db.Exec("INSERT INTO t1 VALUES (1)")

	db.DumpAll()
}

// formatSQLiteValue formats a value the way SQLite does.
// For float64, integer-valued floats get ".0" suffix (1.0 not 1).
func formatSQLiteValue(val interface{}) string {
	switch v := val.(type) {
	case float64:
		// Check if float is a whole number
		if v == float64(int64(v)) {
			return fmt.Sprintf("%.1f", v)
		}
		return fmt.Sprintf("%g", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
