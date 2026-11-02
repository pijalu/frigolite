package frigolite

import (
	"reflect"
	"testing"
)

func TestFullColumnNamesExpandsBareAndQualifiedStars(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE tabc(a, b)",
		"CREATE TABLE txyz(x, y)",
		"CREATE VIEW vtab AS SELECT a, b FROM tabc",
		"INSERT INTO tabc VALUES(1, 2)",
		"INSERT INTO txyz VALUES(3, 4)",
		"PRAGMA short_column_names=OFF",
		"PRAGMA full_column_names=ON",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	// Expectations are the sqlite3 oracle's rendering under
	// short_column_names=OFF + full_column_names=ON (probed): full
	// "table.column" names everywhere, the FROM alias for a star's
	// expansion ("q.*" renders "q.a") but the column's own table for an
	// explicit aliased reference ("q.a" renders "tabc.a" — select.c
	// generateColumnNames resolves the referenced column's table, not the
	// query's alias).
	for _, query := range []struct {
		sql   string
		wants []string
	}{
		{"SELECT * FROM tabc", []string{"tabc.a", "tabc.b"}},
		{"SELECT tabc.* FROM tabc", []string{"tabc.a", "tabc.b"}},
		{"SELECT * FROM tabc, txyz", []string{"tabc.a", "tabc.b", "txyz.x", "txyz.y"}},
		{"SELECT a, txyz.x FROM tabc, txyz", []string{"tabc.a", "txyz.x"}},
		{"SELECT * FROM vtab", []string{"vtab.a", "vtab.b"}},
		{"SELECT * FROM tabc AS q", []string{"q.a", "q.b"}},
		{"SELECT q.* FROM tabc AS q", []string{"q.a", "q.b"}},
		{"SELECT * FROM TABC", []string{"tabc.a", "tabc.b"}},
		{"SELECT * FROM tabc AS q, txyz AS r", []string{"q.a", "q.b", "r.x", "r.y"}},
		{"SELECT q.a FROM tabc AS q", []string{"tabc.a"}},
		{"SELECT tabc.a FROM tabc", []string{"tabc.a"}},
	} {
		result := db.Query(query.sql)
		if result.Error != nil {
			t.Fatalf("Query(%q): %v", query.sql, result.Error)
		}
		if !reflect.DeepEqual(result.Columns, query.wants) {
			t.Errorf("Query(%q) columns = %#v, want %#v", query.sql, result.Columns, query.wants)
		}
	}
}
