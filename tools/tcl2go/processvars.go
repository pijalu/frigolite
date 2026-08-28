// Package main implements the tcl2go tool.
//
// This file handles var/incr/expr/catch/list-append commands.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

func (tp *transpiler) varValueExpr(args []tcl.RawWord) string {
	if len(args) == 0 {
		return `""`
	}
	word := args[0].Text
	if strings.HasPrefix(word, "$") {
		name := tclVarToGo(strings.TrimPrefix(word, "$"))
		// A bare word may hold ADJACENT references ($boundsign$bound):
		// TCL ends a variable name at the next '$', so the word is a
		// concatenation, not a single (sanitizer-mangled) identifier —
		// render it through the general string-parts path (tabfunc01 1380).
		if isValidGoIdent(name) && !strings.Contains(strings.TrimPrefix(word, "$"), "$") {
			return name
		}
		if strings.Contains(word, "$") {
			return tp.buildStringExpr(word)
		}
	}
	// A bracket command ([db one {...}], [string map ...], ...) evaluates at
	// runtime; naive quoting would write the raw TCL text into the channel.
	if strings.HasPrefix(word, "[") && strings.HasSuffix(word, "]") {
		if expr := tp.cmdExpr(strings.TrimSuffix(word[1:], "]")); expr != "" && expr != fmt.Sprintf("%q", strings.TrimSuffix(word[1:], "]")) {
			return expr
		}
	}
	return tp.goStringLiteral(args[0])
}

// tclArrayElementRef returns the name of a TCL associative-array element
// reference ($name($key)) in s, or "" if none. Such references cannot be
// transpiled to a Go variable (the transpiler maps `set map(K) V` to map_K
// but has no dynamic-key lookup).
func tclArrayElementRef(s string) string {
	i := strings.Index(s, "$(")
	if i >= 0 {
		return s[i:]
	}
	// $name(key) — a $ followed by an identifier then (.
	for j := 0; j < len(s); j++ {
		if s[j] == '$' && j+1 < len(s) && isVarStartChar(s[j+1]) {
			k := j + 1
			for k < len(s) && isVarChar(s[k]) {
				k++
			}
			if k < len(s) && s[k] == '(' {
				return s[j:]
			}
		}
	}
	return ""
}

// markQueryVar records a variable as holding query SQL when its assigned or
// appended value starts with a query keyword (SELECT/WITH/VALUES/PRAGMA/
// EXPLAIN). This lets `execsql $var` be transpiled as a query that returns
// rows instead of a bare Exec whose result is discarded.
func (tp *transpiler) markQueryVar(name, value string) {
	if tp.queryVars == nil {
		tp.queryVars = make(map[string]bool)
	}
	trimmed := strings.TrimSpace(value)
	upper := strings.ToUpper(trimmed)
	for _, kw := range []string{"SELECT", "WITH", "VALUES", "PRAGMA", "EXPLAIN"} {
		if strings.HasPrefix(upper, kw) {
			tp.queryVars[name] = true
			return
		}
	}
}

// processReturn handles `return [value]` — emit a Go return. A value argument
// (e.g. hook.test's commit_hook `return 0` / `return 1`) is emitted as the
// returned expression so the closure satisfies its int return type.
func (tp *transpiler) processReturn(args []tcl.RawWord) {
	if len(args) >= 1 {
		val := strings.TrimSpace(args[0].Text)
		if n, err := strconv.Atoi(val); err == nil {
			tp.emitLine("return %d", n)
			return
		}
	}
	tp.emitLine("return")
}

