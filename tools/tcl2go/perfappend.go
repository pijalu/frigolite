package main

import (
	"fmt"
	"strings"
)

// P9.PERF.T1: amortized string accumulation for write-only accumulator vars.
//
// The TCL speed tests (speed1.test, speed1p.test, speed2.test) build one SQL
// batch string with tens of thousands of `append sql ...` commands inside a
// loop. TCL's append is amortized O(1) (in-place object growth), but the
// emitted Go `sql += ...` copies the whole accumulated string on every loop
// iteration — O(n²) byte copies that dominate those packages' wall clock
// (speed1 serial 29.4s → 0.34s with this transform; speed1p 445s → 10.8s).
//
// When the transpiler can PROVE a declared string var is write-only — only
// ever appended (`V += E`) or wholesale-assigned (`V = E`), never read — the
// accumulator is rewritten to a strings.Builder. The final value is
// identical; only the accumulation strategy changes. Vars with any other
// occurrence (a read site) are left untouched, so generated code for every
// other package stays byte-identical and only write-only accumulators
// (today: the speed-family batch strings) change.

// accVar records the write/read classification of one declared string var.
type accVar struct {
	appends       int
	listAppends   int   // `V = tclListAppend(V, ...)` self-appends
	badAssign     bool  // at least one non-empty wholesale assignment
	badAssignIdxs []int // their line indexes
	reads         bool
}

// amortizeStringAppends rewrites write-only string accumulators in an
// emitted test body to strings.Builder operations.
func amortizeStringAppends(body string) string {
	lines := strings.Split(body, "\n")
	codeOf := make([]string, len(lines))
	for i, ln := range lines {
		codeOf[i] = stripGoLineComment(ln)
	}
	rewrite := classifyAccumulators(lines, codeOf)
	if len(rewrite) == 0 {
		return body
	}
	return rewriteAccumulators(lines, codeOf, rewrite)
}

// classifyAccumulators finds plain-string vars that are only appended or
// wholesale-assigned (never read) and returns the set eligible for the
// strings.Builder rewrite.
func classifyAccumulators(lines, codeOf []string) map[string]string {
	accs := map[string]*accVar{}
	var order []string
	for i := range lines {
		code := codeOf[i]
		if isBlank(code) || isDeclLine(code, accs, &order) || isWriteLine(code, accs, order, i) || isSuppressLine(code) {
			continue
		}
		// Any other occurrence of a candidate var is a read.
		markReads(accs, order, code, "")
	}
	settleTerminalAssigns(lines, codeOf, accs, order)
	return qualifiedSet(accs, order)
}

// isBlank reports whether the code part of a line is empty.
func isBlank(code string) bool {
	return strings.TrimSpace(code) == ""
}

// isDeclLine records a `var V string` declaration as a candidate; it returns
// true when the line has that form.
func isDeclLine(code string, accs map[string]*accVar, order *[]string) bool {
	name := declStringName(code)
	if name == "" {
		return false
	}
	if _, ok := accs[name]; !ok {
		accs[name] = &accVar{}
		*order = append(*order, name)
	}
	return true
}

// isWriteLine classifies an assignment `V += ...` / `V = ...` for a
// candidate var (self-references on the right-hand side count as reads); it
// returns true when the line has the assignment form.
func isWriteLine(code string, accs map[string]*accVar, order []string, lineIdx int) bool {
	v, rhs, ok := splitAssign(code)
	if !ok {
		return false
	}
	if a := accs[v]; a != nil {
		op, val := splitOp(rhs)
		switch op {
		case "+=":
			a.appends++
			markReads(accs, order, val, v)
		case "=":
			// A TCL-lappend chain emits `V = tclListAppend(V, items...)`:
			// a self-append through the list helper. The non-target
			// arguments may read other candidates.
			if items, ok := splitListAppendCall(val, v); ok {
				a.listAppends++
				markReads(accs, order, items, v)
				break
			}
			// A self-referencing assignment (`set sql "$sql more"`) reads
			// the var; a plain value assignment is a pure write (a
			// non-empty one is unsafe for the list-builder rewrite UNLESS
			// it is terminal — nothing appends or reads the var after it —
			// in which case the dead store is emitted as a discard).
			if identIn(val, v) {
				a.reads = true
			} else if val != `""` {
				a.badAssign = true
				a.badAssignIdxs = append(a.badAssignIdxs, lineIdx)
			}
		}
	}
	// The right-hand side may read OTHER candidates.
	markReads(accs, order, rhs, v)
	return true
}

