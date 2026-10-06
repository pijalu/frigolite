package frigolite

import (
	"fmt"
	"testing"
)

// TestR9PointShapeMemoStableIdentity pins the shape-verdict memos' statement
// identity contract (the prevalidate memo today, the R9 shape memo family it
// guards): a memo keyed by *sql.SelectStmt may only serve statements whose
// AST pointer is template-STABLE. The slot-path live clone rewrites literal
// leaves of one persistent per-(template, depth) clone, so its pointer is
// stable; the COW scratch clone (multi-statement templates, unsupported slot
// shapes) recycles per-exec-depth structs across TEMPLATES, so a recycled
// address must never satisfy a memo keyed by pointer alone.
//
// The incident shape: a two-statement batch whose FIRST statement fails
// prevalidation retires its errored clone into the depth's scratch free
// list; the next single-literal batch of a DIFFERENT template reuses the
// struct and — without the stability gate — inherits the stale "no such
// column" verdict from the memo (the COW clone aliasing the memo key).
func TestR9PointShapeMemoStableIdentity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, c)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t(id,c) VALUES(1,'x')"); r.Error != nil {
		t.Fatalf("insert: %v", r.Error)
	}
	// Three templates with different abort positions shuffle the depth
	// scratch's LIFO free list: badA aborts after statement 1 (one retired
	// struct), badB aborts after statement 2 (two retired), so the good
	// batch's first statement lands on a struct whose memo verdict belongs
	// to the other template. Batch texts vary per iteration so the
	// exact-text statement cache always misses and every run takes the
	// template-hit COW scratch clone (the recycled-identity path).
	for i := 0; i < 8; i++ {
		goodBatch := fmt.Sprintf("SELECT c FROM t WHERE id=1; SELECT %d", i+100)
		if r := db.Exec(fmt.Sprintf("SELECT badx FROM t WHERE id=%d; SELECT %d", i, i)); r.Error == nil {
			t.Fatalf("run %d: badA batch unexpectedly succeeded", i)
		}
		if r := db.Exec(fmt.Sprintf("SELECT %d; SELECT bady FROM t WHERE id=%d", i, i)); r.Error == nil {
			t.Fatalf("run %d: badB batch unexpectedly succeeded", i)
		}
		r := db.Query(goodBatch)
		if r.Error != nil {
			t.Fatalf("run %d: good batch failed: %v (stale shape-memo verdict)", i, r.Error)
		}
		if len(r.Rows) != 2 || len(r.Rows[0]) != 1 || r.Rows[0][0] != "x" || r.Rows[1][0] != int64(i+100) {
			t.Fatalf("run %d: good batch rows = %v, want [[x] [%d]]", i, r.Rows, i+100)
		}
	}
}