func (tp *transpiler) processIncr(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	goName := tclVarToGo(args[0].Text)
	if !isValidGoIdent(goName) {
		tp.emitLine("// incr %s (invalid identifier, skipped)", args[0].Text)
		return
	}
	amount := "1"
	if len(args) >= 2 {
		// incr VAR [sqlite3_is_interrupted $DB] — increment by the
		// connection's interrupt-flag state (0/1), matching the TCL harness
		// (interrupt.test 2.5.2).
		amountText := strings.TrimSpace(args[1].Text)
		if strings.HasPrefix(amountText, "[sqlite3_is_interrupted ") && strings.HasSuffix(amountText, "]") {
			inner := strings.TrimSuffix(strings.TrimPrefix(amountText, "["), "]")
			fields := strings.Fields(inner)
			if len(fields) >= 2 {
				dbConn := tp.dbArgGo(fields[1])
				amount = fmt.Sprintf("toInt(tclBool01(%s.IsInterrupted()))", dbConn)
			}
		} else {
			amountExpr := tp.goStringLiteral(args[1])
			if len(amountExpr) >= 2 && amountExpr[0] == '"' && amountExpr[len(amountExpr)-1] == '"' {
				amount = amountExpr[1 : len(amountExpr)-1]
			} else {
				amount = amountExpr
			}
		}
	}

	// If amount is not a pure integer, wrap it in a strconv.Atoi conversion
	// to avoid type mismatches (int + string).
	amountInt := amount
	if _, atoiErr := strconv.Atoi(amount); atoiErr != nil {
		// amount is a variable or expression — convert at runtime.
		// Go int expressions emitted above (toInt(...)) are used directly;
		// TCL-specific syntax or spaces fall back to 1.
		if strings.HasPrefix(amount, "toInt(") {
			amountInt = amount
		} else if strings.ContainsAny(amount, "$?\\ ") {
			amountInt = "1"
		} else {
			amountInt = "func() int { _v, _ := strconv.Atoi(" + amount + "); return _v }()"
		}
	}

	// Ensure variable is declared if not already
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s = \"0\"", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("// incr %s %s", goName, amount)
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("_n, _err := strconv.Atoi(%s)", goName)
	tp.emitLine("if _err == nil {")
	tp.emitLine("\t%s = strconv.Itoa(_n + %s)", goName, amountInt)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// emitIncrCounter emits a Go block that increments a TCL-counter string var
// by one (used to fire sqlite3_create_collation_v2 destructor counters).
func (tp *transpiler) emitIncrCounter(goName string) {
	if !isValidGoIdent(goName) {
		return
	}
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s = \"0\"", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("// destructor fired: incr %s", goName)
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("_n, _err := strconv.Atoi(%s)", goName)
	tp.emitLine("if _err == nil {")
	tp.emitLine("\t%s = strconv.Itoa(_n + 1)", goName)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

func (tp *transpiler) processExpr(args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	exprStr := args[0].Text
	result, err := tcl.EvalExpr(exprStr, nil, nil)
	if err == nil {
		tp.emitLine("// expr %s → %q", sanitizeTCLComment(exprStr), result)
		return
	}
	// `expr [cmd ...] OP value` — resolve the command substitution to a Go
	// expression and evaluate the comparison at runtime (dbstatus.test
	// 5.5.x: `expr [sqlite3_stmt_status $::stmt $id 0]>0`). Leave the TCL
	// "1"/"0" result in _r so the enclosing do_test compares it.
	if strings.Contains(exprStr, "[") && strings.Contains(exprStr, "]") {
		if goExpr, ok := tp.exprCmdToGo(exprStr); ok {
			tp.emitLine("// expr %s → runtime", sanitizeTCLComment(exprStr))
			tp.emitLine("_r = strconv.Itoa(%s)", goExpr)
			return
		}
		// `expr [cmd] OP N` — the cmd is a Go string-returning expression
		// (e.g. strconv.FormatInt(...)) and OP compares its numeric value
		// (dbstatus.test 5.5.x `expr [sqlite3_stmt_status ...]>0`).
		if r, ok := tp.exprCmdCompare(exprStr); ok {
			tp.emitLine("// expr %s → runtime compare", sanitizeTCLComment(exprStr))
			tp.emitLine("_r = %s", r)
			return
		}
	}
	// `expr $a OP $b` — runtime arithmetic over TCL variables (dbstatus.test
	// 2.x.a: `expr {$nSchema1-$nSchema2}`). Evaluate via tclExprWith with the
	// live variable values, leaving the decimal result in _r.
	exprVarNames, exprGo := tclExprToGo(exprStr, tp.vars)
	if len(exprVarNames) > 0 && exprGo != exprStr {
		pairs := make([]string, 0, len(exprVarNames))
		for _, v := range exprVarNames {
			pairs = append(pairs, fmt.Sprintf("%q: %s", v, tclVarToGo(strings.TrimPrefix(v, "::"))))
		}
		tp.emitLine("// expr %s → runtime", sanitizeTCLComment(exprStr))
		tp.emitLine("_r = tclExprWith(%q, map[string]string{%s})", exprGo, strings.Join(pairs, ", "))
		return
	}
	tp.emitLine("// expr %s (not evaluated)", sanitizeTCLComment(exprStr))
}

func (tp *transpiler) processCatch(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	bodyCmds := tp.parseBracedBody(args, 0)
	if bodyCmds == nil {
		tp.emitLine("// catch (non-braced)")
		return
	}

	resultVar := "_catchResult"
	errVar := "_catchErrMsg"
	hasResult := false
	if len(args) >= 2 {
		resultVar = tclVarToGo(args[1].Text)
		hasResult = true
	}
	if len(args) >= 3 {
		errVar = tclVarToGo(args[2].Text)
	}

	tp.emitLine("{")
	tp.indent++
	if hasResult {
		if !tp.isVarDeclared(resultVar) {
			tp.emitLine("var %s string // catch result (\"0\"=ok, \"1\"=error)", resultVar)
		}
		if !tp.isVarDeclared(errVar) {
			tp.emitLine("var %s string // catch error message", errVar)
		}
		tp.emitLine("_ = %s // suppress unused warning", resultVar)
		tp.emitLine("_ = %s // suppress unused warning", errVar)
	}
	tp.emitLine("var _catchErr error")
	if !hasResult {
		tp.emitLine("_ = _catchErr // suppress unused warning")
	}
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, dbClosed: tp.dbClosed, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, dbAliases: tp.dbAliases, queryVars: tp.queryVars, unsetVars: tp.unsetVars, dbVarFuncs: tp.dbVarFuncs, constFuncs: tp.constFuncs, rangeListFuncs: tp.rangeListFuncs, varCount: tp.varCount, pendingFileReset: tp.pendingFileReset, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, fixtureVar: tp.fixtureVar}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.dbClosed = bodyTP.dbClosed
	tp.dqsDDL = bodyTP.dqsDDL
	tp.dqsDML = bodyTP.dqsDML
	tp.varCount = bodyTP.varCount
	tp.queryVars = bodyTP.queryVars
	tp.unsetVars = bodyTP.unsetVars
	tp.dbVarFuncs = bodyTP.dbVarFuncs
	tp.constFuncs = bodyTP.constFuncs
	tp.dbAliases = bodyTP.dbAliases
	tp.pendingFileReset = bodyTP.pendingFileReset
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
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
	if hasResult {
		// After body, set the error message if there was an error
		tp.emitLine("if _catchErr != nil {")
		tp.indent++
		tp.emitLine("%s = \"1\"", resultVar)
		tp.emitLine("%s = _catchErr.Error()", errVar)
		tp.indent--
		tp.emitLine("} else {")
		tp.indent++
		tp.emitLine("%s = \"0\"", resultVar)
		tp.emitLine("%s = \"\"", errVar)
		tp.indent--
		tp.emitLine("}")
	}
	tp.indent--
	tp.emitLine("}")
}

// processStringAppend handles: append varName value...
// TCL append to string variable: append sql " WHERE x=1"
func (tp *transpiler) processStringAppend(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	goName := tclVarToGo(args[0].Text)
	valueExpr := tp.varValueExpr(args[1:])
	// Appending to a query var (e.g. `append sql ", t$i"`) keeps it a query.
	if tp.queryVars[args[0].Text] {
		tp.queryVars[args[0].Text] = true
	}
	tp.emitLine("%s += %s", goName, valueExpr)
}

// processListAppend handles: lappend varName value...
func (tp *transpiler) processListAppend(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	goName := tclVarToGo(args[0].Text)
	var items []string
	for _, a := range args[1:] {
		text := strings.TrimSpace(a.Text)
		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			inner := strings.TrimSpace(text[1 : len(text)-1])
			parts := strings.Fields(inner)
			if len(parts) >= 2 {
				if tmpl, ok := tp.specialFuncs[parts[0]]; ok && strings.Contains(tmpl, "$data") {
					data := tp.buildStringExpr(strings.TrimSpace(strings.TrimPrefix(inner, parts[0])))
					items = append(items, strings.Replace(tmpl, "$data", data, 1))
					continue
				}
			}
		}
		items = append(items, tp.goStringLiteral(a))
	}
	if len(items) == 1 {
		tp.emitLine("%s = tclListAppend(%s, %s)", goName, goName, items[0])
	} else {
		tp.emitLine("%s = tclListAppend(%s, %s)", goName, goName, strings.Join(items, ", "))
	}
}

