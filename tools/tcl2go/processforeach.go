// Package main implements the tcl2go tool.
//
// This file handles TCL foreach loops.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// ---- Control flow handlers ----

func (tp *transpiler) processForeach(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	varNames := tp.parseVarList(args[0])
	rawList := stripListCommand(args[1].Text)
	isBracedList := args[1].Braced
	if args[1].Quoted {
		// A double-quoted foreach list processes TCL backslash escapes:
		// `foreach x "a\u00E9 b"` iterates the characters "aé b". Resolve
		// them (including nested \" and \uXXXX) before list splitting
		// (fts4umlaut.test: "Ha N\u1ed9i" is "Ha Nội").
		rawList = tclUnescapeQuoted(rawList)
	}
	listExpr := tp.resolveForeachListExpr(rawList, isBracedList)

	// foreach over a query result — [db eval {SQL}] or [execsql {SQL}
	// [conn]] — emits a Go row loop. This MUST be tried before the
	// script-bodies bailout below: a query source like
	// [execsql {pragma database_list}] also contains the text
	// "execsql {" but names a query, not script bodies (pragma.test 6.1).
	if tp.emitDBEvalForeach(args, varNames) {
		return
	}

	// A foreach whose list items are TCL SCRIPTS (multi-line braced bodies
	// containing execsql — fts3defer.test's `foreach {tn setup} "1 { ... }
	// 2 { ... }"`) cannot be executed: the transpiler has no runtime TCL
	// interpreter. Emit the loop as a comment so no assertions run against
	// an un-setup database.
	if len(varNames) >= 2 && strings.Contains(rawList, "execsql {") {
		tp.emitLine("// foreach %s (TCL script bodies; not transpiled)", sanitizeTCLComment(strings.Join(varNames, " ")))
		return
	}

	// foreach {k v} "array get ARR" — iterate a dynamic-key array's
	// key/value pairs (TCL's array-get idiom). The transpiler tracks such
	// arrays as Go maps (arrayMapVars); emit a Go map range so the keys and
	// values are runtime values (fts4aa.test: foreach {q r} "array get
	// fts4aa_res" { ... }).
	if len(varNames) == 2 && tp.emitArrayGetForeach(args, varNames, rawList) {
		return
	}

	// Record literal list values for foreach loop variables so a later
	// `eval $var` can inline each script's commands (backup.test's
	// foreach zOpenScript { ... } { eval $zOpenScript } pattern; fts4merge4's
	// `foreach {tn2 openclose} {1 {} 2 { db close ; sqlite3 db test.db }}`
	// grid). For a K-variable foreach over N literal values, variable i takes
	// the elements at indexes i, i+K, i+2K, ... (TCL's round-robin
	// assignment).
	tp.recordForeachLitValues(varNames, rawList, listExpr)
	// splitExpr, when non-empty, replaces the tclSplitList(listExpr) iteration
	// source: foreach x [split $var ""] iterates the CHARACTERS of a string
	// variable (TCL split with empty separator), which tclSplitList cannot
	// express (a character may itself be a space).
	splitExpr := splitListExpr(rawList)

	// foreach {v1 v2 ...} $list break — unpack the FIRST list element into the
	// variables (TCL destructuring idiom, e.g. trans2.test's
	// `foreach {id u1 z u2} $rec break`) and exit immediately. The unbraced
	// bare-break body makes parseBracedBody return nil, so without this the
	// whole unpack is silently dropped and the loop variables stay empty.
	if tp.emitBreakUnpack(args, varNames, listExpr) {
		return
	}

	// foreach over a literal list of TCL "varset" scripts:
	//   foreach v [list {set a 1 set b 2} {set a 3}] { eval $v ... }
	// Each element is a braced script of `set name {value}` commands. Emit a Go
	// struct slice so the later `eval $v` can be rewritten as field assignments.
	if tp.emitVarsetForeachOrComment(args, rawList, varNames) {
		return
	}

	// A non-braced [db eval ...] source (dynamic SQL) cannot be bound at
	// generation time.
	if strings.Contains(strings.ToLower(args[1].Text), "db eval") {
		tp.emitLine("// skip: foreach over unresolved TCL command")
		return
	}

	bodyCmds := tp.parseBracedBody(args, 2)

	if bodyCmds == nil {
		tp.emitLine("// foreach %s %s (no body)", strings.Join(varNames, ","), listExpr)
		return
	}

	tp.emitForeachLoop(args, varNames, listExpr, splitExpr, bodyCmds)
}

