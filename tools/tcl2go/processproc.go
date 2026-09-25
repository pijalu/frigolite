// Package main implements the tcl2go tool.
//
// This file contains the `proc` handler: recognition of the suite's
// test-harness proc shapes (constant-returning, counter, predicate, join,
// collation, recorder, hook callback, special-function) and their
// registration for later `db func` / `db collate` handlers.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// userProcHelperFor returns the generated-helper identifier that faithfully
// implements a known test-local proc (fingerprinted by distinctive body
// substrings), or "" when the body has no registered implementation.
func userProcHelperFor(name, body string) string {
	switch name {
	case "rand":
		switch {
		case strings.Contains(body, "1024.0"):
			return "tclRtree4RandFloat" // float variant: int((rand()-0.5)*1024.0*$X)/512.0
		case strings.Contains(body, "(rand()-0.5)*2*"):
			return "tclRtree4RandInt" // rtree_int_only: int((rand()-0.5)*2*$X)
		}
	case "randincr":
		switch {
		case strings.Contains(body, "32.0"):
			return "tclRtree4RandIncrFloat" // int(rand()*$X*32.0)/32.0 until >0
		case strings.Contains(body, "int(rand()*$X)+1"):
			return "tclRtree4RandIncrInt" // int(rand()*$X)+1 (always >0 for X>0)
		}
	case "scramble":
		if strings.Contains(body, "lsort") {
			return "tclUserScramble"
		}
	}
	return ""
}

// emitFsUserProcs registers vtabH.test's filesystem fixture procs
// (src/test_fs.c corpus support): sort_files/list_root_files/list_files/
// contents evaluate against the real filesystem at runtime through the
// harness helpers. Returns true when the proc was one of them and its
// runtime registration was emitted.
func (tp *transpiler) emitFsUserProcs(name, body string) bool {
	switch name {
	case "sort_files":
		if !strings.Contains(body, "lsort") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { nc := \"\"; if len(a) > 1 { nc = a[1] }; return tclSortFiles(a[0], nc) })", name)
		return true
	case "list_root_files":
		if !strings.Contains(body, "-nocomplain") && !strings.Contains(body, "glob") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { return tclListRootFiles() })", name)
		return true
	case "list_files":
		if !strings.Contains(body, "-nocomplain") && !strings.Contains(body, "glob") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { if len(a) == 0 { return \"\" }; return tclListFiles(a[0]) })", name)
		return true
	case "contents":
		if !strings.Contains(body, "list_files") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { if len(a) == 0 { return \"\" }; return tclContents(a[0]) })", name)
		return true
	}
	return false
}

// stripProcBodyOuterBraces removes one balanced pair of outer braces from a
// proc body word. The TCL tokenizer already strips the outer braces of a
// braced word, so a blind TrimSuffix("}") would corrupt any body whose last
// command ends with a brace (having.test's `proc nondeter {args} { incr
// ::V; expr {$::V % 2} }` lost the expr's closing brace and the body-shape
// detection degraded the UDF to a nil stub). Only a leading "{" whose match
// is the final character is stripped.
func stripProcBodyOuterBraces(word string) string {
	body := strings.TrimSpace(word)
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return body
	}
	depth := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 && i != len(body)-1 {
				// The opening brace closes before the end of the word:
				// it is not a wrapper around the whole body.
				return body
			}
		}
	}
	if depth != 0 {
		return body
	}
	return strings.TrimSpace(body[1 : len(body)-1])
}

// procAliasAndRedefine records a proc's alias target and handles
// redefinition with a different body. Returns true when the redefinition
// was fully handled and the caller must stop processing the proc.
func (tp *transpiler) procAliasAndRedefine(name, body string) bool {
	// A proc of the shape used by the `tcl` vtab module
	// (vtabL.test: proc vtab_command {method args} { ... return $::var })
	// acts as an alias returning a TCL global. Record it so every
	// registration site for that global also registers the proc name —
	// the module resolves its schema argument through the same registry.
	if tgt := procReturnGlobalAlias(body); tgt != "" {
		markTclProcAlias(name, tgt)
	}
	// Track proc definitions so a redefinition with a DIFFERENT body emits a
	// fresh SQL-function registration (TCL's `proc` redefines the proc the
	// db-func binding points to; fts4intck1.test redefines slang from the
	// th→d/e→eh map to identity at 2.3).
	if prev, ok := tp.seenProcs[name]; ok && prev != body {
		if tp.emitProcBodyRegistration(name, body) {
			tp.seenProcs[name] = body
			return true
		}
	}
	if tp.seenProcs == nil {
		tp.seenProcs = make(map[string]string)
	}
	tp.seenProcs[name] = body
	return false
}