// processList handles: list values...
// Creates a TCL list from values. If the result is used (via set v [list ...]),
// it becomes a variable assignment.
func (tp *transpiler) processList(args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	var items []string
	colmetaFound := false
	for _, a := range args {
		// `list [catch {BODY} VAR] ...` — execute the catch body (emitting
		// its side effects, e.g. sqlite3_blob_write) and use the catch
		// result var ("" / error message) in the list. This is the common
		// `list [catch {...} msg] $msg` assertion pattern.
		if v, ok := tp.emitListCatchArg(a); ok {
			items = append(items, v)
			continue
		}
		// `list [catch $tstbody msg] [set msg]` where tstbody holds a
		// sqlite3_table_column_metadata command (colmeta.test): emit the
		// metadata call and use its "{code {meta}}" result directly.
		if ok := tp.emitListColmetaArg(a); ok {
			colmetaFound = true
			continue
		}
		// `list [sqlite3_step $::stmt] ...` — execute the prepared
		// statement (SQL side effect) and use its result code in the list
		// (changes2.test's "SQLITE_DONE SQLITE_OK" assertion).
		if v, ok := tp.emitListStepArg(a); ok {
			items = append(items, v)
			continue
		}
		// `list ... [sqlite3_finalize $stmt]` — finalize the tracked
		// prepared statement; the element is the REAL finalize code
		// (SQLITE_OK, or the re-reported step error's code — vdbeapi.c
		// sqlite3VdbeFinalize).
		if v, ok := tp.emitListFinalizeArg(a); ok {
			items = append(items, v)
			continue
		}
		items = append(items, tp.goStringLiteral(a))
	}
	if colmetaFound {
		// The colmeta handler left the full "{code {meta}}" result in _r;
		// the do_test compares _r directly (no tclList wrapper).
		tp.emitLine("_ = _r // colmeta result")
		return
	}
	tp.emitLine("_list := tclList([]string{%s})", strings.Join(items, ", "))
	tp.emitLine("_ = _list")
	// The list result is also the do_test body value when a `list` command
	// closes a do_test body (e.g. `list [catch {sqlite3_blob_write ...} msg]
	// $msg`).
	tp.emitLine("_r = _list")
}

