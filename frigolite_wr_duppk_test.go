package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Regression pins for WITHOUT ROWID PRIMARY KEY uniqueness on INSERT (oracle:
// sqlite3 3.54). A plain INSERT of a row whose WR PK equals an existing row's
// PK must raise exactly "UNIQUE constraint failed: <table>.<pk-cols>" — the
// pre-fix engine accepted it because the single-unique-column fast path seeked
// the WR index btree by cell rowid (which WR cells do not carry).

// TestWithoutRowidInsertDuplicatePKRejected covers the rejected shapes: every
// PK value class (INTEGER/TEXT/REAL/BLOB), column-level and table-level PK
// declarations, and the VALUES / INSERT...SELECT / CTE-fed SELECT / multi-row
// VALUES statement shapes.
func TestWithoutRowidInsertDuplicatePKRejected(t *testing.T) {
	cases := []struct {
		name  string
		table string   // for the post-error row count
		sqls  []string // the last statement must fail with wantErr
		want  string
	}{
		{
			name:  "ipk column-level VALUES",
			table: "t",
			sqls: []string{
				"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID",
				"INSERT INTO t VALUES(1,'x')",
				"INSERT INTO t VALUES(1,'y')",
			},
			want: "UNIQUE constraint failed: t.a",
		},
		{
			name:  "ipk table-level single col",
			table: "tl",
			sqls: []string{
				"CREATE TABLE tl(a INTEGER, b TEXT, PRIMARY KEY(a)) WITHOUT ROWID",
				"INSERT INTO tl VALUES(1,'x')",
				"INSERT INTO tl VALUES(1,'y')",
			},
			want: "UNIQUE constraint failed: tl.a",
		},
		{
			name:  "text PK",
			table: "u",
			sqls: []string{
				"CREATE TABLE u(k TEXT PRIMARY KEY, v) WITHOUT ROWID",
				"INSERT INTO u VALUES('a',1)",
				"INSERT INTO u VALUES('a',2)",
			},
			want: "UNIQUE constraint failed: u.k",
		},
		{
			name:  "real PK",
			table: "r",
			sqls: []string{
				"CREATE TABLE r(a REAL PRIMARY KEY, b) WITHOUT ROWID",
				"INSERT INTO r VALUES(1.5,'x')",
				"INSERT INTO r VALUES(1.5,'y')",
			},
			want: "UNIQUE constraint failed: r.a",
		},
		{
			name:  "blob PK",
			table: "b2",
			sqls: []string{
				"CREATE TABLE b2(a BLOB PRIMARY KEY, v) WITHOUT ROWID",
				"INSERT INTO b2 VALUES(x'4142',1)",
				"INSERT INTO b2 VALUES(x'4142',2)",
			},
			want: "UNIQUE constraint failed: b2.a",
		},
		{
			name:  "composite PK error lists PK key order",
			table: "m",
			sqls: []string{
				"CREATE TABLE m(a, b, c, PRIMARY KEY(b, a)) WITHOUT ROWID",
				"INSERT INTO m VALUES(1,2,3)",
				"INSERT INTO m VALUES(1,2,4)",
			},
			want: "UNIQUE constraint failed: m.b, m.a",
		},
		{
			name:  "nocase PK collation conflicts",
			table: "nc",
			sqls: []string{
				"CREATE TABLE nc(k TEXT COLLATE NOCASE PRIMARY KEY, v) WITHOUT ROWID",
				"INSERT INTO nc VALUES('ABC',1)",
				"INSERT INTO nc VALUES('abc',2)",
			},
			want: "UNIQUE constraint failed: nc.k",
		},
		{
			name:  "INSERT ... SELECT",
			table: "s",
			sqls: []string{
				"CREATE TABLE s(w INTEGER PRIMARY KEY, z TEXT) WITHOUT ROWID",
				"INSERT INTO s VALUES(1,'a')",
				"CREATE TABLE src(a INTEGER, b TEXT)",
				"INSERT INTO src VALUES(1,'dup')",
				"INSERT INTO s SELECT a, b FROM src",
			},
			want: "UNIQUE constraint failed: s.w",
		},
		{
			name:  "CTE-fed INSERT ... SELECT",
			table: "s2",
			sqls: []string{
				"CREATE TABLE s2(w INTEGER PRIMARY KEY, z TEXT) WITHOUT ROWID",
				"INSERT INTO s2 VALUES(1,'a')",
				"WITH d(w,z) AS (VALUES(1,'dup')) INSERT INTO s2 SELECT w,z FROM d",
			},
			want: "UNIQUE constraint failed: s2.w",
		},
		{
			name:  "multi-row VALUES",
			table: "s3",
			sqls: []string{
				"CREATE TABLE s3(w INTEGER PRIMARY KEY, z TEXT) WITHOUT ROWID",
				"INSERT INTO s3 VALUES(1,'a')",
				"INSERT INTO s3 VALUES(2,'b'),(1,'c')",
			},
			want: "UNIQUE constraint failed: s3.w",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for i, s := range tc.sqls {
				res := db.Exec(s)
				if i < len(tc.sqls)-1 {
					if res.Error != nil {
						t.Fatalf("setup error: %v\n  sql: %s", res.Error, s)
					}
					continue
				}
				if res.Error == nil {
					t.Fatalf("duplicate PK accepted (want %q)\n  sql: %s", tc.want, s)
				}
				if got := res.Error.Error(); got != tc.want {
					t.Fatalf("error [%s], want [%s]", got, tc.want)
				}
			}
			// The conflicting row was not written: exactly the setup rows exist.
			r := db.Query("SELECT count(*) FROM " + tc.table)
			if r.Error != nil {
				t.Fatalf("count error: %v", r.Error)
			}
			if got := flattenResult(r); got != "1" {
				t.Fatalf("post-error row count [%s], want [1]", got)
			}
		})
	}
}

