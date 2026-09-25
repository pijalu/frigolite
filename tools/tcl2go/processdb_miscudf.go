// Package main implements the tcl2go tool.
//
// This file recognizes the misc test-suite UDF proc-body shapes
// (tkt3718's f1/f2 recurse-SQL procs, filefmt's a_string counter proc,
// rollback2's format proc) and emits the equivalent RegisterFunction calls.
package main

import (
	"strings"
)

// emitMiscRecurseSQLUDF detects the tkt3718.test proc body shapes (f1/f2)
// and the filefmt.test a_string shape, and emits an equivalent
// RegisterFunction:
//
//	f2: {set a [lindex $args 0]; if {$a == "three"} { error "Three!!" };
//	    return $a}  →  identity UDF with "three" → error("Three!!")
//	f1: {set a [lindex $args 0]; catch { db eval {SELECT f2($a)} } msg;
//	    set msg}     →  recurse-and-return UDF (DB->Query SELECT f2($a),
//	                     return first row cell or error message)
//
// Returns true when a recognized body matched and an emission was emitted.
func (tp *transpiler) emitMiscRecurseSQLUDF(name, procName string) bool {
	body, ok := tp.miscUDFBody(procName)
	if !ok {
		return false
	}
	if tp.emitLiteralDBEvalUDF(name, procName, body) {
		return true
	}
	return tp.emitNamedRecurseUDF(name, procName, body)
}

// miscUDFBody returns the trimmed, de-braced proc body for procName.
func (tp *transpiler) miscUDFBody(procName string) (string, bool) {
	if tp.procBodies == nil {
		return "", false
	}
	body, ok := tp.procBodies[procName]
	if !ok {
		return "", false
	}
	body = strings.TrimSpace(body)
	// Strip the outer braces if present.
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	return body, true
}

// emitNamedRecurseUDF handles the f2 / f1 / a_string proc-body shapes.
func (tp *transpiler) emitNamedRecurseUDF(name, procName, body string) bool {
	if tp.emitF2ShapeUDF(name, procName, body) {
		return true
	}
	if tp.emitF1ShapeUDF(name, procName, body) {
		return true
	}
	return tp.emitAStringUDF(name, procName, body)
}

// emitF2ShapeUDF handles the f2 shape: ... if {$a == "three"} { error
// "Three!!" } ... return $a.
func (tp *transpiler) emitF2ShapeUDF(name, procName, body string) bool {
	if !strings.EqualFold(name, "f2") || !strings.EqualFold(procName, "f2") ||
		!strings.Contains(body, `error "Three!!"`) || !strings.Contains(body, "return $a") {
		return false
	}
	tp.emitLine("// db func f2 f2 (tkt3718 — identity with 'three' → error(\"Three!!\"))")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\ta := function.ValueText(args[0])")
	tp.emitLine("\tif a == \"three\" { return nil, fmt.Errorf(\"Three!!\") }")
	tp.emitLine("\treturn a, nil")
	tp.emitLine("}, 1, 1)")
	return true
}

// emitF1ShapeUDF handles the f1 shape: ... catch { db eval {SELECT f2($a)} }
// msg; set msg.
func (tp *transpiler) emitF1ShapeUDF(name, procName, body string) bool {
	if !strings.EqualFold(name, "f1") || !strings.EqualFold(procName, "f1") ||
		!strings.Contains(body, "SELECT f2(") || !strings.Contains(body, "catch") ||
		!strings.Contains(body, "db eval") {
		return false
	}
	tp.emitLine("// db func f1 f1 (tkt3718 — recursive db eval SELECT f2($a), returns row cell or error msg)")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\ta := function.ValueText(args[0])")
	tp.emitLine("\tq := fmt.Sprintf(\"SELECT f2(%%s)\", sqlLiteral(a))")
	tp.emitLine("\tr := db.Query(q)")
	tp.emitLine("\tif r.Error != nil { return r.Error.Error(), nil }")
	tp.emitLine("\tif len(r.Rows) == 0 || len(r.Rows[0]) == 0 { return nil, nil }")
	tp.emitLine("\treturn r.Rows[0][0], nil")
	tp.emitLine("}, 1, 1)")
	return true
}

// emitAStringUDF handles the a_string shape (filefmt.test): {incr
// ::a_string_counter; string range [string repeat "${::a_string_counter}." $n]
// 1 $n} → counter-suffixed string of length n (tclAString implements the
// counter + repeat/range).
func (tp *transpiler) emitAStringUDF(name, procName, body string) bool {
	if !strings.EqualFold(procName, "a_string") ||
		!strings.Contains(body, "a_string_counter") || !strings.Contains(body, "string repeat") {
		return false
	}
	tp.emitLine("// db func a_string a_string (filefmt — counter-suffixed string)")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return \"\", nil }")
	tp.emitLine("\tn := tclToInt(tclStr(args[0]))")
	tp.emitLine("\treturn tclAString(&a_string_counter, n), nil")
	tp.emitLine("}, 1, 1)")
	return true
}

