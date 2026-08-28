// Package main implements the tcl2go tool.
//
// This file collects TCL procedure definitions (const/error/counter/predicate/
// special/collation/query functions) and resolved variable references.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// walkCommands visits every non-empty command in a TCL command tree
// (recursing into braced sub-bodies) with the given callback.
func walkCommands(cmds [][]tcl.RawWord, fn func(cmd []tcl.RawWord)) {
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		fn(cmd)
		for i := 1; i < len(cmd); i++ {
			if cmd[i].Braced {
				walkCommands(tcl.ParseCommands(cmd[i].Text), fn)
			}
		}
	}
}

// collectUnzipDirs scans all TCL commands for procs that extract an archive
// into a fixed directory (`file mkdir DEST` followed by `exec ... -d DEST`),
// returning the set of destination directories. Extraction cannot run under
// the Go port, but later sections depend on the directory existing, so the
// external-tool guard skip emits os.MkdirAll for each collected dest.
func collectUnzipDirs(cmds [][]tcl.RawWord) map[string]bool {
	result := make(map[string]bool)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text != "proc" || len(cmd) < 4 {
			return
		}
		body := cmd[3].Text
		if !strings.Contains(body, "exec ") {
			return
		}
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "file mkdir ") {
				dest := strings.TrimSpace(strings.TrimPrefix(line, "file mkdir "))
				if dest != "" && strings.Contains(body, "-d "+dest) {
					result[dest] = true
				}
			}
		}
	})
	return result
}

// collectConstFuncs scans all TCL commands (including nested braced bodies)
// for constant-returning procs and returns a map of proc name → constant value.
func collectConstFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "proc" && len(cmd) >= 3 {
			if val := constantProcValue(cmd[2].Text); val != "" {
				result[cmd[1].Text] = val
			}
		}
	})
	return result
}

// collectRangeListFuncs scans all TCL commands for range-list procs (bodies
// like the vtabI.test `all_col_list` helper that build "c1 c2 ... cN" with a
// lappend loop) and returns a map of proc name → the generated list. Callers
// substitute the list value at transpile time (the body is data, not SQL).
func collectRangeListFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		// cmd: proc NAME PARAMS BODY — the body is at index 3.
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if val := rangeListProcValue(cmd[3].Text); val != "" {
				result[cmd[1].Text] = val
			}
		}
	})
	return result
}

// collectIdentityFuncs scans all TCL commands for identity procs
// (`proc NAME {x} {return $x}`) and returns a set of proc names. A registered
// SQL function calling such a proc returns its first argument unchanged
// (trustschema1's f1/f2/f3).
func collectIdentityFuncs(cmds [][]tcl.RawWord) map[string]bool {
	result := make(map[string]bool)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		// cmd: proc NAME PARAMS BODY — the body is at index 3.
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if identityProcValue(cmd[3].Text) {
				result[cmd[1].Text] = true
			}
		}
	})
	return result
}

// collectLIndexFuncs scans all TCL commands for list-index procs
// (`proc NAME {x} { lindex $x N }` — fts4growth.test's `second`) and returns
// the index per proc name. A registered SQL function calling such a proc
// returns the N-th element of its first argument split as a TCL list.
func collectLIndexFuncs(cmds [][]tcl.RawWord) map[string]int {
	result := make(map[string]int)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if idx, ok := lindexProcValue(cmd[3].Text); ok {
				result[cmd[1].Text] = idx
			}
		}
	})
	return result
}

// lindexProcValue parses a proc body of the form `lindex $x N` (optionally
// wrapped in `return ` / braces) and returns N.
func lindexProcValue(body string) (int, bool) {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	body = strings.TrimSpace(body)
	body = strings.TrimPrefix(strings.TrimSpace(body), "return ")
	body = strings.TrimSpace(body)
	fields := strings.Fields(body)
	if len(fields) < 3 || fields[0] != "lindex" {
		return 0, false
	}
	arg := fields[1]
	if !strings.HasPrefix(arg, "$") {
		return 0, false
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, false
	}
	return n, true
}

