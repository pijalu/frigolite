// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
)

// TestBestindexF141DistinctExplainRows pins bestindexF-1.4.1's engine-visible
// contract: a DISTINCT over two FROM terms whose ORDER BY is a DIFFERENT
// table still needs the ephemeral dedup b-tree, so EXPLAIN must carry an
// IdxInsert opcode. The fixture reads that opcode through `db eval "explain
// $sql"` to stamp its `uses_idxinsert` first result column.
func TestBestindexF141DistinctExplainRows(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body := `
	  switch -- $method {
	    xConnect {
	      return "CREATE TABLE t1(a, b, c)"
	    }
	    xBestIndex {
	      set hdl [lindex $args 0]
	      set orderby [$hdl orderby]
	      if {$orderby == "{column 0 desc 0} {column 1 desc 0}"
	       || $orderby == "{column 0 desc 0}"} {
	        return [list orderby 1]
	      }
	      return ""
	    }
	    xFilter {
	      return [list sql {
	        SELECT 1, 1, 'a', 555 UNION ALL
	        SELECT 2, 1, 'a', NULL UNION ALL
	        SELECT 3, 1, 'b', 'text' UNION ALL
	        SELECT 4, 2, 'a', 3.14 UNION ALL
	        SELECT 5, 2, 'b', 0
	      }]
	    }
	  }
	  return {}
	`
	step := TestStep{CallbackProcs: []callbackProc{{Name: "vtab_command", Args: []string{"method", "args"}, Body: body}}}
	runtime := newHarnessTclCallbackRuntime(step, db, make(map[string]string))
	db.RegisterVtabModule("tcl", vtab.NewTclCommandModuleWithRuntime(runtime))
	setup := db.Exec("CREATE VIRTUAL TABLE t1 USING tcl(vtab_command); CREATE TABLE t0(c0); INSERT INTO t0 VALUES(0),(1);")
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}

	query := "SELECT DISTINCT t0.c0 FROM t1, t0 ORDER BY t1.a;"
	eqp := db.Query("EXPLAIN QUERY PLAN " + query)
	if eqp.Error != nil {
		t.Fatal(eqp.Error)
	}
	t.Logf("EQP rows: %v", eqp.Rows)
	explain := db.Query("EXPLAIN " + query)
	if explain.Error != nil {
		t.Fatal(explain.Error)
	}
	opcodes := explainColumn(explain, "opcode")
	t.Logf("EXPLAIN opcodes: %v", strings.Join(opcodes, " "))
	for _, op := range opcodes {
		if op == "IdxInsert" {
			return
		}
	}
	t.Fatalf("EXPLAIN for %q has no IdxInsert opcode: %v", query, opcodes)
}

func explainColumn(res *Result, name string) []string {
	col := -1
	for i, c := range res.Columns {
		if c == name {
			col = i
			break
		}
	}
	if col < 0 {
		return nil
	}
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		if col < len(row) && row[col] != nil {
			out = append(out, strings.TrimSpace(stringValue(row[col])))
		}
	}
	return out
}

func stringValue(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
