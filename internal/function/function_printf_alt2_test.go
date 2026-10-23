package function

import (
	"math"
	"testing"
)

func TestSQLitePrintfAlt2FixedRounding(t *testing.T) {
	if got := renderSQLiteFloat('f', 1.25, 0, false, true, 26); got != "1.0" {
		t.Errorf("renderSQLiteFloat(%%!.0f, 1.25) = %q, want %q", got, "1.0")
	}
	cases := []struct {
		value float64
		want  string
	}{
		{math.Nextafter(100, 0), "99.9999999999999858"},
		{math.Nextafter(math.Nextafter(100, 0), 0), "99.9999999999999716"},
	}
	for _, tc := range cases {
		if got := renderSQLiteFloat('f', tc.value, 30, false, true, 26); got != tc.want {
			t.Errorf("renderSQLiteFloat(%%!.30f, %.17g) = %q, want %q", tc.value, got, tc.want)
		}
	}
}
