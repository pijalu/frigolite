// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// stringHandler executes a `string` subcommand. args are the full command
// arguments (subcommand, string, ...). Handlers set i.vars[""] as the result.
type stringHandler func(i *Interp, args []string) error

// stringHandlers maps `string` subcommand names to their implementations.
// Populated in init() for symmetry with commandHandlers.
var stringHandlers map[string]stringHandler

func init() {
	stringHandlers = map[string]stringHandler{
		"length":    stringLength,
		"tolower":   stringToLower,
		"toupper":   stringToUpper,
		"trim":      stringTrim,
		"range":     stringRange,
		"compare":   stringCompare,
		"equal":     stringEqual,
		"first":     stringFirst,
		"map":       stringMap,
		"repeat":    stringRepeat,
		"index":     stringIndex,
		"match":     stringMatch,
		"is":        stringIs,
		"replace":   stringReplace,
		"trimleft":  stringTrimLeft,
		"trimright": stringTrimRight,
		"last":      stringLast,
		"cat":       stringCat,
		"reverse":   stringReverse,
		"wordstart": stringWordStart,
		"wordend":   stringWordEnd,
	}
}

// stringMatch implements `string match ?-nocase? pattern str` with Tcl's
// glob syntax (Tcl_StringMatch): `*` matches any sequence, `?` any single
// character, `[class]` a character class with ranges, and a backslash escapes
// the next character. There is no class negation.
func stringMatch(i *Interp, args []string) error {
	rest := args[1:]
	nocase := false
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if rest[0] == "-nocase" {
			nocase = true
		}
		rest = rest[1:]
	}
	if len(rest) < 2 {
		i.vars[""] = "0"
		return nil
	}
	pattern, str := rest[0], rest[1]
	if nocase {
		pattern = strings.ToLower(pattern)
		str = strings.ToLower(str)
	}
	if tclStringMatch([]rune(pattern), []rune(str)) {
		i.vars[""] = "1"
	} else {
		i.vars[""] = "0"
	}
	return nil
}

