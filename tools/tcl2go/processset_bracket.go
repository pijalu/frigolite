// Package main implements the tcl2go tool.
//
// This file dispatches `set VAR [cmd ...]` bracket values to the ordered
// special-case emitters (first half of the chain; the tail lives in
// processset_bracket2.go).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// setBracketValueChain is the ORDERED dispatch chain behind
// processSetBracketValue. Order is load-bearing: it replicates the original
// if-else ladder exactly — the first handler whose guard matches wins, and a
// handler returning false means "not mine, keep falling through" (several
// guards can decline after matching their command word). It is a builder
// function (like tclHandlers) because a package-level slice of method
// expressions would form a var-initialization cycle through processSet.
func setBracketValueChain() []func(tp *transpiler, goName, cmdText string, cmdParts []string) bool {
	return []func(tp *transpiler, goName, cmdText string, cmdParts []string) bool{
		(*transpiler).setBracketCksumValue,
		(*transpiler).setBracketLimitPriorValue,
		(*transpiler).setBracketListFormValue,
		(*transpiler).setBracketProcSignatureValue,
		(*transpiler).setBracketUserProcValue,
		(*transpiler).setBracketPendingByteValue,
		(*transpiler).setBracketIntarrayCatchValue,
		(*transpiler).setBracketLreverseValue,
		(*transpiler).setBracketPrepareValue,
		(*transpiler).setBracketTempFileNameValue,
		(*transpiler).setBracketQuotaValue,
		(*transpiler).setBracketLsearchValue,
		(*transpiler).setBracketStringFuncValue,
		(*transpiler).setBracketFindAllValue,
		(*transpiler).setBracketCreateDBValue,
		(*transpiler).setBracketMakeExprValue,
		(*transpiler).setBracketRegexpValue,
		(*transpiler).setBracketInlineQueryValue,
		(*transpiler).setBracketDBEvalValue,
		(*transpiler).setBracketDBOneValue,
		(*transpiler).setBracketCatchsqlValue,
		(*transpiler).setBracketDBChangesValue,
		(*transpiler).setBracketIncrblobValue,
		(*transpiler).setBracketListValue,
		(*transpiler).setBracketBinaryFormatValue,
		(*transpiler).setBracketIntarrayCreateValue,
		(*transpiler).setBracketSqlite3OpenValue,
		(*transpiler).setBracketLimitFallbackValue,
		(*transpiler).setBracketExprValue,
		(*transpiler).setBracketOpenValue,
		(*transpiler).setBracketReadValue,
		(*transpiler).setBracketBlobBytesValue,
		(*transpiler).setBracketDataVersionValue,
		(*transpiler).setBracketDBOneRawValue,
		(*transpiler).setBracketBinaryCodecValue,
		(*transpiler).setBracketSpecialFuncValue,
		(*transpiler).setBracketColmetaValue,
		(*transpiler).setBracketExecValue,
		(*transpiler).setBracketLimitQueryValue,
		(*transpiler).setBracketLimitSetValue,
		(*transpiler).setBracketMiscValue,
	}
}

// processSetBracketValue dispatches `set var [cmd ...]` to the special-case
// emitters. Returns true when the value was fully handled.
func (tp *transpiler) processSetBracketValue(goName, cmdText string) bool {
	cmdParts := strings.Fields(cmdText)
	if len(cmdParts) == 0 {
		return false
	}
	for _, h := range setBracketValueChain() {
		if h(tp, goName, cmdText, cmdParts) {
			return true
		}
	}
	return false
}

// emitLimitPriorAssign emits the sqlite3_limit set-and-restore block: read the
// prior limit, set the new one, and assign the prior value to goName.
func (tp *transpiler) emitLimitPriorAssign(goName, lim, val string) {
	tp.emitLine("{ // set %s [sqlite3_limit %s %s]", goName, lim, val)
	tp.indent++
	tp.emitLine("_prior := db.Limit(%q)", lim)
	tp.emitLine("db.SetLimit(%q, toInt(%q))", lim, val)
	tp.assignSetValue(goName, "strconv.Itoa(_prior)")
	tp.indent--
	tp.emitLine("}")
}

// setBracketCksumValue handles `set VAR [cksum]` / `[cksum db2]` — the
// tester.tcl fingerprint helper.
func (tp *transpiler) setBracketCksumValue(goName, cmdText string, cmdParts []string) bool {
	if len(cmdParts) < 1 || cmdParts[0] != "cksum" {
		return false
	}
	connVar := tp.dbVar
	if len(cmdParts) >= 2 {
		if v := strings.TrimSpace(cmdParts[1]); isValidGoIdent(tclVarToGo(v)) {
			connVar = tclVarToGo(v)
		}
	}
	tp.assignSetValue(goName, fmt.Sprintf("tclCksum(%s)", connVar))
	return true
}

