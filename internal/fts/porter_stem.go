package fts

// This file ports SQLite's FTS3/4 Porter stemmer verbatim from
// ext/fts3/fts3_porter.c: porter_stemmer (the classic Porter algorithm run
// on a REVERSED word), copy_stemmer (the fallback for tokens that are too
// short, too long, or contain non-letter bytes) and the condition helpers
// (isVowel/isConsonant with the 'y' rule, m_gt_0/m_eq_1/m_gt_1, hasVowel,
// doubleConsonant, star_oh, stem). Each Porter step is its own function
// (porterStep1a..porterStep5) operating on (buf, head) — buf is the
// case-folded word in reverse order with five NUL bytes of padding, head is
// the current position of the reversed word's start.

// porterStem stems one token exactly as fts3_porter.c porter_stemmer:
// tokens shorter than 3 or longer than 20 bytes, or containing any byte
// outside [a-zA-Z], fall back to copyStemmer (which truncates long tokens
// to their first+last 10 bytes, or first+last 3 bytes when they contain
// digits — fts3ad 1.3-1.6: 'abcdefghijklmnopqrstuvwyxz' stems to
// 'abcdefghijqrstuvwyxz' and '123456789' to '123789').
func porterStem(word string) string {
	if len(word) < 3 || len(word) >= 21 {
		return copyStemmer(word)
	}
	if !porterIsStemmable(word) {
		return copyStemmer(word)
	}
	buf := porterReversedBuf(word)
	head := porterStep1a(buf, 0)
	head = porterStep1b(buf, head)
	// Step 1c.
	if buf[head] == 'y' && porterHasVowel(buf, head+1) {
		buf[head] = 'i'
	}
	head = porterStep2(buf, head)
	head = porterStep3(buf, head)
	head = porterStep4(buf, head)
	head = porterStep5(buf, head)
	return porterFlipBack(buf, head)
}

