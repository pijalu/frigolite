// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"strconv"
	"strings"
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
		"length":  stringLength,
		"tolower": stringToLower,
		"toupper": stringToUpper,
		"trim":    stringTrim,
		"range":   stringRange,
		"compare": stringCompare,
		"equal":   stringEqual,
		"first":   stringFirst,
		"map":     stringMap,
		"repeat":  stringRepeat,
		"index":   stringIndex,
	}
}

// stringLength implements `string length str`.
func stringLength(i *Interp, args []string) error {
	i.vars[""] = strconv.Itoa(len(args[1]))
	return nil
}

// stringToLower implements `string tolower str`.
func stringToLower(i *Interp, args []string) error {
	i.vars[""] = strings.ToLower(args[1])
	return nil
}

// stringToUpper implements `string toupper str`.
func stringToUpper(i *Interp, args []string) error {
	i.vars[""] = strings.ToUpper(args[1])
	return nil
}

// stringTrim implements `string trim str`.
func stringTrim(i *Interp, args []string) error {
	i.vars[""] = strings.TrimSpace(args[1])
	return nil
}

// stringRange implements `string range str start end`.
func stringRange(i *Interp, args []string) error {
	if len(args) < 4 {
		return nil
	}
	start, _ := strconv.Atoi(args[2])
	end, _ := strconv.Atoi(args[3])
	if end >= len(args[1]) {
		end = len(args[1]) - 1
	}
	if start < 0 {
		start = 0
	}
	if start > end {
		i.vars[""] = ""
	} else {
		i.vars[""] = args[1][start : end+1]
	}
	return nil
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
	idx, _ := strconv.Atoi(args[2])
	if idx >= 0 && idx < len(args[1]) {
		i.vars[""] = string(args[1][idx])
	} else {
		i.vars[""] = ""
	}
	return nil
}
