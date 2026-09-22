package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestNumericCastPin pins NUMERIC conversion contracts (oracle: sqlite3 3.54;
// TCL refs cast-2.x, cast-3.4, cast-4.1, expr-8.41, in4-4.17, check-4.8):
//
// 1. CAST to INTEGER/REAL/NUMERIC converts a BLOB to TEXT first
// (sqlite3VdbeMemCast): CAST(x'39323233333732303336383534373734383030' AS REAL) is 1.0.
// 2. CAST to NUMERIC parses the longest numeric PREFIX of the text
// (CAST('123abc') is 123; CAST('123.5abc') is 123.5; CAST('-') is 0) and
// folds integral reals to INTEGER (CAST('123.0' AS NUMERIC) is integer 123).
// 3. The out-of-range literal -9223372036854775808 (with any leading zeros)
// is INTEGER MinInt64: typeof(-00000009223372036854775808) is integer.
// 4. IN applies the LEFT operand's column affinity to each list item:
// a TEXT '1.0' IN (b NUMERIC 1) matches nothing.
// 5. integrity_check skips CHECK validation while
// PRAGMA ignore_check_constraints is ON.
func TestNumericCastPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rowsText := func(q string) string {
		t.Helper()
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s => ERR %v", q, r.Error)
		}
		var sb strings.Builder
		for _, row := range r.Rows {
			for j, v := range row {
				if j > 0 {
					sb.WriteByte(' ')
				}
				if v == nil {
					sb.WriteString("{}")
					continue
				}
				sb.WriteString(fmt.Sprintf("%v", v))
			}
			sb.WriteByte('|')
		}
		return sb.String()
	}
	for _, tc := range []struct{ q, want string }{
		{"SELECT CAST('123abc' AS numeric), CAST('123.5abc' AS numeric)", "123 123.5|"},
		{"SELECT CAST('-' AS NUMERIC), CAST('+' AS NUMERIC)", "0 0|"},
		{"SELECT typeof(CAST('123.0' AS numeric)), CAST('123.0' AS numeric)", "integer 123|"},
		{"SELECT CAST(x'31' AS REAL)", "1|"},
		{"SELECT CAST(x'39323233333732303336383534373734383030' AS integer)", "9223372036854774800|"},
		{"SELECT typeof(-00000009223372036854775808), -00000009223372036854775808", "integer -9223372036854775808|"},
		{"SELECT typeof(9223372036854775808)", "real|"},
	} {
		if got := rowsText(tc.q); got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}

	// in4-4.17: IN applies the LHS column affinity to the list items.
	db.Exec("CREATE TABLE t4b(a TEXT, b NUMERIC, c)")
	db.Exec("INSERT INTO t4b VALUES('1.0',1,4)")
	if got := rowsText("SELECT c FROM t4b WHERE a IN (b)"); got != "" {
		t.Errorf("IN affinity => got [%s] want []", got)
	}
	// The binary comparison applies NUMERIC affinity to both sides instead.
	if got := rowsText("SELECT c FROM t4b WHERE a = b"); got != "4|" {
		t.Errorf("binary affinity => got [%s] want [4|]", got)
	}

	// check-4.8/4.8.1: integrity_check skips CHECK validation under
	// ignore_check_constraints=ON, and reports it once OFF again.
	db.Exec("CREATE TABLE t4(x, y, CHECK (x+y==11 OR x*y==12 OR x/y BETWEEN 5 AND 8 OR -x==y+10))")
	db.Exec("PRAGMA ignore_check_constraints=ON")
	db.Exec("INSERT INTO t4 VALUES(0,1)")
	if got := rowsText("PRAGMA integrity_check"); got != "ok|" {
		t.Errorf("integrity with ignore ON => got [%s] want [ok|]", got)
	}
	db.Exec("PRAGMA ignore_check_constraints=OFF")
	if got := rowsText("PRAGMA integrity_check"); got != "CHECK constraint failed in t4|" {
		t.Errorf("integrity with ignore OFF => got [%s] want [CHECK constraint failed in t4|]", got)
	}
}
