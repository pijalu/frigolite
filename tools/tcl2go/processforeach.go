// Package main implements the tcl2go tool.
//
// This file handles TCL foreach loops.
package main

import (
	"fmt"
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

	// Record literal braced-list values for single-variable foreach loops so a
	// later `eval $var` can inline each script's commands (backup.test's
	// foreach zOpenScript { ... } { eval $zOpenScript } pattern).
	if len(varNames) == 1 {
		if vals := literalForeachList(rawList); len(vals) > 0 {
			if tp.foreachLitValues == nil {
				tp.foreachLitValues = make(map[string][]string)
			}
			tp.foreachLitValues[varNames[0]] = vals
		}
	}
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
	if len(varNames) == 1 {
		if _, ok, err := tp.emitVarsetForeach(args, rawList, varNames[0]); ok {
			if err != nil {
				tp.emitLine("// foreach %s (varset: %v)", varNames[0], err)
			}
			return
		}
	}

	// foreach over [db eval ...]: the transpiler can't execute TCL at
	// generation time, but the common cleanup pattern
	//   foreach tab [db eval {SELECT name FROM sqlite_master ...}] {
	//     db eval "DROP TABLE $tab"
	//   }
	// is static enough to emit directly as a Go query loop.
	if strings.Contains(strings.ToLower(args[1].Text), "db eval") {
		if tp.emitDBEvalForeach(args, varNames) {
			return
		}
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
		if trimmed[i] == '{' {
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
				return nil
			}
			vals = append(vals, trimmed[start:i])
		} else {
			// Bare word: only literal tokens (no $var or [cmd]) can be inlined.
			start := i
			for i < len(trimmed) && trimmed[i] != ' ' && trimmed[i] != '\t' && trimmed[i] != '\n' {
				if trimmed[i] == '$' || trimmed[i] == '[' {
					return nil
				}
				i++
			}
			vals = append(vals, trimmed[start:i])
		}
	}
	return vals
}

