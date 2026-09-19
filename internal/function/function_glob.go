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
	pi := px + 1
	next := func() rune {
		if pi >= len(pattern) {
			return 0
		}
		r, sz := utf8.DecodeRuneInString(pattern[pi:])
		pi += sz
		return r
	}
	at := func() byte { // pattern byte just past the last decoded rune
		if pi < len(pattern) {
			return pattern[pi]
		}
		return 0
	}
	seen, invert := false, false
	priorC := rune(0)
	c2 := next()
	if c2 == '^' {
		invert = true
		c2 = next()
	}
	if c2 == ']' {
		if ch == ']' {
			seen = true
		}
		c2 = next()
	}
	for c2 != 0 && c2 != ']' {
		if c2 == '-' && at() != ']' && at() != 0 && priorC > 0 {
			c2 = next()
			if ch >= priorC && ch <= c2 {
				seen = true
			}
			priorC = 0
		} else {
			if ch == c2 {
				seen = true
			}
			priorC = c2
		}
		c2 = next()
	}
	if c2 == 0 || seen == invert {
		return false, px, sx
	}
	return true, pi, sx + size
}