// porterIsStemmable reports whether the token qualifies for the Porter rules:
// every byte must be an ASCII letter (otherwise copyStemmer runs).
func porterIsStemmable(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// porterReversedBuf builds the case-folded, reversed working buffer with the
// C's five NUL bytes of padding: the Porter rules run against the word head
// in reversed space (buf[head] is the word's LAST letter), and the
// condition helpers read the terminator past the end.
func porterReversedBuf(word string) []byte {
	n := len(word)
	buf := make([]byte, n+5)
	for i := 0; i < n; i++ {
		c := word[n-1-i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return buf
}

// porterFlipBack flips the reversed stem back into forward order.
func porterFlipBack(buf []byte, head int) string {
	out := make([]byte, 0, len(buf)-head)
	for i := len(buf) - 1; i >= head; i-- {
		if buf[i] == 0 {
			continue
		}
		out = append(out, buf[i])
	}
	return string(out)
}

// porterStep1a ports Step 1a (the sses/ies/ss/s endings).
func porterStep1a(buf []byte, head int) int {
	if buf[head] == 's' {
		if !stemRev(buf, &head, "sess", "ss", nil) &&
			!stemRev(buf, &head, "sei", "i", nil) &&
			!stemRev(buf, &head, "ss", "ss", nil) {
			head++
		}
	}
	return head
}

// porterStep1b ports Step 1b (eed/ed/ing and the post-substitution cleanup).
func porterStep1b(buf []byte, head int) int {
	head2 := head
	if stemRev(buf, &head, "dee", "ee", porterMGt0) {
		// Do nothing. The work was all in the test.
	} else if (stemRev(buf, &head, "gni", "", porterHasVowel) || stemRev(buf, &head, "de", "", porterHasVowel)) && head != head2 {
		head = porterStep1bCleanup(buf, head)
	}
	return head
}

// porterStep1bCleanup ports Step 1b's post-substitution rules: -at/-bl/-iz
// → -ate/-ble/-ize, the doubled-consonant trim (except l/s/z) and the
// m==1 CVC -e restoration.
func porterStep1bCleanup(buf []byte, head int) int {
	if stemRev(buf, &head, "ta", "ate", nil) ||
		stemRev(buf, &head, "lb", "ble", nil) ||
		stemRev(buf, &head, "zi", "ize", nil) {
		// Do nothing. The work was all in the test.
	} else if porterDoubleConsonant(buf, head) && buf[head] != 'l' && buf[head] != 's' && buf[head] != 'z' {
		head++
	} else if porterMEq1(buf, head) && porterStarOh(buf, head) {
		head--
		buf[head] = 'e'
	}
	return head
}

// porterStep2 ports Step 2 (double-suffix reductions, keyed on the second
// reversed byte) and delegates the l/o/s/t groups to per-group helpers.
func porterStep2(buf []byte, head int) int {
	if head+1 >= len(buf) {
		return head
	}
	switch buf[head+1] {
	case 'a':
		if !stemRev(buf, &head, "lanoita", "ate", porterMGt0) {
			stemRev(buf, &head, "lanoit", "tion", porterMGt0)
		}
	case 'c':
		if !stemRev(buf, &head, "icne", "ence", porterMGt0) {
			stemRev(buf, &head, "icna", "ance", porterMGt0)
		}
	case 'e':
		stemRev(buf, &head, "rezi", "ize", porterMGt0)
	case 'g':
		stemRev(buf, &head, "igol", "log", porterMGt0)
	default:
		head = porterStep2LO(buf, head)
	}
	return head
}

// porterStep2LO carries Step 2's 'l' and 'o' ending groups.
func porterStep2LO(buf []byte, head int) int {
	switch buf[head+1] {
	case 'l':
		if !stemRev(buf, &head, "ilb", "ble", porterMGt0) &&
			!stemRev(buf, &head, "illa", "al", porterMGt0) &&
			!stemRev(buf, &head, "iltne", "ent", porterMGt0) &&
			!stemRev(buf, &head, "ile", "e", porterMGt0) {
			stemRev(buf, &head, "ilsuo", "ous", porterMGt0)
		}
	case 'o':
		if !stemRev(buf, &head, "noitazi", "ize", porterMGt0) &&
			!stemRev(buf, &head, "noita", "ate", porterMGt0) {
			stemRev(buf, &head, "rota", "ate", porterMGt0)
		}
	default:
		head = porterStep2S(buf, head)
	}
	return head
}

// porterStep2S carries Step 2's 's' and 't' ending groups.
func porterStep2S(buf []byte, head int) int {
	switch buf[head+1] {
	case 's':
		if !stemRev(buf, &head, "msila", "al", porterMGt0) &&
			!stemRev(buf, &head, "ssenevi", "ive", porterMGt0) &&
			!stemRev(buf, &head, "ssenluf", "ful", porterMGt0) {
			stemRev(buf, &head, "ssensuo", "ous", porterMGt0)
		}
	case 't':
		if !stemRev(buf, &head, "itila", "al", porterMGt0) &&
			!stemRev(buf, &head, "itivi", "ive", porterMGt0) {
			stemRev(buf, &head, "itilib", "ble", porterMGt0)
		}
	}
	return head
}

// porterStep3 ports Step 3 (the -ic/-al/-ful/-ness families, keyed on the
// first reversed byte).
func porterStep3(buf []byte, head int) int {
	switch buf[head] {
	case 'e':
		if !stemRev(buf, &head, "etaci", "ic", porterMGt0) &&
			!stemRev(buf, &head, "evita", "", porterMGt0) {
			stemRev(buf, &head, "ezila", "al", porterMGt0)
		}
	case 'i':
		stemRev(buf, &head, "itici", "ic", porterMGt0)
	case 'l':
		if !stemRev(buf, &head, "laci", "ic", porterMGt0) {
			stemRev(buf, &head, "luf", "", porterMGt0)
		}
	case 's':
		stemRev(buf, &head, "ssen", "", porterMGt0)
	}
	return head
}

// porterStep4 ports Step 4 (the -al/-ence/-er/-ic/-able/-ion family, keyed
// on the second reversed byte) and delegates the n and o/s/t/u/v/z groups.
func porterStep4(buf []byte, head int) int {
	if head+1 >= len(buf) {
		return head
	}
	switch buf[head+1] {
	case 'a':
		head = porterStep4A(buf, head)
	case 'c':
		head = porterStep4C(buf, head)
	case 'e':
		head = porterStep4E(buf, head)
	case 'i':
		head = porterStep4I(buf, head)
	case 'l':
		head = porterStep4L(buf, head)
	case 'n':
		head = porterStep4N(buf, head)
	default:
		head = porterStep4UZ(buf, head)
	}
	return head
}

// porterStep4A carries Step 4's "-al" rule.
func porterStep4A(buf []byte, head int) int {
	if buf[head] == 'l' && porterMGt1(buf, head+2) {
		head += 2
	}
	return head
}

// porterStep4C carries Step 4's "-ance"/"-ence" rule.
func porterStep4C(buf []byte, head int) int {
	if buf[head] == 'e' && at(buf, head+2) == 'n' && (at(buf, head+3) == 'a' || at(buf, head+3) == 'e') && porterMGt1(buf, head+4) {
		head += 4
	}
	return head
}

// porterStep4E carries Step 4's "-er" rule.
func porterStep4E(buf []byte, head int) int {
	if buf[head] == 'r' && porterMGt1(buf, head+2) {
		head += 2
	}
	return head
}

// porterStep4I carries Step 4's "-ic" rule.
func porterStep4I(buf []byte, head int) int {
	if buf[head] == 'c' && porterMGt1(buf, head+2) {
		head += 2
	}
	return head
}

// porterStep4L carries Step 4's "-able"/"-ible" rule.
func porterStep4L(buf []byte, head int) int {
	if buf[head] == 'e' && at(buf, head+2) == 'b' && (at(buf, head+3) == 'a' || at(buf, head+3) == 'i') && porterMGt1(buf, head+4) {
		head += 4
	}
	return head
}

// porterStep4N carries Step 4's 'n' group (-ant/-ement/-ment/-ent).
func porterStep4N(buf []byte, head int) int {
	if buf[head] != 't' {
		return head
	}
	if at(buf, head+2) == 'a' {
		if porterMGt1(buf, head+3) {
			head += 3
		}
	} else if at(buf, head+2) == 'e' {
		if !stemRev(buf, &head, "tneme", "", porterMGt1) &&
			!stemRev(buf, &head, "tnem", "", porterMGt1) {
			stemRev(buf, &head, "tne", "", porterMGt1)
		}
	}
	return head
}

// porterStep4UZ carries Step 4's remaining endings (-ion/-ism/-ate/-ous/
// -ive/-ize), delegating per letter.
func porterStep4UZ(buf []byte, head int) int {
	switch buf[head+1] {
	case 'o':
		head = porterStep4O(buf, head)
	case 's':
		head = porterStep4S(buf, head)
	case 't':
		head = porterStep4T(buf, head)
	case 'u':
		head = porterStep4U(buf, head)
	case 'v', 'z':
		head = porterStep4VZ(buf, head)
	}
	return head
}

// porterStep4O carries Step 4's "-ion" rule.
func porterStep4O(buf []byte, head int) int {
	if buf[head] == 'u' {
		if porterMGt1(buf, head+2) {
			head += 2
		}
	} else if at(buf, head+3) == 's' || at(buf, head+3) == 't' {
		stemRev(buf, &head, "noi", "", porterMGt1)
	}
	return head
}

// porterStep4S carries Step 4's "-ism" rule.
func porterStep4S(buf []byte, head int) int {
	if buf[head] == 'm' && at(buf, head+2) == 'i' && porterMGt1(buf, head+3) {
		head += 3
	}
	return head
}

// porterStep4T carries Step 4's "-ate"/"-iti" rules.
func porterStep4T(buf []byte, head int) int {
	if !stemRev(buf, &head, "eta", "", porterMGt1) {
		stemRev(buf, &head, "iti", "", porterMGt1)
	}
	return head
}

// porterStep4U carries Step 4's "-ous" rule.
func porterStep4U(buf []byte, head int) int {
	if buf[head] == 's' && at(buf, head+2) == 'o' && porterMGt1(buf, head+3) {
		head += 3
	}
	return head
}

// porterStep4VZ carries Step 4's "-ive"/"-ize" rule.
func porterStep4VZ(buf []byte, head int) int {
	if buf[head] == 'e' && at(buf, head+2) == 'i' && porterMGt1(buf, head+3) {
		head += 3
	}
	return head
}

// porterStep5 ports Step 5a (final -e removal) and Step 5b (-ll reduction).
func porterStep5(buf []byte, head int) int {
	// Step 5a.
	if buf[head] == 'e' {
		if porterMGt1(buf, head+1) {
			head++
		} else if porterMEq1(buf, head+1) && !porterStarOh(buf, head+1) {
			head++
		}
	}

	// Step 5b.
	if porterMGt1(buf, head) && buf[head] == 'l' && at(buf, head+1) == 'l' {
		head++
	}
	return head
}

// at returns buf[i], or 0 past the end (the C reads the NUL terminator).
func at(buf []byte, i int) byte {
	if i < len(buf) {
		return buf[i]
	}
	return 0
}

// copyStemmer ports fts3_porter.c copy_stemmer: US-ASCII case folding; when
// the token is longer than mx*2 bytes (mx=10, or 3 for tokens containing a
// digit) only the first mx and last mx bytes are kept.
func copyStemmer(in string) string {
	out := []byte(in)
	hasDigit := false
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c - 'A' + 'a'
		} else if c >= '0' && c <= '9' {
			hasDigit = true
		}
	}
	mx := 10
	if hasDigit {
		mx = 3
	}
	if len(in) > mx*2 {
		out = append(out[:mx], in[len(in)-mx:]...)
	}
	return string(out)
}

// porterCType classifies 'a'..'z': 0 vowel, 1 consonant, 2 'y' (contextual).
var porterCType = [26]byte{
	0, 1, 1, 1, 0, 1, 1, 1, 0, 1, 1, 1, 1, 1, 0, 1, 1, 1, 1, 1, 0,
	1, 1, 1, 2, 1,
}

// porterIsConsonantAt ports isConsonant on the reversed buffer.
func porterIsConsonantAt(buf []byte, i int) bool {
	x := at(buf, i)
	if x == 0 {
		return false
	}
	if x < 'a' || x > 'z' {
		return false
	}
	j := porterCType[x-'a']
	if j < 2 {
		return j != 0
	}
	return at(buf, i+1) == 0 || porterIsVowelAt(buf, i+1)
}

// porterIsVowelAt ports isVowel on the reversed buffer.
func porterIsVowelAt(buf []byte, i int) bool {
	x := at(buf, i)
	if x == 0 {
		return false
	}
	if x < 'a' || x > 'z' {
		return false
	}
	j := porterCType[x-'a']
	if j < 2 {
		return j == 0
	}
	return porterIsConsonantAt(buf, i+1)
}

// porterMGt0 ports m_gt_0: at least one vowel-consonant pair remains.
func porterMGt0(buf []byte, i int) bool {
	for porterIsVowelAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return false
	}
	for porterIsConsonantAt(buf, i) {
		i++
	}
	return at(buf, i) != 0
}

