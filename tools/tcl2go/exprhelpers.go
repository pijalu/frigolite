// Package main implements the tcl2go tool.
//
// This file contains expression helpers used by cmdExpr: capability guards,
// word splitting, and simple expr-to-Go conversion.
package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// unsupportedCapabilities lists TCL ifcapable feature names that friglolite
// does NOT implement. Bodies guarded by these capabilities are skipped at
// transpile time (the generated test does not run them), matching SQLite's
// #ifdef build-selection. Everything else is treated as supported.
var unsupportedCapabilities = map[string]bool{
	// SQLite default build omits SQLITE_ENABLE_MEMORY_MANAGEMENT.
	"memorymanage": true,
	// rtree/rtree_i32 ARE supported natively (P6.RTREE); ifcapable rtree
	// bodies now transpile like any other feature.
	"json1": true, // JSON1 extension not supported
	// load_ext: the C-API loadable-extension mechanism (sqlite3_load_extension
	// / load_extension() SQL function — dlopen of a .so/.dll). A pure-Go engine
	// cannot dlopen C shared libraries; loadext/loadext2.test are guarded by
	// `ifcapable !load_ext { finish_test return }` and skip entirely, matching
	// SQLite builds compiled with SQLITE_OMIT_LOAD_EXTENSION.
	"load_ext": true, // C-API dlopen extension loading not supported
	// windowfunc: window functions ARE supported by the engine (P4.WINDOW);
	// removing it from this map makes `ifcapable !windowfunc { finish_test
	// return }` guard bodies skip, so the window test files actually run.
	"icu":     true, // ICU collation not supported
	"session": true, // session extension not supported
	"rbu":     true, // RBU extension not supported
	"zipfile": true, // zipfile extension not supported
	// stat4: the sqlite_stat4 histogram/sampling machinery. Frigolite creates
	// the sqlite_stat4 table (so ANALYZE's stat4 introspection matches the
	// table shape) but does not populate sample histograms; tests guarded by
	// ifcapable stat4 assert stat4 CONTENTS (test_decode(sample), nLt/nDLt
	// histograms, range row-count estimates) and are skipped.
	"stat4": true,
	// hiddencolumns: the __hidden__-prefixed column hack is a test-only
	// SQLite build option (hidden columns in real tables allow INSERT with
	// fewer values). Frigolite treats __hidden__-prefixed columns as hidden
	// for SELECT * expansion but does not implement the INSERT-count
	// exemption; standard SQLite builds do not enable this option either.
	"hiddencolumns": true,
	// Ordered-set aggregate syntax (WITHIN GROUP) is not parsed by the engine;
	// the percentile/percentile_cont/percentile_disc/median functions themselves
	// ARE implemented (percentile.test exercises them in the non-ifcapable
	// blocks).
	"ordered_set_aggregates": true,
	// api_armor: SQLITE_ENABLE_API_ARMOR is a test-only build option that
	// makes sqlite3_blob_open etc. return SQLITE_MISUSE on bad arguments.
	// Standard builds do not enable it; the api_armor-guarded bodies (blob
	// misuse tests in e_blobopen) are skipped.
	"api_armor": true,
	// shared_cache: SQLITE_ENABLE_SHARED_CACHE is a build option enabling
	// multi-connection shared-cache locking. Frigolite does not implement
	// shared cache or cross-connection table locks (multi-connection locking
	// is DEFERRED); the guarded sections (incrblob2-5.*) are skipped.
	"shared_cache": true,
	// unlock_notify: sqlite3_unlock_notify() C API (SQLITE_ENABLE_UNLOCK_
	// NOTIFY build option) — cross-connection unlock callbacks. notify1/
	// notify2/notify3.test are guarded by
	// `ifcapable !unlock_notify||!shared_cache { finish_test ; return }`,
	// so adding it here makes those files exit at the guard, exactly like a
	// SQLite build compiled without the feature.
	"unlock_notify": true,
	// utf16: UTF-16 database encoding (PRAGMA encoding=UTF16be/le). The
	// engine stores text as UTF-8 only; UTF-16-capable builds of SQLite run
	// extra badutf tests asserting UTF-16 byte patterns (hex('%80') = 0080).
	// Frigolite's PRAGMA encoding accepts only UTF-8, so the guarded bodies
	// (badutf-1.10+) are skipped.
	"utf16": true,
	// debug: SQLITE_DEBUG build option. Debug builds expose extra test-harness
	// functions like utf8_to_utf8 (badutf2-5.1.x asserts the UTF-8 round-trip
	// of a hex byte sequence) that are not part of the public engine.
	"debug": true,
	// rtree_int_only: SQLITE_RTREE_INT_ONLY compile flag (integer-coordinate
	// r-tree variant). Frigolite ships both coordinate flavors as modules
	// instead, so guarded files (rtree9.test) take their skip path exactly
	// like a C build without the flag.
	"rtree_int_only": true,
	// lock_proxy_pragmas / prefer_proxy_locking: the SQLite proxy-locking
	// build option (PRAGMA lock_proxy_file, host-id based cross-process locking
	// via a shared proxy lock file). Frigolite has no proxy-locking layer; the
	// lock6.test body is entirely gated behind
	// `ifcapable lock_proxy_pragmas&&prefer_proxy_locking`, so marking it
	// unsupported drops the whole body — exactly how a stock SQLite build (no
	// SQLITE_ENABLE_LOCKING_STYLE proxy support) runs lock6 as a no-op.
	"lock_proxy_pragmas":  true,
	"prefer_proxy_locking": true,
}

