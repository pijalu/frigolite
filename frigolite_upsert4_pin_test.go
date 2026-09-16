package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestUpsertWRDoUpdateUniquePin pins upsert4 1.3.5 (and the upsert1 600/610
// class): ON CONFLICT DO UPDATE SET must enforce the table's other UNIQUE
// constraints on WITHOUT ROWID tables too. WR cells share the synthetic
// rowid 0, so the conflict check's "exclude the row being updated" step must
// compare declared PK values, not rowids — otherwise every secondary-UNIQUE
// conflict was classified as the row itself and silently allowed.
func TestUpsertWRDoUpdateUniquePin(t *testing.T) {
	schemas := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c UNIQUE)",
		"CREATE TABLE t1(a INT PRIMARY KEY, b, c UNIQUE)",
		"CREATE TABLE t1(a INT PRIMARY KEY, b, c UNIQUE) WITHOUT ROWID",
	}
	for _, schema := range schemas {
		db, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer db.Close()
			if r := db.Exec(schema); r.Error != nil {
				t.Fatalf("%s: %v", schema, r.Error)
			}
			for _, ins := range []string{
				"INSERT INTO t1 VALUES(1, NULL, 'one')",
				"INSERT INTO t1 VALUES(2, NULL, 'two')",
				"INSERT INTO t1 VALUES(3, NULL, 'three')",
			} {
				if r := db.Exec(ins); r.Error != nil {
					t.Fatalf("%s: %s: %v", schema, ins, r.Error)
				}
			}
			// DO UPDATE SET c='one' collides with row 1's c — must fail and
			// leave the table unchanged.
			r := db.Exec("INSERT INTO t1 VALUES(2, NULL, 'zero') ON CONFLICT (a) DO UPDATE SET c = 'one'")
			if r.Error == nil || !strings.Contains(r.Error.Error(), "UNIQUE constraint failed: t1.c") {
				t.Fatalf("%s: want UNIQUE constraint failed: t1.c, got %v", schema, r.Error)
			}
			q := db.Query("SELECT * FROM t1")
			if q.Error != nil {
				t.Fatalf("%s: %v", schema, q.Error)
			}
			want := "1 <nil> one 2 <nil> two 3 <nil> three"
			if got := flattenRows(q.Rows); got != want {
				t.Fatalf("%s: table changed: got [%s] want [%s]", schema, got, want)
			}
			// Non-conflicting DO UPDATE still works (b=2 on row 2).
			if r := db.Exec("INSERT INTO t1 VALUES(2, NULL, 'zero') ON CONFLICT (a) DO UPDATE SET b = 2"); r.Error != nil {
				t.Fatalf("%s: benign DO UPDATE failed: %v", schema, r.Error)
			}
		}()
	}
}