// emitVarsetForeachOrComment emits the varset loop when the list is a literal
// varset list; a successful match carrying a parse error emits the skip
// comment. Returns true when handled.
func (tp *transpiler) emitVarsetForeachOrComment(args []tcl.RawWord, rawList string, varNames []string) bool {
	if len(varNames) != 1 {
		return false
	}
	if _, ok, err := tp.emitVarsetForeach(args, rawList, varNames[0]); ok {
		if err != nil {
			tp.emitLine("// foreach %s (varset: %v)", varNames[0], err)
		}
		return true
	}
	return false
}

// recordForeachLitValues records literal list values for the foreach loop
// variables so a later `eval $var` can inline each script's commands. For a
// K-variable foreach over N literal values, variable i takes the elements at
// indexes i, i+K, i+2K, ... (TCL's round-robin assignment).
func (tp *transpiler) recordForeachLitValues(varNames []string, rawList, listExpr string) {
	vals := literalForeachList(rawList)
	if len(vals) == 0 || len(varNames) < 1 || len(varNames) > len(vals) {
		return
	}
	dynamic := listExpr != strconv.Quote(rawList)
	if tp.foreachLitValues == nil {
		tp.foreachLitValues = make(map[string][]foreachLitValue)
	}
	for i, vn := range varNames {
		var mine []foreachLitValue
		for j := i; j < len(vals); j += len(varNames) {
			raw := stripOuterBraces(vals[j])
			cmp := strconv.Quote(raw)
			if dynamic {
				cmp = tp.buildListStringExpr(raw)
			}
			mine = append(mine, foreachLitValue{raw: raw, cmpExpr: cmp})
		}
		tp.foreachLitValues[vn] = mine
	}
}

// indexMatchingBrace returns the index of the '}' closing the '{' at index 0
// (braces nest in TCL), or -1 when the brace is unbalanced.
func indexMatchingBrace(s string) int {
	if len(s) == 0 || s[0] != '{' {
		return -1
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// stripOuterBraces removes one balanced outer brace layer from a TCL script
// string (e.g. "{ a b }" → " a b "). Returns the input unchanged when the
// braces are not balanced.
func stripOuterBraces(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") || !strings.HasSuffix(t, "}") {
		return s
	}
	depth := 0
	for i, c := range t {
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
		}
		if depth == 0 && i < len(t)-1 {
			return s
		}
	}
	if depth != 0 {
		return s
	}
	return t[1 : len(t)-1]
}

// literalForeachList extracts the literal values of a TCL list (braced or
// unbraced) so a later `eval $var` can inline each element as a script.
// Returns nil when the list contains non-literal elements (variables,
// command substitutions) that cannot be inlined statically.
func literalForeachList(rawList string) []string {
	trimmed := strings.TrimSpace(rawList)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		// [list {...} {...}] — strip the list command wrapper.
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.HasPrefix(strings.ToUpper(inner), "LIST ") {
			trimmed = strings.TrimSpace(inner[5:])
		} else {
			return nil
		}
	}
	var vals []string
	i := 0
	for i < len(trimmed) {
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n') {
			i++
		}
		if i >= len(trimmed) {
			break
		}
		word, next, ok := scanLiteralListWord(trimmed, i)
		if !ok {
			return nil
		}
		vals = append(vals, word)
		i = next
	}
	return vals
}

// scanLiteralListWord scans one element of a literal foreach list at position
// i (already past whitespace): a balanced braced word or a bare literal token
// (no $var or [cmd] substitution). next is the position after the element;
// ok=false when the element cannot be inlined statically.
func scanLiteralListWord(trimmed string, i int) (word string, next int, ok bool) {
	if trimmed[i] == '{' {
		return scanBracedListWord(trimmed, i)
	}
	// Bare word: only literal tokens (no $var or [cmd]) can be inlined.
	start := i
	for i < len(trimmed) && trimmed[i] != ' ' && trimmed[i] != '\t' && trimmed[i] != '\n' {
		if trimmed[i] == '$' || trimmed[i] == '[' {
			return "", i, false
		}
		i++
	}
	return trimmed[start:i], i, true
}