// ifcapableGuardFires reports whether an `ifcapable GUARD` condition is TRUE
// for frigolite (i.e. the body would run at TCL time): plain NAME fires when
// the capability IS present; negated !NAME fires when it is absent. Combined
// expressions joined by & or | are checked against the first operand only
// (the corpus uses simple forms like "trigger&&tempdb").
func ifcapableGuardFires(guard string) bool {
	guard = strings.TrimSpace(guard)
	if guard == "" {
		return false
	}
	neg := strings.HasPrefix(guard, "!")
	name := strings.TrimPrefix(guard, "!")
	// Combined expressions are rare; handle & and | naively by checking the
	// first operand (the tests use simple forms like "trigger&&tempdb").
	if strings.ContainsAny(name, "&|") {
		name = strings.FieldsFunc(name, func(r rune) bool { return r == '&' || r == '|' })[0]
	}
	unsupported := unsupportedCapabilities[strings.ToLower(name)]
	// Plain X fires when supported (!unsupported); negated fires when unsupported.
	return unsupported == neg
}

// tclCmdWords tokenizes a TCL command line (the text inside a [ ... ]
// command substitution) into its argument words, preserving braced-word
// boundaries. Unlike strings.Fields, a braced word containing whitespace
// (e.g. {, } in [join $cols {, }]) stays one argument — its inner text is
// returned without the braces, matching TCL substitution semantics.
func tclCmdWords(cmdText string) []string {
	cmds := tcl.ParseCommands(cmdText)
	if len(cmds) == 0 {
		return nil
	}
	words := cmds[0]
	out := make([]string, 0, len(words))
	for _, w := range words {
		switch {
		case w.Braced:
			out = append(out, w.Text)
		case w.Quoted:
			out = append(out, tclUnescapeQuoted(w.Text))
		default:
			out = append(out, w.Text)
		}
	}
	return out
}

