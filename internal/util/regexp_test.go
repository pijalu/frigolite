package util

import "testing"

func TestCompileRegexpCStyleControlEscapes(t *testing.T) {
	for _, tc := range []struct{ pattern, input string }{
		{`^abc\adef$`, "abc\adef"},
		{`^abc\fdef$`, "abc\fdef"},
		{`^abc\vdef$`, "abc\vdef"},
	} {
		re, err := CompileRegexp(tc.pattern)
		if err != nil {
			t.Errorf("CompileRegexp(%q): %v", tc.pattern, err)
			continue
		}
		if !re.MatchString(tc.input) {
			t.Errorf("%q does not match %q", tc.pattern, tc.input)
		}
	}
}