// setBracketLimitPriorValue handles `set VAR [sqlite3_limit ...]` —
// sqlite3_limit forms (sqllimits1-1.30: `set prior [sqlite3_limit db
// SQLITE_LIMIT_LENGTH 1]` binds VAR to the PRIOR value; `... -1` queries
// without changing).
func (tp *transpiler) setBracketLimitPriorValue(goName, cmdText string, cmdParts []string) bool {
	if len(cmdParts) < 3 || cmdParts[0] != "sqlite3_limit" {
		return false
	}
	// Forms: [sqlite3_limit db LIMIT VAL] (4 parts) or
	// [sqlite3_limit LIMIT VAL] (3 parts, db omitted).
	lim, val := "", ""
	if len(cmdParts) == 4 {
		lim, val = cmdParts[2], cmdParts[3]
	} else if len(cmdParts) == 3 {
		lim, val = cmdParts[1], cmdParts[2]
	}
	if lim == "" {
		return false
	}
	if strings.TrimSpace(val) == "-1" {
		tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(db.Limit(%q))", lim))
	} else {
		tp.emitLimitPriorAssign(goName, lim, val)
	}
	return true
}

// setBracketListFormValue handles `set VAR [list [catch {...} msg] $msg]`.
func (tp *transpiler) setBracketListFormValue(goName, cmdText string, cmdParts []string) bool {
	// memdb1.test 1020: `set res [list [catch {...} msg] $msg]` — the list
	// form routes through processList/emitListCatchArg so the catch body
	// runs and the result feeds res (the backup-interlock error must reach
	// res). cmdText here is the bracket INNER text (outer [ ] stripped by
	// the caller), so parse it as a bare command, not a bracket word.
	if !strings.HasPrefix(strings.TrimSpace(cmdText), "list ") && strings.TrimSpace(cmdText) != "list" {
		return false
	}
	raws := tcl.ParseCommands(cmdText)
	if len(raws) == 1 && len(raws[0]) >= 2 && raws[0][0].Text == "list" {
		tp.processList(raws[0][1:])
		tp.assignSetValue(goName, "_r")
		return true
	}
	return false
}

// setBracketProcSignatureValue handles `set VAR [signature ...]` /
// `set VAR [pager_cache_size ...]` fixture-proc values.
func (tp *transpiler) setBracketProcSignatureValue(goName, cmdText string, cmdParts []string) bool {
	// memdb.test signature (see userProcEmitterFor): [signature one] /
	// [signature two] return the t3 rollback fingerprint.
	body, ok := globalProcBodies[cmdParts[0]]
	if !ok {
		return false
	}
	if userProcEmitterFor(cmdParts[0], body) == "memdb_signature" {
		tp.assignSetValue(goName, fmt.Sprintf("tclMemdbSignature(%s)", tp.dbVar))
		return true
	}
	// cache.test pager_cache_size (btree_pager_stats "page" count).
	if userProcEmitterFor(cmdParts[0], body) == "cache_pager_size" {
		tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(tclPagerCacheSize(%s))", tp.dbVar))
		return true
	}
	return false
}

// setBracketUserProcValue handles `set VAR [userproc args...]` with a
// registry-backed implementation (rtree4's rand/randincr/scramble): emits a
// direct runtime call instead of falling through to the raw-text fallback.
// `$var` arguments pass the Go variable's value; bare tokens go through as
// literals.
func (tp *transpiler) setBracketUserProcValue(goName, cmdText string, cmdParts []string) bool {
	if !globalUserProcs[cmdParts[0]] {
		return false
	}
	callArgs := make([]string, 0, len(cmdParts)-1)
	for _, a := range cmdParts[1:] {
		if strings.HasPrefix(a, "$") && !strings.Contains(a, "(") {
			if gv := tclVarToGo(strings.TrimPrefix(a, "$")); gv != "" {
				callArgs = append(callArgs, gv)
				continue
			}
		}
		callArgs = append(callArgs, strconv.Quote(a))
	}
	tp.assignSetValue(goName,
		fmt.Sprintf("callTclUserProc(%q, %s)", cmdParts[0], strings.Join(callArgs, ", ")))
	return true
}

