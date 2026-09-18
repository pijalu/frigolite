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
	appends int
	reads   bool
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
func classifyAccumulators(lines, codeOf []string) map[string]bool {
	accs := map[string]*accVar{}
	var order []string
	for i := range lines {
		code := codeOf[i]
		if isBlank(code) || isDeclLine(code, accs, &order) || isWriteLine(code, accs, order) || isSuppressLine(code) {
			continue
		}
		// Any other occurrence of a candidate var is a read.
		markReads(accs, order, code, "")
	}
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
func isWriteLine(code string, accs map[string]*accVar, order []string) bool {
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
			// A self-referencing assignment (`set sql "$sql more"`) reads
			// the var; a plain value assignment is a pure write.
			if identIn(val, v) {
				a.reads = true
			}
		}
	}
	// The right-hand side may read OTHER candidates.
	markReads(accs, order, rhs, v)
	return true
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

// qualifiedSet returns the candidates eligible for the rewrite: appended at
// least once (a Builder only pays off for repeated appends) and never read.
func qualifiedSet(accs map[string]*accVar, order []string) map[string]bool {
	rewrite := map[string]bool{}
	for _, v := range order {
		if a := accs[v]; a.appends > 0 && !a.reads {
			rewrite[v] = true
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
func rewriteAccumulators(lines, codeOf []string, rewrite map[string]bool) string {
	var b strings.Builder
	for i, ln := range lines {
		code := codeOf[i]
		comment := goLineComment(ln)
		if v := declStringName(code); v != "" && rewrite[v] {
			b.WriteString(fmt.Sprintf("\tvar %s strings.Builder%s\n", v, comment))
			continue
		}
		if out, ok := rewriteWrite(code, comment, rewrite); ok {
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
func rewriteWrite(code, comment string, rewrite map[string]bool) (string, bool) {
	v, rhs, ok := splitAssign(code)
	if !ok || !rewrite[v] {
		return "", false
	}
	op, val := splitOp(rhs)
	if op == "=" {
		out := fmt.Sprintf("\t%s.Reset()%s\n", v, comment)
		if val != `""` {
			out += fmt.Sprintf("\t%s.WriteString(%s)%s\n", v, val, comment)
		}
		return out, true
	}
	return fmt.Sprintf("\t%s.WriteString(%s)%s\n", v, val, comment), true
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
