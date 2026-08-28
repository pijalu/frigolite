package frigolite_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSwarmvtabErrorContractsNative pins the unionConnect error ordering of
// ext/misc/unionvtab.c (swarmvtab.test 2.4/2.5 and 3.x, verified against a
// ground-truth build of the C extension):
//
//   - the source statement is PREPARED (unionPreparePrintf, unionConnect
//     step 2) BEFORE unionConfigureVtab parses the aux options (step 3), so
//     a source-statement parse error surfaces first, wrapped as
//     "sql error: %s" (unionPrepare);
//   - missing=/openclose= option values are PREPARED as UDF calls at option
//     parse, so an unknown function fails right there ("sql error: no such
//     function: x");
//   - UDF failures at STEP time (notFound firing during the eager source-0
//     open in CREATE, or during scans) surface RAW, unwrapped
//     ("fetch_db error!").
func TestSwarmvtabErrorContractsNative(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// fetch_db: always errors (swarmvtab.test 3.x fixture).
	db.RegisterFunction("fetch_db", func(args []interface{}) (interface{}, error) {
		return nil, fmt.Errorf("fetch_db error!")
	}, 1, 1)

	// Source statement shared by the 3.x cases (test files under dir).
	db1 := filepath.Join(dir, "test.db1")
	db2 := filepath.Join(dir, "test.db2")
	vals := fmt.Sprintf(`VALUES('%s', 't1', 1, 10),('%s', 't1', 11, 20)`, db1, db2)

	cases := []struct {
		name string
		sql  string
		want string // exact expected error text ("" = success)
	}{
		{
			name: "2.4 source-stmt syntax error, no aux options",
			sql:  `CREATE VIRTUAL TABLE temp.x1 USING swarmvtab('SELECT * FROMdir')`,
			want: `sql error: near "FROMdir": syntax error`,
		},
		{
			name: "2.5 syntax error wins over option-UDF validation",
			sql:  `CREATE VIRTUAL TABLE temp.x1 USING swarmvtab('SELECT * FROMdir', 'fetchdb')`,
			want: `sql error: near "FROMdir": syntax error`,
		},
		{
			name: "3.1 missing= names an unknown function",
			sql:  `CREATE VIRTUAL TABLE temp.xyz USING swarmvtab('` + vals + `', 'fetch_db_no_such_function')`,
			want: `sql error: no such function: fetch_db_no_such_function`,
		},
		{
			name: "3.2 missing= UDF step error during CREATE (eager source-0 open)",
			sql:  `CREATE VIRTUAL TABLE temp.xyz USING swarmvtab('` + vals + `', 'fetch_db')`,
			want: `fetch_db error!`,
		},
	}
	for _, tc := range cases {
		if res := db.Exec(tc.sql); res.Error != nil {
			if res.Error.Error() != tc.want {
				t.Errorf("%s:\n  got=%q\n want=%q", tc.name, res.Error.Error(), tc.want)
			}
		} else if tc.want != "" {
			t.Errorf("%s: expected error %q, got none", tc.name, tc.want)
		}
	}

	// 3.3.1 fixture: source 0's file exists with the rowid table -> CREATE
	// succeeds even though fetch_db errors on the (still missing) source 1.
	if res := db.Exec(fmt.Sprintf(`ATTACH '%s' AS aux;
		CREATE TABLE aux.t1(a INTEGER PRIMARY KEY, b);
		INSERT INTO aux.t1 VALUES(1,NULL),(2,NULL),(9,NULL);`, db1)); res.Error != nil {
		t.Fatalf("3.3.1 fixture: %v", res.Error)
	}
	if res := db.Exec(`CREATE VIRTUAL TABLE temp.xyz USING swarmvtab('` + vals + `', 'fetch_db')`); res.Error != nil {
		t.Errorf("3.3.1 CREATE: expected success, got %v", res.Error)
	}

	// 3.3.2: the scan opens missing source 1 -> notFound UDF errors raw.
	if r := db.Query("SELECT count(*) FROM xyz"); r.Error == nil {
		t.Errorf("3.3.2 scan: expected error, got %d rows", len(r.Rows))
	} else if r.Error.Error() != "fetch_db error!" {
		t.Errorf("3.3.2 scan:\n  got=%q\n want=%q", r.Error.Error(), "fetch_db error!")
	}
}
