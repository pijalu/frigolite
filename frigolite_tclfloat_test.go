package frigolite

import "testing"

func TestTclFloatFormat(t *testing.T) {
	cases := map[float64]string{
		1e15:                    "1000000000000000.0",
		1e16:                    "10000000000000000.0",
		1e17:                    "1e+17",
		9223372036854775808.0:   "9.223372036854776e+18",
		4503599627370495.0:      "4503599627370495.0",
		9007199254740992.0:      "9007199254740992.0",
		0.0001:                  "0.0001",
		1.5e-5:                  "1.5e-05",
		123456789012345678.0:    "1.2345678901234568e+17",
		-9223372036854775809e0:  "-9.223372036854776e+18",
		1.5:                     "1.5",
		-0.5:                    "-0.5",
	}
	for f, want := range cases {
		if got := formatTclDouble(f); got != want {
			t.Errorf("formatTclDouble(%v) = %q want %q", f, got, want)
		}
	}
}
