// SPDX-License-Identifier: GPL-3.0-or-later
// Package main implements the tcl2go tool: a TCL-to-Go transpiler that converts
// SQLite TCL test files (.test) into standalone Go test files (_test.go).
//
// This file contains the per-file source scans that run before transpilation:
// prepare-tail variables, named connections, backup/blob objects, the
// pre-declared variable list, and the leading-delete hoisting.
package main

import "strings"

// collectPrepareTailVars returns the tail-variable names of sqlite3_prepare
// commands (`sqlite3_prepare db SQL -1 TAIL` — the last argument names the
// variable that receives the SQL text after the first statement). These are
// pre-declared at function scope so the tail assignment emitted by
// recordPreparedStatement targets a function-scope variable (capi2-2.x reads
// `set SQL` after a multi-statement prepare in a later do_test body).
func collectPrepareTailVars(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "sqlite3_prepare") {
			continue
		}
		// The prepare command may be a bare top-level command or embedded in
		// `set X [sqlite3_prepare ...]`. Extract the command text (strip a
		// leading `set VAR [` and a trailing `]`), then tokenize it with the
		// TCL word splitter so a braced multi-word SQL body stays one token.
		cmdText := trimmed
		if i := strings.Index(cmdText, "["); i >= 0 {
			cmdText = cmdText[i+1:]
			cmdText = strings.TrimSuffix(cmdText, "]")
		}
		words := tclCmdWords(cmdText)
		if len(words) < 5 {
			continue
		}
		if !strings.HasPrefix(words[0], "sqlite3_prepare") {
			continue
		}
		if tv, ok := prepareTailVarWord(words); ok {
			gv := tclVarToGo(tv)
			if !seen[gv] {
				seen[gv] = true
				names = append(names, gv)
			}
		}
	}
	return names
}

// prepareTailVarWord extracts the tail-variable word of a tokenized
// sqlite3_prepare command: the LAST argument, rejecting NULL markers
// ("notused"/"dummy") and flags.
func prepareTailVarWord(words []string) (tv string, ok bool) {
	// Words: CMD DB SQL NBYTES TAILVAR (the tail var is the LAST argument;
	// some calls pass a NULL marker "notused"/"dummy" instead).
	tv = words[len(words)-1]
	tv = strings.TrimPrefix(tv, "$")
	tv = strings.Trim(tv, `"`)
	if tv == "" || strings.HasPrefix(tv, "-") || tv == "notused" || tv == "dummy" || !isValidGoIdent(tclVarToGo(tv)) {
		return "", false
	}
	return tv, true
}

// collectConnectionNames returns the named database connection variables
// created by `sqlite3 NAME [file]` commands (excluding db and db1-db9).
// Dynamic targets (`sqlite3 $con test.db`, where con HOLDS the connection
// name at runtime) are skipped: those variables are plain TCL strings, not
// connection handles.
func collectConnectionNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "sqlite3 ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[1], "$") {
			continue // dynamic connection name — the variable holds a string
		}
		name := tclVarToGo(fields[1])
		if !isValidGoIdent(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// collectBackupNames returns the backup-object variable names created by
// `sqlite3_backup NAME ...` commands in the TCL source (B, B2, B3, ...).
func collectBackupNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "sqlite3_backup ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		name := tclVarToGo(fields[1])
		if !isValidGoIdent(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// collectBlobNames returns the blob-object variable names created by
// `sqlite3_blob_open ... NAME` commands and `set VAR [db incrblob ...]`
// assignments in the TCL source (B, B2, blob, h, b, ...).
//
//lint:ignore U1000 retained for generator compatibility
func collectBlobNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		// Strip trailing TCL comments (";# ...") so the last field is the
		// blob variable, not a comment word.
		if idx := strings.Index(trimmed, ";#"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		if strings.HasPrefix(trimmed, "sqlite3_blob_open ") {
			collectBlobOpenName(trimmed, seen, &names)
		}
		// `set blob [db incrblob ...]` / `set ::blob [db incrblob ...]` —
		// the target var holds a *frigolite.Blob.
		if strings.Contains(trimmed, "[db incrblob ") {
			collectIncrblobSetName(trimmed, seen, &names)
		}
	}
	return names
}

// collectBlobOpenName records the final argument of a sqlite3_blob_open line
// (the blob variable name) when it is a fresh valid Go identifier.
func collectBlobOpenName(trimmed string, seen map[string]bool, names *[]string) {
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return
	}
	name := tclVarToGo(fields[len(fields)-1])
	if !isValidGoIdent(name) || seen[name] {
		return
	}
	seen[name] = true
	*names = append(*names, name)
}

