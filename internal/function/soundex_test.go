package function

import "testing"

func TestSoundexSQLiteVectors(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"euler", "E460"}, {"EULER", "E460"}, {"Euler", "E460"},
		{"ellery", "E460"}, {"gauss", "G200"}, {"ghosh", "G200"},
		{"hilbert", "H416"}, {"Heilbronn", "H416"}, {"knuth", "K530"},
		{"kant", "K530"}, {"Lloyd", "L300"}, {"LADD", "L300"},
		{"Lukasiewicz", "L222"}, {"Lissajous", "L222"}, {"A", "A000"},
		{"12345", "?000"}, {"bfpv", "B000"}, {"b-b", "B100"},
	}
	for _, tc := range cases {
		got, err := fnSOUNDEX([]interface{}{tc.input})
		if err != nil || got != tc.want {
			t.Errorf("fnSOUNDEX(%q) = %v, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	got, err := fnSOUNDEX([]interface{}{nil})
	if err != nil || got != "?000" {
		t.Errorf("fnSOUNDEX(NULL) = %v, %v; want ?000", got, err)
	}
}
