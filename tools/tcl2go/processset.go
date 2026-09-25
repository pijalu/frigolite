// Package main implements the tcl2go tool.
//
// This file handles the TCL set command: the processSet entry point, the
// plain (non-namespace) set path, and the DB-connection skip logic.
// Namespace (`set ::var`) sets live in processset_namespace.go, prepared-
// statement values in processset_prepare.go, bracket values in
// processset_bracket*.go, and shared helpers in processset_vars.go.
package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ---- Variable handlers ----

func (tp *transpiler) processSet(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}

	// A `set var value` gives the variable a value, so it is no longer
	// unset — clear any NULL-marking so $var renders as the value.
	goName := tclVarToGo(args[0].Text)
	if tp.unsetVars != nil {
		delete(tp.unsetVars, goName)
	}
	tp.setIntarrayEvalVar(goName, args)

	// set ::STMT [sqlite3_prepare db "SQL" -1 TAIL] — record the prepared
	// statement so later sqlite3_bind_* / sqlite3_step / sqlite3_reset /
	// sqlite3_finalize calls can be emulated as plain db.Exec INSERTs (the
	// C API itself has no Go equivalent, but the test state it creates does).
	if len(args) >= 2 {
		prepareText := strings.TrimSpace(args[1].Text)
		if tp.maybeRecordPreparedSet(goName, prepareText) {
			return
		}
		tp.trackSetOpenChannel(goName, args[1])
	}

	// Skip set testdir [file dirname $argv0] etc - infrastructure
	if len(args) >= 1 && tp.processNamespaceSet(args) {
		return
	}

	tp.processSetPlain(args)

	// Quota flag mirroring (quota.test / quota2.test): `set
	// ::quota_request_ok V` also records into the runtime registry so the
	// transpiled quota callback closure's `info exists` / value check sees
	// the flag (vtab.TclVarExists / TclVarGet).
	if goName == "quota_request_ok" && len(args) >= 2 {
		valExpr := tp.goStringLiteral(args[1])
		tp.emitLine("%s", `vtab.TclVarSet("quota_request_ok", "", `+valExpr+`)`)
	}
}

// setIntarrayEvalVar flags a TCL var whose value is the literal command
// `sqlite3_intarray_bind` (intarray.test builds such a script var and later
// `eval`s it). The dynamic eval site then dispatches to the runtime
// intarray-bind handler.
func (tp *transpiler) setIntarrayEvalVar(goName string, args []tcl.RawWord) {
	if len(args) >= 2 && strings.HasPrefix(strings.TrimSpace(args[1].Text), "sqlite3_intarray_bind") {
		if tp.intarrayEvalVars == nil {
			tp.intarrayEvalVars = make(map[string]bool)
		}
		tp.intarrayEvalVars[goName] = true
	}
}

// maybeRecordPreparedSet recognizes a prepare-command value
// (`[sqlite3_prepare ...]` or the bracket-stripped form) and records the
// prepared statement. Returns true when handled.
func (tp *transpiler) maybeRecordPreparedSet(goName, prepareText string) bool {
	if strings.HasPrefix(prepareText, "[sqlite3_prepare") && strings.HasSuffix(prepareText, "]") {
		tp.recordPreparedStatement(goName, prepareText)
		return true
	}
	// Bracket delimiters may already be removed when command is nested in
	// catch body; retain same prepare handling for that parser form.
	if strings.HasPrefix(prepareText, "sqlite3_prepare") {
		tp.recordPreparedStatement(goName, "["+prepareText+"]")
		return true
	}
	return false
}

// trackSetOpenChannel handles `set fd [open FILE MODE]` — track the channel's
// path so subsequent `puts $fd TEXT` writes to the right file (regardless of
// MODE: wb, r+, etc.; the corrupt*.test suites open test.db r+ and overwrite
// a byte at a known offset to simulate corruption). Without this, packages
// that run AFTER another test that opens test.tcl for write would inherit
// `activeFileChannels["fd"] = "test.tcl"` and write corruption bytes to the
// wrong file.
func (tp *transpiler) trackSetOpenChannel(goName string, word tcl.RawWord) {
	if path, mode, ok := parseOpenChannelWord(word.Text); ok {
		if strings.HasPrefix(path, "$") {
			activeFileChannels[goName] = tclVarToGo(strings.TrimPrefix(path, "$"))
			activeFileChannelExprs[goName] = true
			if strings.Contains(mode, "w") {
				tp.emitLine("_ = os.WriteFile(%s, nil, 0644)", activeFileChannels[goName])
			}
		} else {
			activeFileChannels[goName] = path
			if strings.Contains(mode, "w") {
				tp.emitLine("_ = os.WriteFile(%s, nil, 0644)", strconv.Quote(path))
			}
		}
	}
}