// porterMEq1 ports m_eq_1: exactly one vowel-consonant pair remains.
func porterMEq1(buf []byte, i int) bool {
	for porterIsVowelAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return false
	}
	for porterIsConsonantAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return false
	}
	for porterIsVowelAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return true
	}
	for porterIsConsonantAt(buf, i) {
		i++
	}
	return at(buf, i) == 0
}

// porterMGt1 ports m_gt_1: more than one vowel-consonant pair remains.
func porterMGt1(buf []byte, i int) bool {
	for porterIsVowelAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return false
	}
	for porterIsConsonantAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return false
	}
	for porterIsVowelAt(buf, i) {
		i++
	}
	if at(buf, i) == 0 {
		return false
	}
	for porterIsConsonantAt(buf, i) {
		i++
	}
	return at(buf, i) != 0
}

// porterHasVowel ports hasVowel: a vowel anywhere from i on.
func porterHasVowel(buf []byte, i int) bool {
	for porterIsConsonantAt(buf, i) {
		i++
	}
	return at(buf, i) != 0
}

// porterDoubleConsonant ports doubleConsonant: the word ends (in reversed
// space, starts) in two identical consonants.
func porterDoubleConsonant(buf []byte, i int) bool {
	return porterIsConsonantAt(buf, i) && at(buf, i) == at(buf, i+1)
}

