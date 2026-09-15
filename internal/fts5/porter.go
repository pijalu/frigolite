package fts5

// This file ports the fts5 porter stemmer (ext/fts5/fts5_tokenize.c:
// fts5PorterCb, fts5PorterStep1A/1B/1B2/2/3/4 and the fts5Porter* condition
// helpers). The fts5 porter variant deliberately differs from the classic
// Porter algorithm that FTS3/4 uses (internal/fts PorterStem): step 1a maps
// "ies"→"ie" and leaves a trailing "ss" untouched, step 1b's "eed" branch is
// unreachable for exactly-"eed" tokens (so "eed" strips to "e" via the "ed"
// rule), and the generated step tables carry the C guards verbatim.

// porterMaxToken is FTS5_PORTER_MAX_TOKEN: longer tokens pass through
// unstemmed.
const porterMaxToken = 64

// porterIsVowel ports fts5PorterIsVowel: a/e/i/o/u, with 'y' counting as a
// vowel only when bYIsVowel is set.
func porterIsVowel(c byte, bYIsVowel bool) bool {
	switch c {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	case 'y':
		return bYIsVowel
	}
	return false
}

// porterGobbleVC ports fts5PorterGobbleVC: scan [CONSONANT]* VOWEL then
// VOWEL* CONSONANT, returning the offset just past the consonant (the
// [VC] boundary count used to compute the porter measure m), or 0 when the
// stem has no such pattern. bPrevCons seeds the consonant state.
func porterGobbleVC(z []byte, bPrevCons bool) int {
	bCons := bPrevCons
	i := 0
	// Scan for a vowel.
	for ; i < len(z); i++ {
		bCons = !porterIsVowel(z[i], bCons)
		if !bCons {
			break
		}
	}
	// Scan for a consonant.
	for i++; i < len(z); i++ {
		bCons = !porterIsVowel(z[i], bCons)
		if bCons {
			return i + 1
		}
	}
	return 0
}

// porterMGt0 ports fts5Porter_MGt0 (condition m > 0).
func porterMGt0(stem []byte) bool {
	return porterGobbleVC(stem, false) != 0
}

// porterMGt1 ports fts5Porter_MGt1 (condition m > 1).
func porterMGt1(stem []byte) bool {
	n := porterGobbleVC(stem, false)
	return n != 0 && porterGobbleVC(stem[n:], true) != 0
}

// porterMEq1 ports fts5Porter_MEq1 (condition m = 1).
func porterMEq1(stem []byte) bool {
	n := porterGobbleVC(stem, false)
	return n != 0 && porterGobbleVC(stem[n:], true) == 0
}

// porterOstar ports fts5Porter_Ostar (condition *o: stem ends cvc where the
// last c is not w, x or y).
func porterOstar(stem []byte) bool {
	last := stem[len(stem)-1]
	if last == 'w' || last == 'x' || last == 'y' {
		return false
	}
	mask := 0
	bCons := false
	for i := 0; i < len(stem); i++ {
		bCons = !porterIsVowel(stem[i], bCons)
		if bCons {
			mask = (mask << 1) + 1
		} else {
			mask = mask << 1
		}
	}
	return mask&0x0007 == 0x0005
}

// porterMGt1ST ports fts5Porter_MGt1_and_S_or_T (m > 1 and (*S or *T)).
func porterMGt1ST(stem []byte) bool {
	last := stem[len(stem)-1]
	return (last == 's' || last == 't') && porterMGt1(stem)
}

// porterVowel ports fts5Porter_Vowel (condition *v*); 'y' counts as a vowel
// from the second character on, exactly as in C.
func porterVowel(stem []byte) bool {
	for i := 0; i < len(stem); i++ {
		if porterIsVowel(stem[i], i > 0) {
			return true
		}
	}
	return false
}

// porterRule is one generated-step rule (PorterRule): strip suffix, require
// cond(stem), append output ("" deletes). ret mirrors the C steps whose
// return value feeds fts5PorterCb's step-1b cleanup.
type porterRule struct {
	suffix string
	output string
	cond   func(stem []byte) bool
	ret    bool
}