// trackArrayKey records a TCL array literal-key assignment (set arr(K) V) so a
// later $arr($keyvar) reference can be transpiled to a runtime selection.
func (tp *transpiler) trackArrayKey(name string) {
	idx := strings.Index(name, "(")
	if idx <= 0 || !strings.HasSuffix(name, ")") {
		return
	}
	base := name[:idx]
	key := name[idx+1 : len(name)-1]
	if base == "" || key == "" || key == "*" || strings.HasPrefix(key, "$") {
		return
	}
	if tp.arrayKeys == nil {
		tp.arrayKeys = make(map[string][]string)
	}
	tp.arrayKeys[base] = append(tp.arrayKeys[base], key)
}

// dynamicArraySet detects a dynamic-key array assignment `set arr($keyvar) V`
// where arr is registered as a map variable. Returns the base array name, the
// key expression (the $var), and true when this form applies.
func (tp *transpiler) dynamicArraySet(name string) (string, string, bool) {
	idx := strings.Index(name, "(")
	if idx <= 0 || !strings.HasSuffix(name, ")") {
		return "", "", false
	}
	base := name[:idx]
	key := name[idx+1 : len(name)-1]
	if base == "" || !strings.HasPrefix(key, "$") {
		return "", "", false
	}
	if isArrayMapBacked(tp, base) {
		return base, strings.TrimPrefix(key, "$"), true
	}
	return "", "", false
}

// mapKeyGoExpr renders the Go index expression for a map-backed TCL array.
// The key text may combine a variable reference with literal characters
// (`$method,t2` — the TCL array key is the substituted concatenation), so a
// dynamic key is re-parsed through the string-parts path emitting the runtime
// concatenation (`method + ",t2"`). Folding the whole key through the name
// sanitizer instead produced an undefined identifier (`method_t2`,
// vtab1-16.x). A literal key renders as a quoted constant.
func (tp *transpiler) mapKeyGoExpr(key string) string {
	if !strings.HasPrefix(key, "$") {
		return strconv.Quote(key)
	}
	return tp.buildStringExpr(key)
}

// emitDynamicArraySet emits `arrMap[keyExpr] = value` for a dynamic-key array
// assignment. The key expression is the loop/var holding the key; the value is
// the remaining set arguments rendered as a string expression.
func (tp *transpiler) emitDynamicArraySet(base, keyVar string, args []tcl.RawWord) {
	mapVar := tclVarToGo(base) + "Map"
	keyExpr := tp.mapKeyGoExpr("$" + keyVar)
	valExpr := `""`
	if len(args) >= 2 {
		valExpr = tp.goStringLiteral(args[1])
	}
	tp.emitLine("%s[%s] = %s", mapVar, keyExpr, valExpr)
}

// processSetPlain handles `set var ...` for plain (non-namespace) variable
// names: identifier checks, the err/db redirects, and bracket-command value
// dispatch.
func (tp *transpiler) processSetPlain(args []tcl.RawWord) {
	// Dynamic-key array assignment `set arr($keyvar) V`: emit a Go map store
	// arrMap[keyvar] = V (the array is declared as map[string]string in the
	// preamble because its keys are runtime values). `set arr($keyvar)` with
	// NO value is TCL's READ form — its result is captured as the enclosing
	// do_test body's got value, never a store (fts3sort tn.9: writing ""
	// cleared the control value before the read and every comparison
	// mismatched).
	if base, key, isDyn := tp.dynamicArraySet(args[0].Text); isDyn && len(args) >= 2 {
		tp.emitDynamicArraySet(base, key, args)
		return
	}
	tp.emitTclvarRegistrySet(args)
	goName := tclVarToGo(args[0].Text)
	tp.trackArrayKey(args[0].Text)
	if goName == "" || !isValidGoIdent(goName) {
		// Variable name is not a valid Go identifier — skip
		tp.emitLine("// set %s (invalid identifier, skipped)", args[0].Text)
		return
	}
	// Avoid type conflicts: 'err' is Go error type in preamble, 'db' is *frigolite.DB.
	// Redirect TCL string assignments to separate variables.
	goName = tp.redirectErrVar(goName)
	// Skip assignments to DB connection variables (db, db1-db9) from sqlite3_open
	// or other commands that return non-DB values — these would cause type conflicts.
	if tp.skipDBConnectionSet(goName, args) {
		return
	}
	rest := args[1:]

	if tp.setHarnessPinnedVar(goName, rest) {
		return
	}

	if len(rest) == 0 {
		return
	}

	// Scalar set commands mirror into the tclvar registry: generated tests
	// seed module-visible interpreter state through plain sets
	// (`set x1 aback` feeding a tclvar scan), which otherwise never reach
	// the virtual table.
	tp.emitScalarTclvarMirror(args, rest)

	// set var [cmd ...] — dispatch the bracket-command special cases.
	if tp.plainBracketValueDispatch(goName, args, rest) {
		return
	}

	// set VAR "concat $tests {LIST}" (or [concat $tests {LIST}]) — the TCL
	// test-suite idiom that appends a literal TCL list to a variable holding
	// another list (colmeta.test's $tests accumulation). Evaluate the concat
	// at transpile time so the runtime variable holds the combined list.
	if tp.processSetConcatList(goName, rest) {
		return
	}

	tp.processSetGeneric(goName, args, rest)
}