// scanBracedListWord scans a balanced braced list element starting at i
// (pointing at the opening brace). next is the position past the closing
// brace; ok=false when the braces are unbalanced.
func scanBracedListWord(trimmed string, i int) (word string, next int, ok bool) {
	depth := 0
	start := i
	for ; i < len(trimmed); i++ {
		if trimmed[i] == '{' {
			depth++
		}
		if trimmed[i] == '}' {
			depth--
			if depth == 0 {
				i++
				break
			}
		}
	}
	if depth != 0 {
		return "", i, false
	}
	return trimmed[start:i], i, true
}

// resolveForeachListExpr computes the Go expression for a foreach list. When
// the list is a single bare $var or a single bracketed command substitution
// (e.g. [execsql {SQL}]), the result is already a flat space-separated list;
// wrapping it in tclListElem (as buildListStringExpr does) would brace the
// entire string and corrupt tclSplitList. In those cases the raw expression
// is used directly. A braced list (isBraced) performs NO substitution at all
// — TCL brace words expand neither [commands] (fts4unicode.test section 9:
// [tokenchars= .] reaches SQL as a bracket-quoted identifier) nor $vars
// (fts5ac 2.3's `{1 {a b} {AND [N $x -- {a}] ...}}` keeps the literal `$x`),
// so the raw word text is emitted verbatim and tclSplitList parses the
// elements at runtime.
func (tp *transpiler) resolveForeachListExpr(rawList string, isBraced bool) string {
	if isBraced {
		trimmed := strings.TrimSpace(rawList)
		if strings.HasPrefix(trimmed, "$") && !strings.ContainsAny(trimmed, " \t\n") {
			return tclVarToGo(strings.TrimPrefix(trimmed, "$"))
		}
		return strconv.Quote(rawList)
	}
	trimmed := strings.TrimSpace(rawList)
	// Single bare $var: use the variable directly.
	if strings.HasPrefix(trimmed, "$") && !strings.ContainsAny(trimmed, " \t\n") {
		return tclVarToGo(strings.TrimPrefix(trimmed, "$"))
	}
	if expr := tp.cmdListSourceExpr(trimmed); expr != "" {
		return expr
	}
	listExpr := tp.buildListStringExpr(rawList)
	return listExpr
}

// cmdListSourceExpr renders a bracketed `[cmd ...]` foreach list source whose
// result is already a flat space-separated list, so the raw command
// expression is used directly (wrapping it in tclListElem — as
// buildListStringExpr does — would brace the entire string and corrupt
// tclSplitList). Returns "" when the command is not a recognized list
// source.
func (tp *transpiler) cmdListSourceExpr(trimmed string) string {
	// Single bracketed command substitution (no nested [..]): use the raw
	// command expression so it is not braced by tclListElem.
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return ""
	}
	if !strings.ContainsAny(trimmed[1:len(trimmed)-1], "[]") {
		inner := trimmed[1 : len(trimmed)-1]
		if !strings.Contains(inner, "[") {
			return tp.cmdExpr(inner)
		}
	}
	// List-producing commands even with nested brackets: their result is
	// already a flat space-separated list, so use the raw command
	// expression (autovacuum.test 1.x's
	// `foreach i [lsort -integer [eval concat $delete_order]]`).
	// The current cmdExprLSort / cmdExprEval / cmdExprConcat paths
	// produce a flat list string; wrapping in tclListElem would
	// produce {"a b c"} which tclSplitList yields as ONE element.
	inner := trimmed[1 : len(trimmed)-1]
	// Peel the leading command word and any flags; for the
	// list-producing procs (lsort/list/concat), the result is a
	// flat list. We also accept eval (which forwards to a
	// list-producing proc) and the proc's $var substitution.
	firstWord := inner
	sp := strings.IndexAny(firstWord, " \t")
	if sp > 0 {
		firstWord = firstWord[:sp]
	}
	switch firstWord {
	case "lsort", "list", "concat", "eval":
		return tp.cmdExpr(inner)
	}
	return ""
}

