package frigolite

import (
	"testing"
)

// Pin for with2-9.2: generate_series with a NULL hidden-column binding
// renders NO ROWS — series.c xFilter skips the series when any constraint
// argument is NULL (ticket fac496b61722daf2) instead of erroring. The giant
// with2-9.2 chart query binds stop = 1+1.0*(maxy-miny)/stepy whose value is
// NULL, so the radygrid arm is empty and the query returns zero rows
// (oracle: /usr/bin/sqlite3 3.51.0, empty result, exit 0).
func TestWith2GenerateSeriesNullStopNoRows(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query("SELECT value, start, stop, step FROM generate_series(1) WHERE stop = (SELECT NULL)")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("got %d rows, want 0 (NULL stop renders no rows)", len(r.Rows))
	}
	// A non-NULL stop still works normally.
	r = db.Query("SELECT value FROM generate_series(1) WHERE stop = 3")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if got := flattenResult(r); got != "1 2 3" {
		t.Fatalf("got [%s], want [1 2 3]", got)
	}
}
