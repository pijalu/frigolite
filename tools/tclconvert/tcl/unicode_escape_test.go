package tcl

import "testing"

func TestDecodeBackslashEscapeShortUnicode(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		nextPos int
	}{
		{`\uE01`, "ก", 5},
		{`\uE02tail`, "ข", 5},
		{`\u0E01ABCD`, "ก", 6},
		{`\uE01\uE02`, "ก", 5},
	}
	for _, c := range cases {
		got, next := decodeBackslashEscape(c.in, 0)
		if got != c.want || next != c.nextPos {
			t.Errorf("decodeBackslashEscape(%q) = %q, %d; want %q, %d", c.in, got, next, c.want, c.nextPos)
		}
	}
}