// emitTclvarRegistrySet registers array/scalar sets into the tclvar
// virtual-table registry.
func (tp *transpiler) emitTclvarRegistrySet(args []tcl.RawWord) {
	// `set arr(key) value` with a literal key also registers into the tclvar
	// virtual-table registry so USING tclvar scans see it (test_tclvar.c).
	base, key, isElem := splitArrayElement(args[0].Text)
	if isElem && !strings.Contains(key, "$") {
		markTclvarBase(base)
		if len(args) >= 2 {
			// Write form.
			valExpr := tp.varValueExpr(args[1:])
			tp.emitLine("vtab.TclVarSet(%q, %q, %s)", base, key, valExpr)
			// Map-backed arrays keep the Go map in sync so the read form
			// below (and incr) observe the write through the same store.
			if isArrayMapBacked(tp, base) {
				tp.emitLine("%sMap[%q] = %s", tclVarToGo(base), key, valExpr)
			}
		} else {
			// Read form (`set arr(key)` with no value): fetch the element.
			goRead := tclVarToGo(args[0].Text)
			if isArrayMapBacked(tp, base) {
				// Map-backed array: read the Go map the writes populate.
				tp.emitLine("%s = %sMap[%q]", goRead, tclVarToGo(base), key)
			} else {
				tp.emitLine("%s = vtab.TclVarGet(%q, %q)", goRead, base, key)
			}
		}
	} else if !isElem && len(args) >= 2 && isValidGoIdent(tclVarToGo(base)) {
		// Scalars are registered too: tclvar exposes the whole interpreter
		// namespace (vtabH-2.0: set xyz 10 then WHERE name='xyz').
		valExpr := tp.varValueExpr(args[1:])
		tp.emitLine("vtab.TclVarSet(%q, %q, %s)", base, "", valExpr)
	} else if !isElem && len(args) >= 2 && isValidGoIdent(tclVarToGo(base)) {
		// Scalars are registered too: tclvar exposes the whole interpreter
		// namespace (vtabH-2.0: set xyz 10 then SELECT ... WHERE name='xyz').
		valExpr := tp.varValueExpr(args[1:])
		tp.emitLine("vtab.TclVarSet(%q, %q, %s)", base, "", valExpr)
	}
}

// emitScalarTclvarMirror mirrors plain scalar sets into the tclvar registry.
func (tp *transpiler) emitScalarTclvarMirror(args, rest []tcl.RawWord) {
	if !strings.Contains(args[0].Text, "(") && len(rest) > 0 && !strings.HasPrefix(strings.TrimSpace(rest[0].Text), "[") {
		valExpr := tp.varValueExpr(rest)
		tp.emitLine(`vtab.TclVarSet(%q, "", %s)`, args[0].Text, valExpr)
	}
}

// plainBracketValueDispatch handles `set var [cmd ...]` — the bracket-command
// special cases. A command-substitution word may be represented with a leading
// space inside the brackets (TCL `set var [ expr {..} ]`); isBracketWord keys
// off !Braced, so also accept any single word whose trimmed text starts with
// "[" as a command substitution. Returns true when the value was handled.
func (tp *transpiler) plainBracketValueDispatch(goName string, args, rest []tcl.RawWord) bool {
	if len(rest) == 1 && (isBracketWord(rest[0]) || strings.HasPrefix(strings.TrimSpace(rest[0].Text), "[")) {
		cmdText := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(rest[0].Text), "["), "]"))
		// Dynamic-key array assignment (`set ARR($key) [cmd]`): store into
		// the XxxMap Go map instead of a scalar variable.
		if base, key, isDyn := tp.dynamicArraySet(args[0].Text); isDyn {
			valExpr := tp.cmdExpr(cmdText)
			mapVar := tclVarToGo(base) + "Map"
			if !tp.isVarDeclared(mapVar) {
				tp.emitLine("%s := map[string]string{}", mapVar)
				tp.vars = append(tp.vars, mapVar)
			}
			tp.emitLine("%s[%s] = %s", mapVar, tclVarToGo(key), valExpr)
			return true
		}
		if tp.processSetBracketValue(goName, cmdText) {
			return true
		}
		// [time { SCRIPT }] and [lindex [time { SCRIPT }] N]: transpile the
		// inner script; timing is not measured, so the variable is bound to
		// "" (time) or "0" (lindex-time, for $microsec<10000000-style
		// comparisons).
		if tp.processSetTimedValue(goName, rest[0].Text) {
			return true
		}
	}
	return false
}

