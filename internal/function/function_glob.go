package function

// GLOB pattern matching (SQLite GLOB operator semantics: *, ?, [...]
// classes with ^ negation, per pattern.c / func.c globMatch).

import "unicode/utf8"

// GlobMatch implements SQLite GLOB matching (* and ? wildcards).
func GlobMatch(s, pattern string) bool {
	px, sx := 0, 0
	nextPx, nextSx := 0, 0
	for px < len(pattern) || sx < len(s) {
		if px < len(pattern) {
			c := pattern[px]
			if c == '*' {
				nextPx, nextSx = px+1, sx+1
				px++
				continue
			}
			if ok, np, ns := globMatchChar(s, pattern, c, px, sx); ok {
				px, sx = np, ns
				continue
			}
		}
		if 0 < nextPx && nextPx <= len(pattern) && nextSx <= len(s) {
			px, sx = nextPx, nextSx
			nextSx++
			continue
		}
		return false
	}
	return true
}

// globMatchChar handles ?, [class] and exact character matching for GLOB
// (func.c patternCompare: '?' is matchOne, '[' is matchOther with
// matchSet=']').
func globMatchChar(s, pattern string, c byte, px, sx int) (bool, int, int) {
	if c == '?' && sx < len(s) {
		return true, px + 1, sx + 1
	}
	if c == '[' {
		return globMatchClass(s, pattern, px, sx)
	}
	if sx < len(s) && s[sx] == c {
		return true, px + 1, sx + 1
	}
	return false, px, sx
}

// globMatchClass matches one input character against a GLOB character class
// whose '[' sits at pattern offset px (func.c patternCompare's bracket
// branch): literal members, a-b ranges (a '-' adjacent to ']' or the class
// end is a literal), and a leading '^' negation. ']' first in the class is a
// member. An unterminated class never matches. Returns ok with the offsets
// past the class (pattern) and past the matched character (input).
func globMatchClass(s, pattern string, px, sx int) (bool, int, int) {
	if sx >= len(s) {
		return false, px, sx
	}
	ch, size := utf8.DecodeRuneInString(s[sx:])
	if ch == utf8.RuneError && size <= 1 {
		ch = rune(s[sx])
	}
	seen, invert, pi := globClassScan(pattern, px+1, ch)
	if seen == invert {
		return false, px, sx
	}
	return true, pi, sx + size
}

// globClassScan walks the class body starting just past '[': an optional
// '^' inversion, then items up to the closing ']' (an unterminated class
// reports no match). It returns whether ch was seen, the inversion flag,
// and the offset just past the class.
func globClassScan(pattern string, pi int, ch rune) (seen, invert bool, end int) {
	priorC := rune(0)
	c2, pi := globClassNext(pattern, pi)
	if c2 == '^' {
		invert = true
		c2, pi = globClassNext(pattern, pi)
	}
	if c2 == ']' { // a ']' first in the class is a literal member
		seen = ch == ']'
		c2, pi = globClassNext(pattern, pi)
	}
	for c2 != 0 && c2 != ']' {
		switch {
		case c2 == '-' && globClassRangeable(pattern, pi, priorC):
			hi, npi := globClassNext(pattern, pi)
			if ch >= priorC && ch <= hi {
				seen = true
			}
			priorC = 0
			pi = npi
		default:
			if ch == c2 {
				seen = true
			}
			priorC = c2
		}
		c2, pi = globClassNext(pattern, pi)
	}
	if c2 == 0 { // unterminated class never matches
		return false, invert, pi
	}
	return seen, invert, pi
}

// globClassNext decodes the next class rune; (0, pi) marks the end of the
// pattern.
func globClassNext(pattern string, pi int) (rune, int) {
	if pi >= len(pattern) {
		return 0, pi
	}
	r, sz := utf8.DecodeRuneInString(pattern[pi:])
	return r, pi + sz
}

// globClassRangeable reports whether the '-' just decoded opens a range: a
// prior item exists and the byte starting the range end is neither ']',
// end-of-pattern, nor NUL (treated as end-of-pattern).
func globClassRangeable(pattern string, pi int, priorC rune) bool {
	return priorC > 0 && pi < len(pattern) &&
		pattern[pi] != ']' && pattern[pi] != 0
}
