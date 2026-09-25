// Package main implements the tcl2go tool.
//
// This file handles `db function NAME PROC` registrations: the dispatch over
// the recognized test-suite UDF shapes (processDBFunction), the named
// harness UDFs, the specialFuncs template emitters and the proc-name
// flag scan (procNameFromRest).
package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// processDBFunction handles `db function NAME procName` / `db func NAME
// procName` — register a scalar SQL function whose behavior is a TCL proc.
func (tp *transpiler) processDBFunction(rest []tcl.RawWord) {
	if len(rest) < 2 {
		tp.emitDBVarFunc(rest)
		return
	}
	name := strings.TrimSpace(rest[0].Text)
	procName := procNameFromRest(rest)
	if tp.emitDBFuncPrologue(name, procName, rest) ||
		tp.emitDBFuncNamedUDF(name, procName) ||
		tp.emitDBFuncFallbacks(name, procName, rest) {
		return
	}
	tp.emitDBVarFunc(rest)
}

// emitDBFuncPrologue handles the `db function` registrations resolved before
// the proc-body matchers: the eval passthrough, inline format bodies,
// variable-accumulating procs and the TCL string-match glob UDF. Reports
// whether a shape matched and was emitted.
func (tp *transpiler) emitDBFuncPrologue(name, procName string, rest []tcl.RawWord) bool {
	// `db func eval <proc>` where the proc runs SQL text (misc8.test's dbeval:
	// `proc dbeval {sql} { db eval $sql }`). The engine's built-in eval()
	// already runs SQL and returns the joined result (EvalExecSQL), so skip
	// the variable-reader stub registration — a nil-returning stub would
	// shadow the real eval and break DELETE/SELECT execution.
	if strings.EqualFold(name, "eval") {
		tp.emitLine("// db func eval %s (db-eval passthrough — built-in eval used)", procName)
		return true
	}
	// `db function hex {format 0x%X}` — an inline single-format-command body
	// (emitted by emitInlineFormatUDF, processdb_format_udf.go).
	if tp.emitInlineFormatUDF(name, rest) {
		return true
	}
	// A TCL proc whose body accumulates into a global variable (a counter or
	// a log): selectH.test's counter (global selectH_cnt; incr ... $amt;
	// return $amt-var), subquery.test's callcnt (incr ::callcnt; return $n)
	// and wherelimit2.test's log (lappend ::log {*}$args). The generated UDF
	// updates the SAME Go variable the assertions read back.
	if tp.emitTclVarUDFFromProc(name, procName) {
		return true
	}
	// `db function match -argcount 2 test_match` — like.test's MATCH
	// overload whose proc body is a single `string match $a $b` command
	// (TCL glob): emit a real glob-based closure instead of a nil stub so
	// the MATCH operator filters rows (like-2.3/2.4).
	if body, ok := globalProcBodies[procName]; ok && isStringMatchBody(body) {
		tp.emitStringMatchUDF(name, procName)
		return true
	}
	return false
}

// emitStringMatchUDF emits the anchored-glob MATCH overload closure for
// `db function match test_match`.
func (tp *transpiler) emitStringMatchUDF(name, procName string) {
	tp.emitLine("// db function %s %s (TCL string match UDF: anchored glob of args[1] against args[0])", name, procName)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 2 { return nil, nil }")
	tp.emitLine("\tpat := function.ValueText(args[0])")
	tp.emitLine("\tstr := function.ValueText(args[1])")
	tp.emitLine("\tif tclStringMatch(pat, str) { return int64(1), nil }")
	tp.emitLine("\treturn int64(0), nil")
	tp.emitLine("}, 2, 2)")
}