// emitListStepArg handles a `[sqlite3_step $stmt]` argument to a `list`
// command. It emits the prepared statement's execution (SQL side effect) and
// returns the Go expression for the step result code (changes2.test's
// "SQLITE_DONE SQLITE_OK" assertions). Returns ("", false) when the arg is not
// a sqlite3_step form.
func (tp *transpiler) emitListStepArg(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	if !strings.HasPrefix(text, "[sqlite3_step ") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "[sqlite3_step "), "]"))
	stmtVar := tclVarToGo(strings.TrimPrefix(inner, "$"))
	ps := tp.preparedStateRef()
	sql, ok := ps.stmts[stmtVar]
	if !ok {
		return "", false
	}
	rendered := renderPreparedSQL(sql, ps.binds[stmtVar])
	sqlExpr := fmt.Sprintf("%q", rendered)
	if strings.HasPrefix(rendered, "$") {
		gv := tclVarToGo(strings.TrimPrefix(rendered, "$"))
		if isValidGoIdent(gv) {
			sqlExpr = gv
		}
	}
	// A prepared ATTACH whose database name is a `file:` URI (e_uri.test)
	// probes C-API URI filename handling; detach first to keep the emulated
	// side effect idempotent.
	if strings.HasPrefix(strings.TrimSpace(rendered), "ATTACH") {
		if m := attachDBNames(rendered); len(m) > 0 {
			tp.emitLine("_res = db.Exec(\"DETACH %s\")", m[len(m)-1])
			tp.emitLine("_ = _res // tolerate not-attached")
		}
	}
	// sqlite3_step runs on the ACTUAL prepared statement handle (the one
	// sqlite3_prepare_v2 returned): the step records its failure on both
	// the statement and the connection, so a following
	// [sqlite3_finalize $stmt] re-reports the same error (backup5-1.6/1.7
	// step a statement whose table the backup just dropped →
	// {SQLITE_ERROR SQLITE_ERROR} and errmsg stays "no such table: t2").
	conn := "db"
	if c := ps.conns[stmtVar]; c != "" {
		conn = c
	}
	return fmt.Sprintf("tclStepPreparedCode(%s, %q, %s)", conn, stmtVar, sqlExpr), true
}