// stripListCommand strips a literal "[list ...]" / "[ list ...]" / "list
// ..." prefix from a foreach list, leaving only the list elements.
func stripListCommand(rawText string) string {
	rawList := strings.TrimSpace(rawText)
	if strings.HasPrefix(rawList, "[list ") {
		rawList = rawList[len("[list "):]
		rawList = strings.TrimSuffix(rawList, "]")
	} else if strings.HasPrefix(rawList, "[ list ") {
		rawList = rawList[len("[ list "):]
		rawList = strings.TrimSuffix(rawList, "]")
	} else if strings.HasPrefix(rawList, "list ") {
		rawList = rawList[len("list "):]
	}
	return rawList
}

// splitListExpr detects a `[split $var ""]` foreach list and returns a Go
// strings.Split expression iterating the string's characters.
func splitListExpr(rawList string) string {
	if !strings.HasPrefix(rawList, "[split ") || !strings.HasSuffix(rawList, "]") {
		return ""
	}
	splitInner := strings.TrimSpace(rawList[len("[split "):])
	splitInner = strings.TrimSuffix(splitInner, "]")
	fields := strings.Fields(splitInner)
	if len(fields) < 1 || !strings.HasPrefix(fields[0], "$") {
		return ""
	}
	goVar := tclVarToGo(strings.TrimPrefix(fields[0], "$"))
	if !isValidGoIdent(goVar) {
		return ""
	}
	sep := `""`
	if len(fields) >= 2 {
		sep = fmt.Sprintf("%q", strings.Trim(fields[1], `"`))
	}
	return fmt.Sprintf("strings.Split(%s, %s)", goVar, sep)
}

// foreachLitValue is one recorded element of a literal foreach list: raw is
// the element's verbatim text (for parseCommands), cmpExpr is the Go string
// expression whose runtime value equals the loop variable's value for that
// iteration (raw %q for static braced lists; the expanded buildListStringExpr
// rendering for lists built with $var/[cmd] substitution).
type foreachLitValue struct {
	raw     string
	cmpExpr string
}

// emitSingleVarForeach emits a `for _, v := range ...` loop header for a
// single loop variable.
func (tp *transpiler) emitSingleVarForeach(varName, listExpr, splitExpr string) {
	goVN := tclVarToGo(varName)
	// A TCL loop variable named 'err' maps to _err (tclVarToGo) so body
	// references to $err see the loop value (unified naming).
	// Avoid shadowing the main DB connection variable (dbVar)
	if goVN == tp.dbVar {
		// The loop variable holds a connection NAME at runtime (TCL
		// `foreach db {db db2} { execsql {...} $db }`); record the rename so
		// $db references inside resolve to the loop var and dispatch through
		// tclConnByName.
		target := goVN + "_iter"
		if tp.runtimeConnVars == nil {
			tp.runtimeConnVars = make(map[string]bool)
		}
		tp.runtimeConnVars[target] = true
		if tp.varRenames == nil {
			tp.varRenames = make(map[string]string)
		}
		tp.varRenames[goVN] = target
		goVN = target
	}
	if splitExpr != "" {
		tp.emitLine("for _, %s := range %s {", goVN, splitExpr)
	} else {
		tp.emitLine("for _, %s := range tclSplitList(%s) {", goVN, listExpr)
	}
	tp.emitLine("_ = %s // suppress unused warning", goVN)
	// Mirror interpreter-state loop sentinels into the tclvar registry so
	// test modules reading TCL globals (test_tclvar.c's ::tclvar_set_omit)
	// observe the same value inside the engine.
	if strings.HasPrefix(goVN, "tclvar_set_") {
		tp.emitLine(`vtab.TclVarSet(%q, "", %s)`, goVN, goVN)
	}
}