// emitDBFuncNamedUDF registers the harness UDFs keyed on the function NAME
// itself (execsql / pmatch / sql / extract, plus the tkt3718 proc-body
// shapes). Reports whether a shape matched and was emitted.
func (tp *transpiler) emitDBFuncNamedUDF(name, procName string) bool {
	// `db function execsql execsql` — the test-harness's execsql command
	// (tkt3080.test) is registered as a SQL function. The body recursively
	// runs its first argument as SQL and returns the joined cell result
	// (or NULL for DDL/DML). This is a generic SQL-executing UDF — emit
	// the same RegisterFunction that the native UCL test uses
	// (frigolite_misc_native_test.go::TestNativeMiscUDFFromHarnessExecutesSQL).
	if strings.EqualFold(name, "execsql") && strings.EqualFold(procName, "execsql") {
		tp.emitExecsqlHarnessUDF(name)
		return true
	}
	// `db func pmatch pmatch` (fts5phrase.test:81) — the substring-probe
	// oracle UDF: proc pmatch {col expr} { return [expr {[string first
	// $expr $col]>=0}] }. Its WHERE clauses generate the expected rowids
	// for the phrase-query comparison loop.
	if strings.EqualFold(name, "pmatch") && strings.EqualFold(procName, "pmatch") {
		tp.emitPmatchHarnessUDF(name)
		return true
	}
	// tkt3718.test's f1/f2 recurse-SQL proc shapes (and filefmt's a_string):
	// see emitMiscRecurseSQLUDF.
	if tp.emitMiscRecurseSQLUDF(name, procName) {
		return true
	}
	// `db func sql [list sql]` (tkt3718-2.1+) — the proc body executes its
	// second argument as SQL when the first is truthy. The transpiler
	// tokenizes `[list sql]` as the literal word "[list sql]", so the
	// procNameFromRest returns "[list"; detect either form.
	if strings.EqualFold(name, "sql") && (strings.EqualFold(procName, "sql") || strings.EqualFold(procName, "[list")) {
		tp.emitConditionalSQLUDF(name)
		return true
	}
	return false
}

// emitExecsqlHarnessUDF emits the tkt3080 test-harness execsql UDF (a
// recursive SQL-executing function).
func (tp *transpiler) emitExecsqlHarnessUDF(name string) {
	tp.emitLine("// db function execsql execsql (test-harness SQL-executing UDF — P8.MISC)")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\tsqlStr := function.ValueText(args[0])")
	tp.emitLine("\tif sqlStr == \"\" { return nil, nil }")
	tp.emitLine("\t// Mark the calling SELECT as an active read statement so")
	tp.emitLine("\t// DROP TABLE inside the recursive SQL triggers the")
	tp.emitLine("\t// OP_Destroy interlock (tkt3080.3 expects 'database table is locked').")
	tp.emitLine("\tdb.BeginActiveStatement()")
	tp.emitLine("\tdefer db.EndActiveStatement()")
	tp.emitLine("\tupper := strings.TrimSpace(strings.ToUpper(sqlStr))")
	tp.emitLine("\tisSelect := strings.HasPrefix(upper, \"SELECT\") || strings.HasPrefix(upper, \"WITH\")")
	tp.emitLine("\tif isSelect {")
	tp.emitLine("\t\tout, err := db.EvalExecSQL(sqlStr, \" \")")
	tp.emitLine("\t\tif err != nil { return nil, err }")
	tp.emitLine("\t\tif out == \"\" { return nil, nil }")
	tp.emitLine("\t\treturn out, nil")
	tp.emitLine("\t}")
	tp.emitLine("\tif r := db.Exec(sqlStr); r.Error != nil { return nil, r.Error }")
	tp.emitLine("\treturn nil, nil")
	tp.emitLine("}, 1, -1)")
}

// emitPmatchHarnessUDF emits the fts5phrase.test substring-probe oracle UDF.
func (tp *transpiler) emitPmatchHarnessUDF(name string) {
	tp.emitLine("// db func pmatch pmatch (fts5phrase.test substring-probe oracle UDF)")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 2 || args[0] == nil || args[1] == nil { return int64(0), nil }")
	tp.emitLine("\tcol := function.ValueText(args[0])")
	tp.emitLine("\texpr := function.ValueText(args[1])")
	tp.emitLine("\tif strings.Contains(col, expr) { return int64(1), nil }")
	tp.emitLine("\treturn int64(0), nil")
	tp.emitLine("}, 0, -1)")
}