// porterStarOh ports star_oh: the word ends consonant-vowel-consonant with
// the final consonant not 'w', 'x' or 'y'.
func porterStarOh(buf []byte, i int) bool {
	return porterIsConsonantAt(buf, i) &&
		at(buf, i) != 'w' && at(buf, i) != 'x' && at(buf, i) != 'y' &&
		porterIsVowelAt(buf, i+1) &&
		porterIsConsonantAt(buf, i+2)
}

// stemRev ports fts3_porter.c stem: match the reversed ending zFrom at the
// head; when it matches and cond holds (or cond is nil), overwrite the
// matched region with zTo written backwards (zTo is in normal order).
// Returns true when zFrom matched — even when the condition failed and no
// substitution happened, exactly like the C.
func stemRev(buf []byte, head *int, zFrom, zTo string, cond func([]byte, int) bool) bool {
	h := *head
	i := 0
	for i < len(zFrom) && h < len(buf) && zFrom[i] == buf[h] {
		h++
		i++
	}
	if i < len(zFrom) {
		return false
	}
	if cond != nil && !cond(buf, h) {
		return true
	}
	w := 0
	for w < len(zTo) {
		h--
		if h < 0 {
			// Cannot happen for the C's rule set (the region written is
			// never longer than the matched suffix plus its stem); guard
			// regardless to avoid corrupting the buffer.
			*head = 0
			return true
		}
		buf[h] = zTo[w]
		w++
	}
	*head = h
	return true
}