// procTrackBody records a proc definition for later registration sites: the
// inline-call registry (zero/single-default-parameter procs), the body and
// parameter mirrors used by hook handlers, the global body mirror, and the
// trace/profile callback candidate registration (trace.test's trace_proc/
// profile_proc).
func (tp *transpiler) procTrackBody(name string, args []tcl.RawWord, body string) {
	// Procs transpilable INLINE at call sites: either zero parameters ("{}")
	// or a single parameter with a default value ("{{module rtree}}" — TCL's
	// optional-argument form). At a zero-arg call the default binds and the
	// body sequences supported commands (setup_simple_db et al).
	paramsInner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimSpace(args[1].Text), "{"), "}"))
	if inlineableParams(paramsInner) {
		if tp.inlineProcs == nil {
			tp.inlineProcs = make(map[string]string)
			tp.inlineProcParams = make(map[string]string)
		}
		tp.inlineProcs[name] = body
		tp.inlineProcParams[name] = args[1].Text
	}
	// Keep every proc body available for later registration sites
	// (e.g. `db preupdate hook preup` transpiling the named proc).
	if tp.procBodies == nil {
		tp.procBodies = make(map[string]string)
	}
	// Track each proc's parameter list so hook handlers (e.g.
	// `db collation_needed cfact` with a dynamic `$nm` collation name) can
	// name the emitted Go closure parameter after the TCL parameter.
	if tp.procParams == nil {
		tp.procParams = make(map[string]string)
	}
	tp.procParams[name] = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "{"), "}"))
	globalProcBodies[name] = body
	// A proc whose body appends [string trim $cmd] to a global list is a
	// trace/profile callback candidate (trace.test's trace_proc/
	// profile_proc): publish the transpiled body so `db trace <name>` hooks
	// resolve the LATEST definition at call time (TCL redefinition
	// semantics — trace.test 2.1 redefines trace_proc after registering).
	if goVar := traceAppendTarget(body); goVar != "" {
		tp.emitLine("tclTraceImplSet(%q, func(sqlText string) {", name)
		tp.emitLine("%s = tclListAppend(%s, tclTrimSpace(sqlText))", goVar, goVar)
		tp.emitLine("})")
		tp.emitLine("tclProfileImplSet(%q, func(sqlText string, ns int64) {", name)
		tp.emitLine("_ = ns")
		tp.emitLine("%s = tclListAppend(%s, tclTrimSpace(sqlText))", goVar, goVar)
		tp.emitLine("})")
	}
	if prev, ok := tp.procBodies[name]; !ok || prev != body {
		tp.procBodies[name] = body
	}
}

// procHookBodyKind stores hook-callback proc bodies (preupdate/commit/
// rollback/update hooks) for their registration sites instead of the
// generic proc kinds. Returns true when the proc was one of them.
func (tp *transpiler) procHookBodyKind(name, body string) bool {
	// `proc preupdate_hook {args} { ... }` (hook2.test) — the connection's
	// preupdate-hook callback. Its body is emitted as a Go closure when the
	// test registers it via `db preupdate hook preupdate_hook`.
	if name == "preupdate_hook" {
		tp.preupdateHookBody = body
		return true
	}
	// Hook callback procs (hook.test): commit_hook, rollback_hook, and the
	// update/preupdate callback procs. Their bodies are emitted as Go
	// closures when the test registers them via db commit_hook / db
	// rollback_hook / db update_hook / db preupdate hook.
	switch name {
	case "commit_hook", "rollback_hook", "update_cb", "preupdate_cb", "commit_hook_cb", "rollback_cb":
		if tp.commitHookBodies == nil {
			tp.commitHookBodies = make(map[string]string)
		}
		tp.commitHookBodies[name] = body
		return true
	}
	return false
}

