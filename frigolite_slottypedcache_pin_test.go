package frigolite_test

import (
	"fmt"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// TestPinSlotPathTypedCacheParity pins the substitution-time typed cache: a
// same-shape statement stream serves one live clone whose literal nodes are
// rewritten IN PLACE per statement (writeSlot), and the pre-cached parsed
// value must track every substitution exactly — the typed result (kind and
// content) of each statement must equal a fresh parse of its text across the
// int/float/edge/negative corpus, including repeated statements (the stale-
// cache hazard: a cache left from the previous substitution would surface
// here). Hex spellings never enter the slot stream (lossy extraction), but a
// hex statement sharing the shape must not be served from a cached neighbor.
func TestPinSlotPathTypedCacheParity(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	one := func(sql string) interface{} {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("%s: want 1x1, got %v", sql, r.Rows)
		}
		return r.Rows[0][0]
	}
	type step struct {
		sql  string
		want interface{}
	}
	steps := []step{
		// Integer family, boundaries, negatives (writeSlot int64 lane).
		{"SELECT 0", int64(0)},
		{"SELECT 0", int64(0)}, // exact repeat: cache reuse, same value
		{"SELECT 25000", int64(25000)},
		{"SELECT 25000", int64(25000)}, // repeat after another substitution
		{"SELECT -1", int64(-1)},
		{"SELECT -9223372036854775807", int64(-9223372036854775807)},
		{"SELECT 9223372036854775807", int64(9223372036854775807)},
		{"SELECT 007", int64(7)}, // leading zeros stay decimal
		{"SELECT 007", int64(7)},
		// REAL family (writeSlot float64 lane; NewFloatLit pre-cache).
		{"SELECT 8.0", float64(8.0)},
		{"SELECT 8.0", float64(8.0)},
		{"SELECT 2.5", float64(2.5)},
		{"SELECT -2.5", float64(-2.5)},
		{"SELECT typeof(8.0)", "real"},
		{"SELECT typeof(8)", "integer"},
		{"SELECT typeof(8.0)", "real"}, // kind flip-flop on one slot
		{"SELECT typeof(8)", "integer"},
		// 2^63 spellings: the folded integer is a distinct template (refuses
		// nothing here — it fresh-parses); the REAL spelling must stay REAL.
		{"SELECT typeof(-9223372036854775808)", "integer"},
		{"SELECT typeof(-9223372036854775808.0)", "real"},
		{"SELECT typeof(-9223372036854775808)", "integer"},
		// Hex never shares a slot stream: fresh-parses to 31 every time.
		{"SELECT 0x1F", int64(31)},
		{"SELECT 0x1F", int64(31)},
		{"SELECT 25000", int64(25000)}, // slot stream healthy after hex neighbors
	}
	for i, s := range steps {
		got := one(s.sql)
		switch want := s.want.(type) {
		case int64:
			g, ok := got.(int64)
			if !ok || g != want {
				t.Fatalf("step %d %q: got %#v (%T), want int64 %d", i, s.sql, got, got, want)
			}
		case float64:
			g, ok := got.(float64)
			if !ok || g != want {
				t.Fatalf("step %d %q: got %#v (%T), want float64 %v", i, s.sql, got, got, want)
			}
		case string:
			g, ok := got.(string)
			if !ok || g != want {
				t.Fatalf("step %d %q: got %#v (%T), want string %q", i, s.sql, got, got, want)
			}
		}
	}

	// Prepared-style flow: the SAME shape repeated through Exec with values
	// cycling in both directions (up then down) — any cache ordering bug
	// (serving the previous statement's value) shows as a half-off result.
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE m(k INTEGER PRIMARY KEY, v INTEGER)")
	for pass := 0; pass < 2; pass++ {
		for i := 1; i <= 25; i++ {
			v := int64(i)
			if pass == 1 {
				v = int64(26 - i)
			}
			must(fmt.Sprintf("INSERT OR REPLACE INTO m VALUES(%d, %d)", i, v))
			got := one(fmt.Sprintf("SELECT v FROM m WHERE k=%d", i))
			if got != v {
				t.Fatalf("pass %d k=%d: v=%v, want %d", pass, i, got, v)
			}
		}
	}
}
