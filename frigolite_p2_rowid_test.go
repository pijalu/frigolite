package frigolite

import (
	"sort"
	"strings"
	"testing"
)

// TestP2RowidAutoindexSQLiteMaster checks that WITHOUT ROWID tables do not
// create sqlite_autoindex_* entries in sqlite_master for PRIMARY KEY
// constraints, and that a UNIQUE constraint whose column set matches the
// PRIMARY KEY is merged into the PK (no separate entry).
func TestP2RowidAutoindexSQLiteMaster(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	cases := []struct {
		name string
		ddl  string
		want string // SELECT type,name FROM sqlite_master flattened
	}{
		{
			name: "ipk-unique-same-col",
			ddl:  "CREATE TABLE t1(x INTEGER PRIMARY KEY UNIQUE, b) WITHOUT ROWID; CREATE INDEX t1x ON t1(x);",
			want: "index t1x | table t1",
		},
		{
			name: "unique-diff-col",
			ddl:  "CREATE TABLE t2(x UNIQUE, y PRIMARY KEY) WITHOUT ROWID;",
			want: "index sqlite_autoindex_t2_1 | table t2",
		},
		{
			name: "pk-plus-other-unique",
			ddl:  "CREATE TABLE t3(x INTEGER PRIMARY KEY, y UNIQUE) WITHOUT ROWID;",
			want: "index sqlite_autoindex_t3_1 | table t3",
		},
		{
			name: "col-unique-matches-table-pk",
			ddl:  "CREATE TABLE t4(x UNIQUE, x2, PRIMARY KEY(x)) WITHOUT ROWID;",
			want: "table t4",
		},
		{
			name: "table-unique-matches-pk",
			ddl:  "CREATE TABLE t5(x, y, PRIMARY KEY(x), UNIQUE(x)) WITHOUT ROWID;",
			want: "table t5",
		},
		{
			name: "multiple-uniques-plus-pk",
			ddl:  "CREATE TABLE t6(x UNIQUE, y UNIQUE, z PRIMARY KEY) WITHOUT ROWID;",
			want: "index sqlite_autoindex_t6_1 | index sqlite_autoindex_t6_2 | table t6",
		},
		{
			name: "ipk-desc-unique",
			ddl:  "CREATE TABLE t7(x INTEGER PRIMARY KEY DESC UNIQUE, y) WITHOUT ROWID;",
			want: "table t7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db.Exec("DROP TABLE IF EXISTS t1; DROP TABLE IF EXISTS t2; DROP TABLE IF EXISTS t3; DROP TABLE IF EXISTS t4; DROP TABLE IF EXISTS t5; DROP TABLE IF EXISTS t6; DROP TABLE IF EXISTS t7; DROP INDEX IF EXISTS t1x;")
			res := db.Exec(tc.ddl)
			if res.Error != nil {
				t.Fatalf("DDL error: %v\nsql: %s", res.Error, tc.ddl)
			}
			q := db.Query("SELECT type, name FROM sqlite_master ORDER BY name")
			if q.Error != nil {
				t.Fatalf("query error: %v", q.Error)
			}
			var parts []string
			for _, row := range q.Rows {
				parts = append(parts, row[0].(string)+" "+row[1].(string))
			}
			sort.Strings(parts)
			got := strings.Join(parts, " | ")
			// want is already sorted by construction
			if got != tc.want {
				t.Errorf("sqlite_master mismatch\n  got:  [%s]\n  want: [%s]", got, tc.want)
			}
		})
	}
}

// TestP2RowidQueryPlanPKOnly checks that the query planner uses only the
// PRIMARY KEY columns for a WITHOUT ROWID table lookup, not extra columns.
func TestP2RowidQueryPlanPKOnly(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t1(a INT PRIMARY KEY) WITHOUT ROWID;")
	db.Exec("INSERT INTO t1(a) VALUES(10);")
	db.Exec("ALTER TABLE t1 ADD COLUMN b INT;")
	db.Exec("CREATE TABLE dual AS SELECT 'X' AS dummy;")

	res := db.Query("EXPLAIN QUERY PLAN SELECT * FROM dual, t1 WHERE a=10 AND b=10;")
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	var got string
	for _, row := range res.Rows {
		for _, v := range row {
			got += v.(string) + " "
		}
	}
	if strings.Contains(got, "b=") {
		t.Errorf("query plan must not use b= in PK lookup\n  got: [%s]", got)
	}
	if !strings.Contains(got, "a=?") {
		t.Errorf("query plan should use a= in PK lookup\n  got: [%s]", got)
	}
}