// processSetConcatList handles `set VAR "concat $OTHER {LIST}"` and
// `set VAR [concat $OTHER {LIST}]` — the TCL test-suite idiom that appends a
// literal braced TCL list to a tracked variable holding another list
// (colmeta.test's `set tests "concat $tests {100 ...}"`). The combined list is
// stored in varConstValues so a later `foreach ... $tests` iterates the real
// entries. Returns true when handled.
func (tp *transpiler) processSetConcatList(goName string, rest []tcl.RawWord) bool {
	if len(rest) < 1 {
		return false
	}
	varName, braced, ok := parseConcatListForm(strings.TrimSpace(rest[0].Text))
	if !ok {
		return false
	}
	baseVar := strings.TrimPrefix(varName, "$")
	baseGo := tclVarToGo(baseVar)
	base := tp.varConstValues[baseGo]
	if base == "" && !tp.isVarDeclared(baseGo) {
		return false
	}
	// Strip outer braces of the appended list.
	appended := strings.TrimSpace(braced)
	appended = strings.TrimPrefix(appended, "{")
	appended = strings.TrimSuffix(appended, "}")
	combined := combineConstListValues(base, appended)
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = combined
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, tp.goStringLiteral(tcl.RawWord{Text: combined}))
	} else {
		tp.emitLine("var %s = %s", goName, tp.goStringLiteral(tcl.RawWord{Text: combined}))
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// parseConcatListForm parses a `concat $VAR {LIST}` value word (with an
// optional bracket wrapper) into the source variable reference and the
// trailing braced list. Returns ok=false for non-concat words.
func parseConcatListForm(text string) (varName, braced string, ok bool) {
	// Unwrap a bracket wrapper: [concat ...]
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	if !strings.HasPrefix(text, "concat ") {
		return "", "", false
	}
	restStr := strings.TrimSpace(strings.TrimPrefix(text, "concat "))
	// Find the variable reference ($tests) and the trailing braced list.
	if i := strings.Index(restStr, "{"); i >= 0 {
		varName = strings.TrimSpace(restStr[:i])
		braced = strings.TrimSpace(restStr[i:])
	}
	if varName == "" {
		return "", "", false
	}
	return varName, braced, true
}

// combineConstListValues joins the tracked base list and the appended literal
// list with a single space.
func combineConstListValues(base, appended string) string {
	combined := strings.TrimSpace(base)
	if combined != "" && strings.TrimSpace(appended) != "" {
		combined += " "
	}
	return combined + strings.TrimSpace(appended)
}

// setHarnessPinnedVar handles the harness-pinned variable assignments:
// sqlite_current_time (fixed clock) and ::sqlite_interrupt_count (arm the
// vdbe.c per-opcode interrupt countdown). Returns true when handled.
func (tp *transpiler) setHarnessPinnedVar(goName string, rest []tcl.RawWord) bool {
	if len(rest) < 1 {
		return false
	}
	switch goName {
	case "sqlite_current_time":
		val := strings.TrimSpace(rest[0].Text)
		if _, err := strconv.ParseInt(val, 10, 64); err == nil {
			tp.emitLine("function.SetNowFunc(func() time.Time { return time.Unix(%s, 0) })", val)
			return true
		}
	case "sqlite_interrupt_count":
		valExpr := tp.buildStringExpr(rest[0].Text)
		tp.emitLine("sqlite_interrupt_count = %s", valExpr)
		tp.emitLine("db.SetInterruptCount(tclInt(sqlite_interrupt_count))")
		tp.emitLine("_ = sqlite_interrupt_count // suppress unused warning")
		return true
	case "sqlite_like_count":
		// sqlite_like_count is the engine's LIKE/GLOB invocation counter
		// (func.c sqlite3_like_count, TCL-linked in tester.tcl): writes
		// reset the counter, and reads (emitSetVarResultCheck) come from
		// the engine, so the LIKE optimization is observable exactly as in
		// SQLite (like.test 3.x).
		valExpr := tp.buildStringExpr(rest[0].Text)
		tp.emitLine("sqlite_like_count = %s", valExpr)
		tp.emitLine("db.ResetLikeCallCount()")
		tp.emitLine("_ = sqlite_like_count // suppress unused warning")
		return true
	}
	return false
}

// maybeArmInterruptCount arms the engine countdown when a TCL-namespace-form
// assignment targets ::sqlite_interrupt_count.
func (tp *transpiler) maybeArmInterruptCount(goName string) {
	if goName == "sqlite_interrupt_count" {
		tp.emitLine("db.SetInterruptCount(tclInt(sqlite_interrupt_count))")
	}
}

// skipDBConnectionSet reports whether a set assignment targets a DB connection
// variable (db, db1-db9) from sqlite3_open or another non-DB-returning command,
// which would cause a type conflict.
func (tp *transpiler) skipDBConnectionSet(goName string, args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	openText := ""
	for _, word := range args[1:] {
		if strings.Contains(word.Text, "sqlite3_open") {
			openText = joinSetArgTexts(args)
			break
		}
	}
	if openText == "" {
		return false
	}
	if !isPreDeclaredDB(goName) && goName != "db" {
		return tp.openDBConnectionVar(goName, openText)
	}
	// A failed-path sqlite3_open (e.g. /bogus/path/test.db) leaves the
	// connection in the "unable to open" state: sqlite3_errmsg reports the
	// message, sqlite3_errcode reports SQLITE_CANTOPEN, and sqlite3_close
	// succeeds (capi3-3.3/3.4/3.5). Record it so the errmsg/errcode/close
	// handlers emit the C-API values. A reopen also clears any prior
	// closed/failed state for this connection.
	tp.trackConnFailedOpen(goName, sqlite3OpenArg(args[1].Text))
	tp.emitLine("// set %s [sqlite3_open ...] (skipped, DB connection)", goName)
	return true
}

// joinSetArgTexts joins the set command's value words (everything after the
// variable name) with single spaces.
func joinSetArgTexts(args []tcl.RawWord) string {
	out := make([]string, 0, len(args)-1)
	for _, w := range args[1:] {
		out = append(out, w.Text)
	}
	return strings.Join(out, " ")
}

// openDBConnectionVar handles the legacy `set ::dbx [sqlite3_open FILE]`
// form: it assigns a real connection handle, not TCL text, so open it
// directly and let later sqlite3_close calls receive *frigolite.DB
// (tableapi.test uses this form). Returns true when opened.
func (tp *transpiler) openDBConnectionVar(goName, openText string) bool {
	openArg := sqlite3OpenArg(openText)
	if openArg == "" {
		return false
	}
	filename := tp.goStringLiteral(tcl.RawWord{Text: openArg})
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s, err = frigolite.Open(%s)", goName, filename)
	} else {
		tp.emitLine("%s, err := frigolite.Open(%s)", goName, filename)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("defer %s.Close()", goName)
	if tp.dbConnVars == nil {
		tp.dbConnVars = make(map[string]bool)
	}
	tp.dbConnVars[goName] = true
	return true
}

// trackConnFailedOpen records a failed-path sqlite3_open so the
// errmsg/errcode/close handlers emit the C-API values; a successful (or
// empty-path) open clears any prior failed-open state (and a reopen clears
// any prior closed state).
func (tp *transpiler) trackConnFailedOpen(goName, openArg string) {
	if openArg != "" && strings.Contains(openArg, "/") && !isMainTestFile(openArg) && !strings.HasPrefix(openArg, "test.db") {
		if tp.connFailedOpen == nil {
			tp.connFailedOpen = make(map[string]string)
		}
		tp.connFailedOpen[goName] = "unable to open database file"
	} else {
		// A successful (or empty-path) open clears any prior failed-open state.
		delete(tp.connFailedOpen, goName)
	}
	delete(tp.connClosed, goName)
}

// sqlite3OpenArg extracts the filename argument of a `sqlite3_open PATH`
// command embedded in a set bracket expression (e.g. `[sqlite3_open
// /bogus/path/test.db {}]`), returning "" when no path is present.
func sqlite3OpenArg(text string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		f = strings.TrimPrefix(f, "[")
		if f == "sqlite3_open" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// redirectErrVar redirects assignments to 'err' (a Go error type in the
// preamble) to a separate TCL string variable, declaring it if needed.
func (tp *transpiler) redirectErrVar(goName string) string {
	if goName != "err" {
		return goName
	}
	goName = tclVarToGo("err") // "_err": same name as the pre-declared var and the string-context references
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	return goName
}
