package fts

// This file ports SQLite's FTS3/4 Porter stemmer verbatim from
// ext/fts3/fts3_porter.c: porter_stemmer (the classic Porter algorithm run
// on a REVERSED word), copy_stemmer (the fallback for tokens that are too
// short, too long, or contain non-letter bytes) and the condition helpers
// (isVowel/isConsonant with the 'y' rule, m_gt_0/m_eq_1/m_gt_1, hasVowel,
// doubleConsonant, star_oh, stem).

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
	for i := 0; i < len(word); i++ {
		c := word[i]
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
			return copyStemmer(word)
		}
	}
	// Reverse the case-folded word: the Porter rules run against the word
	// head in reversed space (z[0] is the word's LAST letter). The C keeps
	// five NUL bytes past the reversed word so the condition helpers can
	// read the terminator.
	n := len(word)
	buf := make([]byte, n+5)
	for i := 0; i < n; i++ {
		c := word[n-1-i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	head := 0

	// Step 1a.
	if buf[head] == 's' {
		if !stemRev(buf, &head, "sess", "ss", nil) &&
			!stemRev(buf, &head, "sei", "i", nil) &&
			!stemRev(buf, &head, "ss", "ss", nil) {
			head++
		}
	}

	// Step 1b.
	head2 := head
	if stemRev(buf, &head, "dee", "ee", porterMGt0) {
		// Do nothing. The work was all in the test.
	} else if (stemRev(buf, &head, "gni", "", porterHasVowel) || stemRev(buf, &head, "de", "", porterHasVowel)) && head != head2 {
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
	}

	// Step 1c.
	if buf[head] == 'y' && porterHasVowel(buf, head+1) {
		buf[head] = 'i'
	}

	// Step 2.
	if head+1 < len(buf) {
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
	}

	// Step 3.
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

	// Step 4.
	if head+1 < len(buf) {
		switch buf[head+1] {
		case 'a':
			if buf[head] == 'l' && porterMGt1(buf, head+2) {
				head += 2
			}
		case 'c':
			if buf[head] == 'e' && at(buf, head+2) == 'n' && (at(buf, head+3) == 'a' || at(buf, head+3) == 'e') && porterMGt1(buf, head+4) {
				head += 4
			}
		case 'e':
			if buf[head] == 'r' && porterMGt1(buf, head+2) {
				head += 2
			}
		case 'i':
			if buf[head] == 'c' && porterMGt1(buf, head+2) {
				head += 2
			}
		case 'l':
			if buf[head] == 'e' && at(buf, head+2) == 'b' && (at(buf, head+3) == 'a' || at(buf, head+3) == 'i') && porterMGt1(buf, head+4) {
				head += 4
			}
		case 'n':
			if buf[head] == 't' {
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
			}
		case 'o':
			if buf[head] == 'u' {
				if porterMGt1(buf, head+2) {
					head += 2
				}
			} else if at(buf, head+3) == 's' || at(buf, head+3) == 't' {
				stemRev(buf, &head, "noi", "", porterMGt1)
			}
		case 's':
			if buf[head] == 'm' && at(buf, head+2) == 'i' && porterMGt1(buf, head+3) {
				head += 3
			}
		case 't':
			if !stemRev(buf, &head, "eta", "", porterMGt1) {
				stemRev(buf, &head, "iti", "", porterMGt1)
			}
		case 'u':
			if buf[head] == 's' && at(buf, head+2) == 'o' && porterMGt1(buf, head+3) {
				head += 3
			}
		case 'v', 'z':
			if buf[head] == 'e' && at(buf, head+2) == 'i' && porterMGt1(buf, head+3) {
				head += 3
			}
		}
	}

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

	// Flip the reversed stem back into forward order.
	out := make([]byte, 0, len(buf)-head)
	for i := len(buf) - 1; i >= head; i-- {
		if buf[i] == 0 {
			continue
		}
		out = append(out, buf[i])
	}
	return string(out)
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