// sanitizeTCLComment returns a Go-safe comment string.
func sanitizeTCLComment(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	// Strip bytes that are not valid UTF-8 so the emitted Go comment compiles
	// (raw multi-byte sequences in TCL strings break the Go source).
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('?')
			i++
			continue
		}
		b.WriteRune(r)
		i += size
	}
	s = b.String()
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// exprHasFuncCall reports whether a TCL expr contains a function call
// (identifier followed by '('), e.g. int(...), rand(), log($x), pow(...).
// Such exprs are rendered as native Go code by exprCmdToGo; pure arithmetic
// exprs use the runtime float-aware evaluator instead.
// exprCmdToGo converts a TCL expr containing [cmd] command substitutions
// (e.g. {$i*100 + [string length $word]}) into a Go int expression.
// Each [cmd] is resolved via cmdExpr (string length → len(), etc.) and each
// $var reference becomes toInt(var). Returns "" when the expr contains a
// command that cannot be resolved to a Go expression.
func (tp *transpiler) exprCmdToGo(expr string) (string, bool) {
	// Replace [cmd ...] substitutions first (innermost-first would be ideal;
	// the corpus uses flat single-level [string length $x] forms).
	var b strings.Builder
	i := 0
	for i < len(expr) {
		if expr[i] == '[' {
			repl, next, ok := tp.exprCmdToGoBracket(expr, i)
			if !ok {
				return "", false
			}
			b.WriteString("(" + repl + ")")
			i = next
			continue
		}
		if expr[i] == '$' {
			repl, next, ok := tp.exprVarToGo(expr, i)
			if !ok {
				return "", false
			}
			b.WriteString("toInt(" + repl + ")")
			i = next
			continue
		}
		b.WriteByte(expr[i])
		i++
	}
	s := b.String()
	if s == "" || !isSimpleArithExpr(s) {
		return "", false
	}
	// Replace TCL rand() with the deterministic tclRand() helper so tests
	// that build self-consistent data with rand (cse.test) compile and stay
	// consistent between the SQL-building and answer-building code.
	s = strings.ReplaceAll(s, "rand()", "tclRand()")
	return coerceRandFloatOperands(s), true
}

// coerceRandFloatOperands rewrites `tclRand()*X` multiplications so the
// emitted Go expression compiles: TCL's rand() produces a float, so every
// multiplier must be float64 too. Numeric literals already mix cleanly with
// floats; variables / toInt(...) / parenthesized arithmetic are wrapped in
// float64(...) (rtree2.test: int(rand()*($nDim*2+1))-1).
func coerceRandFloatOperands(s string) string {
	const key = "tclRand()*"
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		b.WriteString(key)
		s = s[i+len(key):]
		rest := strings.TrimLeft(s, " ")
		b.WriteString(strings.Repeat(" ", len(s)-len(rest)))
		if rest == "" {
			break
		}
		end := arithOperandEnd(rest)
		tok := rest[:end]
		if isPlainNumber(tok) {
			b.WriteString(tok)
		} else {
			b.WriteString("float64(" + tok + ")")
		}
		s = rest[end:]
	}
	return b.String()
}

// arithOperandEnd returns the index just past the leading arithmetic operand
// of s: either a balanced parenthesis group, an identifier/function call up
// to the first top-level operator, or a numeric run.
func arithOperandEnd(s string) int {
	if strings.HasPrefix(s, "(") {
		depth := 0
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return len(s)
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= '0' && c <= '9':
			continue
		default:
			return i
		}
	}
	return len(s)
}

