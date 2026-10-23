// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// commandLSort implements `lsort ?options? list`. The options are -ascii
// (the default), -integer, -real, -dictionary, -command, -index index and
// -decreasing; the result is the sorted list.
//
// The corpus relies on a real sort: trans2.test's `hash1`/`hash2` compute an
// MD5 over the *sorted* records, so a stub that returned the input unchanged
// produced the wrong expectation for every case.
func commandLSort(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) == 0 {
		i.vars[""] = ""
		return nil
	}
	mode := "ascii"
	index := -1
	decreasing := false
	unique := false
	rest := args
	// consumed counts the option words, so the raw word of the list
	// argument can be found (the raw words are parallel to the arguments).
	consumed := 0
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		switch rest[0] {
		case "-ascii", "-integer", "-real", "-dictionary":
			mode = strings.TrimPrefix(rest[0], "-")
			rest = rest[1:]
			consumed++
		case "-decreasing":
			decreasing = true
			rest = rest[1:]
			consumed++
		case "-unique":
			unique = true
			rest = rest[1:]
			consumed++
		case "-index":
			if len(rest) < 2 {
				i.vars[""] = ""
				return nil
			}
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				i.vars[""] = ""
				return nil
			}
			index = n
			rest = rest[2:]
			consumed += 2
		case "-command", "-stride", "-nocase":
			// Not modelled: skip the option (and its argument for
			// -command) so the list argument is still found.
			rest = rest[1:]
			consumed++
			if rest[0] == "-command" && len(rest) > 0 {
				rest = rest[1:]
				consumed++
			}
		default:
			rest = rest[1:]
			consumed++
		}
	}
	if len(rest) == 0 {
		i.vars[""] = ""
		return nil
	}
	// A sort of a runtime query result cannot be evaluated at conversion
	// time: the rows only exist when the statement runs. The fixture
	// records the sort, and the harness applies it to the rows it renders
	// (where7's count_steps_sort helper).
	if id, ok := i.sortedRuntimeQuery(rawWords, consumed, rest[0]); ok {
		if stmt := i.runtimeStatement(id); stmt != nil {
			stmt.ResultTransform = lsortTransform(mode, decreasing, unique, index)
		}
		// The sorted rows exist only at run time: the value stays empty,
		// as it was before the runtime value was marked. The sort itself
		// is recorded on the statement, and the harness applies it to the
		// rows it renders.
		i.vars[""] = ""
		return nil
	}
	items := splitList(rest[0])
	key := func(s string) string {
		if index < 0 {
			return s
		}
		parts := splitList(s)
		if index >= len(parts) {
			return ""
		}
		return parts[index]
	}
	sort.SliceStable(items, func(a, b int) bool {
		ka, kb := key(items[a]), key(items[b])
		var less bool
		switch mode {
		case "integer":
			ia, oka := tclIntegerValue(ka)
			ib, okb := tclIntegerValue(kb)
			switch {
			case oka && okb:
				less = ia < ib
			case oka:
				less = true
			case okb:
				less = false
			default:
				less = ka < kb
			}
		case "real":
			fa, oka := tclRealValue(ka)
			fb, okb := tclRealValue(kb)
			switch {
			case oka && okb:
				less = fa < fb
			case oka:
				less = true
			case okb:
				less = false
			default:
				less = ka < kb
			}
		case "dictionary":
			less = strings.ToLower(ka) < strings.ToLower(kb)
		default: // -ascii
			less = ka < kb
		}
		if decreasing {
			return !less && ka != kb
		}
		return less
	})
	i.vars[""] = tclList(items)
	return nil
}

// sortedRuntimeQuery reports the query whose rows a sort operates on. The
// rows are reached either through the value itself (the marker of a runtime
// query value) or through the variable that holds it (`set r [db eval $sql]`
// then `lsort -integer $r`).
func (i *Interp) sortedRuntimeQuery(rawWords []rawWord, consumed int, value string) (int, bool) {
	if id, ok := runtimeValueID(value); ok {
		return id, true
	}
	if len(rawWords) <= consumed+1 {
		return 0, false
	}
	name, ok := runtimeVarRef(strings.TrimSpace(rawWords[consumed+1].Text))
	if !ok {
		return 0, false
	}
	id, ok := i.runtimeVars[strings.TrimPrefix(name, "::")]
	return id, ok
}

// lsortTransform renders the fixture's record of a Tcl sort applied to a
// query's rows: the mode, the flags and the element index the comparison
// uses. The harness reads it back and sorts the rendered rows the same way.
func lsortTransform(mode string, decreasing, unique bool, index int) string {
	parts := []string{"lsort", mode}
	if decreasing {
		parts = append(parts, "decreasing")
	}
	if unique {
		parts = append(parts, "unique")
	}
	if index >= 0 {
		parts = append(parts, "index", strconv.Itoa(index))
	}
	return strings.Join(parts, ":")
}

// tclIntegerValue parses a Tcl integer ("0x10", "010", " 42 "); ok is false
// when the text is not an integer.
func tclIntegerValue(s string) (int64, bool) {
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
	case strings.HasPrefix(t, "0b"), strings.HasPrefix(t, "0B"):
		base, t = 2, t[2:]
	case strings.HasPrefix(t, "0o"), strings.HasPrefix(t, "0O"):
		base, t = 8, t[2:]
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

// tclRealValue parses a Tcl double; ok is false when the text is not numeric.
func tclRealValue(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// commandMD5 implements the `md5` command the testfixture provides: the
// lowercase hex MD5 digest of the argument (trans2.test's hash1/hash2 use it).
func commandMD5(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) == 0 {
		i.vars[""] = ""
		return nil
	}
	sum := md5.Sum([]byte(args[0]))
	i.vars[""] = hex.EncodeToString(sum[:])
	return nil
}

// commandRandomUUID implements the testfixture's `random_uuid`: a fresh
// random UUID per call. The converter's PRNG is deterministic (see tclRand),
// so the same file always yields the same UUIDs — the fixture's data and the
// expectations derived from it stay consistent.
func commandRandomUUID(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	var b [16]byte
	for idx := 0; idx < 16; idx += 2 {
		v := int64(i.tclRand() * 65536)
		b[idx] = byte(v >> 8)
		b[idx+1] = byte(v)
	}
	// RFC 4122 version 4 / variant bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	i.vars[""] = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return nil
}