// TestWithoutRowidInsertDistinctPKsAccepted pins the non-conflicting shapes a
// wrong fix could over-reject: composite PKs sharing a prefix, binary text
// differing in case, and a fresh key after a rejected duplicate.
func TestWithoutRowidInsertDistinctPKsAccepted(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE m(a, b, c, PRIMARY KEY(a,b)) WITHOUT ROWID",
		"INSERT INTO m VALUES(1,2,3)",
		"INSERT INTO m VALUES(1,3,4)",
		"CREATE TABLE bi(k TEXT PRIMARY KEY, v) WITHOUT ROWID",
		"INSERT INTO bi VALUES('ABC',1)",
		"INSERT INTO bi VALUES('abc',2)",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	// A rejected duplicate must not poison the table: the next distinct row
	// inserts and the surviving rows read back intact.
	if res := db.Exec("INSERT INTO m VALUES(1,2,9)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "UNIQUE constraint failed: m.a, m.b") {
		t.Fatalf("expected composite PK conflict, got %v", res.Error)
	}
	if res := db.Exec("INSERT INTO m VALUES(2,2,5)"); res.Error != nil {
		t.Fatalf("distinct insert after conflict failed: %v", res.Error)
	}
	r := db.Query("SELECT * FROM m ORDER BY a, b")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if got := flattenResult(r); got != "1 2 3 1 3 4 2 2 5" {
		t.Fatalf("rows [%s], want [1 2 3 1 3 4 2 2 5]", got)
	}
}

// TestWithoutRowidInsertOrIgnoreReplaceUpsert pins the conflict-resolution
// semantics around the enforced WR PK (oracle parity).
func TestWithoutRowidInsertOrIgnoreReplaceUpsert(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE i(w INTEGER PRIMARY KEY, z TEXT) WITHOUT ROWID",
		"INSERT INTO i VALUES(1,'a')",
		// OR IGNORE: the duplicate is skipped silently.
		"INSERT OR IGNORE INTO i VALUES(1,'b')",
		// OR REPLACE: the conflicting row is deleted, then the new one written.
		"INSERT OR REPLACE INTO i VALUES(1,'c')",
		// Upsert DO NOTHING / DO UPDATE on the WR PK.
		"INSERT INTO i VALUES(1,'d') ON CONFLICT (w) DO NOTHING",
		"INSERT INTO i VALUES(1,'e') ON CONFLICT (w) DO UPDATE SET z=excluded.z",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	r := db.Query("SELECT * FROM i")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if got := flattenResult(r); got != "1 e" {
		t.Fatalf("rows [%s], want [1 e]", got)
	}
}

// TestWithoutRowidInsertDuplicatePKSeekDeepTable exercises the O(log n) PK-key
// seek over a many-leaf btree (interior index pages): the duplicate targets a
// key in the middle of a 2000-row WR table.
func TestWithoutRowidInsertDuplicatePKSeekDeepTable(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE d(a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID"); res.Error != nil {
		t.Fatalf("create error: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO d SELECT value, 'v' || value FROM generate_series(1, 2000)"); res.Error != nil {
		t.Fatalf("bulk insert error: %v", res.Error)
	}
	for _, probe := range []int{1, 1000, 2000, 1377} {
		res := db.Exec(fmt.Sprintf("INSERT INTO d VALUES(%d, 'dup')", probe))
		want := "UNIQUE constraint failed: d.a"
		if res.Error == nil || res.Error.Error() != want {
			t.Fatalf("probe %d: error %v, want [%s]", probe, res.Error, want)
		}
	}
	r := db.Query("SELECT count(*) FROM d")
	if got := flattenResult(r); got != "2000" {
		t.Fatalf("row count [%s], want [2000]", got)
	}
}

// TestRowidTableInsertDuplicatePKControl pins that the rowid-table path is
// unchanged by the WR fix.
func TestRowidTableInsertDuplicatePKControl(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE rr(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO rr VALUES(1,'x')",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	res := db.Exec("INSERT INTO rr VALUES(1,'y')")
	if res.Error == nil || res.Error.Error() != "UNIQUE constraint failed: rr.a" {
		t.Fatalf("error %v, want [UNIQUE constraint failed: rr.a]", res.Error)
	}
}
