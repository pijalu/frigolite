// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"strconv"
	"strings"
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

// stringMap implements `string map` (simplified: identity).
func stringMap(i *Interp, args []string) error {
	i.vars[""] = args[1]
	return nil
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
