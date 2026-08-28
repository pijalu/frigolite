// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Regression tests for blob literal and substr() blob handling.

package frigolite

import "testing"

// TestBlobLiteralSubstrQuote is a regression test for the SQLite cast
// harness cases cast-8.1/cast-8.2: X'...' blob literals must evaluate as
// blobs (not integers), and substr() on a blob must return a blob so that
// quote() renders the two expressions identically.
func TestBlobLiteralSubstrQuote(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	tests := []struct {
		sql  string
		want string
	}{
		// cast-8.1: quote(blob) == quote(substr(blob, 1)) must be true.
		{"SELECT quote(X'310032003300')==quote(substr(X'310032003300', 1))", "1"},
		// cast-8.2: CAST(... AS TEXT) equality (kept passing).
		{"SELECT CAST(X'310032003300' AS TEXT)==CAST(substr(X'310032003300', 1) AS TEXT)", "1"},
		// A blob literal keeps its blob type.
		{"SELECT typeof(X'310032003300')", "blob"},
		// substr() of a blob returns a blob.
		{"SELECT typeof(substr(X'310032003300', 1))", "blob"},
		{"SELECT typeof(substr(X'310032003300', 2, 2))", "blob"},
		// quote() renders blobs as X'...' literals.
		{"SELECT quote(X'310032003300')", "X'310032003300'"},
		{"SELECT quote(substr(X'310032003300', 1))", "X'310032003300'"},
		// substr() on a blob with explicit offset and length.
		{"SELECT quote(substr(X'313233', 2))", "X'3233'"},
		{"SELECT quote(substr(X'313233', 1, 2))", "X'3132'"},
		// Empty blob result (out-of-range start) renders as X''.
		{"SELECT quote(substr(x'313233343536373839',0x7ffffffffffffffe,5))", "X''"},
		// Text substr() behavior is unchanged.
		{"SELECT substr('hello', 2)", "ello"},
		{"SELECT substr('hello', -3)", "llo"},
	}
	for _, tt := range tests {
		res := db.Query(tt.sql)
		if res.Error != nil {
			t.Errorf("query error: %v\n  sql: %s", res.Error, tt.sql)
			continue
		}
		if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
			t.Errorf("no result row\n  sql: %s", tt.sql)
			continue
		}
		got := formatSQLiteValue(res.Rows[0][0])
		if got != tt.want {
			t.Errorf("result mismatch\n  sql:  %s\n  got:  [%s]\n  want: [%s]", tt.sql, got, tt.want)
		}
	}
}
