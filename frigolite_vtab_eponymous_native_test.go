package frigolite

import (
	"strings"
	"testing"
)

// TestNativeEponymousGenerateSeries covers series.c eponymous virtual table
// semantics (tabfunc01): FROM generate_series with hidden-column WHERE
// constraints, CREATE rejection, missing-start error, and table_xinfo.
func TestNativeEponymousGenerateSeries(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"eponymous constraints", "SELECT *, '|' FROM generate_series WHERE start=1 AND stop=9 AND step=2;", "1 | 3 | 5 | 7 | 9 |"},
		{"tvf args", "SELECT * FROM generate_series(1,9,2);", "1 3 5 7 9"},
		{"step in WHERE", "SELECT * FROM generate_series(1,10) WHERE step=3;", "1 4 7 10"},
		{"IN on start", "SELECT * FROM generate_series WHERE start IN (1,7) AND stop=20 AND step=10 ORDER BY +1;", "1 7 11 17"},
		{"schema-qualified", "SELECT * FROM main.generate_series(1,4);", "1 2 3 4"},
		{"rowid order", "SELECT rowid, * FROM generate_series(0,32,5) ORDER BY value DESC;", "30 30 25 25 20 20 15 15 10 10 5 5 0 0"},
	}
	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Errorf("%s: query error: %v", tc.name, r.Error)
			continue
		}
		var parts []string
		for _, row := range r.Rows {
			for _, v := range row {
				if v == nil {
					parts = append(parts, "NULL")
				} else {
					parts = append(parts, formatSQLiteValue(v))
				}
			}
		}
		if got := strings.Join(parts, " "); got != tc.want {
			t.Errorf("%s: got [%s] want [%s]", tc.name, got, tc.want)
		}
	}

	// CREATE VIRTUAL TABLE on an eponymous-only module must fail.
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING generate_series;"); res.Error == nil ||
		res.Error.Error() != "no such module: generate_series" {
		t.Errorf("CREATE USING generate_series: got %v", res.Error)
	}

	// Missing usable START must fail with series.c's message.
	if res := db.Query("SELECT * FROM generate_series LIMIT 5;"); res.Error == nil ||
		res.Error.Error() != `first argument to "generate_series()" missing or unusable` {
		t.Errorf("missing start: got %v", res.Error)
	}

	// PRAGMA table_xinfo exposes hidden columns.
	r := db.Query("PRAGMA table_xinfo(generate_series);")
	if r.Error != nil {
		t.Fatalf("table_xinfo: %v", r.Error)
	}
	var names []string
	for _, row := range r.Rows {
		names = append(names, row[1].(string))
	}
	if got := strings.Join(names, ","); got != "value,start,stop,step" {
		t.Errorf("xinfo columns: got %s", got)
	}
}
