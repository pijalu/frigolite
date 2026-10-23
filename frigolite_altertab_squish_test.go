package frigolite

import (
	"strings"
	"testing"
)

func TestAltertabSquishHarnessFunction(t *testing.T) {
	db := setupDBWithoutEcho(t)
	defer db.Close()
	harnessCollationFixtures["altertab"](db)
	res := db.Query(`SELECT squish('  alpha
	 beta   gamma  ')`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] != "alpha beta gamma" {
		t.Fatalf("squish result = %#v, want [[alpha beta gamma]]", res.Rows)
	}
	if got := strings.Join(strings.Fields("  alpha\n\t beta   gamma  "), " "); got != "alpha beta gamma" {
		t.Fatalf("reference Tcl squish semantics = %q", got)
	}
}