// splitListAppendCall recognizes `tclListAppend(V, items...)` where V is the
// accumulator itself (the emitted form of a TCL `lappend V items...`), and
// returns the items argument text.
func splitListAppendCall(val, v string) (string, bool) {
	call := "tclListAppend("
	if !strings.HasPrefix(val, call) || !strings.HasSuffix(val, ")") {
		return "", false
	}
	args := strings.TrimSpace(val[len(call) : len(val)-1])
	first, rest, ok := strings.Cut(args, ",")
	if !ok {
		return "", false
	}
	if strings.TrimSpace(first) != v {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// isSuppressLine recognizes the generated `_ = V ...` unused-suppression
// lines (neither read nor write for qualification purposes).
func isSuppressLine(code string) bool {
	s := strings.TrimSpace(code)
	rest, ok := strings.CutPrefix(s, "_ = ")
	if !ok {
		return false
	}
	name := rest
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		name = rest[:idx]
	}
	return isGoIdent(name)
}

// markReads marks every accumulator var (except the named exclusion — an
// assignment target on its own write line) occurring in text as read.
func markReads(accs map[string]*accVar, order []string, text, exclude string) {
	for _, v := range order {
		if v == exclude {
			continue
		}
		if !accs[v].reads && identIn(text, v) {
			accs[v].reads = true
		}
	}
}

// settleTerminalAssigns downgrades badAssign flags that are TERMINAL: no
// occurrence of the var in any later line (suppress lines excepted). The
// assigned value overwrites the built list in the original code and nothing
// reads it afterwards, so the rewrite may emit the dead store as a discard.
func settleTerminalAssigns(lines, codeOf []string, accs map[string]*accVar, order []string) {
	for _, v := range order {
		a := accs[v]
		if !a.badAssign {
			continue
		}
		terminal := true
		for _, idx := range a.badAssignIdxs {
			for i := idx + 1; i < len(lines); i++ {
				code := codeOf[i]
				if isSuppressLine(code) {
					continue
				}
				if w, rhs, ok := splitAssign(code); ok && w == v {
					op, val := splitOp(rhs)
					if _, isApp := splitListAppendCall(val, v); isApp || op == "+=" {
						// A chained self-append (or +=) consumes the stored
						// value: the assignment is not dead.
						terminal = false
					}
					// Any other wholesale write severs the dataflow — the
					// stored value is discarded, so stop scanning here.
					break
				}
				if identIn(code, v) {
					terminal = false
					break
				}
			}
			if !terminal {
				break
			}
		}
		if terminal {
			a.badAssign = false
		}
	}
}

// qualifiedSet returns the candidates eligible for the rewrite, keyed by
// kind ("string" for append-accumulated SQL batches, "list" for
// tclListAppend-accumulated TCL lists): appended at least once, never read,
// never mixed-kind, and without a non-empty wholesale assignment.
func qualifiedSet(accs map[string]*accVar, order []string) map[string]string {
	rewrite := map[string]string{}
	for _, v := range order {
		a := accs[v]
		if a.reads || a.badAssign {
			continue
		}
		switch {
		case a.appends > 0 && a.listAppends == 0:
			rewrite[v] = "string"
		case a.listAppends > 0 && a.appends == 0:
			rewrite[v] = "list"
		}
	}
	return rewrite
}

// rewriteAccumulators applies the strings.Builder rewrite for the named
// candidate vars: declarations become `var V strings.Builder`, appends
// become WriteString calls, and wholesale assignments become Reset
// (optionally followed by WriteString of the assigned value). Trailing
// newline parity with the original body is preserved (a strings.Split on
// "\n" leaves a final "" element exactly when the body ended with a
// newline).
func rewriteAccumulators(lines, codeOf []string, rewrite map[string]string) string {
	var b strings.Builder
	for i, ln := range lines {
		code := codeOf[i]
		comment := goLineComment(ln)
		if v := declStringName(code); v != "" && rewrite[v] != "" {
			decl := "strings.Builder"
			if rewrite[v] == "list" {
				decl = "*tclListBuilder"
			}
			indent := ln[:len(ln)-len(strings.TrimLeft(ln, "\t"))]
			b.WriteString(fmt.Sprintf("%svar %s %s%s\n", indent, v, decl, comment))
			continue
		}
		if out, ok := rewriteWrite(ln, code, comment, rewrite, i); ok {
			b.WriteString(out)
			continue
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	s := b.String()
	if len(lines) > 0 && lines[len(lines)-1] != "" {
		s = strings.TrimSuffix(s, "\n")
	}
	return s
}

// rewriteWrite emits the Builder form of one assignment line; ok is false
// when the line is not a write to a rewritten var.
func rewriteWrite(origLine, code, comment string, rewrite map[string]string, lineIdx int) (string, bool) {
	v, rhs, ok := splitAssign(code)
	if !ok || rewrite[v] == "" {
		return "", false
	}
	// Preserve the original line's indentation (the accumulator's writes are
	// often inside loop bodies).
	indent := origLine[:len(origLine)-len(strings.TrimLeft(origLine, "\t"))]
	op, val := splitOp(rhs)
	if rewrite[v] == "list" {
		// List builders: a wholesale "" assignment starts a fresh builder;
		// a self-append calls Append (which applies TCL bracing per item,
		// matching tclListAppend's fast path); a terminal non-empty
		// assignment overwrites the built list without ever reading it —
		// emit the dead store as a discard.
		if op == "=" && val == `""` {
			return fmt.Sprintf("%s%s = &tclListBuilder{}%s\n", indent, v, comment), true
		}
		if items, ok := splitListAppendCall(val, v); ok {
			return fmt.Sprintf("%s%s.Append(%s)%s\n", indent, v, items, comment), true
		}
		if op == "=" && val != `""` {
			return fmt.Sprintf("%s_ = %s%s\n", indent, val, comment), true
		}
		return "", false
	}
	if op == "=" {
		out := fmt.Sprintf("%s%s.Reset()%s\n", indent, v, comment)
		if val != `""` {
			out += fmt.Sprintf("%s%s.WriteString(%s)%s\n", indent, v, val, comment)
		}
		return out, true
	}
	return fmt.Sprintf("%s%s.WriteString(%s)%s\n", indent, v, val, comment), true
}

// splitOp splits an operator-with-RHS part into the operator and the
// trimmed right-hand side.
func splitOp(opRhs string) (op, val string) {
	if strings.HasPrefix(opRhs, "+=") {
		return "+=", strings.TrimSpace(opRhs[2:])
	}
	return "=", strings.TrimSpace(opRhs[1:])
}

// declStringName recognizes `var V string` (function-scope pre-declaration
// of a TCL var) and returns V, or "" when the line is not that form.
func declStringName(code string) string {
	s := strings.TrimSpace(code)
	rest, ok := strings.CutPrefix(s, "var ")
	if !ok {
		return ""
	}
	name, tail, ok := strings.Cut(rest, " ")
	if !ok || !isGoIdent(name) || strings.TrimSpace(tail) != "string" {
		return ""
	}
	return name
}

// splitAssign recognizes an assignment statement `V <op> ...` (op is += or
// =) written to a bare identifier at statement start, returning the var and
// the operator-with-RHS part. It rejects `_ = ...` boilerplate, `==`
// comparisons and tuple assignments.
func splitAssign(code string) (v, opRhs string, ok bool) {
	s := strings.TrimSpace(code)
	if s == "" || s[0] == '_' {
		return "", "", false
	}
	name, end := identAt(s, 0)
	if name == "" {
		return "", "", false
	}
	rest := strings.TrimSpace(s[end:])
	if strings.HasPrefix(rest, "+=") || (strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==")) {
		return name, rest, true
	}
	return "", "", false
}

// identAt returns the identifier starting at byte i and the offset just past
// it, or ("", i) when no identifier starts there.
func identAt(s string, i int) (string, int) {
	if i >= len(s) || !isIdentByte(s[i]) || (s[i] >= '0' && s[i] <= '9') {
		return "", i
	}
	j := i + 1
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	return s[i:j], j
}

// identIn reports whether name occurs in code as a standalone Go identifier
// outside string literals (comment text is assumed already stripped).
func identIn(code, name string) bool {
	inStr := false
	for i := 0; i < len(code); i++ {
		switch {
		case inStr:
			if code[i] == '\\' {
				i++
			} else if code[i] == '"' {
				inStr = false
			}
		case code[i] == '"':
			inStr = true
		case isIdentStart(code[i]):
			if tok, end := identAt(code, i); tok == name && end == i+len(name) {
				return true
			}
		}
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isGoIdent(s string) bool {
	if s == "" {
		return false
	}
	if !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return true
}

// stripGoLineComment removes a trailing // comment from one emitted line,
// honoring double-quoted string literals.
func stripGoLineComment(ln string) string {
	if idx := indexGoLineComment(ln); idx >= 0 {
		return ln[:idx]
	}
	return ln
}

// goLineComment returns the trailing // comment of a line ("" when none).
func goLineComment(ln string) string {
	if idx := indexGoLineComment(ln); idx >= 0 {
		return ln[idx:]
	}
	return ""
}

// indexGoLineComment returns the byte offset of a // comment start outside
// string literals, or -1.
func indexGoLineComment(ln string) int {
	inStr := false
	for i := 0; i < len(ln); i++ {
		if inStr {
			if ln[i] == '\\' {
				i++
			} else if ln[i] == '"' {
				inStr = false
			}
			continue
		}
		switch ln[i] {
		case '"':
			inStr = true
		case '/':
			if i+1 < len(ln) && ln[i+1] == '/' {
				return i
			}
		}
	}
	return -1
}