// tclStringMatch matches str against a Tcl glob pattern; single `*`
// backtracking keeps it linear in practice.
func tclStringMatch(pattern, str []rune) bool {
	px, sx := 0, 0
	starPx, starSx := -1, 0
	for sx < len(str) {
		if px < len(pattern) {
			p := pattern[px]
			switch {
			case p == '*':
				starPx, starSx = px, sx
				px++
				continue
			case p == '?':
				px++
				sx++
				continue
			case p == '[':
				next, matched, ok := matchCharClass(pattern, px, str[sx])
				if ok {
					if matched {
						px, sx = next, sx+1
						continue
					}
				} else if str[sx] == '[' {
					px++
					sx++
					continue
				}
			case p == '\\' && px+1 < len(pattern):
				if pattern[px+1] == str[sx] {
					px += 2
					sx++
					continue
				}
			default:
				if p == str[sx] {
					px++
					sx++
					continue
				}
			}
		}
		if starPx >= 0 {
			starSx++
			sx = starSx
			px = starPx + 1
			continue
		}
		return false
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// matchCharClass matches one `[...]` class starting at pattern[px]. It returns
// the index after the class, whether ch is a member, and ok=false when the
// class is unterminated (the '[' is then a literal character).
func matchCharClass(pattern []rune, px int, ch rune) (int, bool, bool) {
	i := px + 1
	matched := false
	for i < len(pattern) {
		if pattern[i] == ']' {
			return i + 1, matched, true
		}
		lo := pattern[i]
		i++
		hi := lo
		if i+1 < len(pattern) && pattern[i] == '-' && pattern[i+1] != ']' {
			hi = pattern[i+1]
			i += 2
		}
		if lo <= ch && ch <= hi {
			matched = true
		}
	}
	return px + 1, false, false
}

// stringIs implements `string is class ?-strict? ?-failindex var? str` for the
// character classes used by the test suite.
func stringIs(i *Interp, args []string) error {
	if len(args) < 3 {
		i.vars[""] = "0"
		return nil
	}
	class := args[1]
	strict := false
	rest := args[2:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if rest[0] == "-strict" {
			strict = true
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		i.vars[""] = "0"
		return nil
	}
	str := rest[len(rest)-1]
	ok := tclStringIsClass(class, str)
	if strict && str == "" {
		ok = false
	}
	if ok {
		i.vars[""] = "1"
	} else {
		i.vars[""] = "0"
	}
	return nil
}

// tclStringIsClass reports whether str consists entirely of characters of the
// named class (`string is`). The empty string belongs to every class.
func tclStringIsClass(class, str string) bool {
	if str == "" {
		return true
	}
	runes := []rune(str)
	all := func(pred func(rune) bool) bool {
		for _, r := range runes {
			if !pred(r) {
				return false
			}
		}
		return true
	}
	switch class {
	case "integer", "wideinteger", "entier":
		if _, ok := parseTclIntLiteral(str); ok {
			return true
		}
		return false
	case "double":
		if _, ok := parseTclDoubleLiteral(str); ok {
			return true
		}
		return false
	case "boolean":
		switch strings.ToLower(str) {
		case "0", "1", "true", "false", "yes", "no", "on", "off":
			return true
		}
		return false
	case "true":
		return strings.ToLower(str) == "true" || str == "1" ||
			strings.EqualFold(str, "yes") || strings.EqualFold(str, "on")
	case "false":
		return strings.ToLower(str) == "false" || str == "0" ||
			strings.EqualFold(str, "no") || strings.EqualFold(str, "off")
	case "alpha":
		return all(unicode.IsLetter)
	case "alnum":
		return all(func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
	case "digit":
		return all(unicode.IsDigit)
	case "xdigit":
		return all(func(r rune) bool { return r <= unicode.MaxLatin1 && isHexDigit(byte(r)) })
	case "space":
		return all(unicode.IsSpace)
	case "lower":
		return all(unicode.IsLower)
	case "upper":
		return all(unicode.IsUpper)
	case "ascii":
		return all(func(r rune) bool { return r < 0x80 })
	case "control":
		return all(unicode.IsControl)
	case "print":
		return all(unicode.IsPrint)
	case "graph":
		return all(func(r rune) bool { return unicode.IsGraphic(r) && !unicode.IsSpace(r) })
	case "punct":
		return all(func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) })
	case "wordchar":
		return all(func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) })
	case "list":
		return tclIsWellFormedList(str)
	}
	return false
}

