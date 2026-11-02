package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFPNumCompare pins the fpnum_compare port (src/test1.c:6168): the
// SQLite test suite's numeric expectation comparison used after the exact
// string compare fails.
func TestFPNumCompare(t *testing.T) {
	cases := []struct {
		got, want string
		wantMatch bool
	}{
		{"60.000000", "60.0", true},             // trailing fractional zeros (func7-pg-301)
		{"30.000000", "30.0", true},             // func7-pg-311
		{"42.0", "42.0", true},                  // exact
		{"1.4142136", "1.4142136", true},        // exact fraction
		{"60.0000001", "60.0", false},           // non-zero extra digit
		{"0.1", "1.0", false},                   // different leading digits
		{"abc", "abc", true},                    // non-numeric tokens match exactly
		{"abc", "abd", false},                   // non-numeric tokens must be identical
		{"1 2.500000 3", "1 2.5 3", false},      // mid-token zeros need 15 matched digits (test1.c: zB has a following token)
		{"1 2.500001 3", "1 2.5 3", false},      // any token mismatch fails
		{"1.2300000000000000001", "1.23", true}, // zeros beyond 15 digits
		{"2.0000000000000009", "2.0", true},     // once 15 digits matched, remaining digits are skipped
	}
	for _, tc := range cases {
		if got := fpnumCompare(tc.got, tc.want); got != tc.wantMatch {
			t.Errorf("fpnumCompare(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.wantMatch)
		}
	}
}

// TestHarnessFuncFixtureUDFs pins the test1.c/test_func.c fixture UDFs the
// corpus families rely on: the destructor count drains at statement
// boundaries, legacy_count reports the aggregated row count, test_isolation
// returns its second argument and test_error(NULL) carries an empty message.
func TestHarnessFuncFixtureUDFs(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registerTest1Functions(t, db)

	if res := db.Query("SELECT test_destructor('hello')"); res.Error != nil || len(res.Rows) != 1 {
		t.Fatalf("test_destructor: %v %v", res.Error, res.Rows)
	}
	// test_func.c attaches the destructor to the result value; the value dies
	// with the statement that produced it. The harness drains the pending
	// destructors at each step boundary — model that boundary here.
	drainTestDestructors()
	if res := db.Query("SELECT test_destructor_count()"); res.Error != nil {
		t.Fatalf("test_destructor_count: %v", res.Error)
	} else if len(res.Rows) != 1 || res.Rows[0][0] != int64(0) {
		t.Errorf("test_destructor_count after statement = %v, want 0", res.Rows)
	}
	if res := db.Query("SELECT legacy_count() FROM (SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3)"); res.Error != nil {
		t.Fatalf("legacy_count: %v", res.Error)
	} else if len(res.Rows) != 1 || res.Rows[0][0] != int64(3) {
		t.Errorf("legacy_count = %v, want [3]", res.Rows)
	}
	if res := db.Query("SELECT test_isolation('a','b')"); res.Error != nil {
		t.Fatalf("test_isolation: %v", res.Error)
	} else if len(res.Rows) != 1 || res.Rows[0][0] != "b" {
		t.Errorf("test_isolation = %v, want [b]", res.Rows)
	}
	if err := db.Exec("SELECT test_error(NULL)").Error; err == nil || err.Error() != "" {
		t.Errorf("test_error(NULL) error = %v, want empty message", err)
	}
	if err := db.Exec("SELECT test_error('whoa')").Error; err == nil || err.Error() != "whoa" {
		t.Errorf("test_error('whoa') error = %v, want whoa", err)
	}
	_ = os.Remove(filepath.Join(dir, "p.db"))
}