// setBracketPendingByteValue handles `set VAR
// [sqlite3_test_control_pending_byte N]` — the C command sets the pending
// byte and returns the PREVIOUS offset (src/test2.c testPendingByte);
// pager1.test 42.x captures it in pending_prev to restore later. Performs the
// engine override, mirrors the shadow var, and assigns the previous value.
func (tp *transpiler) setBracketPendingByteValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_test_control_pending_byte" || len(cmdParts) < 2 {
		return false
	}
	arg := strings.TrimSpace(strings.Join(cmdParts[1:], " "))
	if strings.HasPrefix(arg, "$") {
		goVar := tclVarToGo(strings.TrimPrefix(arg, "$"))
		if isValidGoIdent(goVar) && tp.isVarDeclared(goVar) {
			tp.emitLine("_r = strconv.FormatUint(uint64(%s.SetPendingByte(uint32(tclAtoi(%s)))), 10)", tp.dbVar, goVar)
			tp.emitLine("%s = _r", goName)
			tp.emitLine("sqlite_pending_byte = %s", goVar)
			return true
		}
		return false
	} else if n, err := strconv.ParseInt(arg, 0, 64); err == nil {
		tp.emitLine("_r = strconv.FormatUint(uint64(%s.SetPendingByte(%d)), 10)", tp.dbVar, n)
		tp.emitLine("%s = _r", goName)
		tp.emitLine("sqlite_pending_byte = %q", strconv.FormatInt(n, 10))
		return true
	}
	return false
}

// setBracketIntarrayCatchValue handles `set VAR [catch {sqlite3_intarray_create
// DB NAME} RESULTVAR]` — the intarray create runs inside a catch; RESULTVAR
// receives the create's RETURN VALUE (the handle), while the bracket result
// (assigned to VAR) is the catch code. test_intarray.c 1.1b.
func (tp *transpiler) setBracketIntarrayCatchValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "catch" {
		return false
	}
	for i := 0; i < len(cmdParts); i++ {
		if strings.HasPrefix(cmdParts[i], "{sqlite3_intarray_create") && i+2 < len(cmdParts) {
			name := strings.TrimSuffix(cmdParts[i+2], "}")
			name = strings.Trim(name, "'\"")
			resultVar := tclVarToGo(cmdParts[len(cmdParts)-1])
			tp.emitLine("_r = vtab.IntarrayRegisterHandle(%q)", name)
			tp.emitLine("_res = %s.Exec(\"CREATE VIRTUAL TABLE temp.%s USING intarray('%s')\")", tp.dbVar, name, name)
			tp.emitLine("if _res.Error != nil { t.Errorf(\"intarray create: %%v\", _res.Error) }")
			if resultVar != "" && resultVar != goName {
				tp.emitLine("%s = _r", resultVar)
			}
			tp.emitLine("_r = \"0\"")
			// The bracket result (the catch code) IS the set target's
			// value: `set rc [catch {...} ia1]` assigns rc = "0". The
			// missing assignment left rc holding its PREVIOUS value, so
			// intarray-1.1b's lappend built a stale list ("0X5" instead
			// of "0 X5") and the /0 [0-9A-Z]+/ comparison failed.
			tp.assignSetValue(goName, "_r")
			return true
		}
	}
	return false
}

// setBracketLreverseValue handles `[lreverse $VAR]` — reverse a TCL list
// variable at runtime (fts3first.test's order=DESC comparisons).
func (tp *transpiler) setBracketLreverseValue(goName, cmdText string, cmdParts []string) bool {
	if len(cmdParts) != 2 || cmdParts[0] != "lreverse" || !strings.HasPrefix(cmdParts[1], "$") {
		return false
	}
	if gv := tclVarToGo(strings.TrimPrefix(cmdParts[1], "$")); gv != "" {
		tp.emitLine("%s = tclLreverse(%s)", goName, gv)
		tp.emitLine("\t_ = %s // suppress unused warning", goName)
		return true
	}
	return false
}

// setBracketPrepareValue handles `set VAR [sqlite3_prepare ...]` and the
// sqlite3_prepare_v2 form (statement recording for bind/step emulation).
func (tp *transpiler) setBracketPrepareValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_prepare" && cmdParts[0] != "sqlite3_prepare_v2" {
		return false
	}
	tp.recordPreparedStatement(goName, "["+cmdText+"]")
	return true
}

// setBracketTempFileNameValue handles `set VAR [file_control_tempfilename DB]`
// — test1.c file_control_tempfilename: SQLITE_FCNTL_TEMPFILENAME returns a VFS
// temp filename (unix: <tempdir>/etilqs_<16 random alphanumerics>, os_unix.c
// unixTempFileNameExclusive; filectrl-1.6 asserts the etilqs_ prefix).
func (tp *transpiler) setBracketTempFileNameValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "file_control_tempfilename" {
		return false
	}
	tp.assignSetValue(goName, fmt.Sprintf("tclFileControlTempFileName(%s)", tp.dbVar))
	return true
}