// emitConditionalSQLUDF emits the tkt3718-2.1+ conditional SQL-execute UDF.
func (tp *transpiler) emitConditionalSQLUDF(name string) {
	tp.emitLine("// db func sql sql (test-harness conditional SQL-execute UDF — P8.MISC tkt3718)")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 2 || args[0] == nil || args[1] == nil { return nil, nil }")
	tp.emitLine("\tdoit := function.ValueText(args[0])")
	tp.emitLine("\tif doit == \"\" || doit == \"0\" { return nil, nil }")
	tp.emitLine("\tzSql := function.ValueText(args[1])")
	tp.emitLine("\tif zSql == \"\" { return nil, nil }")
	tp.emitLine("\t// Mark the calling SELECT as an active read statement so")
	tp.emitLine("\t// DDL/DML inside the recursive SQL triggers the OP_Destroy")
	tp.emitLine("\t// interlock (tkt3718-* nesting case).")
	tp.emitLine("\tdb.BeginActiveStatement()")
	tp.emitLine("\tdefer db.EndActiveStatement()")
	tp.emitLine("\t// Detect SELECT prefix and route through EvalExecSQL so the")
	tp.emitLine("\t// joined cells come back as a string (matching TCL catchsql).")
	tp.emitLine("\tupper := strings.TrimSpace(strings.ToUpper(zSql))")
	tp.emitLine("\tif strings.HasPrefix(upper, \"SELECT\") || strings.HasPrefix(upper, \"WITH\") {")
	tp.emitLine("\t\tout, err := db.EvalExecSQL(zSql, \" \")")
	tp.emitLine("\t\tif err != nil { return nil, nil }")
	tp.emitLine("\t\tif out == \"\" { return nil, nil }")
	tp.emitLine("\t\treturn out, nil")
	tp.emitLine("\t}")
	tp.emitLine("\t// TCL's catchsql swallows errors and returns the result; mimic")
	tp.emitLine("\t// by ignoring db.Exec error here so the calling INSERT survives.")
	tp.emitLine("\tdb.Exec(zSql)")
	tp.emitLine("\treturn nil, nil")
	tp.emitLine("}, 2, 2)")
}

// emitDBFuncFallbacks handles the remaining `db function` registrations after
// the named matchers: the TCL eval-command UDF, the collected registered-
// function patterns, the extract helper, the specialFuncs templates and the
// int32 blob-surgery harness procs. Reports whether a shape matched.
func (tp *transpiler) emitDBFuncFallbacks(name, procName string, rest []tcl.RawWord) bool {
	// `db function tcl eval` — SQL function backed by the TCL *eval*
	// built-in command (tkt3992-2.3); see emitTclEvalCommandUDF.
	if tp.emitTclEvalCommandUDF(name, procName) {
		return true
	}
	if tp.emitRegisteredFunction(name, procName, rest) {
		return true
	}
	// `db func extract extract` (fts3offsets.test) — the proc annotates the
	// document text with parentheses at each offsets() hit span.
	if name == "extract" && procName == "extract" {
		tp.emitExtractUDF(name)
		return true
	}
	if tp.emitSpecialFuncTemplate(name, procName) {
		return true
	}
	if tp.emitSwapInt32UDF(name, procName, "swap_int32", false) {
		return true
	}
	if tp.emitSwapInt32UDF(name, procName, "set_int32", true) {
		return true
	}
	return tp.emitZipFuncVar(name, procName)
}

// emitExtractUDF emits the fts3offsets.test extract UDF.
func (tp *transpiler) emitExtractUDF(name string) {
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 2 { return \"\", nil }")
	tp.emitLine("\treturn tclExtractOffsets(tclStr(args[0]), tclStr(args[1])), nil")
	tp.emitLine("}, 2, 2)")
}

