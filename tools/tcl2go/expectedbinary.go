// Package main implements the tcl2go tool.
//
// This file renders do_test expected values that embed binary-format /
// string command substitutions ([binary format ...], [string repeat ...],
// [string range ...], lreverse, ifcapable folds, userProc calls) into Go
// string expressions (split from processblob.go for file-size hygiene).
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// expectedStringExpr renders a do_test expected value that contains
// `[binary format ...]` / `[string repeat ...]` command substitutions into a
// Go string expression. Returns ("", false) when the word is not one of the
// supported binary-format forms, so the caller falls back to goStringLiteral.
func (tp *transpiler) expectedStringExpr(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	// [userProc args...] — a fixture proc registered by the generated test
	// resolves to a runtime registry call whose result is the wanted value
	// (vtabH 3.x: [sort_files $res true], [contents $pwd]).
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		cmdText := strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
		fields := tclCmdWords(cmdText)
		// [ifcapable GUARD {BODY} [else {BODY}]] — a capability-selected
		// expected value folds at transpile time (autoinc-2.70/2.71: the
		// sqlite_sequence contents differ only for a !tempdb build). The
		// chosen body is a `list a b c` script whose rendering is the
		// static word list. Unknown/elseif forms are not folded.
		if len(fields) >= 3 && fields[0] == "ifcapable" {
			return foldIfcapableExpected(fields)
		}
		if len(fields) >= 1 && globalUserProcs[fields[0]] {
			return userProcExpectedCall(fields), true
		}
		// Otherwise fall through: the bracketed word may still be one of
		// the supported [string repeat [binary format ...]] /
		// [binary format ...] forms below.
	}
	// `lreverse $VAR` — reverse a TCL list variable at runtime
	// (fts3first.test's order=DESC comparisons).
	if strings.HasPrefix(text, "lreverse $") {
		return lreverseExpectedExpr(text)
	}
	// [string repeat [binary format c 0] N] — repeat a single-byte pattern.
	if strings.HasPrefix(text, "[string repeat [binary format ") && strings.HasSuffix(text, "]") {
		return tp.binaryRepeatExpectedExpr(text)
	}
	// [binary format SPEC ARGS...] — build the byte string at runtime.
	if strings.HasPrefix(text, "[binary format ") && strings.HasSuffix(text, "]") {
		return tp.binaryFormatExpectedExpr(text)
	}
	return "", false
}

// userProcExpectedCall renders [userProc args...] as a callTclUserProc
// runtime registry call ($var arguments pass their Go variables).
func userProcExpectedCall(fields []string) string {
	callArgs := make([]string, 0, len(fields)-1)
	for _, a := range fields[1:] {
		if strings.HasPrefix(a, "$") && !strings.Contains(a, "(") {
			if gv := tclVarToGo(strings.TrimPrefix(a, "$")); gv != "" && isValidGoIdent(gv) {
				callArgs = append(callArgs, gv)
				continue
			}
		}
		callArgs = append(callArgs, strconv.Quote(a))
	}
	return fmt.Sprintf("callTclUserProc(%q, %s)", fields[0], strings.Join(callArgs, ", "))
}

// lreverseExpectedExpr renders `lreverse $VAR` as a runtime list reversal.
func lreverseExpectedExpr(text string) (string, bool) {
	varName := strings.TrimSpace(text[len("lreverse $"):])
	if goVar := tclVarToGo(varName); goVar != "" {
		return fmt.Sprintf("tclLreverse(%s)", goVar), true
	}
	return "", false
}

// binaryRepeatExpectedExpr renders [string repeat [binary format SPEC ARG] N]
// — repeat a single-byte pattern.
func (tp *transpiler) binaryRepeatExpectedExpr(text string) (string, bool) {
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "[string repeat [binary format "), "]")
	// inner: "c 0] N" — split at "]".
	closeIdx := strings.Index(inner, "]")
	if closeIdx < 0 {
		return "", false
	}
	formatAndArg := strings.Fields(strings.TrimSpace(inner[:closeIdx]))
	countExpr := strings.TrimSpace(inner[closeIdx+1:])
	if len(formatAndArg) < 2 {
		return "", false
	}
	pattern, ok := binaryFormatBytes(formatAndArg[0], formatAndArg[1:])
	if !ok || len(pattern) != 1 {
		return "", false
	}
	countGo := tp.valueExpr(tcl.RawWord{Text: countExpr})
	return fmt.Sprintf("tclStringRepeat(string([]byte{%d}), %s)", pattern[0], countGo), true
}

// binaryFormatExpectedExpr renders `[binary format SPEC ARGS...]` — build
// the byte string at runtime.
func (tp *transpiler) binaryFormatExpectedExpr(text string) (string, bool) {
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "[binary format "), "]")
	fields := tclCmdWords(inner)
	if len(fields) < 2 {
		return "", false
	}
	spec := fields[0]
	// Resolve each arg: $var refs, integer literals, and supported
	// [string range ...] / [string repeat ...] command substitutions.
	args := fields[1:]
	vals := make([]string, 0, len(args))
	for _, a := range args {
		vals = append(vals, tp.binaryArgExpr(a))
	}
	return binaryFormatGoExpr(spec, vals)
}

