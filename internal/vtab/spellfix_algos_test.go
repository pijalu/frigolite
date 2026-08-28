package vtab

import "testing"

// TestSpellfixEditDist1 checks the default-cost edit distance model against
// values implied by spellfix.test's MATCH results (oracle: spellfix.test
// 4.1.1 distances come through the full query; here we verify the matrix).
func TestSpellfixEditDist1(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"kosher", "kosher", 0},
		{"k", "kosher", 80}, // inserts o(25) s(25) h(1/4=0) e(20/4=5 near r) r(25)
		{"abc", "abd", 75},  // c->d: consonants in different classes
	}
	for _, c := range cases {
		if got := editdist1(c.a, c.b, nil); got != c.want {
			t.Errorf("editdist1(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// Prefix search: extra tail characters of B get minimal cost.
	var ml int
	if got := editdist1("kos*", "kosherness", &ml); got != 0 {
		t.Errorf("editdist1 prefix of itself = %d, want 0 (matchlen %d)", got, ml)
	}
	// Non-ASCII input is an error (-2).
	if got := editdist1("héllo", "hello", nil); got != -2 {
		t.Errorf("non-ASCII editdist1 = %d, want -2", got)
	}
}

// TestSpellfixScriptCode pins spellfix1_scriptcode against spellfix3.test's
// oracle expectations.
func TestSpellfixScriptCode(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"And God said, “Let there be light”", 215},
		{"Бог сказал: \"Да будет свет\"", 220},
		{"και ειπεν ο θεος γενηθητω φως και εγενετο φως", 200},
		{"וַיֹּ֥אמֶר אֱלֹהִ֖ים יְהִ֣י א֑וֹר וַֽיְהִי־אֽוֹר׃", 125},
		{"فِي ذَلِكَ الوَقتِ، قالَ اللهُ: لِيَكُنْ نُورٌ. فَصَارَ نُورٌ.", 160},
		{"+3.14159", 215},
		{"And God said: \"Да будет свет\"", 998},
		{"+3.14159 light", 215},
		{"+3.14159 свет", 220},
		{"וַיֹּ֥אמֶר +3.14159", 125},
	}
	for _, c := range cases {
		if got := spellfix1ScriptCode(c.in); got != c.want {
			t.Errorf("scriptcode(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSpellfixTransliterate checks a few translit-table mappings and the '?'
// fallback (oracle: spellfix1_translit in spellfix.c docs).
func TestSpellfixTransliterate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"Über", "Ueber"}, // 0x00DC -> "Ue"
		{"Æther", "AEther"},
	}
	for _, c := range cases {
		if got := transliterate(c.in); got != c.want {
			t.Errorf("transliterate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// An unmapped non-ASCII character becomes '?'.
	if got := transliterate("☃"); got != "?" {
		t.Errorf("transliterate unmapped = %q, want ?", got)
	}
}

// TestSpellfixPhoneticHash checks k2 invariants: deterministic, uppercase
// class symbols, no vowels beside L/R (oracle: spellfix1_phonehash docs).
func TestSpellfixPhoneticHash(t *testing.T) {
	h1 := phoneticHash("kosherness")
	h2 := phoneticHash("kosherness")
	if h1 != h2 {
		t.Fatalf("phoneticHash not deterministic")
	}
	if h1 == "" {
		t.Fatalf("phoneticHash empty")
	}
	// The hash must contain only the 13 class symbols.
	for _, ch := range h1 {
		if !stringContains(spellfixClassName, byte(ch)) {
			t.Errorf("phoneticHash %q has illegal symbol %q", h1, ch)
		}
	}
}

func stringContains(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}