// emitMultiVarForeach emits a `for idx := 0; idx+N <= len(items); idx += N`
// loop header that unpacks N loop variables per iteration.
func (tp *transpiler) emitMultiVarForeach(varNames []string, listExpr string) {
	// Use unique variable names per foreach to avoid redeclaration
	itemsVar := fmt.Sprintf("_items%d", tp.varCount)
	idxVar := fmt.Sprintf("_idx%d", tp.varCount)
	tp.varCount++
	tp.emitLine("// foreach {%s} %s", strings.Join(varNames, " "), listExpr)
	tp.emitLine("%s := tclSplitList(%s)", itemsVar, listExpr)
	numVars := len(varNames)
	tp.emitLine("for %s := 0; %s+%d <= len(%s); %s += %d {", idxVar, idxVar, numVars, itemsVar, idxVar, numVars)
	tp.indent++
	for i, vn := range varNames {
		goVN := tclVarToGo(vn)
		if goVN == "_err" && !tp.isVarDeclared(goVN) {
			tp.vars = append(tp.vars, goVN)
		}
		tp.emitLine("%s := %s[%s+%d]", goVN, itemsVar, idxVar, i)
		tp.emitLine("_ = %s // suppress unused warning", goVN)
	}
	tp.emitLine("_ = %s", idxVar) // suppress unused warning
	// NOTE: indent is intentionally left at the loop-body level here; the
	// caller's bodyTP (emitForeachLoop) increments it once more for the loop
	// body, matching the original multi-var foreach emission.
}

// emitDBEvalForeach transpiles a foreach whose list is a bracketed
// `db eval {SQL}` command (e.g. the common "drop all tables" cleanup):
//
//	foreach tab [db eval {SELECT name FROM sqlite_master WHERE type = 'table'}] {
//	  db eval "DROP TABLE $tab"
//	}
//
// It emits a Go loop over db.Query(SQL).Rows with the loop variable bound
// to the first column of each row. Returns false when the pattern does not
// match so the caller can fall back to the skip comment.
func (tp *transpiler) emitDBEvalForeach(args []tcl.RawWord, varNames []string) bool {
	if len(args) < 3 || len(varNames) == 0 {
		return false
	}
	connExpr, sql, ok := tp.dbEvalForeachSource(args[1].Text)
	if !ok {
		return false
	}
	bodyCmds := tp.parseBracedBody(args, 2)
	if bodyCmds == nil {
		return false
	}
	// A loop body that uses the TCL file-channel harness (open/fconfigure/
	// puts/close on a file descriptor) cannot be transpiled — the engine has
	// no `open` command, so the file is never written and downstream
	// size/readback checks fail (shell7 1.$tn.1: writes a blob to a file
	// then asserts its size). Keep those loops skipped.
	if !dbEvalForeachBodyTranspiled(bodyCmds) {
		return false
	}
	rowsVar := fmt.Sprintf("_rows%d", tp.varCount)
	rowVar := fmt.Sprintf("_row%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := %s.Query(%q)", rowsVar, connExpr, sql)
	tp.emitLine("if %s.Error != nil {", rowsVar)
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", %s.Error, %q)", rowsVar, sql)
	tp.emitLine("}")
	tp.emitLine("for _, %s := range %s.Rows {", rowVar, rowsVar)
	tp.emitLine("_ = %s // suppress unused warning", rowVar)
	// execsql semantics: [db eval SQL] returns a FLAT list of every cell of
	// every result row. A single loop variable therefore iterates CELLS —
	// emit a nested cell loop (trigger2-1.x: foreach v [execsql {...}]
	// lappends int($v) for all seven rlog columns; binding only column 0
	// dropped every other column). Multiple variables destructure the row
	// columns in order (fts4opt 1.1: foreach {docid words} [db eval {...]}).
	cellLoop := tp.emitDBEvalRowBinding(varNames, rowVar)
	tp.indent++
	tp.runDBEvalForeachBody(bodyCmds)
	if cellLoop {
		tp.indent--
		tp.emitLine("}")
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// dbEvalForeachSource parses a foreach list source of `[db eval {SQL}]` or
// `[execsql {SQL} [conn]]` into the Go connection expression and SQL text.
// Returns ok=false for any other form.
func (tp *transpiler) dbEvalForeachSource(listText string) (connExpr, sql string, ok bool) {
	text := strings.TrimSpace(listText)
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		return "", "", false
	}
	inner := strings.TrimSpace(text[1 : len(text)-1])
	switch {
	case strings.HasPrefix(inner, "db eval "):
		rest := strings.TrimSpace(inner[len("db eval "):])
		if !strings.HasPrefix(rest, "{") || !strings.HasSuffix(rest, "}") {
			return "", "", false
		}
		return tp.dbVar, strings.TrimSpace(rest[1 : len(rest)-1]), true
	case strings.HasPrefix(inner, "execsql "):
		// [execsql {SQL}] — the harness-level execsql on the main
		// connection (pragma.test 6.1: foreach {idx name file}
		// [execsql {pragma database_list}] {...}) — or
		// [execsql {SQL} conn] with an explicit connection name.
		return tp.dbEvalExecsqlSource(inner)
	default:
		return "", "", false
	}
}

