// Package main implements the tcl2go tool.
//
// This file holds the shared command-shape predicates, registry variables,
// and channel/array-map helpers used by the `set`-command handlers.
package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// isBracketWord reports whether w is an unbraced word starting with "[".
func isBracketWord(w tcl.RawWord) bool {
	return !w.Braced && strings.HasPrefix(w.Text, "[")
}

// isLsearchCmd reports whether cmdParts is `lsearch ...` with >= 3 words.
func isLsearchCmd(cmdParts []string) bool {
	return len(cmdParts) >= 3 && cmdParts[0] == "lsearch"
}

// isMakeExprCmd reports whether cmdParts starts with make_expr1/2/3.
func isMakeExprCmd(cmdParts []string) bool {
	if len(cmdParts) < 1 {
		return false
	}
	return cmdParts[0] == "make_expr1" || cmdParts[0] == "make_expr2" || cmdParts[0] == "make_expr3"
}

// isRegexpCmd reports whether cmdParts is `regexp ...` with >= 3 words.
func isRegexpCmd(cmdParts []string) bool {
	return len(cmdParts) >= 3 && cmdParts[0] == "regexp"
}

// isDBEvalCmd reports whether cmdParts is `db eval ...`.
func isDBEvalCmd(cmdParts []string) bool {
	if len(cmdParts) < 2 || cmdParts[1] != "eval" {
		return false
	}
	conn := cmdParts[0]
	return conn == "db" || isPreDeclaredDB(conn) || strings.HasPrefix(conn, "db")
}

// isDBOneCmd reports whether cmdParts is `db one ...` or `db onecolumn ...`.
func isDBOneCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "db" && len(cmdParts) >= 2 && (cmdParts[1] == "one" || cmdParts[1] == "onecolumn")
}

// isDBIncrblobCmd reports whether cmdParts is `<conn> incrblob ...` where
// <conn> is a database connection (db, db2, ...).
func isDBIncrblobCmd(cmdParts []string) bool {
	if len(cmdParts) < 2 || cmdParts[1] != "incrblob" {
		return false
	}
	conn := cmdParts[0]
	return conn == "db" || isPreDeclaredDB(conn) || strings.HasPrefix(conn, "db")
}

// isSqlite3OpenCmd reports whether cmdParts is `sqlite3 ...` with >= 3 words.
func isSqlite3OpenCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "sqlite3" && len(cmdParts) >= 3
}

// isCatchCmd reports whether cmdParts is `catch ...` with >= 2 words.
func isCatchCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "catch" && len(cmdParts) >= 2
}

// isListCmd reports whether cmdParts starts with "list".
func isListCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "list"
}

// isExprCmd reports whether cmdParts starts with "expr".
func isExprCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "expr"
}

// inlineQueryFuncValue inlines a query-proc result (`set var [queryProc]`)
// when the command is a registered query proc. Returns true when inlined.
// Only a BARE proc call (no arguments) inlines: a call with arguments
// (memdb.test's `set sig2 [signature two]`) invokes a value-taking proc
// whose TCL-list result is not a db-eval query, so it must fall through to
// the generic set handling (literal text), not fabricated query rows.
func (tp *transpiler) inlineQueryFuncValue(goName string, cmdParts []string) bool {
	if len(cmdParts) != 1 || len(tp.queryFuncs) == 0 {
		return false
	}
	sql, ok := tp.queryFuncs[cmdParts[0]]
	if !ok {
		return false
	}
	// set var [queryProc] — the proc returns a db-eval result
	// (e.g. `proc signature {} { return [db eval {SELECT ...}] }`);
	// inline the query and assign the flattened result.
	tp.emitQueryVarAssign(goName, sql)
	return true
}

// globalUserProcs records test-local procs with faithful Go runtime
// implementations. It is package-level because do_test/db-eval/for/foreach
// bodies transpile through cloned sub-transpilers that would otherwise drop
// per-instance registration state; gen.go clears it before each file.
var globalUserProcs = map[string]bool{}

// globalProcBodies mirrors tp.procBodies across sub-transpiler scopes so the
// dispatch layer can fingerprint a file-local definition at its CALL site
// (rtree8/rtreeA fixture procs). gen.go clears it before each file.
var globalProcBodies = map[string]string{}

// markUserProcGlobal registers a proc name as registry-backed for this file.
func markUserProcGlobal(name string) { globalUserProcs[name] = true }

func init() {
	// keep package-level helpers together; no-op initializer
}

// processSetBracketValue dispatches `set var [cmd ...]` to the special-case
// emitters. Returns true when the value was fully handled.

// activeFileChannels tracks TCL file channels opened in write mode
// (`set fd [open FILE wb]`): var name -> path. `puts $fd text` appends to
// the file; `close $fd` unregisters (csv01 5.x setup parity).
var activeFileChannels = map[string]string{}