// emitSpecialFuncTemplate registers a specialFunc template proc (the
// test-harness blob() hex decoder, fts3 matchinfo decoder, fts4 record
// builder) as a SQL function. Reports whether a template matched.
func (tp *transpiler) emitSpecialFuncTemplate(name, procName string) bool {
	// `db func blob blob` — the proc is a specialFunc (e.g. the test-harness
	// blob() hex decoder, fts3corrupt4). Emit a RegisterFunction whose body
	// decodes the argument.
	if tp.specialFuncs == nil || name == "" {
		return false
	}
	tmpl, ok := tp.specialFuncs[procName]
	if !ok {
		return false
	}
	switch tmpl {
	case "tclBlobHexDecode":
		tp.emitBlobHexDecodeUDF(name, procName)
		return true
	case "tclMatchinfoDecode":
		tp.emitMatchinfoDecodeUDF(name)
		return true
	case "tclFts3Record":
		tp.emitFts3RecordUDF(name)
		return true
	}
	return false
}

// emitBlobHexDecodeUDF emits the harness blob() hex decoder (with the
// engine-level []byte handling for remove_timestamps).
func (tp *transpiler) emitBlobHexDecodeUDF(name, procName string) {
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return []byte{}, nil }")
	// Engine-level BLOBs (zipfile() archive results feeding
	// remove_timestamps) arrive as []byte, not hex text.
	if procName == "remove_timestamps" {
		tp.emitLine("\tif b, ok := args[0].([]byte); ok { return tclRemoveTimestamps(b), nil }")
		tp.emitLine("\tif s, ok := args[0].(string); ok { return tclRemoveTimestamps(tclHexDecode(s)), nil }")
		tp.emitLine("\treturn tclHexDecode(tclStr(args[0])), nil")
	} else {
		tp.emitLine("\treturn tclHexDecode(tclStr(args[0])), nil")
	}
	tp.emitLine("}, 0, -1)")
}

// emitMatchinfoDecodeUDF emits the fts3 matchinfo decoder UDF.
func (tp *transpiler) emitMatchinfoDecodeUDF(name string) {
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return \"\", nil }")
	tp.emitLine("\treturn tclMatchinfoDecode(args[0]), nil")
	tp.emitLine("}, 0, -1)")
}

// emitFts3RecordUDF emits the fts4record.test make_fts3record wrapper UDF.
func (tp *transpiler) emitFts3RecordUDF(name string) {
	// `db func record make_record_wrapper` — the wrapper calls
	// make_fts3record $args: build an FTS3 segment record blob
	// from the SQL function's arguments (fts4record.test).
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\treturn tclFts3Record(args), nil")
	tp.emitLine("}, 0, -1)")
}

