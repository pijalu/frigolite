// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"strings"
	"testing"
)

// TestTrigger7UpdateOfExplainMarkers pins trigger7-2.x's engine-visible
// contract: EXPLAIN UPDATE includes each firing trigger's sub-program, and
// UPDATE OF only fires the triggers whose column list intersects the SET
// clause. The fixture finds the sub-programs via Tcl string-match on the
// SELECT literal carried in an opcode's p4 column.
func TestTrigger7UpdateOfExplainMarkers(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.Exec(`
		CREATE TABLE t1(x,y,z);
		CREATE TRIGGER r1 AFTER UPDATE OF x ON t1 BEGIN
		  SELECT '___update_t1.x___';
		END;
		CREATE TRIGGER r2 AFTER UPDATE OF y ON t1 BEGIN
		  SELECT '___update_t1.y___';
		END;
	`)
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}
	cases := []struct {
		stmt  string
		wantX bool
		wantY bool
	}{
		{"UPDATE t1 SET x=5", true, false},
		{"UPDATE t1 SET y=5", false, true},
		{"UPDATE t1 SET rowid=5", false, false},
	}
	for _, tc := range cases {
		res := db.Query("EXPLAIN " + tc.stmt)
		if res.Error != nil {
			t.Fatalf("EXPLAIN %q: %v", tc.stmt, res.Error)
		}
		flat := explainFlatten(res)
		if got := strings.Contains(flat, "___update_t1.x___"); got != tc.wantX {
			t.Errorf("%q x-marker = %v, want %v; opcodes: %s", tc.stmt, got, tc.wantX, strings.Join(explainColumn(res, "opcode"), " "))
		}
		if got := strings.Contains(flat, "___update_t1.y___"); got != tc.wantY {
			t.Errorf("%q y-marker = %v, want %v; opcodes: %s", tc.stmt, got, tc.wantY, strings.Join(explainColumn(res, "opcode"), " "))
		}
	}
}

func explainFlatten(res *Result) string {
	parts := make([]string, 0, len(res.Rows)*len(res.Columns))
	for _, row := range res.Rows {
		for _, cell := range row {
			parts = append(parts, " "+strings.TrimSpace(formatSQLiteValue("", cell))+" ")
		}
	}
	return strings.Join(parts, "")
}