// activeFileChannelExprs marks channels whose stored destination is a Go
// EXPRESSION (variable TCL path) rather than a quoted literal.
var activeFileChannelExprs = map[string]bool{}

// fileChannelSeek tracks the current byte position of each write-mode file
// channel so that `seek $fd N start` followed by `puts -nonewline $fd DATA`
// writes to the right offset (TCL fconfigure -translation binary + seek +
// puts is the canonical pattern for hex-corrupting a database file at a
// known offset, used by every corrupt*.test suite). The seek offset is
// applied via tclChannelAppendAt on the next puts. corrupt2.test 1.4/1.5
// relies on this to write "\xFF\xFF" at byte 101 — without it the bytes
// land at end-of-file and the corruption detection never fires.
var fileChannelSeek = map[string]int64{}

// channelDestExpr renders a channel's destination: quoted literal, or the
// stored Go expression verbatim for variable TCL paths.
func channelDestExpr(chName, path string) string {
	if activeFileChannelExprs[chName] && isValidGoIdent(path) {
		return path
	}
	return strconv.Quote(path)
}

// parseOpenChannelWord recognizes a bracketed `[open PATH MODE]`
// command-substitution word used as a set RHS. Returns the path and mode.
func parseOpenChannelWord(word string) (path, mode string, ok bool) {
	w := strings.TrimSpace(word)
	if !strings.HasPrefix(w, "[") || !strings.HasSuffix(w, "]") {
		return "", "", false
	}
	inner := strings.TrimSpace(w[1 : len(w)-1])
	fields := strings.Fields(inner)
	if len(fields) < 2 || fields[0] != "open" {
		return "", "", false
	}
	ppath := strings.Trim(fields[1], "\"'")
	pmode := ""
	if len(fields) >= 3 {
		pmode = fields[2]
	}
	return ppath, pmode, true
}

// splitArrayElement splits a TCL variable reference "arr(key)" into
// (arr, key, true); plain names return false.
func splitArrayElement(ref string) (base, key string, ok bool) {
	idx := strings.Index(ref, "(")
	if idx <= 0 || !strings.HasSuffix(ref, ")") {
		return "", "", false
	}
	base = strings.TrimSpace(ref[:idx])
	key = strings.TrimSpace(ref[idx+1 : len(ref)-1])
	return base, key, true
}

// activeTclvarBases tracks array bases whose elements are registered in the
// tclvar registry (package-level so nested body transpilers see it).
var activeTclvarBases = map[string]bool{}

// globalArrayMapVars is the per-file registration of dynamic-key arrays
// (collectArrayMapVars plus emit-time `array set` discoveries). Many cloned
// body transpilers do not carry the arrayMapVars map, so the array-lookup
// guards consult this fallback (see isArrayMapBacked).
var globalArrayMapVars = map[string]bool{}

// isArrayMapBacked reports whether base is a registered dynamic-key array
// whose Go map (XxxMap) the preamble declares. Falls back to the per-file
// global registration when this transpiler (a body clone) carries no map.
func isArrayMapBacked(tp *transpiler, base string) bool {
	base = strings.TrimPrefix(base, "::")
	if tp != nil && tp.arrayMapVars != nil &&
		(tp.arrayMapVars[base] || tp.arrayMapVars["::"+base]) {
		return true
	}
	return globalArrayMapVars[base] || globalArrayMapVars["::"+base]
}

// tclProcVarAliases maps proc names to the TCL global their body returns
// (`proc p {} { return $::g }` → p→g). Registration sites for g also
// register p so the `tcl` vtab module can resolve its argument.
var tclProcVarAliases = map[string]string{}

// markTclProcAlias records that proc name returns global target.
func markTclProcAlias(name, target string) {
	tclProcVarAliases[name] = target
}

// procReturnGlobalAlias extracts the global from a body whose only effect is
// `return $::name`; returns "" otherwise.
func procReturnGlobalAlias(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "return ") {
			continue
		}
		ref := strings.TrimSpace(strings.TrimPrefix(ln, "return "))
		if strings.HasPrefix(ref, "$::") {
			return strings.TrimPrefix(ref, "$::")
		}
	}
	return ""
}

// emitTclProcAliasRegistrations emits extra registry writes so proc aliases
// of var nm carry the same value at runtime.
func (tp *transpiler) emitTclProcAliasRegistrations(nm, valExpr string) {
	for procName, target := range tclProcVarAliases {
		if target == nm && isValidGoIdent(tclVarToGo(procName)) {
			tp.emitLine("vtab.TclVarSet(%q, %q, %s)", procName, "", valExpr)
		}
	}
}

func markTclvarBase(base string) {
	if base != "" {
		activeTclvarBases[base] = true
	}
}