// emitListFinalizeArg handles a `[sqlite3_finalize $stmt]` argument to a
// `list` command: it emits nothing (the finalize runs when the list is
// built) and returns a call to the tclFinalizePreparedCode helper, which
// re-reports the statement's failed-step error code (vdbeapi.c
// sqlite3VdbeFinalize). Returns ("", false) when the arg is not a
// sqlite3_finalize form over a tracked prepared statement.
func (tp *transpiler) emitListFinalizeArg(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	if !strings.HasPrefix(text, "[sqlite3_finalize ") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "[sqlite3_finalize "), "]"))
	stmtVar := tclVarToGo(strings.TrimPrefix(inner, "$"))
	ps := tp.preparedStateRef()
	if _, ok := ps.stmts[stmtVar]; !ok {
		return "", false
	}
	conn := "db"
	if c := ps.conns[stmtVar]; c != "" {
		conn = c
	}
	return fmt.Sprintf("tclFinalizePreparedCode(%s, %q)", conn, stmtVar), true
}

// emitListColmetaArg handles a `[catch $tstbody msg]` argument to a `list`
// command where tstbody holds a sqlite3_table_column_metadata command
// (colmeta.test's `concat sqlite3_table_column_metadata $::DB $params`
// pattern). It emits the metadata call and leaves its "{code {meta}}" result
// in _r. The params are "$schema $table $column" (space-separated runtime
// words).
func (tp *transpiler) emitListColmetaArg(w tcl.RawWord) bool {
	text := strings.TrimSpace(w.Text)
	if !strings.HasPrefix(text, "[catch ") || !strings.HasSuffix(text, "]") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "[catch "), "]")
	// inner: $tstbody msg
	fields := strings.Fields(inner)
	if len(fields) < 1 || !strings.HasPrefix(fields[0], "$") {
		return false
	}
	cmdVar := tclVarToGo(strings.TrimPrefix(fields[0], "$"))
	cmd, ok := tp.colmetaCmds[cmdVar]
	if !ok {
		return false
	}
	// cmd = "sqlite3_table_column_metadata $DB $params" — extract the params
	// expression (the trailing $params variable reference).
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, "sqlite3_table_column_metadata"))
	words := strings.Fields(rest)
	if len(words) < 2 {
		return false
	}
	// The last word is the runtime params variable ($params), a space-separated
	// "schema table column" string. Split it at runtime.
	paramsVar := strings.TrimPrefix(words[len(words)-1], "$")
	paramsGo := tclVarToGo(paramsVar)
	if !isValidGoIdent(paramsGo) {
		return false
	}
	// Emit the metadata call: tclTableColumnMetadata(db, schema, table, col)
	// where schema/table/col are the 1st/2nd/3rd words of $params.
	tp.emitLine("_colmeta := tclSplitList(%s)", paramsGo)
	tp.emitLine("_schema := \"main\"; _table := \"\"; _col := \"\"")
	tp.emitLine("if len(_colmeta) >= 1 { _schema = _colmeta[0] }")
	tp.emitLine("if len(_colmeta) >= 2 { _table = _colmeta[1] }")
	tp.emitLine("if len(_colmeta) >= 3 { _col = _colmeta[2] }")
	tp.emitLine("_r = tclTableColumnMetadata(db, _schema, _table, _col)")
	return true
}