func isPlainNumber(tok string) bool {
	if tok == "" {
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// exprCmdToGoBracket handles a [cmd ...] substitution at position i (the
// opening bracket), returning the resolved Go expression and the index after
// the closing bracket.
func (tp *transpiler) exprCmdToGoBracket(expr string, i int) (string, int, bool) {
	// Find matching ] (no nesting in the corpus patterns).
	end := strings.IndexByte(expr[i+1:], ']')
	if end < 0 {
		return "", 0, false
	}
	cmdText := expr[i+1 : i+1+end]
	// Special-case [string length $x] and [llength $x] to bare int
	// expressions (len(x) / tclLLength(x)), since cmdExpr's string
	// renderings are strconv.Itoa(...) (strings, not usable in int
	// arithmetic).
	if sl, ok := stringLengthExpr(cmdText); ok {
		return sl, i + end + 2, true
	}
	if ll, ok := listLengthExpr(cmdText); ok {
		return ll, i + end + 2, true
	}
	// [file size PATH] — the bare int file size (usable in int arithmetic
	// like `[file size test.db]/1024`).
	if fs, ok := fileSizeExpr(cmdText); ok {
		return fs, i + end + 2, true
	}
	goCmd := tp.cmdExpr(cmdText)
	// cmdExpr returns the raw quoted command text when unresolvable.
	if goCmd == "" || goCmd == fmt.Sprintf("%q", cmdText) {
		return "", 0, false
	}
	return goCmd, i + end + 2, true
}

// exprVarToGo handles a $var reference at position i, returning the Go
// variable name and the index after the variable name.
func (tp *transpiler) exprVarToGo(expr string, i int) (string, int, bool) {
	j := i + 1
	for j < len(expr) && isVarChar(expr[j]) {
		j++
	}
	name := expr[i+1 : j]
	// Resolve array element references through tracked TCL array bindings.
	// This path is needed inside native expr rendering, not only string
	// rendering (for example, [llength $Q(pri_queue)]).
	if j < len(expr) && expr[j] == '(' {
		end := strings.IndexByte(expr[j+1:], ')')
		if end < 0 {
			return "", 0, false
		}
		key := expr[j+1 : j+1+end]
		base := expr[i+1 : j]
		return tp.arrayLookupExpr(base, key), j + end + 2, true
	}
	goVar := tclVarToGo(name)
	if goVar == "" {
		return "", 0, false
	}
	return goVar, j, true
}

// isSimpleArithExpr reports whether s is a simple arithmetic expression
// (letters, digits, operators, parens, underscores, dots, commas).
func isSimpleArithExpr(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isSimpleArithChar(s[i]) {
			return false
		}
	}
	return true
}

// isSimpleArithChar reports whether c is allowed in a simple arithmetic
// expression (letters, digits, operators, parens, underscores, dots, commas).
func isSimpleArithChar(c byte) bool {
	if strings.ContainsRune(" \t+-*/()._.,", rune(c)) {
		return true
	}
	return isASCIIAlphaNum(c)
}

// isASCIIAlphaNum reports whether c is an ASCII letter or digit.
func isASCIIAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// exprCmdCompare handles `expr [cmd ...] OP N` where the command resolves to
// a Go string expression. If the trailing N is a string literal ("..."), the
// compare is a Go string == (e.g. `expr {[catchsql {PRAGMA integrity_check}] == "0 ok"}`,
// journal2.test 1.17/1.20 — the catchsql result is a list string, never a number).
// If N is numeric or a $var reference, the compare uses toInt(goCmd) vs N
// (e.g. `expr [sqlite3_stmt_status $stmt $id 0]>0`, dbstatus.test 5.5.x). It
// returns a Go expression producing the TCL truth string "1"/"0" and whether
// the form was recognized.
func (tp *transpiler) exprCmdCompare(expr string) (string, bool) {
	// Find the single [cmd ...] and the trailing OP N (or N OP cmd).
	open := strings.Index(expr, "[")
	closeIdx := strings.Index(expr[open+1:], "]")
	if open < 0 || closeIdx < 0 {
		return "", false
	}
	close := open + 1 + closeIdx
	cmdText := expr[open+1 : close]
	rest := strings.TrimSpace(expr[close+1:])
	if rest == "" {
		return "", false
	}
	// Match OP N (e.g. ">0", "== 1", `== "0 ok"`).
	var op, num string
	for _, cand := range []string{">=", "<=", "==", "!=", ">", "<"} {
		if strings.HasPrefix(rest, cand) {
			op = cand
			num = strings.TrimSpace(strings.TrimPrefix(rest, cand))
			break
		}
	}
	if op == "" {
		return "", false
	}
	if num == "" {
		return "", false
	}
	goCmd := tp.cmdExpr(cmdText)
	if goCmd == "" || goCmd == fmt.Sprintf("%q", cmdText) {
		return "", false
	}
	cmp := map[string]string{">": " > ", "<": " < ", ">=": " >= ", "<=": " <= ", "==": " == ", "!=": " != "}[op]
	// String-literal RHS: command result is a list/string, compare as strings.
	// e.g. catchsql returns "0 {rows}" / "1 {msg}" — comparing to "0 ok" or
	// a literal list must stay a string compare.
	if len(num) >= 2 && num[0] == '"' && num[len(num)-1] == '"' {
		return fmt.Sprintf("tclBool01(%s %s %s)", goCmd, cmp, num), true
	}
	// Numeric or $var RHS: command holds a numeric string, use toInt semantics.
	numExpr := num
	if strings.HasPrefix(num, "$") {
		gv := tclVarToGo(strings.TrimPrefix(num, "$"))
		if isValidGoIdent(gv) {
			numExpr = "toInt(" + gv + ")"
		}
	}
	return fmt.Sprintf("tclBool01(toInt(%s) %s %s)", goCmd, cmp, numExpr), true
}

// listLengthExpr converts an "llength ..." command into a Go tclLLength()
// int expression. Returns ("", false) when cmdText is not an llength
// command or its operand is not resolvable.
func listLengthExpr(cmdText string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(cmdText))
	if len(fields) < 2 || fields[0] != "llength" {
		return "", false
	}
	if len(fields) != 2 || !strings.HasPrefix(fields[1], "$") {
		return "", false
	}
	goVar := tclVarToGo(strings.TrimPrefix(fields[1], "$"))
	if goVar == "" {
		return "", false
	}
	return "tclLLength(" + goVar + ")", true
}

