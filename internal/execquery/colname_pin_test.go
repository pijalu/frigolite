package execquery_test

import (
	"reflect"
	"testing"

	"github.com/pijalu/frigolite"
)

// The result-column naming modes of PRAGMA short_column_names /
// full_column_names (select.c sqlite3GenerateColumnNames), pinned against
// /usr/bin/sqlite3 3.54.0 probes of the select1-6.9.x / colname-4.x shapes:
//
//	short=ON, full=OFF (default): direct column references name the bare
//	                              column; star expansions stay bare.
//	short=ON, full=ON:            direct references name RESOLVED-TABLE.COLUMN
//	                              with the REAL table name — never the alias
//	                              ("SELECT a.f1 FROM test1 a" → "test1.f1").
//	short=OFF, full=OFF:          the name is the expression's original span
//	                              text ("test1 . f1" keeps written spacing;
//	                              "F1" keeps written case; quotes stay).
//	short=OFF, full=ON:           star expansions prefix each column with its
//	                              FROM item's name AS WRITTEN — the alias
//	                              ("a.f1"), a subquery's alias ("b.x"), or
//	                              "(subquery-N)" when unaliased.
//
// A FROM-clause subquery's synthesized table is named after its alias, or
// "(subquery-0)" unaliased (select.c sqlite3ExpandSubquery; select1-6.9.7's
// source normalizes the number with regsub, so the value is contractually
// arbitrary and the converted fixture pins the literal 0).
func TestPinResultColumnNameModes(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE test1(f1 int, f2 int)`,
		`CREATE TABLE t3(a int, b int)`,
		`CREATE TABLE t4(a int, b int)`,
		`CREATE TABLE t7(x INTEGER PRIMARY KEY, y)`,
		`INSERT INTO test1 VALUES(11,22)`,
		`INSERT INTO t3 VALUES(1,2)`,
		`INSERT INTO t4 VALUES(3,4)`,
		`INSERT INTO t7 VALUES(1,2)`,
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}

	type combo struct {
		label string
		short string
		full  string
	}
	modes := []combo{
		{"default", "ON", "OFF"},
		{"short+full", "ON", "ON"},
		{"span", "OFF", "OFF"},
		{"longNames", "OFF", "ON"},
	}
	// cases keyed by the mode labels they apply to.
	cases := []struct {
		sql  string
		want map[string][]string
	}{
		{
			// select1-6.9.9/6.9.11/6.9.15: the REAL table name, not the alias.
			sql: `SELECT a.f1, b.f2 FROM test1 a, test1 b LIMIT 1`,
			want: map[string][]string{
				"default":    {"f1", "f2"},
				"short+full": {"test1.f1", "test1.f2"},
				"span":       {"a.f1", "b.f2"},
				"longNames":  {"test1.f1", "test1.f2"},
			},
		},
		{
			// select1-6.9.3: the span text keeps the written spacing; the
			// fullName modes render table.column without spaces.
			sql: `SELECT test1 . f1, test1 . f2 FROM test1 LIMIT 1`,
			want: map[string][]string{
				"default":    {"f1", "f2"},
				"short+full": {"test1.f1", "test1.f2"},
				"span":       {"test1 . f1", "test1 . f2"},
				"longNames":  {"test1.f1", "test1.f2"},
			},
		},
		{
			// Span mode keeps the written case; named modes use the declared
			// column spelling; full=ON qualifies with the table name.
			sql: `SELECT F1 FROM test1 LIMIT 1`,
			want: map[string][]string{
				"default":    {"f1"},
				"short+full": {"test1.f1"},
				"span":       {"F1"},
				"longNames":  {"test1.f1"},
			},
		},
		{
			// select1-6.9.6/colname-4.1: star expansions prefix each column
			// with the FROM item's name as written under longNames only.
			sql: `SELECT * FROM test1 a, test1 b LIMIT 1`,
			want: map[string][]string{
				"default":    {"f1", "f2", "f1", "f2"},
				"short+full": {"f1", "f2", "f1", "f2"},
				"span":       {"f1", "f2", "f1", "f2"},
				"longNames":  {"a.f1", "a.f2", "b.f1", "b.f2"},
			},
		},
		{
			// select1-6.9.7/6.9.8: a FROM-clause subquery's synthesized table
			// is named after its alias, or "(subquery-0)" when unaliased.
			sql: `SELECT * FROM test1 a, (select 5, 6) LIMIT 1`,
			want: map[string][]string{
				"default":    {"f1", "f2", "5", "6"},
				"short+full": {"f1", "f2", "5", "6"},
				"span":       {"f1", "f2", "5", "6"},
				"longNames":  {"a.f1", "a.f2", "(subquery-0).5", "(subquery-0).6"},
			},
		},
		{
			sql: `SELECT * FROM test1 a, (select 5 AS x, 6 AS y) AS b LIMIT 1`,
			want: map[string][]string{
				"default":    {"f1", "f2", "x", "y"},
				"short+full": {"f1", "f2", "x", "y"},
				"span":       {"f1", "f2", "x", "y"},
				"longNames":  {"a.f1", "a.f2", "b.x", "b.y"},
			},
		},
		{
			// select1-11.8: a bare column reference and a qualified star both
			// name bare columns in every non-longNames mode.
			sql: `SELECT t3.b, t4.* FROM t3, t4`,
			want: map[string][]string{
				"default":    {"b", "a", "b"},
				"short+full": {"t3.b", "a", "b"},
				"span":       {"t3.b", "a", "b"},
				"longNames":  {"t3.b", "t4.a", "t4.b"},
			},
		},
		{
			// select1-11.15: a subquery operand's star expands through the
			// subquery's OWN result-column names (span-named aggregates),
			// not through the outer join's merged row-map keys.
			sql: `SELECT y.*, t3.* FROM t3, (SELECT max(a), max(b) FROM t4) AS y`,
			want: map[string][]string{
				"default":    {"max(a)", "max(b)", "a", "b"},
				"short+full": {"max(a)", "max(b)", "a", "b"},
				"span":       {"max(a)", "max(b)", "a", "b"},
				"longNames":  {"y.max(a)", "y.max(b)", "t3.a", "t3.b"},
			},
		},
		{
			// colname-4.5: an explicit qualified reference names the REAL
			// table; the star expansion prefixes with the FROM item's written
			// name under longNames.
			sql: `SELECT Tabc.f1, Txyz.f2, * FROM test1 Tabc, test1 Txyz`,
			want: map[string][]string{
				"default":    {"f1", "f2", "f1", "f2", "f1", "f2"},
				"short+full": {"test1.f1", "test1.f2", "f1", "f2", "f1", "f2"},
				"span":       {"Tabc.f1", "Txyz.f2", "f1", "f2", "f1", "f2"},
				"longNames":  {"test1.f1", "test1.f2", "Tabc.f1", "Tabc.f2", "Txyz.f1", "Txyz.f2"},
			},
		},
		{
			// colname-7.1: a rowid reference resolves to the IPK column's
			// declared name; under full=ON both it and the star expansion are
			// table-qualified (the star with the FROM item's written name).
			sql: `SELECT rowid, * FROM t7`,
			want: map[string][]string{
				"default":    {"x", "x", "y"},
				"short+full": {"t7.x", "x", "y"},
				"span":       {"rowid", "x", "y"},
				"longNames":  {"t7.x", "t7.x", "t7.y"},
			},
		},
	}
	for _, c := range cases {
		for _, m := range modes {
			want, ok := c.want[m.label]
			if !ok {
				continue
			}
			db.Exec("PRAGMA short_column_names=" + m.short)
			db.Exec("PRAGMA full_column_names=" + m.full)
			res := db.Query(c.sql)
			if res.Error != nil {
				t.Errorf("%s %q: %v", m.label, c.sql, res.Error)
				continue
			}
			if !reflect.DeepEqual(res.Columns, want) {
				t.Errorf("%s %q: columns = %v, want %v", m.label, c.sql, res.Columns, want)
			}
		}
	}
}

// TestPinResultColumnNameSubqueryAndAliases pins the longNames naming of
// views and CTEs (the synthesized FROM-item tables): a view names its columns
// after the view (never the alias), a CTE after the CTE name, and an aliased
// FROM item after its alias.
func TestPinResultColumnNameSubqueryAndAliases(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE test1(f1 int, f2 int)`,
		`INSERT INTO test1 VALUES(11,22)`,
		`CREATE VIEW v1 AS SELECT f1, f2 FROM test1`,
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	db.Exec(`PRAGMA short_column_names=OFF`)
	db.Exec(`PRAGMA full_column_names=ON`)
	cases := []struct {
		sql  string
		want []string
	}{
		{`SELECT * FROM v1`, []string{"v1.f1", "v1.f2"}},
		{`SELECT f1 FROM v1`, []string{"v1.f1"}},
		{`SELECT * FROM v1 AS vv`, []string{"vv.f1", "vv.f2"}},
		{`WITH c AS (SELECT 9 AS z) SELECT * FROM c`, []string{"c.z"}},
		{`WITH c(x) AS (SELECT 9) SELECT * FROM c`, []string{"c.x"}},
		{`SELECT a.* FROM test1 AS a`, []string{"a.f1", "a.f2"}},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Errorf("%q: %v", c.sql, res.Error)
			continue
		}
		if !reflect.DeepEqual(res.Columns, c.want) {
			t.Errorf("%q: columns = %v, want %v", c.sql, res.Columns, c.want)
		}
	}
}