// parseTclIntLiteral parses a Tcl integer literal: an optional sign followed
// by decimal digits, or a 0x/0o/0b prefixed value. Surrounding whitespace is
// allowed (Tcl accepts it for `string is integer`).
func parseTclIntLiteral(s string) (int64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	neg := false
	switch t[0] {
	case '+':
		t = t[1:]
	case '-':
		neg = true
		t = t[1:]
	}
	base := 10
	switch {
	case strings.HasPrefix(t, "0x"), strings.HasPrefix(t, "0X"):
		base, t = 16, t[2:]
	case strings.HasPrefix(t, "0o"), strings.HasPrefix(t, "0O"):
		base, t = 8, t[2:]
	case strings.HasPrefix(t, "0b"), strings.HasPrefix(t, "0B"):
		base, t = 2, t[2:]
	}
	if t == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(t, base, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// parseTclDoubleLiteral parses a Tcl double literal, including the Inf and
// NaN spellings.
func parseTclDoubleLiteral(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	switch strings.ToLower(t) {
	case "inf", "+inf", "-inf", "infinity", "+infinity", "-infinity",
		"nan", "+nan", "-nan":
		return 0, true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// isHexDigit reports whether b is a hexadecimal digit.
func isHexDigit(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'f', b >= 'A' && b <= 'F':
		return true
	}
	return false
}

// tclIsWellFormedList reports whether s parses as a well-formed Tcl list
// (balanced braces, terminated quotes).
func tclIsWellFormedList(s string) bool {
	pos := 0
	for {
		pos = skipListWS(s, pos)
		if pos >= len(s) {
			return true
		}
		switch s[pos] {
		case '{':
			depth := 1
			i := pos + 1
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '{' {
					depth++
				} else if s[i] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
				i++
			}
			if depth != 0 {
				return false
			}
			pos = i + 1
		case '"':
			i := pos + 1
			closed := false
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '"' {
					closed = true
					break
				}
				i++
			}
			if !closed {
				return false
			}
			pos = i + 1
		default:
			_, pos = readListPlain(s, pos)
		}
	}
}

// stringReplace implements `string replace str first last ?newstring?`. Tcl's
// rules: a negative first index is clamped to 0, an empty or inverted range
// leaves the string unchanged, and last is clamped to the last character.
func stringReplace(i *Interp, args []string) error {
	if len(args) < 4 {
		return nil
	}
	s := args[1]
	first, okFirst := parseTclReplaceIndex(args[2], len(s))
	last, okLast := parseTclReplaceIndex(args[3], len(s))
	replacement := ""
	if len(args) >= 5 {
		replacement = args[4]
	}
	if !okFirst || !okLast {
		i.vars[""] = s
		return nil
	}
	if first < 0 {
		first = 0
	}
	if last < 0 || first > last || first >= len(s) {
		i.vars[""] = s
		return nil
	}
	if last >= len(s) {
		last = len(s) - 1
	}
	i.vars[""] = s[:first] + replacement + s[last+1:]
	return nil
}

// parseTclReplaceIndex resolves an index for `string replace`. Unlike other
// Tcl index forms a negative index is not counted from the end here: it keeps
// its negative value so the caller can apply Tcl's clamping rules.
func parseTclReplaceIndex(spec string, length int) (int, bool) {
	t := strings.TrimSpace(spec)
	if strings.HasPrefix(t, "-") {
		if i := strings.IndexAny(t, "+-"); i > 0 {
			base, err := strconv.Atoi(t[:i])
			if err != nil {
				return 0, false
			}
			off, err := strconv.Atoi(t[i:])
			if err != nil {
				return 0, false
			}
			return base + off, true
		}
		v, err := strconv.Atoi(t)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	idx, ok := parseTclIndex(t, length)
	if !ok {
		return 0, false
	}
	return idx, true
}

// stringLength implements `string length str`.
func stringLength(i *Interp, args []string) error {
	i.vars[""] = strconv.Itoa(len(args[1]))
	return nil
}

// stringToLower implements `string tolower str`.
func stringToLower(i *Interp, args []string) error {
	i.vars[""] = strings.ToLower(args[1])
	if i.curTest != "" && len(i.stmts) > 0 {
		last := &i.stmts[len(i.stmts)-1]
		if last.TestName == i.curTest && last.Type == "query" {
			last.ResultTransform = "tolower"
		}
	}
	return nil
}

// stringToUpper implements `string toupper str`.
func stringToUpper(i *Interp, args []string) error {
	i.vars[""] = strings.ToUpper(args[1])
	return nil
}

// stringTrim implements `string trim str`.
func stringTrim(i *Interp, args []string) error {
	if len(args) < 2 {
		i.vars[""] = ""
		return nil
	}
	cutset := tclWhitespace
	if len(args) > 2 {
		cutset = args[2]
	}
	i.vars[""] = strings.Trim(args[1], cutset)
	return nil
}

// stringTrimLeft implements `string trimleft string ?chars?`. The chars
// argument is a Tcl *charlist*: every character of the string is a member of
// the set, so "ab" trims both 'a' and 'b'.
func stringTrimLeft(i *Interp, args []string) error {
	if len(args) < 2 {
		i.vars[""] = ""
		return nil
	}
	cutset := tclWhitespace
	if len(args) > 2 {
		cutset = args[2]
	}
	i.vars[""] = strings.TrimLeft(args[1], cutset)
	return nil
}

// stringTrimRight implements `string trimright string ?chars?`.
//
// round1.test computes its expectation with `string trimright $r 0`; without
// this subcommand the converter used to answer with the *input*, which
// produced 9,356 wrong expectations (the engine returns the REAL that
// SQLite's round() produces, "90821.84", not the trailing-zero text).
func stringTrimRight(i *Interp, args []string) error {
	if len(args) < 2 {
		i.vars[""] = ""
		return nil
	}
	cutset := tclWhitespace
	if len(args) > 2 {
		cutset = args[2]
	}
	i.vars[""] = strings.TrimRight(args[1], cutset)
	return nil
}

// stringLast implements `string last needle haystack ?lastIndex?`.
func stringLast(i *Interp, args []string) error {
	if len(args) < 3 {
		i.vars[""] = "-1"
		return nil
	}
	haystack := args[2]
	if len(args) > 3 {
		end, ok := parseTclIndex(args[3], len(haystack))
		if !ok {
			i.vars[""] = "-1"
			return nil
		}
		if end >= 0 {
			haystack = haystack[:end+1]
		}
	}
	i.vars[""] = fmt.Sprintf("%d", strings.LastIndex(haystack, args[1]))
	return nil
}

// stringCat implements `string cat ?string ...?`.
func stringCat(i *Interp, args []string) error {
	i.vars[""] = strings.Join(args[1:], "")
	return nil
}

// stringReverse implements `string reverse string`.
func stringReverse(i *Interp, args []string) error {
	if len(args) < 2 {
		i.vars[""] = ""
		return nil
	}
	runes := []rune(args[1])
	for l, r := 0, len(runes)-1; l < r; l, r = l+1, r-1 {
		runes[l], runes[r] = runes[r], runes[l]
	}
	i.vars[""] = string(runes)
	return nil
}

// stringWordStart implements `string wordstart string index`.
func stringWordStart(i *Interp, args []string) error {
	if len(args) < 3 {
		i.vars[""] = "-1"
		return nil
	}
	s := args[1]
	idx, ok := parseTclIndex(args[2], len(s))
	if !ok || idx < 0 {
		i.vars[""] = "-1"
		return nil
	}
	if !isTclWordByte(s[idx]) {
		i.vars[""] = "-1"
		return nil
	}
	for idx > 0 && isTclWordByte(s[idx-1]) {
		idx--
	}
	i.vars[""] = fmt.Sprintf("%d", idx)
	return nil
}

// stringWordEnd implements `string wordend string index`.
func stringWordEnd(i *Interp, args []string) error {
	if len(args) < 3 {
		i.vars[""] = "-1"
		return nil
	}
	s := args[1]
	idx, ok := parseTclIndex(args[2], len(s))
	if !ok || idx < 0 {
		i.vars[""] = "-1"
		return nil
	}
	if !isTclWordByte(s[idx]) {
		i.vars[""] = "-1"
		return nil
	}
	for idx+1 < len(s) && isTclWordByte(s[idx+1]) {
		idx++
	}
	// Tcl returns the index just *after* the end of the word.
	i.vars[""] = fmt.Sprintf("%d", idx+1)
	return nil
}

// tclWhitespace is Tcl's default character set for the trim family.
const tclWhitespace = " \t\n\r\v\f"

// isTclWordByte reports whether c is a word character for string wordstart /
// string wordend (letters, digits and the underscore).
func isTclWordByte(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// stringRange implements `string range str start end`.
func stringRange(i *Interp, args []string) error {
	if len(args) < 4 {
		return nil
	}
	s := args[1]
	start, okStart := parseTclIndex(args[2], len(s))
	end, okEnd := parseTclIndex(args[3], len(s))
	if !okStart || !okEnd {
		i.vars[""] = ""
		return nil
	}
	if start < 0 {
		start = 0
	}
	if end >= len(s) {
		end = len(s) - 1
	}
	if start > end || start >= len(s) || end < 0 {
		i.vars[""] = ""
	} else {
		i.vars[""] = s[start : end+1]
	}
	return nil
}

// parseTclIndex resolves a Tcl index specification against a sequence length
// (string characters or list elements): `5`, `-1` (from the end), `end`,
// `end-2`, `end+1`, `2+3`. ok is false for malformed specifications;
// out-of-range results are clamped by the caller.
func parseTclIndex(spec string, length int) (int, bool) {
	base, offset := spec, 0
	if !strings.HasPrefix(spec, "-") && !strings.HasPrefix(spec, "+") {
		if i := strings.IndexAny(spec, "+-"); i > 0 {
			base = spec[:i]
			off, err := strconv.Atoi(spec[i:])
			if err != nil {
				return 0, false
			}
			offset = off
		}
	}
	var idx int
	if base == "end" {
		idx = length - 1
	} else {
		n, err := strconv.Atoi(base)
		if err != nil {
			return 0, false
		}
		if n < 0 {
			idx = length + n
		} else {
			idx = n
		}
	}
	return idx + offset, true
}

// stringCompare implements `string compare str1 str2`.
func stringCompare(i *Interp, args []string) error {
	if len(args) < 3 {
		return nil
	}
	i.vars[""] = strconv.Itoa(strings.Compare(args[1], args[2]))
	return nil
}

// stringEqual implements `string equal str1 str2`.
func stringEqual(i *Interp, args []string) error {
	if len(args) < 3 {
		return nil
	}
	if args[1] == args[2] {
		i.vars[""] = "1"
	} else {
		i.vars[""] = "0"
	}
	return nil
}

// stringFirst implements `string first sub str`.
func stringFirst(i *Interp, args []string) error {
	if len(args) < 3 {
		return nil
	}
	i.vars[""] = strconv.Itoa(strings.Index(args[2], args[1]))
	return nil
}

// mapPair is one from→to replacement rule for `string map`.
type mapPair struct {
	from   string // match key (lowercased when -nocase)
	to     string // replacement
	length int    // len(from), precomputed
}

// stringMap implements `string map ?-nocase? {from to ...} string`. Like
// TCL it scans in a SINGLE pass: matches are found at the earliest position
// (earlier list entries win ties), replaced text is never rescanned, and
// unmatched characters are copied verbatim. Test files use it to splice
// schema variants (e.g. substituting the /D/ placeholder with
// "DEFERRABLE INITIALLY DEFERRED"), so the mapping itself must be applied.
func stringMap(i *Interp, args []string) error {
	idx := 1
	nocase := false
	if idx < len(args) && args[idx] == "-nocase" {
		nocase = true
		idx++
	}
	if len(args) < idx+2 {
		return nil
	}
	pairs := parseMapPairs(splitList(args[idx]), nocase)
	i.vars[""] = applyStringMap(args[idx+1], pairs)
	return nil
}

// parseMapPairs extracts the non-empty from/to rules of a `string map` spec.
func parseMapPairs(spec []string, nocase bool) []mapPair {
	var pairs []mapPair
	for k := 0; k+1 < len(spec); k += 2 {
		if spec[k] == "" {
			continue
		}
		from := spec[k]
		if nocase {
			from = strings.ToLower(from)
		}
		pairs = append(pairs, mapPair{from: from, to: spec[k+1], length: len(from)})
	}
	return pairs
}

// applyStringMap scans input left-to-right, replacing the first matching rule
// at each position (never rescanning replaced text).
func applyStringMap(input string, pairs []mapPair) string {
	haystack := input
	var b strings.Builder
	pos := 0
	for pos < len(input) {
		from, to, ok := matchAt(haystack[pos:], pairs)
		if ok {
			b.WriteString(to)
			pos += from
			continue
		}
		// Copy one full UTF-8 rune so multibyte input stays intact.
		_, size := utf8.DecodeRuneInString(input[pos:])
		if size == 0 {
			size = 1
		}
		b.WriteString(input[pos : pos+size])
		pos += size
	}
	return b.String()
}

// matchAt finds the first rule whose key prefixes s; it returns the key
// length, the replacement, and whether any rule matched.
func matchAt(s string, pairs []mapPair) (int, string, bool) {
	for _, p := range pairs {
		if strings.HasPrefix(s, p.from) {
			return p.length, p.to, true
		}
	}
	return 0, "", false
}

// stringRepeat implements `string repeat str n`.
func stringRepeat(i *Interp, args []string) error {
	if len(args) < 3 {
		return nil
	}
	n, _ := strconv.Atoi(args[2])
	i.vars[""] = strings.Repeat(args[1], n)
	return nil
}

// stringIndex implements `string index str idx`.
func stringIndex(i *Interp, args []string) error {
	if len(args) < 3 {
		return nil
	}
	s := args[1]
	idx, ok := parseTclIndex(args[2], len(s))
	if !ok || idx < 0 || idx >= len(s) {
		i.vars[""] = ""
	} else {
		i.vars[""] = string(s[idx])
	}
	return nil
}