// collectStringMapFuncs scans all TCL commands for string-map procs
// (`proc NAME {x} { return [string map {OLD NEW ...} $x] }` —
// fts4intck1.test's `slang`) and returns a map of proc name → the TCL
// string-map pair list (e.g. "{th d} {e eh}"). A registered SQL function
// calling such a proc applies the replacements in order.
func collectStringMapFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if pairs := stringMapProcValue(cmd[3].Text); pairs != "" {
				result[cmd[1].Text] = pairs
			}
		}
	})
	return result
}

// stringMapProcValue parses a proc body of the form
// `return [string map {OLD NEW ...} $x]` (optionally braced) and returns the
// map pair list with the outer braces stripped ("th d e eh"). Returns ""
// when the body is not a single string-map return.
func stringMapProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	body = strings.TrimSpace(body)
	body = strings.TrimPrefix(strings.TrimSpace(body), "return ")
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "[string map ") {
		return ""
	}
	rest := strings.TrimSpace(strings.TrimPrefix(body, "[string map "))
	if len(rest) == 0 || rest[0] != '{' {
		return ""
	}
	// Find the matching close brace for the map list.
	depth := 0
	end := -1
	for i, c := range rest {
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
		}
		if depth == 0 {
			end = i
			break
		}
	}
	if end < 0 {
		return ""
	}
	mapContent := strings.TrimSpace(rest[1:end])
	if mapContent == "" {
		return ""
	}
	// The arg after the map list must be a $-variable (single arg proc).
	after := strings.TrimSpace(rest[end+1:])
	if !strings.HasPrefix(after, "$") {
		return ""
	}
	return mapContent
}

// identityProcValue reports whether a proc body returns its first argument
// unchanged: `return $x` (optionally braced) or `return $data` for a proc
// whose first parameter is named data (fts3comp1's comp/uncomp).
func identityProcValue(body string) bool {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(strings.ToLower(body), "return $") {
		return false
	}
	// Match `return $<ident>` where ident is any identifier.
	rest := strings.TrimSpace(body[len("return"):])
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "$"))
	return isValidGoIdent(rest)
}

// collectErrorFuncs scans all TCL commands for error-raising procs
// (`proc NAME {} { error "MESSAGE" }`) and returns a map of proc name → the
// message. A registered SQL function calling such a proc raises the message
// (regexp2.test's `proc sql_error {} { error "SQL error!" }` + `db func error
// sql_error`).
func collectErrorFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		// cmd: proc NAME PARAMS BODY — the body is at index 3.
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if msg := errorProcValue(cmd[3].Text); msg != "" {
				result[cmd[1].Text] = msg
			}
		}
	})
	return result
}

// errorProcValue extracts the message from an error-raising proc body like
// "{ error \"SQL error!\" }". Returns the unquoted message, or "" when the
// body is not a single `error "MSG"` command.
func errorProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	if !strings.HasPrefix(body, "error ") {
		return ""
	}
	msg := strings.TrimSpace(body[len("error "):])
	if len(msg) >= 2 && msg[0] == '"' && msg[len(msg)-1] == '"' {
		msg = msg[1 : len(msg)-1]
	}
	if msg == "" {
		return ""
	}
	return msg
}

// collectCounterFuncs scans all TCL commands for counter procs
// (`proc NAME {} { incr ::VAR }`) and returns a map of proc name → Go var.
func collectCounterFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "proc" && len(cmd) >= 3 {
			if val := counterProcValue(cmd[2].Text); val != "" {
				result[cmd[1].Text] = val
			}
		}
	})
	return result
}