// dbEvalExecsqlSource parses the inner text of an `[execsql {SQL} [conn]]`
// foreach source (without the outer brackets).
func (tp *transpiler) dbEvalExecsqlSource(inner string) (connExpr, sql string, ok bool) {
	rest := strings.TrimSpace(inner[len("execsql "):])
	if !strings.HasPrefix(rest, "{") {
		return "", "", false
	}
	end := indexMatchingBrace(rest)
	if end < 0 {
		return "", "", false
	}
	sql = strings.TrimSpace(rest[1:end])
	connExpr = tp.dbVar
	if tail := strings.TrimSpace(rest[end+1:]); tail != "" {
		// The connection word: a declared db variable (db/db2/...)
		// resolved through the alias map, else reject (a dynamic
		// expression cannot be bound at generation time).
		goConn, connOK := tp.execsqlConnVar(tail)
		if !connOK {
			return "", "", false
		}
		connExpr = goConn
	}
	return connExpr, sql, true
}

// execsqlConnVar resolves an `execsql {SQL} CONN` connection word to its Go
// variable. Returns ok=false for a dynamic expression.
func (tp *transpiler) execsqlConnVar(tail string) (string, bool) {
	goConn := tclVarToGo(tail)
	if renamed, ok := tp.varRenames[goConn]; ok {
		goConn = renamed
	}
	if goConn == "db" || isPreDeclaredDB(goConn) || tp.dbConnVars[goConn] {
		if target, ok := tp.dbAliases[goConn]; ok {
			return target, true
		}
		return goConn, true
	}
	return "", false
}

// dbEvalForeachBodyTranspiled reports whether a db-eval foreach loop body
// avoids the TCL file-channel harness (open/fconfigure/puts/close on a file
// descriptor) — the engine has no `open` command, so the file is never
// written and downstream size/readback checks fail (shell7 1.$tn.1: writes a
// blob to a file then asserts its size).
func dbEvalForeachBodyTranspiled(bodyCmds [][]tcl.RawWord) bool {
	for _, cmd := range bodyCmds {
		if len(cmd) == 0 {
			continue
		}
		name := strings.ToLower(cmd[0].Text)
		if name == "open" || name == "fconfigure" || name == "close" || name == "flush" {
			return false
		}
		for _, w := range cmd {
			if strings.Contains(strings.ToLower(w.Text), "puts -nonewline") {
				return false
			}
		}
	}
	return true
}

// emitDBEvalRowBinding emits the loop-variable binding for a db-eval foreach:
// a single variable iterates the CELLS of each row (nested cell loop); a
// variable list destructures the row columns in order. Returns cellLoop=true
// when the nested cell loop was emitted (the caller closes it).
func (tp *transpiler) emitDBEvalRowBinding(varNames []string, rowVar string) bool {
	if len(varNames) != 1 {
		// Bind each loop variable to the corresponding row column.
		for i, vn := range varNames {
			goVN := tclVarToGo(vn)
			if goVN == tp.dbVar {
				goVN = goVN + "_iter"
			}
			tp.emitLine("%s := fmt.Sprint(%s[%d])", goVN, rowVar, i)
			tp.emitLine("_ = %s // suppress unused warning", goVN)
		}
		return false
	}
	cellVar := fmt.Sprintf("_cell%d", tp.varCount)
	tp.varCount++
	tp.emitLine("for _, %s := range %s {", cellVar, rowVar)
	goVN := tclVarToGo(varNames[0])
	if goVN == tp.dbVar {
		goVN = goVN + "_iter"
	}
	tp.emitLine("%s := fmt.Sprint(%s)", goVN, cellVar)
	tp.emitLine("_ = %s // suppress unused warning", goVN)
	tp.indent++
	return true
}