// collectIncrblobSetName records the target variable of a
// `set VAR [db incrblob ...]` line (the var holds a *frigolite.Blob) when it
// is a fresh valid Go identifier.
func collectIncrblobSetName(trimmed string, seen map[string]bool, names *[]string) {
	fields := strings.Fields(trimmed)
	if len(fields) < 2 || fields[0] != "set" {
		return
	}
	name := tclVarToGo(fields[1])
	if !isValidGoIdent(name) || seen[name] {
		return
	}
	seen[name] = true
	*names = append(*names, name)
}

// collectPredeclaredVars merges the set and referenced variable names into the
// list to pre-declare at function scope, filtering out globals, db vars, and
// sqlite connection targets.
func collectPredeclaredVars(src string, setVars, refVars []string, knownGlobals, sqliteTargets map[string]bool) []string {
	var preDeclared []string
	seen := make(map[string]bool)
	for _, v := range append(append([]string{}, setVars...), refVars...) {
		gv := tclVarToGo(v)
		// The TCL variable err maps to _err (tclVarToGo) everywhere — never
		// the generated db-open error var; pre-declare it so a `set err`
		// inside an if/else branch is still visible after the branch.
		gv = tclVarToGo(gv)
		legacyOpenTarget := strings.Contains(src, "set ::"+v+" [sqlite3_open") || strings.Contains(src, "set "+v+" [sqlite3_open")
		if legacyOpenTarget {
			sqliteTargets[gv] = true
		}
		if gv != "" && gv != "_" && !seen[gv] && !knownGlobals[gv] && gv != "db" && gv != "t" && isValidGoIdent(gv) {
			seen[gv] = true
			preDeclared = append(preDeclared, gv)
		}
	}
	return preDeclared
}

// preambleDeclaredNames returns every Go variable name that emitTestPreamble
// already declares (common vars, backup objects, named connections, and the
// prepare-tail variables not claimed by preDeclared) so the function-scope
// preDeclared loop does not redeclare them.
func preambleDeclaredNames(src string, preDeclared []string) map[string]bool {
	preSet := make(map[string]bool, len(preDeclared))
	for _, pv := range preDeclared {
		preSet[pv] = true
	}
	// Common vars declared unconditionally at the top of every test function.
	declared := map[string]bool{"msg": true, "_res": true, "r": true, "_r": true}
	// sqlite_options_default_autovacuum is declared (with initial value) by
	// emitTestPreamble when the TCL source references it. Without this entry,
	// the preDeclared loop would emit a second `var ... string` declaration
	// and the build would fail with "sqlite_options_default_autovacuum
	// redeclared in this block".
	if strings.Contains(src, "sqlite_options_default_autovacuum") || strings.Contains(src, "sqlite_options(default_autovacuum)") {
		declared["sqlite_options_default_autovacuum"] = true
	}
	// sqlite_pending_byte is shadow-declared (initialised) by
	// emitTestPreamble whenever the TCL source references
	// ::sqlite_pending_byte; the preDeclared loop must skip it to
	// avoid a redeclared-in-this-block build error.
	if strings.Contains(src, "sqlite_pending_byte") {
		declared["sqlite_pending_byte"] = true
	}
	for _, bn := range collectBackupNames(src) {
		declared[bn] = true
	}
	for _, cn := range collectConnectionNames(src) {
		if cn == "db" || isPreDeclaredDB(cn) {
			continue
		}
		declared[cn] = true
	}
	for _, tv := range collectPrepareTailVars(src) {
		if !preSet[tv] { // preDeclared wins — the tail loop skips those
			declared[tv] = true
		}
	}
	return declared
}

// genPreDeleted counts the hoisted pre-Open deletes per path (a file may
// delete the same path several times in its leading region — delete4.test's
// do_execsql_test-only body makes the whole file "head", so mid-file
// forcedeletes land in the list too). Consumption by a body occurrence must
// decrement the count, not flip a bool: with two hoisted entries only ONE
// body occurrence is the duplicate, and later real deletes must still emit.
// (single-threaded generation; reset per file in generateTestFile).
var genPreDeleted map[string]int
var genPreDeletedList []string

// sourceLeadingDeletes scans the source region before the first do_test /
// sqlite3 open for forcedelete / file delete / delete_file commands and
// returns their literal path arguments.
func sourceLeadingDeletes(src string) []string {
	head := src[:leadingDeletesHeadEnd(src)]
	var paths []string
	for _, line := range strings.Split(head, "\n") {
		t := strings.TrimSpace(line)
		// A loop header starts a re-executed region: forcedelete/file-delete
		// occurrences after it are LOOP-BODY deletes (fts3snippet's
		// `forcedelete test.db` inside the foreach, run once per encoding),
		// not one-shot leading deletes. Stop the scan so they are emitted at
		// their real position by processFileDelete — pre-consuming them here
		// swallowed every per-iteration delete and left stale databases
		// behind ("table ft already exists" on the loop's second pass).
		if strings.HasPrefix(t, "foreach ") || strings.HasPrefix(t, "for {") ||
			strings.HasPrefix(t, "while {") {
			break
		}
		paths = appendDeleteArgs(paths, strings.TrimPrefix(t, "catch {"))
	}
	return paths
}