// predicateProcValue extracts a Go comparison expression from a predicate proc
// body like "{ expr $x < 10 }" (used by check.test's `proc myfunc {x} {expr
// $x < 10}`). Returns a Go expression "arg < 10" with the parameter name arg,
// or "" when the body is not a single-variable comparison against a numeric
// literal.
func predicateProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	lower := strings.ToLower(body)
	if !strings.HasPrefix(lower, "expr ") {
		return ""
	}
	exprBody := strings.TrimSpace(body[len("expr "):])
	// exprBody must be: $X OP NUMBER  (X the single parameter, OP a comparison)
	if !strings.HasPrefix(exprBody, "$") {
		return ""
	}
	sp := strings.IndexAny(exprBody, " \t")
	if sp < 0 {
		return ""
	}
	param := exprBody[1:sp]
	_ = param // the parameter name must be a valid Go identifier, but only the comparison is emitted
	rest := strings.TrimSpace(exprBody[sp:])
	// Match: OP NUMBER, where OP is one of < <= > >= == != <>
	var op string
	for _, cand := range []string{"<=", ">=", "==", "!=", "<>", "<", ">"} {
		if strings.HasPrefix(rest, cand) {
			op = cand
			rest = strings.TrimSpace(rest[len(cand):])
			break
		}
	}
	if op == "" {
		return ""
	}
	if _, err := strconv.ParseFloat(rest, 64); err != nil {
		return ""
	}
	if op == "<>" {
		op = "!="
	}
	if op == "==" {
		op = "=="
	}
	return fmt.Sprintf("%s %s %s", "arg", op, rest)
}

// collectPredFuncs scans all TCL commands for predicate procs and returns a
// map of proc name → Go comparison expression.
func collectPredFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "proc" && len(cmd) >= 3 {
			if val := predicateProcValue(cmd[2].Text); val != "" {
				result[cmd[1].Text] = val
			}
		}
	})
	return result
}

// collectSpecialFuncs scans all TCL commands for test-infrastructure procs
// whose bodies the transpiler cannot inline but which have standard runtime
// Go equivalents: scramble (random shuffle), random_uuid (random hex string),
// hash1/hash2 (MD5 of sorted data columns, trans2.test). Returns a map of
// proc name → Go helper call template; "$data" in the template is replaced
// with the caller's argument at use sites.
func collectSpecialFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			switch cmd[1].Text {
			case "scramble":
				result["scramble"] = "tclScramble($data)"
			case "random_uuid":
				result["random_uuid"] = "tclRandomUUID()"
			case "hash1":
				result["hash1"] = "tclHashByIndex($data, 1)"
			case "hash2":
				result["hash2"] = "tclHashByIndex($data, 3)"
			case "columns":
				// e_createtable's `columns N` proc generates "c0, c1, ..., c(N-1)"
				// (a comma-separated column list for CREATE TABLE with many
				// columns).
				result["columns"] = "tclColumns($data)"
			case "wordset":
				// fts3ab's `wordset i` proc returns the quoted list of
				// {one two three four five} words whose bits are set in i
				// ("'one two three'").
				result["wordset"] = "tclWordset($data)"
			case "bin_to_hex":
				// blob.test decodes each SQL blob result through binary scan
				// and formats bytes as uppercase hexadecimal.
				result["bin_to_hex"] = "tclBinToHex($data)"
			}
		}
	})
	return result
}

// collationProcGo maps a TCL collation proc body to a Go closure expression
// suitable for db.RegisterCollation. It recognizes the collation procs used
// by the SQLite test suite:
//
//	text_collate / string_compare:   string compare $a $b            → BINARY
//	caseless:                        string compare -nocase $a $b    → NOCASE
//	reverse_sort:                    string compare $rhs $lhs        → reversed BINARY
//	backwards_collate:               reverse each string, then compare → REVERSED-STRING
//	hex_collate:                     hex-aware compare               → HEX
//	numeric_collate:                 numeric compare                 → NUMERIC
//
// Returns "" when the body is not a recognized collation proc.
func collationProcGo(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	// Normalize "return [string compare ...]" / "[list string compare ...]"
	// forms by extracting the inner string-compare invocation.
	lower := strings.ToLower(body)
	if strings.HasPrefix(lower, "return ") {
		lower = strings.TrimSpace(lower[len("return "):])
	}
	// Strip a leading command-substitution bracket so `[string compare $a $b]`
	// is detected like the bare form.
	scLower := strings.TrimPrefix(lower, "[")
	// string compare $a $b  /  string compare -nocase $a $b
	if strings.HasPrefix(lower, "[list string compare") {
		return collationCompare(lower[len("[list string compare"):])
	}
	if strings.HasPrefix(scLower, "string compare") {
		return collationStringCompare(lower, scLower[len("string compare"):])
	}
	if expr, ok := backwardsCollation(lower); ok {
		return expr
	}
	if expr, ok := hexCollation(lower); ok {
		return expr
	}
	if expr, ok := numericCollation(lower); ok {
		return expr
	}
	return ""
}