// stringLengthExpr converts a "string length ..." command into a Go len()
// int expression. Returns ("", false) when cmdText is not a string-length
// command or its operand is not resolvable.
func stringLengthExpr(cmdText string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(cmdText))
	if len(fields) < 2 || fields[0] != "string" || fields[1] != "length" {
		return "", false
	}
	if len(fields) != 3 || !strings.HasPrefix(fields[2], "$") {
		return "", false
	}
	goVar := tclVarToGo(strings.TrimPrefix(fields[2], "$"))
	if goVar == "" {
		return "", false
	}
	return "len(" + goVar + ")", true
}

// fileSizeExpr renders `[file size PATH]` as the bare int file size
// expression (usable in int arithmetic like `[file size test.db]/1024`).
func fileSizeExpr(cmdText string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(cmdText))
	if len(fields) < 2 || fields[0] != "file" || fields[1] != "size" {
		return "", false
	}
	if len(fields) != 3 {
		return "", false
	}
	if strings.HasPrefix(fields[2], "$") {
		goVar := tclVarToGo(strings.TrimPrefix(fields[2], "$"))
		if goVar == "" {
			return "", false
		}
		return "tclFileSize(" + goVar + ")", true
	}
	return "tclFileSize(" + fmt.Sprintf("%q", fields[2]) + ")", true
}

// fileSizeArithExpr renders a TCL expr string containing `[file size PATH]`
// substitutions (e.g. `[file size test.db]/1024`) as a Go int expression by
// replacing each `[file size PATH]` with tclFileSize(PATH). Returns ("", false)
// when the expr contains anything beyond file-size substitutions and simple
// arithmetic.
func fileSizeArithExpr(exprStr string) (string, bool) {
	var b strings.Builder
	i := 0
	for i < len(exprStr) {
		if exprStr[i] == '[' {
			end := strings.IndexByte(exprStr[i+1:], ']')
			if end < 0 {
				return "", false
			}
			cmdText := exprStr[i+1 : i+1+end]
			fs, ok := fileSizeExpr(cmdText)
			if !ok {
				return "", false
			}
			b.WriteString(fs)
			i = i + end + 2
			continue
		}
		if !isSimpleArithChar(exprStr[i]) && !strings.ContainsRune(" /%*+-()<>=!", rune(exprStr[i])) {
			return "", false
		}
		b.WriteByte(exprStr[i])
		i++
	}
	s := b.String()
	if s == "" {
		return "", false
	}
	return s, true
}

func isVarStartChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == ':'
}

func isVarChar(c byte) bool {
	return isVarStartChar(c) || (c >= '0' && c <= '9')
}
