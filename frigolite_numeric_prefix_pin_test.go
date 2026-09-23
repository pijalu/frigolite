package frigolite_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestBlobNumericPrefixClass pins sqlite3AtoF's TEXT/BLOB numeric-prefix
// classification for arithmetic (vacuum-into "no such column: Inf"):
//
//   - a mantissa needs at least one digit, before OR after the dot —
//     x'2E39' ('.9') + 1 is 1.9 REAL, but x'2E65' ('.e') + 1 is INTEGER 1
//     (no numeric prefix at all, coerced 0);
//   - a dot-only or dot-plus-non-digit blob never promotes the addition to
//     REAL and never yields +/-Inf (only a genuine exponent overflow like
//     '9e999'+0 is Inf REAL).
func TestBlobNumericPrefixClass(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, tc := range []struct{ expr, wantType, wantVal string }{
		{"x'2E'+1", "integer", "1"},       // "." -> no numeric prefix
		{"x'2EEF'+1", "integer", "1"},     // "." + junk byte
		{"x'2E65'+1", "integer", "1"},     // ".e" -> degenerate, no digits
		{"x'2E6539'+1", "integer", "1"},   // ".e9" -> degenerate, no mantissa
		{"x'2E39'+1", "real", "1.9"},      // ".9" -> valid REAL prefix
		{"x'2E39'+0", "real", "0.9"},      // ".9" + 0 keeps REAL
		{"x'39653939393939393939'+1", "real", "+Inf"}, // "9e999999999" overflow -> Inf REAL
		{"x'393939'+1", "integer", "1000"}, // "999" digits-only -> INTEGER
	} {
		r := db.Query("SELECT typeof(" + tc.expr + "), " + tc.expr)
		if r.Error != nil {
			t.Fatalf("%s: %v", tc.expr, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("%s: rows %v", tc.expr, r.Rows)
		}
		gotType := fmt.Sprint(r.Rows[0][0])
		gotVal := fmt.Sprint(r.Rows[0][1])
		if gotType != tc.wantType || gotVal != tc.wantVal {
			t.Errorf("%s: got (%s, %s) want (%s, %s)", tc.expr, gotType, gotVal, tc.wantType, tc.wantVal)
		}
	}
}