// backwardsCollation matches the backwards_collate body: reverse each string
// then compare.
func backwardsCollation(lower string) (string, bool) {
	if !strings.Contains(lower, "split $a {}") || !strings.Contains(lower, "split $b {}") || !strings.Contains(lower, "string compare") {
		return "", false
	}
	return `func(a, b string) int {
	ra, rb := "", ""
	for i := len(a) - 1; i >= 0; i-- { ra += string(a[i]) }
	for i := len(b) - 1; i >= 0; i-- { rb += string(b[i]) }
	return strings.Compare(ra, rb)
}`, true
}

// hexCollation matches the hex_collate body: both hex → numeric compare;
// hex-only sorts first; else BINARY.
func hexCollation(lower string) (string, bool) {
	if !strings.Contains(lower, "regexp") || !strings.Contains(lower, "scan $lhs %x") {
		return "", false
	}
	return `func(a, b string) int {
	aisHex, _ := regexp.MatchString("^(0x|)[1234567890abcdefABCDEF]+$", a)
	bisHex, _ := regexp.MatchString("^(0x|)[1234567890abcdefABCDEF]+$", b)
	if aisHex && bisHex {
		av, _ := strconv.ParseInt(strings.TrimPrefix(a, "0x"), 16, 64)
		bv, _ := strconv.ParseInt(strings.TrimPrefix(b, "0x"), 16, 64)
		if av < bv { return -1 }
		if av > bv { return 1 }
		return 0
	}
	if aisHex { return -1 }
	if bisHex { return 1 }
	return strings.Compare(a, b)
}`, true
}

// numericCollation matches the numeric_collate body: numeric compare.
func numericCollation(lower string) (string, bool) {
	if !strings.Contains(lower, "expr ($lhs>$rhs)") && !(strings.Contains(lower, "expr") && strings.Contains(lower, "$lhs") && strings.Contains(lower, "$rhs")) {
		return "", false
	}
	return `func(a, b string) int {
	if a == b { return 0 }
	af, aerr := strconv.ParseFloat(a, 64)
	bf, berr := strconv.ParseFloat(b, 64)
	if aerr == nil && berr == nil {
		if af < bf { return -1 }
		return 1
	}
	return strings.Compare(a, b)
}`, true
}

// collationCompare handles the `[list string compare ...]` form.
func collationCompare(rest string) string {
	rest = strings.TrimSpace(rest)
	nocase := strings.HasPrefix(rest, "-nocase")
	if nocase {
		return "func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) }"
	}
	return "func(a, b string) int { return strings.Compare(a, b) }"
}

// collationStringCompare handles the `string compare ...` form.
func collationStringCompare(lower, rest string) string {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "-nocase") {
		return "func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) }"
	}
	// Bare `string compare` (no explicit args) is TCL's string-compare
	// command applied to the two collation operands → BINARY.
	if strings.TrimSpace(strings.Trim(rest, "[]")) == "" {
		return "func(a, b string) int { return strings.Compare(a, b) }"
	}
	// string compare $a $b → BINARY; string compare $rhs $lhs → reversed.
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "["))
	rest = strings.TrimSuffix(rest, "]")
	if expr := collationFieldCompare(rest); expr != "" {
		return expr
	}
	// [string compare $a $b] as a braced command list.
	if strings.Contains(lower, "[string compare $a $b]") {
		return "func(a, b string) int { return strings.Compare(a, b) }"
	}
	if strings.Contains(lower, "[string compare -nocase $a $b]") {
		return "func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) }"
	}
	return ""
}

