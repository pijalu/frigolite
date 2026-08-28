// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Regression tests for table-valued pragma functions (pragma_table_info).

package frigolite

import "testing"

// TestPragmaTableInfoViewCast is a regression test for the SQLite cast
// harness cases cast-10.7/cast-10.8: pragma_table_info('view') must
// materialize the view's columns with names and declared types derived from
// the view's SELECT expressions (a CAST inside a compound yields type NUM).
func TestPragmaTableInfoViewCast(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	tests := []struct {
		sql  string
		want string
	}{
		// cast-10.7: UNION ALL of CAST(44 AS REAL) and 55 → type NUM.
		{"CREATE VIEW v1 AS SELECT CAST(44 AS REAL) AS 'm' UNION ALL SELECT 55; SELECT name, type FROM pragma_table_info('v1')", "m NUM"},
		// cast-10.8: multi-row VALUES with CAST → type NUM.
		{"CREATE VIEW v2 AS VALUES(CAST(44 AS REAL)),(55); SELECT type FROM pragma_table_info('v2')", "NUM"},
		// Single SELECT CAST keeps the standard type name (REAL, not NUM).
		{"CREATE VIEW v3 AS SELECT CAST(44 AS REAL) AS a; SELECT name, type FROM pragma_table_info('v3')", "a REAL"},
		// pragma_table_info on a real table reports its declared types.
		{"CREATE TABLE t1(a REAL, b INTEGER, c TEXT); SELECT name, type FROM pragma_table_info('t1')", "a REAL b INTEGER c TEXT"},
		// Star expansion returns all six pragma columns.
		{"CREATE TABLE t2(x); SELECT count(*) FROM pragma_table_info('t2')", "1"},
		// Unknown table returns zero rows, not an error.
		{"SELECT name FROM pragma_table_info('no_such_table')", ""},
	}
	for _, tt := range tests {
		res := db.Query(tt.sql)
		if res.Error != nil {
			t.Errorf("query error: %v\n  sql: %s", res.Error, tt.sql)
			continue
		}
		got := flattenResult(res)
		if got != tt.want {
			t.Errorf("result mismatch\n  sql:  %s\n  got:  [%s]\n  want: [%s]", tt.sql, got, tt.want)
		}
	}
}