// resolveForeachListExpr computes the Go expression for a foreach list. When
// the list is a single bare $var or a single bracketed command substitution
// (e.g. [execsql {SQL}]), the result is already a flat space-separated list;
// wrapping it in tclListElem (as buildListStringExpr does) would brace the
// entire string and corrupt tclSplitList. In those cases the raw expression
// is used directly. A braced list (isBraced) keeps [...] literal
// (buildListStringExprNoCmd), since TCL brace words do not substitute
// commands (fts4unicode.test section 9: [tokenchars= .] reaches SQL as a
// bracket-quoted identifier).
func (tp *transpiler) resolveForeachListExpr(rawList string, isBraced bool) string {
	if isBraced {
		listExpr := tp.buildListStringExprNoCmd(rawList)
		trimmed := strings.TrimSpace(rawList)
		if strings.HasPrefix(trimmed, "$") && !strings.ContainsAny(trimmed, " \t\n") {
			return tclVarToGo(strings.TrimPrefix(trimmed, "$"))
		}
		return listExpr
	}
	listExpr := tp.buildListStringExpr(rawList)
	trimmed := strings.TrimSpace(rawList)
	// Single bare $var: use the variable directly.
	if strings.HasPrefix(trimmed, "$") && !strings.ContainsAny(trimmed, " \t\n") {
		return tclVarToGo(strings.TrimPrefix(trimmed, "$"))
	}
	// Single bracketed command substitution (no nested [..]): use the raw
	// command expression so it is not braced by tclListElem.
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") &&
		!strings.ContainsAny(trimmed[1:len(trimmed)-1], "[]") {
		inner := trimmed[1 : len(trimmed)-1]
		if !strings.Contains(inner, "[") {
			return tp.cmdExpr(inner)
		}
	}
	return listExpr
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

// emitBreakUnpack handles `foreach {v1 v2 ...} $list break` — unpack the first
// list element into the variables and exit immediately.
func (tp *transpiler) emitBreakUnpack(args []tcl.RawWord, varNames []string, listExpr string) bool {
	if len(args) < 3 || args[2].Braced || strings.TrimSpace(args[2].Text) != "break" || len(varNames) <= 1 {
		return false
	}
	itemsVar := fmt.Sprintf("_items%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclSplitList(%s)", itemsVar, listExpr)
	tp.emitLine("if len(%s) >= %d {", itemsVar, len(varNames))
	tp.indent++
	for i, vn := range varNames {
		goVN := tclVarToGo(vn)
		if goVN == "err" {
			goVN = "_err_tcl"
		}
		if !tp.isVarDeclared(goVN) && !isPreDeclaredDB(goVN) && goVN != tp.dbVar {
			tp.emitLine("var %s string", goVN)
			tp.vars = append(tp.vars, goVN)
		}
		tp.emitLine("%s = %s[%d]", goVN, itemsVar, i)
		tp.emitLine("_ = %s // suppress unused warning", goVN)
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitForeachLoop emits the generic foreach loop (single-var range or
// multi-var index unpack) with the body transpiled in a fresh sub-transpiler.
func (tp *transpiler) emitForeachLoop(args []tcl.RawWord, varNames []string, listExpr, splitExpr string, bodyCmds [][]tcl.RawWord) {
	if len(varNames) == 1 {
		tp.emitSingleVarForeach(varNames[0], listExpr, splitExpr)
	} else {
		tp.emitMultiVarForeach(varNames, listExpr)
	}
	_ = listExpr // suppress unused warning if body is empty

	tp.indent++
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		// A foreach loop has no increment clause: continue targets this loop,
		// so the innermost entry is empty (plain Go continue).
		forIncrs:   append(tp.forIncrs, nil),
		testPrefix: tp.testPrefix, preparedState: tp.preparedState,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		collateGoFuncs:   tp.collateGoFuncs,
		collateDtorVars:  tp.collateDtorVars,
		varConstValues:   tp.varConstValues,
		foreachLitValues: tp.foreachLitValues,
		varsetLoopVars:   tp.varsetLoopVars,
		dbConnVars:       tp.dbConnVars,
		runtimeConnVars:  tp.runtimeConnVars,
		varRenames:       tp.varRenames,
		blobChans:        tp.blobChans,
		blobChannelVars:  tp.blobChannelVars,
		blobVarNames:     tp.blobVarNames,
		usedChannels:     tp.usedChannels,
		blobSeq:          tp.blobSeq,
		testDir:          tp.testDir,
		genesisPreamble:  tp.genesisPreamble,
		ftsBuildPreamble: tp.ftsBuildPreamble,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.varConstValues = bodyTP.varConstValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
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
	tp.indent--
	tp.emitLine("}")
}

// emitArrayGetForeach transpiles `foreach {k v} "array get ARR" {BODY}` — the
// TCL idiom that iterates a dynamic-key array's key/value pairs. The
// transpiler represents such arrays as Go maps (arrayMapVars), so the loop
// becomes a Go map range with k and v bound to the key and value. Returns true
// when the pattern matched and the loop was emitted.
func (tp *transpiler) emitArrayGetForeach(args []tcl.RawWord, varNames []string, rawList string) bool {
	trimmed := strings.TrimSpace(rawList)
	trimmed = strings.TrimPrefix(trimmed, "[")
	trimmed = strings.TrimSuffix(trimmed, "]")
	trimmed = strings.TrimPrefix(trimmed, `"`)
	trimmed = strings.TrimSuffix(trimmed, `"`)
	fields := strings.Fields(trimmed)
	if len(fields) != 3 || fields[0] != "array" || fields[1] != "get" {
		return false
	}
	base := strings.TrimPrefix(fields[2], "::")
	if !tp.arrayMapVars[base] && !tp.arrayMapVars["::"+base] {
		return false
	}
	mapVar := tclVarToGo(base) + "Map"
	keyVar := tclVarToGo(varNames[0])
	valVar := tclVarToGo(varNames[1])
	if keyVar == "err" {
		keyVar = "_err_tcl"
	}
	if valVar == "err" {
		valVar = "_err_tcl"
	}
	if !isValidGoIdent(keyVar) || !isValidGoIdent(valVar) {
		return false
	}
	tp.emitLine("// foreach {%s} %s", strings.Join(varNames, " "), trimmed)
	tp.emitLine("for %s, %s := range %s {", keyVar, valVar, mapVar)
	tp.indent++
	bodyCmds := tp.parseBracedBody(args, 2)
	if bodyCmds != nil {
		bodyTP := &transpiler{
			sb:            tp.sb,
			indent:        tp.indent,
			dbVar:         tp.dbVar,
			t:             tp.t,
			varCount:      tp.varCount,
			vars:          append(append([]string{}, tp.vars...), keyVar, valVar),
			arrayKeys:     tp.arrayKeys,
			arrayMapVars:  tp.arrayMapVars,
			forIncrs:      append(tp.forIncrs, nil),
			testPrefix:    tp.testPrefix,
			preparedState: tp.preparedState,
			queryFuncs:    tp.queryFuncs,
			specialFuncs:  tp.specialFuncs, procStringMaps: tp.procStringMaps,
			collateGoFuncs:   tp.collateGoFuncs,
			collateDtorVars:  tp.collateDtorVars,
			varConstValues:   tp.varConstValues,
			foreachLitValues: tp.foreachLitValues,
			varsetLoopVars:   tp.varsetLoopVars,
			dbConnVars:       tp.dbConnVars,
			runtimeConnVars:  tp.runtimeConnVars,
			varRenames:       tp.varRenames,
			blobChans:        tp.blobChans,
			blobChannelVars:  tp.blobChannelVars,
			blobVarNames:     tp.blobVarNames,
			usedChannels:     tp.usedChannels,
			blobSeq:          tp.blobSeq,
			testDir:          tp.testDir,
			genesisPreamble:  tp.genesisPreamble,
			ftsBuildPreamble: tp.ftsBuildPreamble,
		}
		bodyTP.processCommands(bodyCmds)
		tp.varCount = bodyTP.varCount
		tp.indent = bodyTP.indent
		tp.varConstValues = bodyTP.varConstValues
		tp.foreachLitValues = bodyTP.foreachLitValues
		tp.varsetLoopVars = bodyTP.varsetLoopVars
		tp.dbConnVars = bodyTP.dbConnVars
		tp.runtimeConnVars = bodyTP.runtimeConnVars
		tp.varRenames = bodyTP.varRenames
		tp.genesisPreamble = bodyTP.genesisPreamble
		tp.ftsBuildPreamble = bodyTP.ftsBuildPreamble
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitSingleVarForeach emits a `for _, v := range ...` loop header for a
// single loop variable.
func (tp *transpiler) emitSingleVarForeach(varName, listExpr, splitExpr string) {
	goVN := tclVarToGo(varName)
	// A TCL loop variable named 'err' must map to _err_tcl so body
	// references to $err (redirected to _err_tcl) see the loop value.
	if goVN == "err" {
		goVN = "_err_tcl"
	}
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
		// A TCL loop variable named 'err' must map to _err_tcl so body
		// references to $err (redirected to _err_tcl) see the loop value.
		if goVN == "err" {
			goVN = "_err_tcl"
			if !tp.isVarDeclared(goVN) {
				tp.vars = append(tp.vars, goVN)
			}
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
	text := strings.TrimSpace(args[1].Text)
	prefix := "[db eval "
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, "]") {
		return false
	}
	inner := strings.TrimSpace(text[len(prefix):])
	inner = strings.TrimSuffix(inner, "]")
	if !strings.HasPrefix(inner, "{") || !strings.HasSuffix(inner, "}") {
		return false
	}
	sql := strings.TrimSpace(inner[1 : len(inner)-1])
	bodyCmds := tp.parseBracedBody(args, 2)
	if bodyCmds == nil {
		return false
	}
	// A loop body that uses the TCL file-channel harness (open/fconfigure/
	// puts/close on a file descriptor) cannot be transpiled — the engine has
	// no `open` command, so the file is never written and downstream
	// size/readback checks fail (shell7 1.$tn.1: writes a blob to a file
	// then asserts its size). Keep those loops skipped.
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
	rowsVar := fmt.Sprintf("_rows%d", tp.varCount)
	rowVar := fmt.Sprintf("_row%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := db.Query(%q)", rowsVar, sql)
	tp.emitLine("if %s.Error != nil {", rowsVar)
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", %s.Error, %q)", rowsVar, sql)
	tp.emitLine("}")
	tp.emitLine("for _, %s := range %s.Rows {", rowVar, rowsVar)
	tp.emitLine("_ = %s // suppress unused warning", rowVar)
	// Bind each loop variable to the corresponding row column. A single
	// variable gets column 0; multiple variables destructure the row columns
	// in order (fts4opt 1.1: foreach {docid words} [db eval {SELECT * FROM
	// t1}] { INSERT INTO t2(docid, words) VALUES($docid, $words) }).
	for i, vn := range varNames {
		goVN := tclVarToGo(vn)
		if goVN == tp.dbVar {
			goVN = goVN + "_iter"
		}
		tp.emitLine("%s := fmt.Sprint(%s[%d])", goVN, rowVar, i)
		tp.emitLine("_ = %s // suppress unused warning", goVN)
	}
	tp.indent++
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
	tp.indent--
	tp.emitLine("}")
	return true
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
	if bodyCmds != nil {
		vsetMap := map[string]varsetInfo{}
		for k, v := range tp.varsetLoopVars {
			vsetMap[k] = v
		}
		vsetMap[goVN] = varsetInfo{fields: allFields, structName: structName}
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
	tp.indent--
	tp.emitLine("}")
	return varsetInfo{fields: allFields, structName: structName}, true, nil
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