// emitSwapInt32UDF registers rtreecheck.test's blob-surgery procs
// (`db func swap_int32 swap_int32` / `db func set_int32 set_int32` — big-
// endian u32 word swap / overwrite over %_node data blobs). Real closures,
// not $data templates: both procs take exactly three arguments.
func (tp *transpiler) emitSwapInt32UDF(name, procName, want string, set bool) bool {
	if procName != want || !(name == procName || !strings.HasPrefix(name, "$")) {
		return false
	}
	setArg := "false"
	if set {
		setArg = "true"
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tb, err := tclSwapInt32Args(args, %s)", setArg)
	tp.emitLine("\tif err != nil { return nil, err }")
	tp.emitLine("\treturn b, nil")
	tp.emitLine("}, 3, 3)")
	return true
}

// emitZipFuncVar handles `db func $zip zip` — the name is a runtime variable
// (loop var holding "zip" or "z.i.p!!") and the proc is the fts3comp1
// compression harness. Emit a stateful closure registered under the runtime
// variable value so FTS4 compress='<value>' finds it (fts3comp1 1.x: content
// table stores the integer keys returned by zip).
func (tp *transpiler) emitZipFuncVar(name, procName string) bool {
	if !strings.HasPrefix(name, "$") || !tp.zipUnzipFunc(procName) {
		return false
	}
	if tp.specialFuncs == nil {
		return false
	}
	if tmpl, ok := tp.specialFuncs[procName]; ok {
		tp.emitZipUnzipFunction(strings.TrimPrefix(name, "$"), procName, tmpl)
		return true
	}
	return false
}

// emitTclEvalCommandUDF emits the RegisterFunction for `db function tcl
// eval` (tkt3992-2.3) — the SQL function `tcl` is backed by the TCL *eval*
// built-in command: `tcl('set res', <value>)` concatenates its arguments
// and evaluates "set res <value>", i.e. the scalar variable res receives
// the second argument. The generated code only depends on that
// `set VAR VALUE` shape; wire each pre-scanned variable (genTclEvalSetVars,
// see scanTclEvalSetVars) into the closure. Returns true when emitted.
func (tp *transpiler) emitTclEvalCommandUDF(name, procName string) bool {
	if !strings.EqualFold(name, "tcl") || !strings.EqualFold(procName, "eval") || len(genTclEvalSetVars) == 0 {
		return false
	}
	tp.emitLine("// db function tcl eval (TCL eval-command UDF: tcl('set VAR', value) sets VAR)")
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) >= 2 {")
	tp.emitLine("\t\tparts := make([]string, 0, len(args))")
	tp.emitLine("\t\tfor _, a := range args { parts = append(parts, tclStr(a)) }")
	tp.emitLine("\t\tf := strings.Fields(strings.Join(parts, \" \"))")
	tp.emitLine("\t\tif len(f) == 3 && f[0] == \"set\" {")
	tp.emitLine("\t\t\tswitch f[1] {")
	for _, v := range genTclEvalSetVars {
		tp.emitLine("\t\t\tcase %q:", v)
		tp.emitLine("\t\t\t\t%s = f[2]", v)
		tp.emitLine("\t\t\t\tvtab.TclVarSet(%q, \"\", f[2])", v)
	}
	tp.emitLine("\t\t\t}")
	tp.emitLine("\t\t}")
	tp.emitLine("\t}")
	tp.emitLine("\treturn nil, nil")
	tp.emitLine("}, 0, -1)")
	return true
}

// procNameFromRest finds the TCL proc name in `db func NAME [-deterministic]
// PROC` — the first non-flag argument (a braced word like {joinx cross}
// contributes its first token). Value-taking flags (-argcount N,
// -returntype T) consume their value; boolean flags (-deterministic,
// -directonly, -innocuous) do not — `-deterministic myfunc` previously
// swallowed the proc name and the registration degraded to a nil stub
// (check-7.x).
func procNameFromRest(rest []tcl.RawWord) string {
	skipNext := false
	for _, a := range rest[1:] {
		arg := strings.TrimSpace(a.Text)
		if arg == "" {
			continue
		}
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			skipNext = procFlagSkipsValue(arg)
			continue
		}
		if fields := strings.Fields(arg); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// procFlagSkipsValue reports whether a `db func` flag consumes the following
// word as its value: value flags (-argcount 2, -returntype T) do, boolean
// flags (-deterministic/-directonly/-innocuous) and inline -flag=value forms
// do not.
func procFlagSkipsValue(arg string) bool {
	switch strings.ToLower(strings.TrimPrefix(arg, "-")) {
	case "deterministic", "directonly", "innocuous":
		// Boolean flags: no value consumed.
		return false
	}
	return !strings.Contains(arg, "=")
}

// isStringMatchBody reports whether a proc body is a single
// `string match $a $b` command (like.test's test_match), the TCL-glob
// MATCH overload shape.
func isStringMatchBody(body string) bool {
	b := strings.TrimSpace(body)
	if strings.HasPrefix(b, "{") && strings.HasSuffix(b, "}") {
		b = strings.TrimSpace(b[1 : len(b)-1])
	}
	if strings.HasPrefix(strings.ToLower(b), "return ") {
		b = strings.TrimSpace(b[len("return "):])
	}
	// Allow both a bare command and a bracket-command word:
	// `[string match $a $b]`.
	if strings.HasPrefix(b, "[") && strings.HasSuffix(b, "]") {
		b = strings.TrimSpace(b[1 : len(b)-1])
	}
	cmds := tcl.ParseCommands(b)
	if len(cmds) != 1 {
		return false
	}
	w := cmds[0]
	if len(w) < 3 || w[0].Text != "string" || w[1].Text != "match" {
		return false
	}
	for _, a := range w[2:] {
		if !strings.HasPrefix(a.Text, "$") {
			return false
		}
	}
	return true
}