// porterStepRules groups a step's rules by their suffix's second-to-last
// byte — the switch(aBuf[nBuf-2]) of the generated C code.
var (
	porterStep1BRules = map[byte][]porterRule{
		'e': {
			{suffix: "eed", output: "ee", cond: porterMGt0},
			{suffix: "ed", output: "", cond: porterVowel, ret: true},
		},
		'n': {
			{suffix: "ing", output: "", cond: porterVowel, ret: true},
		},
	}
	porterStep1B2Rules = map[byte][]porterRule{
		'a': {{suffix: "at", output: "ate", ret: true}},
		'b': {{suffix: "bl", output: "ble", ret: true}},
		'i': {{suffix: "iz", output: "ize", ret: true}},
	}
	porterStep2Rules = map[byte][]porterRule{
		'a': {
			{suffix: "ational", output: "ate", cond: porterMGt0},
			{suffix: "tional", output: "tion", cond: porterMGt0},
		},
		'c': {
			{suffix: "enci", output: "ence", cond: porterMGt0},
			{suffix: "anci", output: "ance", cond: porterMGt0},
		},
		'e': {
			{suffix: "izer", output: "ize", cond: porterMGt0},
		},
		'g': {
			{suffix: "logi", output: "log", cond: porterMGt0},
		},
		'l': {
			{suffix: "bli", output: "ble", cond: porterMGt0},
			{suffix: "alli", output: "al", cond: porterMGt0},
			{suffix: "entli", output: "ent", cond: porterMGt0},
			{suffix: "eli", output: "e", cond: porterMGt0},
			{suffix: "ousli", output: "ous", cond: porterMGt0},
		},
		'o': {
			{suffix: "ization", output: "ize", cond: porterMGt0},
			{suffix: "ation", output: "ate", cond: porterMGt0},
			{suffix: "ator", output: "ate", cond: porterMGt0},
		},
		's': {
			{suffix: "alism", output: "al", cond: porterMGt0},
			{suffix: "iveness", output: "ive", cond: porterMGt0},
			{suffix: "fulness", output: "ful", cond: porterMGt0},
			{suffix: "ousness", output: "ous", cond: porterMGt0},
		},
		't': {
			{suffix: "aliti", output: "al", cond: porterMGt0},
			{suffix: "iviti", output: "ive", cond: porterMGt0},
			{suffix: "biliti", output: "ble", cond: porterMGt0},
		},
	}
	porterStep3Rules = map[byte][]porterRule{
		'a': {
			{suffix: "ical", output: "ic", cond: porterMGt0},
		},
		's': {
			{suffix: "ness", output: "", cond: porterMGt0},
		},
		't': {
			{suffix: "icate", output: "ic", cond: porterMGt0},
			{suffix: "iciti", output: "ic", cond: porterMGt0},
		},
		'u': {
			{suffix: "ful", output: "", cond: porterMGt0},
		},
		'v': {
			{suffix: "ative", output: "", cond: porterMGt0},
		},
		'z': {
			{suffix: "alize", output: "al", cond: porterMGt0},
		},
	}
	porterStep4Rules = map[byte][]porterRule{
		'a': {
			{suffix: "al", output: "", cond: porterMGt1},
		},
		'c': {
			{suffix: "ance", output: "", cond: porterMGt1},
			{suffix: "ence", output: "", cond: porterMGt1},
		},
		'e': {
			{suffix: "er", output: "", cond: porterMGt1},
		},
		'i': {
			{suffix: "ic", output: "", cond: porterMGt1},
		},
		'l': {
			{suffix: "able", output: "", cond: porterMGt1},
			{suffix: "ible", output: "", cond: porterMGt1},
		},
		'n': {
			{suffix: "ant", output: "", cond: porterMGt1},
			{suffix: "ement", output: "", cond: porterMGt1},
			{suffix: "ment", output: "", cond: porterMGt1},
			{suffix: "ent", output: "", cond: porterMGt1},
		},
		'o': {
			{suffix: "ion", output: "", cond: porterMGt1ST},
			{suffix: "ou", output: "", cond: porterMGt1},
		},
		's': {
			{suffix: "ism", output: "", cond: porterMGt1},
		},
		't': {
			{suffix: "ate", output: "", cond: porterMGt1},
			{suffix: "iti", output: "", cond: porterMGt1},
		},
		'u': {
			{suffix: "ous", output: "", cond: porterMGt1},
		},
		'v': {
			{suffix: "ive", output: "", cond: porterMGt1},
		},
		'z': {
			{suffix: "ize", output: "", cond: porterMGt1},
		},
	}
)

