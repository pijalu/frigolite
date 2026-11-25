package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestQuoteDQSProtectedDDL pins quote-2.*: with DQS disabled for DDL and
// enabled for DML, double-quoted string literals are rejected in CHECK
// constraints and index expressions, but writable_schema still loads the
// legacy schema verbatim.
func TestQuoteDQSProtectedDDL(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetDQS(false, true)

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	query := func(sql, want string) {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		got := make([]string, len(r.Rows))
		for i, row := range r.Rows {
			parts := make([]string, len(row))
			for j, value := range row {
				parts[j] = fmt.Sprint(value)
			}
			got[i] = strings.Join(parts, " ")
		}
		if joined := strings.Join(got, " | "); joined != want {
			t.Fatalf("%s: got %q, want %q", sql, joined, want)
		}
	}

	exec(`CREATE TABLE t1(x, y, z);`)
	const suffix = ` - should this be a string literal in single-quotes?`
	for _, tc := range []struct{ sql, name string }{
		{`CREATE TABLE xyz(a, b, c CHECK (c!="null") )`, `null`},
		{`CREATE INDEX i2 ON t1(x, y, z||"abc")`, `abc`},
		{`CREATE INDEX i3 ON t1("w")`, `w`},
		{`CREATE INDEX i4 ON t1(x) WHERE z="w"`, `w`},
	} {
		r := db.Query(tc.sql)
		want := `no such column: "` + tc.name + `"` + suffix
		if r.Error == nil || r.Error.Error() != want {
			t.Fatalf("%s: error = %v, want %s", tc.sql, r.Error, want)
		}
	}

	exec(`PRAGMA writable_schema = 1;
  CREATE TABLE xyz(a, b, c CHECK (c!="null") );
  CREATE INDEX i2 ON t1(x, y, z||"abc");
  CREATE INDEX i3 ON t1("w"||"");
  CREATE INDEX i4 ON t1(x) WHERE z="w";`)
	exec(`INSERT INTO xyz VALUES(1, 2, 3);`)

	r := db.Query(`INSERT INTO xyz VALUES(1, 2, 'null');`)
	if r.Error == nil || r.Error.Error() != `CHECK constraint failed: c!="null"` {
		t.Fatalf("CHECK insert error = %v, want CHECK constraint failed", r.Error)
	}

	query(`INSERT INTO t1 VALUES(1, 2, 3);
  INSERT INTO t1 VALUES(4, 5, 'w');
  SELECT * FROM t1 WHERE z='w';`, `4 5 w`)
	query(`SELECT sql FROM sqlite_master;`,
		`CREATE TABLE t1(x, y, z) | CREATE TABLE xyz(a, b, c CHECK (c!="null") ) | `+
			`CREATE INDEX i2 ON t1(x, y, z||"abc") | CREATE INDEX i3 ON t1("w"||"") | `+
			`CREATE INDEX i4 ON t1(x) WHERE z="w"`)
}