// registerEarlySpecialProcs tries the special-function proc recognizers in
// order and registers the first match. Returns true when one matched (the
// caller stops processing the proc).
func (tp *transpiler) registerEarlySpecialProcs(name string, args []tcl.RawWord, body string) bool {
	// `proc zip {x} { incr ::next_x; set ::strings($::next_x) $x; return
	// $::next_x }` and `proc unzip {x} { return $::strings($x) }` — the
	// fts3comp1 compression harness. Register them so `db func $zip zip`
	// emits a stateful Go function (counter + map) instead of a no-op stub.
	if tp.registerZipUnzipProcs(name, body) {
		return true
	}
	// `proc autovac_page_callback {schema filesize freesize pagesize} { ... }`
	// (autovacuum2.test) — the sqlite3_autovacuum_pages callback. Emits a Go
	// closure that the testgen passes to db.SetAutovacuumPagesCallback so
	// the COMMIT-time autovacuum hook fires the callback and appends its
	// args to ::autovac_callback_data.
	if tp.registerAutovacPageCallbackProc(name, body) {
		return true
	}
	// `proc blob {a} { binary decode hex $a }` (fts3corrupt4) — the
	// corruption tests build modified root blobs through this hex decoder.
	if tp.registerBlobProc(name, body) {
		return true
	}
	// `proc tx x {return [string map [list ( \173 ) \175 ' \042 < \133 > \135] $x]}`
	// (json101) — a single-argument character-mapping proc. Register the
	// pairs so a later [tx $var] command substitution emits a Go
	// strings.NewReplacer expression.
	if tp.registerStringMapProc(name, args, body) {
		return true
	}
	// `proc make_record_wrapper {args} { make_fts3record $args }`
	// (fts4record.test) — the test-harness record builder registered as
	// `db func record make_record_wrapper`.
	if tp.registerFts3RecordProc(name, body) {
		return true
	}
	// `proc mit {blob} { ... binary scan ... }` (fts3matchinfo) — the
	// matchinfo blob decoder.
	return tp.registerMatchinfoProc(name, body)
}

// procLateKind registers the remaining proc kinds (TCL authorizer proc, the
// simple constant/counter/predicate/join/prefix/collation kinds, the
// arg-recorder kind, and e_vacuum's create_db setup proc). Returns true
// when one matched (the caller stops processing the proc).
func (tp *transpiler) procLateKind(name string, args []tcl.RawWord, body string) bool {
	// `proc auth {code arg1 arg2 arg3 arg4 args} { ... }` — a TCL authorizer
	// proc registered via `db authorizer ::auth`. Transpile the body into a
	// Go type implementing auth.Authorizer.
	if tp.processAuthorizerProc(args) {
		return true
	}
	if tp.registerProcKinds(name, body) {
		return true
	}
	// `proc trigfunc {args} { set ::TRIGGER $args }` (alter.test) — the SQL
	// function REPLACES a TCL global with the TCL rendering of its argument
	// list on every call. Register the target variable so `db func NAME
	// NAME` emits the recorder closure.
	if tp.registerRecorderProcKind(name, args[1].Text, body) {
		return true
	}
	// `proc create_db {{sql ""}} { ... }` (e_vacuum.test) creates test.db
	// with page_size 1024, auto_vacuum settings, and the t1/t2 tables used by
	// the vacuum tests. The file-size return value is VACUUM-dependent and
	// cannot be reproduced; emit the CREATE/INSERT setup so later tests see
	// t1/t2 (the file-size assertions are skipped as VACUUM).
	if name == "create_db" && strings.Contains(body, "CREATE TABLE t1") {
		if tp.constFuncs == nil {
			tp.constFuncs = make(map[string]string)
		}
		if tp.specialFuncs == nil {
			tp.specialFuncs = make(map[string]string)
		}
		tp.specialFuncs[name] = "tclCreateDB"
		return true
	}
	return false
}