// collationFieldCompare matches the two-operand forms of a `string compare`
// collation body ($a $b / $lhs $rhs / $rhs $lhs), returning the Go closure
// expression or "" when the operands do not match a recognized form.
func collationFieldCompare(rest string) string {
	fields := strings.Fields(rest)
	if len(fields) == 2 && fields[0] == "$a" && fields[1] == "$b" {
		return "func(a, b string) int { return strings.Compare(a, b) }"
	}
	if len(fields) == 2 && fields[0] == "$b" && fields[1] == "$a" {
		return "func(a, b string) int { return -strings.Compare(a, b) }"
	}
	if len(fields) == 2 && fields[0] == "$lhs" && fields[1] == "$rhs" {
		return "func(a, b string) int { return strings.Compare(a, b) }"
	}
	if len(fields) == 2 && fields[0] == "$rhs" && fields[1] == "$lhs" {
		return "func(a, b string) int { return -strings.Compare(a, b) }"
	}
	return ""
}

// collectCollateFuncs scans all TCL commands for collation procs (recognized
// by collationProcGo) and returns a map of proc name → Go closure expression.
func collectCollateFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		// cmd = [proc, NAME, {args}, {body}]
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if val := collationProcGo(cmd[3].Text); val != "" {
				result[cmd[1].Text] = val
			}
		}
	})
	return result
}

// collectCollateDtorVars scans all TCL commands for sqlite3_create_collation_v2
// registrations and returns a map of collation name → Go destructor counter
// var (the `incr ::VAR` in the destructor body, possibly via a $var holding a
// [list incr ::VAR]). This pre-scan runs before processing so destructor
// tracking is available to every do_test body regardless of order.
func collectCollateDtorVars(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text == "sqlite3_create_collation_v2" && len(cmd) >= 5 {
			collName := strings.TrimSpace(cmd[2].Text)
			dtor := strings.TrimSpace(cmd[4].Text)
			if incrVar := counterProcValue(dtor); incrVar != "" && collName != "" {
				result[strings.ToUpper(collName)] = incrVar
			}
		}
	})
	return result
}

// queryProcValue extracts the SQL from a query-proc body like
// "{ return [db eval {SELECT count(*), md5sum(x) FROM t3}] }" (trans.test's
// `proc signature {}`). Returns the SQL text, or "" when the body is not a
// single db-eval query.
func queryProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	lower := strings.ToLower(body)
	if !strings.HasPrefix(lower, "return [db eval ") {
		return ""
	}
	rest := strings.TrimSpace(body[len("return [db eval "):])
	// rest is either {SQL}] or "SQL"] — strip the trailing ] and the
	// surrounding braces/quotes.
	rest = strings.TrimSuffix(rest, "]")
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && ((rest[0] == '{' && rest[len(rest)-1] == '}') || (rest[0] == '"' && rest[len(rest)-1] == '"')) {
		rest = rest[1 : len(rest)-1]
	}
	if strings.TrimSpace(rest) == "" {
		return ""
	}
	return rest
}

// collectQueryFuncs scans all TCL commands for query procs (`proc NAME {} {
// return [db eval {SQL}] }`) and returns a map of proc name → SQL.
func collectQueryFuncs(cmds [][]tcl.RawWord) map[string]string {
	result := make(map[string]string)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		// cmd: proc NAME PARAMS BODY
		if cmd[0].Text == "proc" && len(cmd) >= 4 {
			if val := queryProcValue(cmd[3].Text); val != "" {
				result[cmd[1].Text] = val
			}
		}
	})
	return result
}

func collectRefVars(src string) []string {
	var names []string
	seen := make(map[string]bool)
	pos := 0
	for pos < len(src) {
		if src[pos] != '$' {
			pos++
			continue
		}
		name, next, ok := scanTCLVarRef(src, pos)
		if ok && name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		pos = next
	}
	return names
}