// binaryArgExpr renders one argument of a `binary format` spec: a $var
// reference, an integer literal, or a supported [string range ...] command
// substitution (used by the corruption tests to slice the root blob).
func (tp *transpiler) binaryArgExpr(a string) string {
	a = strings.TrimSpace(a)
	if strings.HasPrefix(a, "[string range ") && strings.HasSuffix(a, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(a, "[string range "), "]")
		parts := tclCmdWords(inner)
		if len(parts) == 3 {
			strExpr := tp.valueExpr(tcl.RawWord{Text: parts[0]})
			startExpr := tp.valueExpr(tcl.RawWord{Text: parts[1]})
			endExpr := tp.valueExpr(tcl.RawWord{Text: parts[2]})
			return fmt.Sprintf("tclStringRange(%s, %s, %s)", strExpr, startExpr, endExpr)
		}
	}
	if strings.HasPrefix(a, "[string repeat ") && strings.HasSuffix(a, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(a, "[string repeat "), "]")
		parts := tclCmdWords(inner)
		if len(parts) == 2 {
			strExpr := tp.valueExpr(tcl.RawWord{Text: parts[0]})
			countExpr := tp.valueExpr(tcl.RawWord{Text: parts[1]})
			return fmt.Sprintf("tclStringRepeat(%s, %s)", strExpr, countExpr)
		}
	}
	return tp.valueExpr(tcl.RawWord{Text: a})
}

// binaryFormatBytes evaluates a `binary format` spec with literal integer
// arguments, returning the resulting bytes. Only the single-char 'c' spec
// with one literal arg is supported (used by string-repeat patterns).
func binaryFormatBytes(spec string, args []string) ([]byte, bool) {
	if spec != "c" && spec != "b" {
		return nil, false
	}
	if len(args) != 1 {
		return nil, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil {
		return nil, false
	}
	return []byte{byte(n)}, true
}

// binaryFormatGoExpr renders a `[binary format SPEC ARGS...]` into a Go
// string expression. Integer specifiers (c/b/s/i) and byte-string specifiers
// (aN/a*) are supported; each contributes a `string(...)` fragment that is
// concatenated. A spec like "ccc" repeats the format char once per arg.
func binaryFormatGoExpr(spec string, args []string) (string, bool) {
	var parts []string
	ai := 0
	i := 0
	for i < len(spec) {
		ch := spec[i]
		switch ch {
		case 'c', 'b', 's', 'i', 'a':
		default:
			return "", false
		}
		var part string
		var next int
		var ok bool
		if ch == 'a' {
			// aN copies the first N bytes of the string arg; a* copies all.
			part, next, ok = binaryAFormatPart(spec, i, args, &ai)
		} else {
			part, next, ok = binaryIntFormatPart(spec, i, ch, args, &ai)
		}
		if !ok {
			return "", false
		}
		if part != "" {
			parts = append(parts, part)
		}
		i = next
	}
	if len(parts) == 0 {
		return "", false
	}
	if len(parts) == 1 {
		return parts[0], true
	}
	return "(" + strings.Join(parts, " + ") + ")", true
}

// binaryAFormatPart renders one 'a' spec unit starting at i: a* copies the
// whole string/blob argument (and must end the spec); aN copies the first N
// bytes. It returns the fragment, the next spec index, and whether the unit
// was valid.
func binaryAFormatPart(spec string, i int, args []string, ai *int) (string, int, bool) {
	if *ai >= len(args) {
		return "", 0, false
	}
	arg := args[*ai]
	*ai++
	if i+1 < len(spec) && spec[i+1] == '*' {
		i += 2
		if i < len(spec) {
			return "", 0, false
		}
		return fmt.Sprintf("string(tclBlobBytes(%s))", arg), i, true
	}
	// aN: copy N bytes (the arg is a string/blob).
	n := 0
	j := i + 1
	for j < len(spec) && spec[j] >= '0' && spec[j] <= '9' {
		n = n*10 + int(spec[j]-'0')
		j++
	}
	if n == 0 && j == i+1 {
		return "", 0, false
	}
	return fmt.Sprintf("string(tclBlobBytes(%s)[:%d])", arg, n), j, true
}

// binaryIntFormatPart renders one integer spec unit (c/b/s/i) starting at i,
// including the consume-all 'X*' form (which must end the spec). It returns
// the fragment, the next spec index, and whether the unit was valid.
func binaryIntFormatPart(spec string, i int, ch byte, args []string, ai *int) (string, int, bool) {
	if i+1 < len(spec) && spec[i+1] == '*' {
		// c* consumes ALL remaining args as bytes.
		if i+2 < len(spec) {
			return "", 0, false
		}
		var byteExprs []string
		for _, a := range args[*ai:] {
			byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s))", a))
		}
		if len(byteExprs) == 0 {
			return "", i + 2, true
		}
		return fmt.Sprintf("string([]byte{%s})", strings.Join(byteExprs, ", ")), i + 2, true
	}
	if *ai >= len(args) {
		return "", 0, false
	}
	a := args[*ai]
	*ai++
	var byteExprs []string
	switch ch {
	case 'c', 'b':
		byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s))", a))
	case 's':
		byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s)&0xff), byte((tclBlobInt(%s)>>8)&0xff)", a, a))
	case 'i':
		byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s)&0xff), byte((tclBlobInt(%s)>>8)&0xff), byte((tclBlobInt(%s)>>16)&0xff), byte((tclBlobInt(%s)>>24)&0xff)", a, a, a, a))
	}
	return fmt.Sprintf("string([]byte{%s})", strings.Join(byteExprs, ", ")), i + 1, true
}