// processProc recognizes simple test-harness procs (constant-returning,
// counter, predicate, join, collation) and registers them for later `db func`
// / `db collate` handlers.
func (tp *transpiler) processProc(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitLine("// proc definition (not transpiled)")
		return
	}
	name := strings.TrimSpace(args[0].Text)
	body := stripProcBodyOuterBraces(args[2].Text)
	if tp.procAliasAndRedefine(name, body) {
		return
	}
	if tp.emitFsUserProcs(name, body) {
		return
	}
	// Procs with a known faithful Go implementation (rtree4.test's
	// rand/randincr/scramble and variants) register into the generated
	// test's runtime proc registry so every later [name arg...] bracket —
	// plain set RHS, nested expr, interpolated SQL — evaluates through it.
	if fnIdent := userProcHelperFor(name, body); fnIdent != "" {
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { if len(a) == 0 { return \"\" }; return %s(a[0]) })", name, fnIdent)
	}
	tp.procTrackBody(name, args, body)
	if tp.procHookBodyKind(name, body) {
		return
	}
	// `proc int2str {i} { string range [string repeat "$i." 450] 0 899 }`
	// — the test-harness int2str builds a 900-char string. Register a
	// deterministic Go function for it (the transpiler emits a stub
	// otherwise, breaking comparisons with int2str(...) results).
	tp.registerInt2str(name, body)
	if tp.registerEarlySpecialProcs(name, args, body) {
		return
	}
	// `proc utf8_to_hstr {in} { ... }` — badutf2's hex→%XX converter.
	tp.registerUtf8ToHstr(name, body)
	if tp.procLateKind(name, args, body) {
		return
	}
	tp.emitLine("// proc definition (not transpiled)")
}

// registerRecorderProcKind registers a `proc NAME {args} { set ::VAR $args }`
// body as a recorder UDF kind. Returns true when the body matched.
func (tp *transpiler) registerRecorderProcKind(name, params, body string) bool {
	goVar := recorderProcVar(params, body)
	if goVar == "" {
		return false
	}
	if tp.recorderFuncs == nil {
		tp.recorderFuncs = make(map[string]string)
	}
	tp.recorderFuncs[name] = goVar
	tp.emitLine("// proc %s records its args into %s (registered via db func)", name, goVar)
	return true
}

// registerProcKinds tries each simple proc kind (constant, counter, predicate,
// join, collation) in order and registers the first match. Returns true when a
// kind was registered (the caller stops processing the proc).
func (tp *transpiler) registerProcKinds(name, body string) bool {
	if constVal := constantProcValue(body); constVal != "" {
		return tp.registerConstProc(name, constVal)
	}
	if varName := counterProcValue(body); varName != "" {
		return tp.registerCounterProc(name, varName)
	}
	if pred := predicateProcValue(body); pred != "" {
		return tp.registerPredProc(name, pred)
	}
	if sep := joinProcValue(body); sep != "" {
		return tp.registerJoinProc(name, sep)
	}
	if prefix := prefixProcValue(body); prefix != "" {
		return tp.registerPrefixProc(name, prefix)
	}
	if goFn := collationProcGo(body); goFn != "" {
		return tp.registerCollateProc(name, goFn)
	}
	return false
}

