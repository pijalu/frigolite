package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestUnsignedHexLiteralInt64Wrap pins SQLite's sqlite3DecOrHexToI64
// contract (hexlit-163/164, literal-1.5.*): an unsigned hex literal of up
// to 16 significant digits reinterprets its 64-bit pattern as int64.
func TestUnsignedHexLiteralInt64Wrap(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := func(sql, want string) {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		got := make([]string, len(r.Rows[0]))
		for i, v := range r.Rows[0] {
			got[i] = fmt.Sprint(v)
		}
		if len(r.Rows) != 1 || strings.Join(got, " ") != want {
			t.Fatalf("%s = %#v, want %s", sql, r.Rows, want)
		}
	}
	query(`SELECT 0x8000000000000000`, "-9223372036854775808")
	query(`SELECT 0XFFFFFFFFFFFFFFFF`, "-1")
	query(`SELECT typeof(0xFFFFFFFFFFFFFFFF), 0xFFFFFFFFFFFFFFFF`, "integer -1")
	query(`SELECT 0x0000000000000000FFFFFFFFFFFFFFFF`, "-1")
	query(`SELECT -0xFFFFFFFFFFFFFFFF`, "1")
	query(`SELECT -0x8000000000000001`, "9223372036854775807")

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec(`CREATE TABLE x1(a)`)
	exec(`INSERT INTO x1 VALUES(123)`)
	exec(`ALTER TABLE x1 ADD COLUMN b DEFAULT 0x0000000000000000FFFFFFFFFFFFFFFF`)
	query(`SELECT typeof(b), b FROM x1`, "integer -1")
	exec(`CREATE TABLE x2(a DEFAULT 0xFFFFFFFFFFFFFFFF)`)
	exec(`INSERT INTO x2 DEFAULT VALUES`)
	query(`SELECT typeof(a), a FROM x2`, "integer -1")

	for _, sql := range []string{
		`SELECT 0x10000000000000000`,
		`SELECT -0x8000000000000000`,
	} {
		r := db.Query(sql)
		if r.Error == nil || !strings.Contains(r.Error.Error(), "hex literal too big") {
			t.Fatalf("%s error = %v, want hex literal too big", sql, r.Error)
		}
	}
}