// leadingDeletesHeadEnd returns the offset where the leading (pre-test-block)
// region ends: the first test-block command of any flavor (do_test,
// do_execsql_test, do_catchsql_test, do_eqp_test, ...). Matching only
// "\ndo_test " left do_execsql_test-driven files (delete4.test) with their
// WHOLE body classified as head: every mid-file forcedelete was hoisted into
// the preamble and the body occurrences consumed/silenced, so
// close/forcedelete/reopen resets reused stale databases ("table t1 already
// exists").
func leadingDeletesHeadEnd(src string) int {
	headEnd := len(src)
	for _, marker := range []string{"\ndo_test ", "\ndo_execsql_test ", "\ndo_catchsql_test ", "\ndo_eqp_test ", "\ndo_realnum_test ", "\ndo_nullid_test "} {
		if i := strings.Index(src, marker); i >= 0 && i < headEnd {
			headEnd = i
		}
	}
	return headEnd
}

// appendDeleteArgs appends the literal path arguments of one forcedelete /
// delete_file / file delete line (already stripped of a catch wrapper).
func appendDeleteArgs(paths []string, t string) []string {
	for _, kw := range []string{"forcedelete ", "delete_file ", "file delete "} {
		if strings.HasPrefix(t, kw) {
			t = strings.TrimPrefix(t, kw)
			for _, f := range strings.Fields(strings.TrimSuffix(t, "}")) {
				f = strings.Trim(f, "{}")
				if f == "" || f == "-force" || f == "--" {
					continue
				}
				paths = append(paths, f)
			}
			break
		}
	}
	return paths
}

// knownGlobalVars returns the set of variable names declared in the helpers
// file (package-level globals). These must NOT be pre-declared at function
// scope because they already have values.
func knownGlobalVars() map[string]bool {
	return map[string]bool{
		"tcl_platform_platform": true, "tcl_platform_byteOrder": true,
		"tcl_platform_os": true, "tcl_platform_pointerSize": true,
		"tcl_platform_wordSize": true, "_tcl_platform_platform": true,
		"_tcl_platform_byteOrder": true, "_tcl_platform_os": true,
		"_tcl_platform": true, "tcl_platform": true,
		"MEMDEBUG": true, "sqlite_options": true, "_sqlite_options": true,
		"SQLITE_MAX_LENGTH": true, "SQLITE_MAX_SQL_LENGTH": true,
		"SQLITE_MAX_COLUMN": true, "SQLITE_MAX_EXPR_DEPTH": true,
		"SQLITE_MAX_TRIGGER_DEPTH":   true,
		"SQLITE_MAX_COMPOUND_SELECT": true, "SQLITE_MAX_VDBE_OP": true,
		"SQLITE_MAX_FUNCTION_ARG": true, "SQLITE_MAX_ATTACHED": true,
		"SQLITE_MAX_LIKE_PATTERN_LENGTH": true, "SQLITE_MAX_VARIABLE_NUMBER": true,
		"SQLITE_MAX_WORKER_THREADS": true, "SQLITE_MAX_SCHEMA": true,
		"SQLITE_MAX_PAGE_SIZE": true, "_SQLITE_MAX_PAGE_SIZE": true,
		"AUTOVACUUM": true, "TEMP_STORE": true, "_TEMP_STORE": true,
		"SQLITE_DEFAULT_SYNCHRONOUS": true, "SQLITE_DEFAULT_WAL_SYNCHRONOUS": true,
		"_SQLITE_DEFAULT_CACHE_SIZE": true, "tcl_version": true, "_tcl_version": true,
		"SQL": true, "TAIL": true, "TAIL_": true, "_G": true, "G": true,
		"_error": true, "argv": true, "has_codec": true, "bitmask_size": true,
		"tcl_precision": true, "highPrecision": true,
		"upperBound": true, "prefix": true, "dirname": true,
		"msg": true, "_res": true, "r": true, "_r": true,
		// db1-db9 are pre-declared as *frigolite.DB in the function preamble
		"db1": true, "db2": true, "db3": true, "db4": true, "db5": true,
		"db6": true, "db7": true, "db8": true, "db9": true,
		// oplog is the testvfs-equivalent journal-sidecar event sink
		// (journal2 test suite). Declared package-level in the helpers
		// template so the process-wide journal-file-op hook can append
		// events to it directly from any goroutine.
		"oplog": true,
	}
}