// runDBEvalForeachBody transpiles a db-eval foreach body in a fresh
// sub-transpiler sharing the output buffer and state.
func (tp *transpiler) runDBEvalForeachBody(bodyCmds [][]tcl.RawWord) {
	bodyTP := &transpiler{
		sb:         tp.sb,
		indent:     tp.indent,
		dbVar:      tp.dbVar,
		t:          tp.t,
		varCount:   tp.varCount,
		vars:       tp.vars,
		forIncrs:   append(tp.forIncrs, nil),
		testPrefix: tp.testPrefix, preparedState: tp.preparedState,
		blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	if len(bodyTP.blobChans) > 0 {
		tp.blobChans = bodyTP.blobChans
	}
	if len(bodyTP.blobChannelVars) > 0 {
		tp.blobChannelVars = bodyTP.blobChannelVars
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	tp.blobSeq = bodyTP.blobSeq
}

// emitVarsetForeach transpiles a foreach whose list elements are TCL "varset"
// scripts — braced sequences of `set name {value}` commands, commonly used as
//
//	foreach v [list {set a 1 set b 2} {set a 3}] { eval $v ... }
//
// It emits a Go struct slice and records the loop variable so a later
// `eval $v` becomes field assignments. Returns ok=false when the list is not a
// literal varset list; the caller falls back to the generic list loop.
func (tp *transpiler) emitVarsetForeach(args []tcl.RawWord, rawList, varName string) (varsetInfo, bool, error) {
	// Do not TrimSpace: the list often ends with a backslash-newline TCL
	// continuation, and trimming the trailing newline would orphan the
	// backslash into a bogus element. tclSplitList handles leading/trailing
	// whitespace and backslash-newline continuations itself.
	elements := tclSplitList(rawList)
	if len(elements) == 0 {
		return varsetInfo{}, false, nil
	}
	allFields, rows, ok := parseVarsetElements(elements)
	if !ok {
		return varsetInfo{}, false, nil
	}
	goVN := tclVarToGo(varName)
	if goVN == tp.dbVar {
		goVN = goVN + "_iter"
	}
	structName := fmt.Sprintf("_varset%d", tp.varCount)
	sliceVar := fmt.Sprintf("_varsets%d", tp.varCount)
	tp.varCount++
	tp.emitVarsetStruct(structName, allFields)
	tp.emitVarsetSlice(sliceVar, structName, allFields, rows)
	tp.emitLine("for _, %s := range %s {", goVN, sliceVar)
	tp.emitLine("_ = %s // suppress unused warning", goVN)
	tp.indent++
	bodyCmds := tp.parseBracedBody(args, 2)
	tp.runVarsetForeachBody(bodyCmds, goVN, varsetInfo{fields: allFields, structName: structName})
	tp.indent--
	tp.emitLine("}")
	return varsetInfo{fields: allFields, structName: structName}, true, nil
}

// runVarsetForeachBody transpiles a varset foreach body in a fresh
// sub-transpiler whose varsetLoopVars maps the loop variable to the emitted
// struct info (so a later `eval $v` becomes field assignments).
func (tp *transpiler) runVarsetForeachBody(bodyCmds [][]tcl.RawWord, goVN string, info varsetInfo) {
	if bodyCmds == nil {
		return
	}
	vsetMap := map[string]varsetInfo{}
	for k, v := range tp.varsetLoopVars {
		vsetMap[k] = v
	}
	vsetMap[goVN] = info
	bodyTP := &transpiler{
		sb:             tp.sb,
		indent:         tp.indent,
		dbVar:          tp.dbVar,
		t:              tp.t,
		varCount:       tp.varCount,
		vars:           tp.vars,
		forIncrs:       append(tp.forIncrs, nil),
		varsetLoopVars: vsetMap,
		testPrefix:     tp.testPrefix,
		preparedState:  tp.preparedState,
		blobChans:      tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	if len(bodyTP.blobChans) > 0 {
		tp.blobChans = bodyTP.blobChans
	}
	if len(bodyTP.blobChannelVars) > 0 {
		tp.blobChannelVars = bodyTP.blobChannelVars
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	tp.blobSeq = bodyTP.blobSeq
}

// parseVarsetElements parses a literal varset list into its field names (in
// first-appearance order) and per-element field values. Returns ok=false when
// an element is not a static `set name {value}` script.
type varsetFieldVal struct {
	name  string
	value string
}

func parseVarsetElements(elements []string) ([]string, [][]varsetFieldVal, bool) {
	var allFields []string
	seen := map[string]bool{}
	rows := make([][]varsetFieldVal, 0, len(elements))
	for _, el := range elements {
		row, ok := parseVarsetElement(el, seen, &allFields)
		if !ok {
			return nil, nil, false
		}
		rows = append(rows, row)
	}
	return allFields, rows, true
}

// parseVarsetElement parses one varset element (a braced script of `set name
// {value}` commands) into its field values, recording new field names in
// allFields.
func parseVarsetElement(el string, seen map[string]bool, allFields *[]string) ([]varsetFieldVal, bool) {
	el = strings.TrimSpace(el)
	// tclSplitList strips the outer braces of braced elements, so the
	// element is already the bare script text (it may still contain
	// inner braced values). Parse it directly as commands.
	cmds := parseCommands(el)
	if len(cmds) == 0 {
		return nil, false
	}
	row := []varsetFieldVal{}
	for _, cmdArgs := range cmds {
		if len(cmdArgs) < 3 || cmdArgs[0].Text != "set" {
			return nil, false
		}
		vn := tclVarToGo(cmdArgs[1].Text)
		val := rawValueText(cmdArgs[2])
		if strings.Contains(val, "$") || strings.Contains(val, "[") {
			// Dynamic values cannot be represented as static struct fields.
			return nil, false
		}
		row = append(row, varsetFieldVal{vn, val})
		if !seen[vn] {
			seen[vn] = true
			*allFields = append(*allFields, vn)
		}
	}
	return row, true
}

// emitVarsetStruct emits the `type _varsetN struct { F string; FSet bool ... }`
// declaration for a varset loop.
func (tp *transpiler) emitVarsetStruct(structName string, allFields []string) {
	tp.emitLine("type %s struct {", structName)
	tp.indent++
	for _, f := range allFields {
		tp.emitLine("%s string", f)
		tp.emitLine("%sSet bool", f)
	}
	tp.indent--
	tp.emitLine("}")
}

// emitVarsetSlice emits the `_varsetsN := []_varsetN{...}` literal for a
// varset loop.
func (tp *transpiler) emitVarsetSlice(sliceVar, structName string, allFields []string, rows [][]varsetFieldVal) {
	tp.emitLine("%s := []%s{", sliceVar, structName)
	tp.indent++
	for _, row := range rows {
		m := map[string]string{}
		for _, fv := range row {
			m[fv.name] = fv.value
		}
		parts := make([]string, 0, len(allFields)*2)
		for _, f := range allFields {
			if _, ok := m[f]; ok {
				parts = append(parts, fmt.Sprintf("%q, true", m[f]))
			} else {
				parts = append(parts, `"", false`)
			}
		}
		tp.emitLine("{%s},", strings.Join(parts, ", "))
	}
	tp.indent--
	tp.emitLine("}")
}

// rawValueText returns the effective text of a TCL word: braced and quoted
// words drop their delimiters (the parser already stripped them from Text),
// quoted words still need their backslash escapes resolved.
func rawValueText(w tcl.RawWord) string {
	if w.Quoted {
		return tclUnescapeQuoted(w.Text)
	}
	return w.Text
}

func (tp *transpiler) parseVarList(w tcl.RawWord) []string {
	text := w.Text
	if w.Braced {
		return strings.Fields(text)
	}
	if !strings.Contains(text, " ") && !strings.Contains(text, "\t") {
		return []string{text}
	}
	return strings.Fields(text)
}

func (tp *transpiler) parseBracedBody(args []tcl.RawWord, idx int) [][]tcl.RawWord {
	if idx < len(args) && args[idx].Braced && len(args[idx].Text) > 0 {
		return parseCommands(args[idx].Text)
	}
	return nil
}