// emitProcBodyRegistration emits a fresh RegisterFunction for a proc body
// that was REdefined with a different body. TCL's `proc NAME {x} {BODY}`
// replaces the proc the db-func binding points to, so SQL calls see the new
// body (fts4intck1.test redefines slang from a string-map to identity).
// Returns true when the body matched a known proc kind and was emitted.
func (tp *transpiler) emitProcBodyRegistration(name, body string) bool {
	if name == "" {
		return false
	}
	if pairs := stringMapProcValue(body); pairs != "" {
		items := tclCmdWords(pairs)
		if len(items) < 2 || len(items)%2 != 0 {
			return false
		}
		tp.emitLine("// proc %s redefined (string map) — re-register", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
		tp.emitLine("\ts := tclStr(args[0])")
		for i := 0; i+1 < len(items); i += 2 {
			tp.emitLine("\ts = strings.ReplaceAll(s, %q, %q)", items[i], items[i+1])
		}
		tp.emitLine("\treturn s, nil")
		tp.emitLine("}, 0, -1)")
		return true
	}
	if identityProcValue(body) {
		tp.emitLine("// proc %s redefined (identity) — re-register", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
		tp.emitLine("\treturn args[0], nil")
		tp.emitLine("}, 0, -1)")
		return true
	}
	if constVal := constantProcValue(body); constVal != "" {
		tp.emitLine("// proc %s redefined (constant) — re-register", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return int64(%s), nil }, 0, -1)", tp.dbVar, name, constVal)
		return true
	}
	return false
}

// registerPrefixProc registers a prefix proc (`proc NAME {args} { return
// "P: $args" }`) so later `db func NAME NAME` emits a scalar SQL function
// that joins its args with a space and prepends the fixed prefix.
func (tp *transpiler) registerPrefixProc(name, prefix string) bool {
	if name == "" {
		return false
	}
	if tp.prefixFuncs == nil {
		tp.prefixFuncs = make(map[string]string)
	}
	tp.prefixFuncs[name] = prefix
	tp.emitLine("// proc %s prepends %q to its args (registered via db func)", name, prefix)
	return true
}

// registerInt2str recognizes the test-harness int2str proc and registers it as
// a special function mapped to the tclInt2str Go helper.
func (tp *transpiler) registerInt2str(name, body string) {
	if name != "int2str" {
		return
	}
	if !strings.Contains(body, "string repeat") || !strings.Contains(body, "450") || !strings.Contains(body, "899") {
		return
	}
	if tp.constFuncs == nil {
		tp.constFuncs = make(map[string]string)
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclInt2str"
}

// registerUtf8ToHstr recognizes the test-harness utf8_to_hstr proc (badutf2):
// `proc utf8_to_hstr {in} { regsub -all -- {(..)} $in {%[format "%s" \1]} out;
// subst $out }` — converts a hex string like "C3BF" to "%C3%BF". The body is
// a regsub+subst TCL idiom the transpiler cannot inline, so register a Go
// helper.
func (tp *transpiler) registerUtf8ToHstr(name, body string) {
	if name != "utf8_to_hstr" {
		return
	}
	if !strings.Contains(body, "regsub") || !strings.Contains(body, "subst") {
		return
	}
	if tp.constFuncs == nil {
		tp.constFuncs = make(map[string]string)
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclUtf8ToHstr($data)"
}

// registerBlobProc recognizes the test-harness blob proc (`proc blob {a} {
// binary decode hex $a }` — fts3corrupt4). It registers the name in
// specialFuncs so `db func blob blob` emits a Go hex-decoder function.
func (tp *transpiler) registerBlobProc(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(compact, "binary decode hex") {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclBlobHexDecode"
	return true
}

// registerStringMapProc recognizes a single-argument character-mapping proc:
// `proc NAME x {return [string map [list K1 V1 K2 V2 ...] $x]}` (json101's
// `proc tx x`, which converts '(' ')' '\” '<' '>' shorthand into JSON
// braces/quotes/brackets). The old/new pairs are stored flat so a later
// [NAME $var] substitution emits a Go strings.NewReplacer chain.
func (tp *transpiler) registerStringMapProc(name string, args []tcl.RawWord, body string) bool {
	if name == "" || len(args) < 2 {
		return false
	}
	param := strings.TrimSpace(args[1].Text)
	if param == "" || strings.ContainsAny(param, " \t") {
		return false // exactly one parameter
	}
	const prefix = "return [string map [list "
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.HasPrefix(compact, prefix) {
		return false
	}
	tail := strings.TrimSpace(compact[len(prefix):])
	closer := "] $" + param + "]"
	if !strings.HasSuffix(tail, closer) {
		return false
	}
	pairsText := strings.TrimSpace(tail[:len(tail)-len(closer)])
	words := tclCmdWords(pairsText)
	if len(words) == 0 || len(words)%2 != 0 {
		return false
	}
	// Resolve TCL backslash escapes (octal \173 → '{', \042 → '"') so the
	// Replacer carries the real characters.
	pairs := make([]string, len(words))
	for i, w := range words {
		pairs[i] = unescapeBareWord(w)
	}
	if tp.procStringMaps == nil {
		tp.procStringMaps = make(map[string][]string)
	}
	tp.procStringMaps[name] = pairs
	tp.emitLine("// proc %s: string map %q", name, pairs)
	return true
}

// registerFts3RecordProc recognizes the fts4record wrapper proc
// (`proc make_record_wrapper {args} { make_fts3record $args }`) — the
// test-harness record builder registered as `db func record
// make_record_wrapper`. The SQL function builds an FTS3 segment record blob
// (varint-encoded integers + raw string bytes, src/test_hexio.c
// make_fts3record).
func (tp *transpiler) registerFts3RecordProc(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(compact, "make_fts3record") {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclFts3Record"
	return true
}

// registerMatchinfoProc recognizes the FTS matchinfo test-harness proc
// (`proc mit {blob} { set scan(littleEndian) i*; set scan(bigEndian) I*;
// binary scan $blob $scan($::tcl_platform(byteOrder)) r; return $r }` —
// fts3matchinfo/fts3matchinfo2). It decodes the matchinfo blob as a list of
// little-endian 32-bit integers. It registers the name in specialFuncs so
// `db func mit mit` emits a Go blob→int-list decoder instead of a no-op stub.
func (tp *transpiler) registerMatchinfoProc(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(compact, "binary scan") || !strings.Contains(compact, "littleEndian") {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclMatchinfoDecode"
	return true
}

// registerZipUnzipProcs recognizes the fts3comp1 compression-harness procs
// (`proc zip {x} { incr ::next_x; set ::strings($::next_x) $x; return
// $::next_x }` and `proc unzip {x} { return $::strings($x) }`). They are
// registered in specialFuncs so `db func $zip zip` emits a stateful Go
// closure (a counter + a map) instead of a no-op stub — the FTS4
// compress/uncompress functions must return integer keys the content table
// stores and the test compares.
func (tp *transpiler) registerZipUnzipProcs(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	isZip := strings.Contains(compact, "incr ::next_x") &&
		strings.Contains(compact, "set ::strings(") &&
		strings.Contains(compact, "return $::next_x")
	isUnzip := strings.Contains(compact, "return $::strings(")
	if !isZip && !isUnzip {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	if isZip {
		tp.specialFuncs[name] = "tclZipFn"
	} else {
		tp.specialFuncs[name] = "tclUnzipFn"
	}
	return true
}

// registerAutovacPageCallbackProc recognizes the autovacuum2.test
// `autovac_page_callback` and `autovac_page_callback_off` procs
// (sqlite3_autovacuum_pages). The transpiler emits a Go closure that
// the testgen can pass to db.SetAutovacuumPagesCallback, mirroring the
// TCL semantics:
//   - autovac_page_callback: appends (schema, filesize, freesize, pagesize)
//     to ::autovac_callback_data and returns freesize/2 (per-batch limit).
//   - autovac_page_callback_off: returns 0 (no vacuum this batch).
//
// The Go function name follows the proc name verbatim (camelCase). Stored
// on the transpiler's autovacCallbacks map so the sqlite3_autovacuum_pages
// dispatcher can emit the SetAutovacuumPagesCallback call.
func (tp *transpiler) registerAutovacPageCallbackProc(name, body string) bool {
	if name != "autovac_page_callback" && name != "autovac_page_callback_off" {
		return false
	}
	if tp.autovacCallbacks == nil {
		tp.autovacCallbacks = make(map[string]string)
	}
	if _, exists := tp.autovacCallbacks[name]; exists {
		// Already emitted for this proc (redefinition is a no-op).
		return true
	}
	tp.autovacCallbacks[name] = body
	// Emit a Go variable with the proc name in camelCase, holding a
	// closure that matches the TCL semantics.
	goName := tclVarToGo(name) // autovac_page_callback -> autovacPageCallback
	if name == "autovac_page_callback" {
		// TCL body: global autovac_callback_data; lappend it
		// $schema $filesize $freesize $pagesize; return [expr {$freesize/2}]
		tp.emitLine("// proc %s {schema filesize freesize pagesize}: appends callback args to", name)
		tp.emitLine("// autovac_callback_data and returns freesize/2 (per-batch vacuum limit).")
		tp.emitLine("var %s = func(schema string, fileSize, nFree, pageSize uint32) uint32 {", goName)
		tp.emitLine("\tautovac_callback_data = tclListAppend(autovac_callback_data, schema,")
		tp.emitLine("\t\tstrconv.FormatUint(uint64(fileSize), 10),")
		tp.emitLine("\t\tstrconv.FormatUint(uint64(nFree), 10),")
		tp.emitLine("\t\tstrconv.FormatUint(uint64(pageSize), 10))")
		tp.emitLine("\treturn nFree / 2")
		tp.emitLine("}")
	} else {
		// autovac_page_callback_off: returns 0 (no vacuum).
		tp.emitLine("// proc %s {schema filesize freesize pagesize}: returns 0 (no vacuum).", name)
		tp.emitLine("var %s = func(schema string, fileSize, nFree, pageSize uint32) uint32 {", goName)
		tp.emitLine("\treturn 0")
		tp.emitLine("}")
	}
	return true
}

// registerConstProc registers a constant-returning proc. Returns false when
// the name is empty (caller falls through to the other proc kinds).
func (tp *transpiler) registerConstProc(name, constVal string) bool {
	if name == "" {
		return false
	}
	if tp.constFuncs == nil {
		tp.constFuncs = make(map[string]string)
	}
	tp.constFuncs[name] = constVal
	tp.emitLine("// proc %s returns constant %s (registered via db func)", name, constVal)
	return true
}

// registerCounterProc registers a counter proc (`proc NAME {} { incr ::VAR }`).
func (tp *transpiler) registerCounterProc(name, varName string) bool {
	if name == "" {
		return false
	}
	if tp.counterFuncs == nil {
		tp.counterFuncs = make(map[string]string)
	}
	tp.counterFuncs[name] = varName
	tp.emitLine("// proc %s increments counter var %s (registered via db func)", name, varName)
	return true
}

// registerPredProc registers a predicate proc (`proc NAME {x} { expr $x < N }`).
func (tp *transpiler) registerPredProc(name, pred string) bool {
	if name == "" {
		return false
	}
	if tp.predFuncs == nil {
		tp.predFuncs = make(map[string]string)
	}
	tp.predFuncs[name] = pred
	tp.emitLine("// proc %s predicate %s (registered via db func)", name, pred)
	return true
}

// registerJoinProc registers a join proc (`proc NAME {args} { return [join
// $args -] }`).
func (tp *transpiler) registerJoinProc(name, sep string) bool {
	if name == "" {
		return false
	}
	if tp.joinFuncs == nil {
		tp.joinFuncs = make(map[string]string)
	}
	tp.joinFuncs[name] = sep
	tp.emitLine("// proc %s joins args with %q (registered via db func)", name, sep)
	return true
}

// registerCollateProc registers a collation proc (`proc NAME {a b} { ... }`).
func (tp *transpiler) registerCollateProc(name, goFn string) bool {
	if name == "" {
		return false
	}
	if tp.collateGoFuncs == nil {
		tp.collateGoFuncs = make(map[string]string)
	}
	tp.collateGoFuncs[name] = goFn
	// TCL resolves a `db collate NAME PROC` binding through PROC at every
	// collation call, so redefining an already-registered collation proc
	// re-points the comparison from that point on (windowE.test 1.3
	// redefines custom between two queries). Emit a fresh registration.
	if dbVar := tp.collateEmittedProcs[name]; dbVar != "" {
		tp.emitLine("// proc %s collation redefined — re-register (TCL late binding)", name)
		tp.emitLine("%s.RegisterCollation(%q, %s)", dbVar, name, goFn)
		return true
	}
	tp.emitLine("// proc %s collation (registered via db collate)", name)
	return true
}

// inlineableParams reports whether a proc parameter word makes the proc
// inlineable: zero parameters ("" after brace stripping) or exactly one
// parameter with a default value ("module rtree" — TCL's {{name default}}
// optional-argument form). Multi-parameter procs are NOT inlineable.
func inlineableParams(paramsInner string) bool {
	if paramsInner == "" {
		return true
	}
	fields := strings.Fields(paramsInner)
	return len(fields) == 2 && !strings.ContainsAny(paramsInner, "\"{}[]$") &&
		isValidGoIdent(tclVarToGo(fields[0]))
}

// inlineProcDefaultAssign returns the Go assignment for an inline proc's optional
// parameter bound to its default value ("module := \"rtree\"").
func inlineProcDefaultAssign(paramsInner string) string {
	fields := strings.Fields(paramsInner)
	if len(fields) != 2 {
		return ""
	}
	name := tclVarToGo(fields[0])
	// A parameter named like a reserved preamble variable must not be bound to
	// its string default: `db = "db"` clobbers the *frigolite.DB connection
	// handle and breaks the build (e_dropview/e_droptrigger's
	// `proc list_all_views {{db db}}`); `err = ...` clobbers the error var.
	// The inlined body's $db references resolve to the connection variable
	// regardless.
	if name == "db" || isPreDeclaredDB(name) || name == "err" {
		return ""
	}
	def := strings.TrimSpace(fields[1])
	def = strings.TrimSuffix(strings.TrimPrefix(def, "{"), "}")
	def = strings.Trim(def, "'\"")
	return fmt.Sprintf("%s = %q", name, def)
}