// emitListCatchArg handles a `[catch {BODY} VAR]` argument to a `list`
// command. It emits the catch body (side effects) and returns the Go
// expression for the catch result ("" on success, the error message on
// failure) plus the result var's string. Returns ("", false) when the arg is
// not a catch form.
func (tp *transpiler) emitListCatchArg(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	if !strings.HasPrefix(text, "[catch ") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "[catch "), "]")
	// inner: {BODY} VAR
	braceEnd := -1
	if strings.HasPrefix(inner, "{") {
		depth := 0
		for i := 0; i < len(inner); i++ {
			if inner[i] == '{' {
				depth++
			} else if inner[i] == '}' {
				depth--
				if depth == 0 {
					braceEnd = i
					break
				}
			}
		}
	}
	if braceEnd < 0 {
		return "", false
	}
	bodyText := inner[1:braceEnd]
	resultVar := strings.TrimSpace(inner[braceEnd+1:])
	if resultVar == "" {
		return "", false
	}
	// Emit the catch body and capture its error into the result var.
	bodyCmds := tcl.ParseCommands(bodyText)
	tp.emitLine("_rc := \"0\"")
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("var _catchErr error")
	goResult := tclVarToGo(resultVar)
	if !tp.isVarDeclared(goResult) {
		tp.emitLine("var %s string", goResult)
		tp.vars = append(tp.vars, goResult)
	}
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, dbClosed: tp.dbClosed, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, dbAliases: tp.dbAliases, queryVars: tp.queryVars, unsetVars: tp.unsetVars, dbVarFuncs: tp.dbVarFuncs, constFuncs: tp.constFuncs, rangeListFuncs: tp.rangeListFuncs, varCount: tp.varCount, pendingFileReset: tp.pendingFileReset, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, fixtureVar: tp.fixtureVar}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	tp.blobSeq = bodyTP.blobSeq
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
	// TCL catch returns "1" on error, "0" on success; the error message goes
	// into the result var (goResult).
	tp.emitLine("if _catchErr != nil { %s = _catchErr.Error() } else { %s = \"\" }", goResult, goResult)
	tp.emitLine("if _catchErr != nil { _rc = \"1\" }")
	tp.indent--
	tp.emitLine("}")
	return "_rc", true
}

// processClose handles: close $channel  or  db close
// In TCL tests this usually closes a database or file handle.
func (tp *transpiler) processClose(args []tcl.RawWord) {
	if len(args) >= 1 {
		ch := args[0].Text
		// close $var where var holds a blob channel name: resolve at runtime
		// (the transpile-time channel map is unreliable across body blocks;
		// the runtime resolution maps the var's current channel-name string
		// to the actual open handle).
		if strings.HasPrefix(ch, "$") {
			goName := tclVarToGo(strings.TrimPrefix(ch, "$"))
			if isValidGoIdent(goName) && tp.isVarDeclared(goName) && tp.isBlobVarName(goName) {
				tp.emitLine("tclBlobResolve(%s%s).Close()", goName, tp.blobArgsSuffix())
				return
			}
		}
		// close on an incremental-blob channel → Blob.Close() (static path)
		if goName := tp.resolveBlobChannel(args[0]); goName != "" {
			tp.emitLine("%s.Close()", goName)
			return
		}
		// db close → db.Close()
		if ch == "db" || ch == "$db" {
			tp.emitLine("db.Close()")
			return
		}
		// db2 close → db2.Close() (for secondary connections)
		if strings.HasPrefix(ch, "db") || strings.HasPrefix(ch, "$db") {
			goName := tclVarToGo(ch)
			// The connection may be nil (a skipped section never opened it);
			// guard the close.
			tp.emitLine("if %s != nil { %s.Close() }", goName, goName)
			return
		}
		// General close - emit as comment
		tp.emitLine("// close %s", describeArgsShort(args))
	}
}