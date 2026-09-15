package frigolite_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteView3RefLimitPin pins view3.test 1.1 (ticket d58ccbb3f1b): the
// name-resolution phase counts references to each view's Table object and
// aborts with `too many references to "%s": max 65535` once a view is
// expanded more than 65535 times (select.c selectExpander, nTabRef guard).
func TestSQLiteView3RefLimitPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var b strings.Builder
	b.WriteString("CREATE TABLE t1(x);\nINSERT INTO t1 VALUES(5);\n")
	b.WriteString("CREATE VIEW v1 AS SELECT x*2 FROM t1;\n")
	// Build v2, v4, ..., v32768: each doubles its predecessor.
	names := []string{"v1"}
	for p := 2; p <= 32768; p *= 2 {
		names = append(names, viewNameFor(p))
	}
	for i := 1; i < len(names); i++ {
		b.WriteString("CREATE VIEW " + names[i] + " AS SELECT * FROM " + names[i-1] +
			" UNION SELECT * FROM " + names[i-1] + ";\n")
	}
	b.WriteString("SELECT * FROM " + names[len(names)-1] + " UNION SELECT * FROM " + names[len(names)-1] + ";")
	r := db.Query(b.String())
	if r.Error == nil || !strings.Contains(r.Error.Error(), `too many references to "v1": max 65535`) {
		t.Fatalf("view ref limit: got %v, want `too many references to \"v1\": max 65535`", r.Error)
	}
}

func viewNameFor(n int) string {
	return "v" + strconv.Itoa(n)
}