// porterStep1A ports fts5PorterStep1A:
//
//	SSES -> SS
//	IES  -> IE
//	SS   -> SS
//	S    -> (deleted, but not after s)
func porterStep1A(b []byte) []byte {
	n := len(b)
	if n < 2 || b[n-1] != 's' {
		return b
	}
	if b[n-2] == 'e' {
		if (n > 4 && b[n-4] == 's' && b[n-3] == 's') || (n > 3 && b[n-3] == 'i') {
			return b[:n-2]
		}
		return b[:n-1]
	} else if b[n-2] != 's' {
		return b[:n-1]
	}
	return b
}

// porterApplyRules applies one generated step: the rules for the buffer's
// second-to-last byte are tried in order and the FIRST suffix match wins
// (the C else-if chain — a matching suffix whose condition fails applies
// nothing). ret reports whether the applied rule carried the step's return
// flag.
func porterApplyRules(b []byte, table map[byte][]porterRule) ([]byte, bool) {
	ret := false
	if len(b) < 2 {
		return b, ret
	}
	for _, r := range table[b[len(b)-2]] {
		if len(b) > len(r.suffix) && byteSuffix(b, r.suffix) {
			stem := b[:len(b)-len(r.suffix)]
			if r.cond == nil || r.cond(stem) {
				b = append(stem, r.output...)
				ret = r.ret
			}
			break
		}
	}
	return b, ret
}

// byteSuffix reports whether b ends with the ASCII suffix s.
func byteSuffix(b []byte, s string) bool {
	return len(b) >= len(s) && string(b[len(b)-len(s):]) == s
}

// porterStem stems one token exactly as fts5PorterCb does: tokens shorter
// than 3 or longer than 64 bytes pass through untouched.
func porterStem(token string) string {
	if len(token) > porterMaxToken || len(token) < 3 {
		return token
	}
	// Spare capacity mirrors C's aBuf[FTS5_PORTER_MAX_TOKEN + 64]: rules
	// append in place past the current length.
	buf := make([]byte, len(token), len(token)+8)
	copy(buf, token)
	b := porterStep1(buf)
	b = porterStep1C(b)
	// Steps 2 through 4.
	b, _ = porterApplyRules(b, porterStep2Rules)
	b, _ = porterApplyRules(b, porterStep3Rules)
	b, _ = porterApplyRules(b, porterStep4Rules)
	return string(porterStep5(b))
}

// porterStep1 ports fts5PorterCb's step-1 block: step 1a, the step-1b table
// and — when 1b stripped a suffix but 1b2 did not apply — the double
// consonant / *o cleanup.
func porterStep1(b []byte) []byte {
	b = porterStep1A(b)
	b, ret := porterApplyRules(b, porterStep1BRules)
	if !ret {
		return b
	}
	applied := false
	b, applied = porterApplyRules(b, porterStep1B2Rules)
	if applied || len(b) < 2 {
		return b
	}
	c := b[len(b)-1]
	if !porterIsVowel(c, false) && c != 'l' && c != 's' && c != 'z' && c == b[len(b)-2] {
		return b[:len(b)-1]
	}
	if porterMEq1(b) && porterOstar(b) {
		return append(b, 'e')
	}
	return b
}

// porterStep1C ports fts5PorterCb's step-1c: a 'y' preceded by a vowel
// becomes 'i'.
func porterStep1C(b []byte) []byte {
	if len(b) >= 2 && b[len(b)-1] == 'y' && porterVowel(b[:len(b)-1]) {
		b[len(b)-1] = 'i'
	}
	return b
}

// porterStep5 ports fts5PorterCb's step 5a (drop a final 'e' per the
// measure/*o conditions) and 5b (drop one 'l' of a final "-ll" when m > 1).
func porterStep5(b []byte) []byte {
	// Step 5a.
	if len(b) >= 2 && b[len(b)-1] == 'e' {
		stem := b[:len(b)-1]
		if porterMGt1(stem) || (porterMEq1(stem) && !porterOstar(stem)) {
			b = stem
		}
	}
	// Step 5b.
	if len(b) >= 3 && b[len(b)-1] == 'l' && b[len(b)-2] == 'l' && porterMGt1(b[:len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}