// emitLiteralDBEvalUDF handles the generic literal-SQL db-eval proc body:
// the body is exactly `catch {db eval {SQL}}` or `db eval {SQL}` with a
// literal SQL string (no $vars, single statement). The UDF executes the SQL
// on the same connection re-entrantly: TCL's catch swallows the error
// (tkt-f777251dc7a's force_rollback: INSERT OR ROLLBACK mid-statement aborts
// the enclosing statement with "abort due to ROLLBACK"), while the bare form
// propagates it (tkt-f777251dc7a's ins: INSERT INTO t3 from a SELECT scan).
func (tp *transpiler) emitLiteralDBEvalUDF(name, procName, body string) bool {
	sqlText, swallow, ok := literalDBEvalProcBody(body)
	if !ok {
		return false
	}
	tp.emitLine("// db func %s %s (literal-SQL db-eval UDF%s)", name, procName, map[bool]string{true: ", catch form", false: ""}[swallow])
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	if swallow {
		tp.emitLine("\t%s.Exec(%q)", tp.dbVar, sqlText)
	} else {
		tp.emitLine("\tif r := %s.Exec(%q); r.Error != nil { return nil, r.Error }", tp.dbVar, sqlText)
	}
	tp.emitLine("\treturn nil, nil")
	tp.emitLine("}, 0, -1)")
	return true
}

// stripOneBraced strips one balanced {...} layer from the start of s and
// returns the content. A proc body stored via raw word text can be missing
// its final closing brace (lexer artifact on nested braced words —
// tkt-f777251dc7a's `catch {db eval {...}}` stored with one trailing "}"),
// so a single unclosed open brace is tolerated as the word's terminator.
func stripOneBraced(s string) (string, bool) {
	if len(s) == 0 || s[0] != '{' {
		return "", false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	if depth == 1 {
		return s[1:], true
	}
	return "", false
}

// literalDBEvalProcBody recognizes a TCL proc body whose entire content is a
// single `db eval {SQL}` command, optionally wrapped in `catch {...}`, where
// SQL is a literal script (no variable references). It returns the SQL text,
// whether the call was error-swallowing (catch), and whether the body matched.
func literalDBEvalProcBody(body string) (sqlText string, swallow bool, ok bool) {
	body = strings.TrimSpace(body)
	if body == "catch" || strings.HasPrefix(body, "catch ") {
		swallow = true
		inner, ok2 := stripOneBraced(strings.TrimSpace(body[len("catch"):]))
		if !ok2 {
			return "", false, false
		}
		body = strings.TrimSpace(inner)
	}
	if !strings.HasPrefix(strings.ToLower(body), "db eval ") {
		return "", false, false
	}
	rest := strings.TrimSpace(body[len("db eval "):])
	inner, ok2 := stripOneBraced(rest)
	if !ok2 {
		return "", false, false
	}
	sqlText = strings.TrimSpace(inner)
	if sqlText == "" || strings.Contains(sqlText, "$") {
		return "", false, false
	}
	return sqlText, swallow, true
}

// emitFormatFunction recognizes `proc NAME {v} { format FMT $v }` — a
// single printf-style format command over one integer argument (rollback2's
// int2hex: format %.2X $i) — and emits the equivalent Go closure.
func (tp *transpiler) emitFormatFunction(name, procName string) bool {
	if tp.procBodies == nil || name == "" || procName == "" {
		return false
	}
	body, ok := tp.procBodies[procName]
	if !ok {
		return false
	}
	verb, ok := formatProcVerb(body)
	if !ok {
		return false
	}
	tp.emitLine("// db func %s %s (format %s)", name, procName, verb)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\tn, err := strconv.ParseInt(strings.TrimSpace(tclStr(args[0])), 0, 64)")
	tp.emitLine("\tif err != nil { return nil, err }")
	tp.emitLine("\treturn fmt.Sprintf(%q, n), nil", verb)
	tp.emitLine("}, 1, 1)")
	return true
}

// formatProcVerb extracts the printf verb of a `format FMT $v` proc body and
// reports whether the verb is one of the supported integer forms.
func formatProcVerb(body string) (string, bool) {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	fields := strings.Fields(body)
	if len(fields) != 3 || !strings.EqualFold(fields[0], "format") || !strings.HasPrefix(fields[2], "$") {
		return "", false
	}
	switch fields[1] {
	case "%.2X", "%02X", "%X", "%x", "%d", "%o":
		return fields[1], true
	}
	return "", false
}