// setBracketQuotaValue handles `set VAR [sqlite3_quota_* ARGS]` — quota
// commands are value-producing (fopen handles, fread content, ftell positions,
// dump lists): run the same statement handler (which leaves its result in _r)
// and assign it.
func (tp *transpiler) setBracketQuotaValue(goName, cmdText string, cmdParts []string) bool {
	if !strings.HasPrefix(cmdParts[0], "sqlite3_quota_") {
		return false
	}
	if os.Getenv("QDBG11") != "" {
		fmt.Fprintf(os.Stderr, "QDBG11 set-bracket route: %s\n", cmdText)
	}
	if h, ok := tclHandlers()[cmdParts[0]]; ok {
		raws := tcl.ParseCommands(cmdText)
		if len(raws) > 0 {
			h(tp, raws[0][1:])
			tp.assignSetValue(goName, "_r")
			return true
		}
	}
	return false
}

// setBracketLsearchValue routes `set VAR [lsearch ...]` to setLsearchValue.
func (tp *transpiler) setBracketLsearchValue(goName, cmdText string, cmdParts []string) bool {
	if !isLsearchCmd(cmdParts) {
		return false
	}
	return tp.setLsearchValue(goName, cmdText, cmdParts)
}

// setBracketStringFuncValue routes `set VAR [string first NEEDLE HAY ...]` to
// setStringFuncValue: the runtime string-op result lands in VAR
// (zipfile.test 24.x patches a central-directory offset computed from $zip at
// runtime).
func (tp *transpiler) setBracketStringFuncValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "string" || len(cmdParts) <= 1 {
		return false
	}
	return setStringFuncValue(tp, goName, cmdParts)
}

// setBracketFindAllValue handles `set L [findall NEEDLE HAYSTACK]`: the
// zipfile2.test proc returning every occurrence index (runtime call —
// $archive patches below).
func (tp *transpiler) setBracketFindAllValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "findall" || len(cmdParts) != 3 {
		return false
	}
	n, ok := tp.stringWordExpr(cmdParts[1])
	if !ok {
		return false
	}
	h, ok2 := tp.stringWordExpr(cmdParts[2])
	if !ok2 {
		return false
	}
	tp.assignSetValue(goName, fmt.Sprintf("tclFindAll(%s, %s)", n, h))
	return true
}

// setBracketCreateDBValue routes `set nPage [create_db "..."]` to
// setCreateDBValue.
func (tp *transpiler) setBracketCreateDBValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "create_db" {
		return false
	}
	return tp.setCreateDBValue(goName)
}

// setBracketMakeExprValue routes `set VAR [make_exprN ...]` to
// setMakeExprValue.
func (tp *transpiler) setBracketMakeExprValue(goName, cmdText string, cmdParts []string) bool {
	if !isMakeExprCmd(cmdParts) {
		return false
	}
	return tp.setMakeExprValue(goName, cmdText, cmdParts[0])
}

// setBracketRegexpValue routes `set VAR [regexp ...]` to setRegexpValue.
func (tp *transpiler) setBracketRegexpValue(goName, cmdText string, cmdParts []string) bool {
	if !isRegexpCmd(cmdParts) {
		return false
	}
	return tp.setRegexpValue(goName, cmdParts)
}

// setBracketInlineQueryValue inlines `set VAR [queryProc]` when the command is
// a registered query proc (see inlineQueryFuncValue).
func (tp *transpiler) setBracketInlineQueryValue(goName, cmdText string, cmdParts []string) bool {
	return tp.inlineQueryFuncValue(goName, cmdParts)
}

// setBracketDBEvalValue routes `set var [db eval "SQL"]` to setDBEvalValue.
func (tp *transpiler) setBracketDBEvalValue(goName, cmdText string, cmdParts []string) bool {
	if !isDBEvalCmd(cmdParts) {
		return false
	}
	return tp.setDBEvalValue(goName, cmdText, cmdParts)
}

// setBracketDBOneValue routes `set var [db one "SQL"]` /
// `[db onecolumn "SQL"]` to setDBOneValue.
func (tp *transpiler) setBracketDBOneValue(goName, cmdText string, cmdParts []string) bool {
	if !isDBOneCmd(cmdParts) {
		return false
	}
	return tp.setDBOneValue(goName, cmdText, cmdParts)
}

// setBracketCatchsqlValue routes `set VAR [catchsql {SQL}]` to
// setCatchsqlSetValue.
func (tp *transpiler) setBracketCatchsqlValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "catchsql" {
		return false
	}
	return tp.setCatchsqlSetValue(goName, cmdText, cmdParts)
}
