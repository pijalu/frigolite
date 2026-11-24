// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"strings"
	"testing"
)

// TestDistinctAggregateExplainEphemeral pins distinctagg's engine-visible
// contract: an aggregate DISTINCT set needs an ephemeral b-tree only when
// the scan cannot deliver the DISTINCT argument in order. The fixture
// checks this through `lsearch $prg OpenEphemeral` over EXPLAIN rows.
func TestDistinctAggregateExplainEphemeral(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.Exec(`
		CREATE TABLE t1(a,b,c);
		CREATE INDEX t1a ON t1(a);
		CREATE INDEX t1bc ON t1(b,c);
		CREATE TABLE t2(d,e,f);
		CREATE INDEX t2def ON t2(d,e,f);
	`)
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT count(DISTINCT a) FROM t1", false},              // t1a delivers a in order
		{"SELECT count(DISTINCT c) FROM t1", true},               // c is only the second t1bc column
		{"SELECT count(DISTINCT c) FROM t1 WHERE b=3", false},    // equality prefix b orders c
		{"SELECT count(DISTINCT a) FROM t1 GROUP BY b", true},    // grouping b breaks a's order
		{"SELECT count(DISTINCT f) FROM t2 GROUP BY d,e", false}, // t2def delivers d,e,f
		{"SELECT count(DISTINCT f) FROM t2 GROUP BY d", true},    // f is not ordered under group d
	}
	for _, tc := range cases {
		res := db.Query("EXPLAIN " + tc.sql)
		if res.Error != nil {
			t.Fatalf("EXPLAIN %q: %v", tc.sql, res.Error)
		}
		opcodes := explainColumn(res, "opcode")
		got := false
		for _, op := range opcodes {
			if op == "OpenEphemeral" {
				got = true
				break
			}
		}
		t.Logf("%q opcodes: %s", tc.sql, strings.Join(opcodes, " "))
		if got != tc.want {
			t.Errorf("OpenEphemeral for %q = %v, want %v", tc.sql, got, tc.want)
		}
	}
}
