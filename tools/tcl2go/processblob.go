// Package main implements the tcl2go tool.
//
// This file handles the sqlite3_blob_* C-API emulation: sqlite3_blob_open,
// sqlite3_blob_bytes, sqlite3_blob_read, sqlite3_blob_write, sqlite3_blob_close
// and the TCL `db incrblob` method. The engine's Blob type
// (frigolite.OpenBlob) backs the generated calls.

package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// expectedStringExpr renders a do_test expected value that contains
// `[binary format ...]` / `[string repeat ...]` command substitutions into a
// Go string expression. Returns ("", false) when the word is not one of the
// supported binary-format forms, so the caller falls back to goStringLiteral.
func (tp *transpiler) expectedStringExpr(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	// [userProc args...] — a fixture proc registered by the generated test
	// resolves to a runtime registry call whose result is the wanted value
	// (vtabH 3.x: [sort_files $res true], [contents $pwd]).
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		cmdText := strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
		fields := tclCmdWords(cmdText)
		if len(fields) >= 1 && globalUserProcs[fields[0]] {
			callArgs := make([]string, 0, len(fields)-1)
			for _, a := range fields[1:] {
				if strings.HasPrefix(a, "$") && !strings.Contains(a, "(") {
					if gv := tclVarToGo(strings.TrimPrefix(a, "$")); gv != "" && isValidGoIdent(gv) {
						callArgs = append(callArgs, gv)
						continue
					}
				}
				callArgs = append(callArgs, strconv.Quote(a))
			}
			return fmt.Sprintf("callTclUserProc(%q, %s)", fields[0], strings.Join(callArgs, ", ")), true
		}
	}
	// `lreverse $VAR` — reverse a TCL list variable at runtime
	// (fts3first.test's order=DESC comparisons).
	if strings.HasPrefix(text, "lreverse $") {
		varName := strings.TrimSpace(text[len("lreverse $"):])
		if tclVarToGo(varName) != "" {
			return fmt.Sprintf("tclLreverse(%s)", tclVarToGo(varName)), true
		}
	}
	// [string repeat [binary format c 0] N] — repeat a single-byte pattern.
	if strings.HasPrefix(text, "[string repeat [binary format ") && strings.HasSuffix(text, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(text, "[string repeat [binary format "), "]")
		// inner: "c 0] N" — split at "]".
		closeIdx := strings.Index(inner, "]")
		if closeIdx < 0 {
			return "", false
		}
		formatAndArg := strings.Fields(strings.TrimSpace(inner[:closeIdx]))
		countExpr := strings.TrimSpace(inner[closeIdx+1:])
		if len(formatAndArg) < 2 {
			return "", false
		}
		spec := formatAndArg[0]
		pattern, ok := binaryFormatBytes(spec, formatAndArg[1:])
		if !ok {
			return "", false
		}
		if len(pattern) != 1 {
			return "", false
		}
		countGo := tp.valueExpr(tcl.RawWord{Text: countExpr})
		return fmt.Sprintf("tclStringRepeat(string([]byte{%d}), %s)", pattern[0], countGo), true
	}
	// [binary format SPEC ARGS...] — build the byte string at runtime.
	if strings.HasPrefix(text, "[binary format ") && strings.HasSuffix(text, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(text, "[binary format "), "]")
		fields := tclCmdWords(inner)
		if len(fields) < 2 {
			return "", false
		}
		spec := fields[0]
		// Resolve each arg: $var refs, integer literals, and supported
		// [string range ...] / [string repeat ...] command substitutions.
		args := fields[1:]
		vals := make([]string, 0, len(args))
		for _, a := range args {
			vals = append(vals, tp.binaryArgExpr(a))
		}
		expr, ok := binaryFormatGoExpr(spec, vals)
		if !ok {
			return "", false
		}
		return expr, true
	}
	return "", false
}

// binaryArgExpr renders one argument of a `binary format` spec: a $var
// reference, an integer literal, or a supported [string range ...] command
// substitution (used by the corruption tests to slice the root blob).
func (tp *transpiler) binaryArgExpr(a string) string {
	a = strings.TrimSpace(a)
	if strings.HasPrefix(a, "[string range ") && strings.HasSuffix(a, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(a, "[string range "), "]")
		parts := tclCmdWords(inner)
		if len(parts) == 3 {
			strExpr := tp.valueExpr(tcl.RawWord{Text: parts[0]})
			startExpr := tp.valueExpr(tcl.RawWord{Text: parts[1]})
			endExpr := tp.valueExpr(tcl.RawWord{Text: parts[2]})
			return fmt.Sprintf("tclStringRange(%s, %s, %s)", strExpr, startExpr, endExpr)
		}
	}
	if strings.HasPrefix(a, "[string repeat ") && strings.HasSuffix(a, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(a, "[string repeat "), "]")
		parts := tclCmdWords(inner)
		if len(parts) == 2 {
			strExpr := tp.valueExpr(tcl.RawWord{Text: parts[0]})
			countExpr := tp.valueExpr(tcl.RawWord{Text: parts[1]})
			return fmt.Sprintf("tclStringRepeat(%s, %s)", strExpr, countExpr)
		}
	}
	return tp.valueExpr(tcl.RawWord{Text: a})
}

// binaryFormatBytes evaluates a `binary format` spec with literal integer
// arguments, returning the resulting bytes. Only the single-char 'c' spec
// with one literal arg is supported (used by string-repeat patterns).
func binaryFormatBytes(spec string, args []string) ([]byte, bool) {
	if spec != "c" && spec != "b" {
		return nil, false
	}
	if len(args) != 1 {
		return nil, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil {
		return nil, false
	}
	return []byte{byte(n)}, true
}

// binaryFormatGoExpr renders a `[binary format SPEC ARGS...]` into a Go
// string expression. Integer specifiers (c/b/s/i) and byte-string specifiers
// (aN/a*) are supported; each contributes a `string(...)` fragment that is
// concatenated. A spec like "ccc" repeats the format char once per arg.
func binaryFormatGoExpr(spec string, args []string) (string, bool) {
	var parts []string
	ai := 0
	i := 0
	for i < len(spec) {
		ch := spec[i]
		switch ch {
		case 'c', 'b', 's', 'i', 'a':
		default:
			return "", false
		}
		if ch == 'a' {
			// aN copies the first N bytes of the string arg; a* copies all.
			if ai >= len(args) {
				return "", false
			}
			arg := args[ai]
			ai++
			if i+1 < len(spec) && spec[i+1] == '*' {
				parts = append(parts, fmt.Sprintf("string(tclBlobBytes(%s))", arg))
				i += 2
				if i < len(spec) {
					return "", false
				}
				break
			}
			// aN: copy N bytes (the arg is a string/blob).
			n := 0
			j := i + 1
			for j < len(spec) && spec[j] >= '0' && spec[j] <= '9' {
				n = n*10 + int(spec[j]-'0')
				j++
			}
			if n == 0 && j == i+1 {
				return "", false
			}
			parts = append(parts, fmt.Sprintf("string(tclBlobBytes(%s)[:%d])", arg, n))
			i = j
			continue
		}
		if i+1 < len(spec) && spec[i+1] == '*' {
			// c* consumes ALL remaining args as bytes.
			var byteExprs []string
			for _, a := range args[ai:] {
				byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s))", a))
			}
			if len(byteExprs) > 0 {
				parts = append(parts, fmt.Sprintf("string([]byte{%s})", strings.Join(byteExprs, ", ")))
			}
			i += 2
			if i < len(spec) {
				return "", false
			}
			break
		}
		if ai >= len(args) {
			return "", false
		}
		a := args[ai]
		ai++
		var byteExprs []string
		switch ch {
		case 'c', 'b':
			byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s))", a))
		case 's':
			byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s)&0xff), byte((tclBlobInt(%s)>>8)&0xff)", a, a))
		case 'i':
			byteExprs = append(byteExprs, fmt.Sprintf("byte(tclBlobInt(%s)&0xff), byte((tclBlobInt(%s)>>8)&0xff), byte((tclBlobInt(%s)>>16)&0xff), byte((tclBlobInt(%s)>>24)&0xff)", a, a, a, a))
		}
		if len(byteExprs) > 0 {
			parts = append(parts, fmt.Sprintf("string([]byte{%s})", strings.Join(byteExprs, ", ")))
		}
		i++
	}
	if len(parts) == 0 {
		return "", false
	}
	if len(parts) == 1 {
		return parts[0], true
	}
	return "(" + strings.Join(parts, " + ") + ")", true
}

// processBlobWriteTest handles `blob_write_test TN ID IOFFSET BLOB NDATA
// FINAL` — the e_blobwrite.test proc that opens a write blob on t1.t at the
// given rowid, writes NDATA bytes of BLOB at IOFFSET, closes, and asserts the
// final column value. Emits the open/write/close side effects and the
// do_execsql comparison.
func (tp *transpiler) processBlobWriteTest(args []tcl.RawWord) {
	if len(args) < 6 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	rowID := tp.valueExpr(args[1])
	offset := tp.valueExpr(args[2])
	data := tp.sqlStringValue(args[3])
	n := tp.valueExpr(args[4])
	// Open a write blob on t1.t at rowid.
	handle := tp.newBlobChannel()
	tp.emitLine("%s, _berr = %s.OpenBlob(%q, %q, %q, tclRowID(%s), true)", handle, tp.dbVar, "main", "t1", "t", rowID)
	tp.emitLine("if _berr != nil {")
	tp.emitLine("\t%s = nil", handle)
	tp.emitLine("} else {")
	tp.emitLine("\tif _err2 := %s.Write(tclBlobInt(%s), []byte(%s), tclBlobInt(%s)); _err2 != nil {", handle, offset, data, n)
	tp.emitLine("\t\t_r = \"\"")
	tp.emitLine("\t} else {")
	tp.emitLine("\t\t_r = \"\"")
	tp.emitLine("\t}")
	tp.emitLine("\t%s.Close()", handle)
	tp.emitLine("}")
	// Assert the final column value with a SELECT.
	tp.emitLine("{ // %s (blob_write_test final)", nameExpr)
	tp.indent++
	tp.emitLine("r = db.Query(\"SELECT t FROM t1 WHERE a=\" + sqlLiteral(%s))", rowID)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, \"SELECT t FROM t1 WHERE a=\"+sqlLiteral(%s))", rowID)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("got := flatten(r)")
	tp.emitLine("want := %s", tp.goStringLiteral(tcl.RawWord{Text: strings.TrimSpace(args[5].Text)}))
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", got, want, %s)", nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processBlobWriteErrorTest handles `blob_write_error_test TN B IOFFSET BLOB
// NDATA ERRCODE ERRMSG` — the e_blobwrite.test proc that writes on an open
// blob handle and asserts the catch result, errcode, and errmsg. Emits the
// blob write (with the catch comparison) and the errcode/errmsg assertions.
func (tp *transpiler) processBlobWriteErrorTest(args []tcl.RawWord) {
	if len(args) < 7 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	blobExpr := tp.blobArgExpr(args[1])
	offset := tp.valueExpr(args[2])
	data := tp.sqlStringValue(args[3])
	n := tp.valueExpr(args[4])
	errcode := strings.ToUpper(args[5].Text)
	// Emit the blob write with its result ("" on success, error message on
	// failure) as a catch-style comparison.
	if tp.isVarDeclared("_rc") {
		tp.emitLine("_rc = \"0\"")
	} else {
		tp.emitLine("_rc := \"0\"")
		tp.vars = append(tp.vars, "_rc")
	}
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("var _catchErr error")
	if blobExpr != "nil" {
		tp.emitLine("if _err2 := %s.Write(tclBlobInt(%s), []byte(%s), tclBlobInt(%s)); _err2 != nil {", blobExpr, offset, data, n)
		tp.emitLine("\t_catchErr = _err2")
		tp.emitLine("}")
	}
	tp.emitLine("if _catchErr != nil { _rc = \"1\" }")
	tp.emitLine("msg = \"\"")
	tp.emitLine("if _catchErr != nil { msg = _catchErr.Error() }")
	tp.indent--
	tp.emitLine("}")
	// The expected list is {isError ret} — e.g. {0 {}} or {1 SQLITE_READONLY}.
	// Compare the catch rc and msg.
	if errcode == "SQLITE_OK" {
		tp.emitLine("if _rc != \"0\" || msg != \"\" {")
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s %%s]\\n  want: [0 {}]\\n  body: do_test %%s\", _rc, msg, %s)", nameExpr)
		tp.emitLine("}")
	} else {
		tp.emitLine("if _rc != \"1\" || !strings.Contains(msg, %q) {", errcode)
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s %%s]\\n  want: [1 %s]\\n  body: do_test %%s\", _rc, msg, %s)", errcode, nameExpr)
		tp.emitLine("}")
	}
	// errcode/errmsg assertions (the proc calls sqlite3_errcode/errmsg).
	tp.emitLine("_r = %s.LastErrCode()", tp.dbVar)
	tp.emitLine("if _r != %q {", errcode)
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%s]\\n  body: do_test %%s\", _r, %s)", errcode, nameExpr)
	tp.emitLine("}")
}

// processCreateT1 handles the incrblob4.test `create_t1` proc: sets
// page_size 1024 and creates t1(k INTEGER PRIMARY KEY, v).
func (tp *transpiler) processCreateT1(args []tcl.RawWord) {
	tp.emitLine("_res = %s.Exec(\"PRAGMA page_size = 1024; CREATE TABLE t1(k INTEGER PRIMARY KEY, v);\")", tp.dbVar)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\", _res.Error)")
	tp.emitLine("}")
}

// processPopulateT1 handles the incrblob4.test `populate_t1` proc: inserts 26
// rows (a-z) each with a 900-char repeated string.
func (tp *transpiler) processPopulateT1(args []tcl.RawWord) {
	tp.emitLine("for _, _ch := range []string{\"a\", \"b\", \"c\", \"d\", \"e\", \"f\", \"g\", \"h\", \"i\", \"j\", \"k\", \"l\", \"m\", \"n\", \"o\", \"p\", \"q\", \"r\", \"s\", \"t\", \"u\", \"v\", \"w\", \"x\", \"y\", \"z\"} {")
	tp.indent++
	tp.emitLine("_res = %s.Exec(\"INSERT INTO t1(v) VALUES(\" + sqlLiteral(tclStringRepeat(_ch, 900)) + \")\")", tp.dbVar)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\", _res.Error)")
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

func (tp *transpiler) processSqlite3BlobOpen(args []tcl.RawWord) {
	if len(args) < 7 {
		tp.emitLine("// sqlite3_blob_open (malformed)")
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	schemaName := tp.goStringLiteral(args[1])
	table := tp.goStringLiteral(args[2])
	column := tp.goStringLiteral(args[3])
	rowID := tp.valueExpr(args[4])
	flags := tp.valueExpr(args[5])
	blobName := args[6].Text
	goName := tclVarToGo(blobName)
	if !isValidGoIdent(goName) {
		tp.emitLine("// sqlite3_blob_open %s (invalid identifier)", blobName)
		return
	}
	if !tp.isVarDeclared(goName) {
		tp.vars = append(tp.vars, goName)
		tp.emitLine("var %s string", goName)
		tp.emitLine("_ = %s", goName)
	}
	// The blob handle: allocate a fresh channel and store its name in the
	// TCL variable (B), so sqlite3_blob_bytes/read/write/close resolve the
	// handle via the channel registry.
	handle := tp.newBlobChannel()
	tp.registerBlobVar(goName, handle)
	tp.emitLine("%s = %q", goName, handle)
	// flags: 0 = read-only, non-zero = read/write.
	tp.emitLine("%s, _berr = %s.OpenBlob(%s, %s, %s, tclRowID(%s), tclBlobWritable(%s))", handle, dbConn, schemaName, table, column, rowID, flags)
	tp.emitLine("if _berr != nil {")
	tp.emitLine("\t%s = nil", handle)
	tp.emitLine("\t_r = \"\"")
	if tp.catchMode {
		tp.emitLine("\t_catchErr = _berr")
	}
	tp.emitLine("} else {")
	tp.emitLine("\t_r = %q", blobName)
	tp.emitLine("}")
}

// processSqlite3BlobBytes handles `sqlite3_blob_bytes $B`.
func (tp *transpiler) processSqlite3BlobBytes(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	blobExpr := tp.blobArgExpr(args[0])
	if blobExpr == "nil" {
		tp.emitLine("_r = \"0\"")
		return
	}
	tp.emitLine("_r = strconv.Itoa(%s.Bytes())", blobExpr)
}

// processSqlite3BlobRead handles `sqlite3_blob_read $B offset n`.
func (tp *transpiler) processSqlite3BlobRead(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	blobExpr := tp.blobArgExpr(args[0])
	offset := tp.valueExpr(args[1])
	n := tp.valueExpr(args[2])
	if blobExpr == "nil" {
		tp.emitLine("_r = \"\"")
		return
	}
	// The TCL wrapper returns the read bytes; on failure it raises.
	tp.emitLine("if _r2, _err2 := %s.Read(tclBlobInt(%s), tclBlobInt(%s)); _err2 != nil {", blobExpr, offset, n)
	tp.emitLine("\t_r = \"\"")
	if tp.catchMode {
		tp.emitLine("\t_catchErr = _err2")
	}
	tp.emitLine("} else {")
	tp.emitLine("\t_r = string(_r2)")
	tp.emitLine("}")
}

// processSqlite3BlobWrite handles `sqlite3_blob_write $B offset data n`.
// On success the TCL wrapper returns "" (SQLITE_OK); on failure it raises.
func (tp *transpiler) processSqlite3BlobWrite(args []tcl.RawWord) {
	if len(args) < 4 {
		return
	}
	blobExpr := tp.blobArgExpr(args[0])
	offset := tp.valueExpr(args[1])
	data := tp.sqlStringValue(args[2])
	n := tp.valueExpr(args[3])
	if blobExpr == "nil" {
		tp.emitLine("_r = \"\"")
		return
	}
	tp.emitLine("if _err2 := %s.Write(tclBlobInt(%s), []byte(%s), tclBlobInt(%s)); _err2 != nil {", blobExpr, offset, data, n)
	tp.emitLine("\t_r = \"\"")
	if tp.catchMode {
		tp.emitLine("\t_catchErr = _err2")
	}
	tp.emitLine("} else {")
	tp.emitLine("\t_r = \"\"")
	tp.emitLine("}")
}

// processSqlite3BlobReopen handles `sqlite3_blob_reopen $B rowid`.
func (tp *transpiler) processSqlite3BlobReopen(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	blobExpr := tp.blobArgExpr(args[0])
	rowID := tp.valueExpr(args[1])
	if blobExpr == "nil" {
		tp.emitLine("_r = \"\"")
		return
	}
	tp.emitLine("if _err2 := %s.Reopen(tclRowID(%s)); _err2 != nil {", blobExpr, rowID)
	tp.emitLine("\t_r = \"\"")
	if tp.catchMode {
		tp.emitLine("\t_catchErr = _err2")
	}
	tp.emitLine("} else {")
	tp.emitLine("\t_r = \"\"")
	tp.emitLine("}")
}

// processSqlite3BlobClose handles `sqlite3_blob_close $B`. A literal handle
// (e.g. `sqlite3_blob_close 0` closing a NULL handle) is a no-op.
func (tp *transpiler) processSqlite3BlobClose(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	blobExpr := tp.blobArgExpr(args[0])
	if blobExpr == "nil" {
		tp.emitLine("// sqlite3_blob_close %s (no open handle)", args[0].Text)
		return
	}
	tp.emitLine("%s.Close()", blobExpr)
}

// processOpen handles `open PATH` — opens a file channel. The TCL fd name is
// stored in the target variable (via `set fd [open PATH]`); the path text is
// recorded so a later `read $fd` / `close $fd` can read the file.
func (tp *transpiler) processOpen(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	// Standalone `open PATH` — emit the path as the command result.
	pathExpr := tp.goStringLiteral(args[0])
	tp.emitLine("_r = %s", pathExpr)
}

// processFConfigure handles `fconfigure $fd -translation binary` — a no-op
// (Go file/blob reads are already binary).
func (tp *transpiler) processFConfigure(args []tcl.RawWord) {
	// no-op: reads are binary by default.
}

// processRead handles `read $channel [n]`. Blob channels read from the
// incremental-blob handle; file channels read the file recorded at open time.
func (tp *transpiler) processRead(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	if goName := tp.resolveBlobChannel(args[0]); goName != "" {
		tp.processBlobRead(args)
		return
	}
	tp.emitLine("// read %s", describeArgsShort(args))
}

// processSeek handles `seek $channel offset`. Blob channels seek the cursor;
// file channels are emitted as comments.
func (tp *transpiler) processSeek(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	if goName := tp.resolveBlobChannel(args[0]); goName != "" {
		tp.processBlobSeek(args)
		return
	}
	tp.emitLine("// seek %s", describeArgsShort(args))
}

// blobArgExpr renders a blob handle argument ($B) as a Go expression naming
// the *frigolite.Blob variable. It emits a runtime channel resolution when
// the argument is a variable holding a blob channel name, so the handle
// resolves correctly even when the transpile-time channel map is not shared
// across body blocks.
func (tp *transpiler) blobArgExpr(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	text = strings.TrimPrefix(text, "$")
	if false {
		goName := tclVarToGo(strings.TrimPrefix(text, "$"))
		if isValidGoIdent(goName) && tp.isVarDeclared(goName) {
			// If the var is a known channel var, prefer the static channel
			// resolution; otherwise fall back to runtime resolution from the
			// var's string value.
			if ch := tp.resolveBlobChannel(w); ch != "" {
				return ch
			}
			return fmt.Sprintf("tclBlobResolve(%s%s)", goName, tp.blobArgsSuffix())
		}
	}
	if ch := tp.resolveBlobChannel(w); ch != "" {
		return ch
	}
	return "nil"
}

// blobArgsSuffix renders the trailing *frigolite.Blob arguments for
// tclBlobResolve (all predeclared incrblob_N channel variables).
func (tp *transpiler) blobArgsSuffix() string {
	var parts []string
	for i := 1; i <= 64; i++ {
		parts = append(parts, fmt.Sprintf("incrblob_%d", i))
	}
	return ", " + strings.Join(parts, ", ")
}

// processDBIncrblob handles `db incrblob [options] table column rowid` — the
// TCL incremental-blob command. When targetGo is non-empty (a `set VAR [db
// incrblob ...]` assignment), the *frigolite.Blob is assigned directly to
// that Go variable and registered as a blob channel; otherwise a fresh
// channel variable is created. The result (channel name) is left in `_r`.
func (tp *transpiler) processDBIncrblob(rest []tcl.RawWord) {
	tp.processDBIncrblobTo("", "db", rest)
}

// processDBIncrblobTo is processDBIncrblob with an explicit target Go
// variable name (from `set VAR [db incrblob ...]`) and connection name.
// When targetGo is "" a fresh incrblob_N variable is allocated.
func (tp *transpiler) processDBIncrblobTo(targetGo, dbConn string, rest []tcl.RawWord) {
	// Options: -readonly / -write (default read-write).
	write := true
	schemaName := "main"
	i := 0
	for i < len(rest) {
		t := strings.ToLower(rest[i].Text)
		if t == "-readonly" {
			write = false
			i++
			continue
		}
		if t == "-write" {
			write = true
			i++
			continue
		}
		break
	}
	// Schema may be given as the first non-option argument when it matches a
	// known schema name (incrblob3.test: `db incrblob -readonly aux t1 b 4`).
	args := rest[i:]
	if len(args) < 3 {
		tp.emitLine("// db incrblob (malformed)")
		return
	}
	tableArg := args[0].Text
	columnArg := args[1].Text
	rowIDArg := args[2].Text
	// A bare name that is a known attached schema (aux/main/temp) followed
	// by table+column is the schema-qualified form.
	if len(args) >= 4 && isSchemaName(tableArg) {
		schemaName = tableArg
		tableArg = args[1].Text
		columnArg = args[2].Text
		rowIDArg = args[3].Text
	}
	// The channel: allocate a fresh one; the TCL variable (targetGo) holds
	// the channel NAME string so `string match incrblob_* $blob` works and
	// the var can also be reassigned to a file handle later.
	handle := tp.newBlobChannel()
	if targetGo != "" && isValidGoIdent(targetGo) {
		if !tp.isVarDeclared(targetGo) {
			tp.vars = append(tp.vars, targetGo)
			tp.emitLine("var %s string", targetGo)
			tp.emitLine("_ = %s", targetGo)
		}
		tp.registerBlobVar(targetGo, handle)
		tp.emitLine("%s = %q", targetGo, handle)
	}
	tp.emitLine("%s, _berr = %s.OpenBlob(%q, %s, %s, tclRowID(%s), %v)", handle, dbConn, schemaName, tp.sqlStringValue(tcl.RawWord{Text: tableArg}), tp.sqlStringValue(tcl.RawWord{Text: columnArg}), tp.valueExpr(tcl.RawWord{Text: rowIDArg}), write)
	tp.emitLine("if _berr != nil {")
	if tp.catchMode {
		tp.emitLine("\t_catchErr = _berr")
	}
	tp.emitLine("\t%s = nil", handle)
	tp.emitLine("\t_r = \"\"")
	tp.emitLine("} else {")
	tp.emitLine("\t_r = %q", handle)
	tp.emitLine("}")
}

// processDBIncrblobEvalTo handles `set VAR [eval db incrblob $arg TABLE COL
// ROWID]` — the TCL eval form with a dynamic option variable ($arg is "" or
// "-readonly"). The -readonly flag is emitted as a runtime comparison on the
// arg variable.
func (tp *transpiler) processDBIncrblobEvalTo(targetGo, dbConn string, rest []tcl.RawWord) {
	writeExpr := "true"
	// If the first word is a $var holding the option ("-readonly"), compute
	// write access at runtime.
	if len(rest) > 0 && strings.HasPrefix(rest[0].Text, "$") {
		goVar := tclVarToGo(strings.TrimPrefix(rest[0].Text, "$"))
		if isValidGoIdent(goVar) && tp.isVarDeclared(goVar) {
			writeExpr = "!strings.Contains(" + goVar + ", \"-readonly\")"
			rest = rest[1:]
		}
	}
	if len(rest) < 3 {
		tp.emitLine("// db incrblob eval (malformed)")
		return
	}
	schemaName := "main"
	tableArg := rest[0].Text
	columnArg := rest[1].Text
	rowIDArg := rest[2].Text
	if len(rest) >= 4 && isSchemaName(tableArg) {
		schemaName = tableArg
		tableArg = rest[1].Text
		columnArg = rest[2].Text
		rowIDArg = rest[3].Text
	}
	handle := tp.newBlobChannel()
	if targetGo != "" && isValidGoIdent(targetGo) {
		if !tp.isVarDeclared(targetGo) {
			tp.vars = append(tp.vars, targetGo)
			tp.emitLine("var %s string", targetGo)
			tp.emitLine("_ = %s", targetGo)
		}
		tp.registerBlobVar(targetGo, handle)
		tp.emitLine("%s = %q", targetGo, handle)
	}
	tp.emitLine("%s, _berr = %s.OpenBlob(%q, %s, %s, tclRowID(%s), %s)", handle, dbConn, schemaName, tp.sqlStringValue(tcl.RawWord{Text: tableArg}), tp.sqlStringValue(tcl.RawWord{Text: columnArg}), tp.valueExpr(tcl.RawWord{Text: rowIDArg}), writeExpr)
	tp.emitLine("if _berr != nil {")
	if tp.catchMode {
		tp.emitLine("\t_catchErr = _berr")
	}
	tp.emitLine("\t%s = nil", handle)
	tp.emitLine("\t_r = \"\"")
	tp.emitLine("} else {")
	tp.emitLine("\t_r = %q", handle)
	tp.emitLine("}")
}

// isSchemaName reports whether a name is a known database schema qualifier
// (main/temp/temporary or an attached database name seen in the source).
func isSchemaName(name string) bool {
	switch strings.ToLower(name) {
	case "main", "temp", "temporary":
		return true
	}
	return false
}

// blobChannel records that a TCL variable holds an incremental-blob channel
// name; goName is the Go *frigolite.Blob variable for that channel.
type blobChannel struct {
	goName string
}

// blobChannels is the transpiler's set of blob-channel variable mappings
// (TCL var name → channel descriptor).
func (tp *transpiler) blobChannels() map[string]blobChannel {
	if tp.blobChans == nil {
		tp.blobChans = make(map[string]blobChannel)
	}
	return tp.blobChans
}

// blobVarsByChannel returns the channel-name → Go-var map.
func (tp *transpiler) blobVarsByChannel() map[string]string {
	if tp.blobChannelVars == nil {
		tp.blobChannelVars = make(map[string]string)
	}
	return tp.blobChannelVars
}

// newBlobChannel allocates a fresh blob channel name, declares its Go
// *frigolite.Blob variable, and returns the channel name (a string the TCL
// code stores in a variable). Channel variables are pre-declared in the test
// preamble so they remain visible across do_test body blocks.
func (tp *transpiler) newBlobChannel() string {
	// Find the first unused channel name (never reuse a channel that was
	// already allocated — the package-level genBlobUsedChannels records every
	// allocation and persists across all body-block copies).
	if genBlobUsedChannels == nil {
		genBlobUsedChannels = make(map[string]bool)
	}
	for i := 1; i <= 64; i++ {
		name := fmt.Sprintf("incrblob_%d", i)
		if !genBlobUsedChannels[name] {
			genBlobUsedChannels[name] = true
			goName := name
			if !tp.isVarDeclared(goName) {
				tp.vars = append(tp.vars, goName)
				tp.emitLine("var %s *frigolite.Blob", goName)
				tp.emitLine("_ = %s", goName)
			}
			tp.blobVarsByChannel()[name] = goName
			return name
		}
	}
	// Fallback beyond 64 channels (never expected in practice).
	tp.blobSeq++
	name := fmt.Sprintf("incrblob_%d", tp.blobSeq)
	goName := name
	if !tp.isVarDeclared(goName) {
		tp.vars = append(tp.vars, goName)
		tp.emitLine("var %s *frigolite.Blob", goName)
		tp.emitLine("_ = %s", goName)
	}
	tp.blobVarsByChannel()[name] = goName
	genBlobUsedChannels[name] = true
	return name
}

// registerBlobVar records that a TCL variable (e.g. "blob") holds the blob
// channel name channelName. Later `read $blob` / `seek $blob` / `close $blob`
// resolve through this mapping to the channel's Go variable.
func (tp *transpiler) registerBlobVar(varName, channelName string) {
	if genBlobVarNames == nil {
		genBlobVarNames = make(map[string]bool)
	}
	genBlobVarNames[varName] = true
	if goName, ok := tp.blobVarsByChannel()[channelName]; ok {
		tp.blobChannels()[varName] = blobChannel{goName: goName}
	}
}

// isBlobVarName reports whether a TCL variable name was ever used as a blob
// channel (from registerBlobVar), independent of the current map state.
func (tp *transpiler) isBlobVarName(name string) bool {
	return genBlobVarNames != nil && genBlobVarNames[name]
}

// resolveBlobChannel resolves a blob-channel reference ($blob, $::blob, or a
// bare channel name) to the Go variable holding the *frigolite.Blob, or ""
// when the channel is not a known blob channel.
func (tp *transpiler) resolveBlobChannel(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	text = strings.TrimPrefix(text, "$")
	// Normalize the TCL namespace prefix: $::blob and ::blob map to blob.
	plain := strings.TrimPrefix(text, "::")
	if ch, ok := tp.blobChannels()[plain]; ok {
		return ch.goName
	}
	if goName, ok := tp.blobVarsByChannel()[plain]; ok {
		return goName
	}
	if ch, ok := tp.blobChannels()[text]; ok {
		return ch.goName
	}
	if goName, ok := tp.blobVarsByChannel()[text]; ok {
		return goName
	}
	return ""
}

// processBlobRead handles `read $blob [n]` on an incremental-blob channel.
func (tp *transpiler) processBlobRead(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	goName := tp.resolveBlobChannel(args[0])
	if goName == "" {
		tp.emitLine("// read %s (not a blob channel)", args[0].Text)
		return
	}
	tp.blobCursorDecl(goName)
	if len(args) >= 2 {
		n := tp.valueExpr(args[1])
		tp.emitLine("_r = string(blobReadN(%s, %s, %s))", goName, tp.blobCursorName(goName), n)
	} else {
		tp.emitLine("_r = string(blobReadAll(%s, %s))", goName, tp.blobCursorName(goName))
	}
}

// processBlobSeek handles `seek $blob offset [end]`.
func (tp *transpiler) processBlobSeek(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	goName := tp.resolveBlobChannel(args[0])
	if goName == "" {
		tp.emitLine("// seek %s (not a blob channel)", args[0].Text)
		return
	}
	tp.blobCursorDecl(goName)
	offset := tp.intValueExpr(args[1])
	// seek $blob N end — seek relative to the end of the blob.
	if len(args) >= 3 && args[2].Text == "end" {
		tp.emitLine("%s = blobSeekEnd(%s, %s)", tp.blobCursorName(goName), goName, offset)
		return
	}
	tp.emitLine("%s = blobSeek(%s, %s)", tp.blobCursorName(goName), goName, offset)
}

// processBlobPuts handles `puts -nonewline $blob STR` (write STR at the
// current cursor position, advancing it).
func (tp *transpiler) processBlobPuts(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	nonewline := false
	i := 0
	if args[0].Text == "-nonewline" {
		nonewline = true
		i = 1
	}
	if len(args) <= i {
		return
	}
	goName := tp.resolveBlobChannel(args[i])
	if goName == "" {
		tp.emitLine("// puts %s (not a blob channel)", args[i].Text)
		return
	}
	tp.blobCursorDecl(goName)
	dataExpr := tp.sqlStringValue(args[i+1])
	// puts -nonewline $blob [read $fd2] — the data is a file-channel read
	// whose value is the file contents.
	if cmd := strings.TrimSpace(args[i+1].Text); strings.HasPrefix(cmd, "[read $") && strings.HasSuffix(cmd, "]") {
		chanVar := tclVarToGo(strings.TrimSuffix(strings.TrimPrefix(cmd, "[read $"), "]"))
		if isValidGoIdent(chanVar) && tp.isVarDeclared(chanVar) {
			dataExpr = "tclReadFile(" + chanVar + ")"
		}
	}
	tp.emitLine("%s = blobPuts(%s, %s, []byte(%s), %v)", tp.blobCursorName(goName), goName, tp.blobCursorName(goName), dataExpr, nonewline)
}

// processBlobFlush handles `flush $blob` (a no-op: writes are immediate).
//
//lint:ignore U1000 retained for blob compatibility
func (tp *transpiler) processBlobFlush(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	goName := tp.resolveBlobChannel(args[0])
	if goName == "" {
		tp.emitLine("// flush %s (not a blob channel)", args[0].Text)
		return
	}
	// no-op: blob writes are immediate.
}

// processBlobCloseChan handles `close $blob` on an incremental-blob channel.
//
//lint:ignore U1000 retained for blob compatibility
func (tp *transpiler) processBlobCloseChan(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	goName := tp.resolveBlobChannel(args[0])
	if goName == "" {
		tp.emitLine("// close %s (not a blob channel)", args[0].Text)
		return
	}
	tp.emitLine("%s.Close()", goName)
}

// blobCursorName returns the Go variable name holding a blob channel's byte
// cursor (seek position).
func (tp *transpiler) blobCursorName(goName string) string {
	return goName + "_pos"
}

// blobCursorDecl declares the cursor variable for a blob channel if not
// already declared. Called when a seek/puts/read-with-offset uses the cursor.
func (tp *transpiler) blobCursorDecl(goName string) {
	cursor := tp.blobCursorName(goName)
	if !tp.isVarDeclared(cursor) {
		tp.vars = append(tp.vars, cursor)
		tp.emitLine("var %s = 0", cursor)
		tp.emitLine("_ = %s // suppress unused warning", cursor)
	}
}

// valueExpr renders a RawWord as a Go string expression (literal or $var).
func (tp *transpiler) valueExpr(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	if text == "" {
		return "0"
	}
	if _, err := strconv.ParseInt(text, 10, 64); err == nil {
		return text
	}
	// A $var argument resolves to the Go variable when it is declared
	// (e.g. tkt2332.test: sqlite3_blob_open ... $::iKey -> iKey); an
	// undeclared variable keeps the literal-name fallback.
	if strings.HasPrefix(text, "$") {
		goName := tclVarToGo(strings.TrimPrefix(text, "$"))
		if isValidGoIdent(goName) && tp.isVarDeclared(goName) {
			return goName
		}
		return fmt.Sprintf("%q", strings.TrimPrefix(text, "$"))
	}
	return fmt.Sprintf("%q", text)
}

// sqlStringValue renders a RawWord as a Go string expression suitable for
// passing to []byte(...).
func (tp *transpiler) sqlStringValue(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	text = strings.TrimPrefix(text, "$")
	if false {
		goName := tclVarToGo(strings.TrimPrefix(text, "$"))
		if isValidGoIdent(goName) && tp.isVarDeclared(goName) {
			return goName
		}
	}
	return tp.goStringLiteral(w)
}

// intValueExpr renders a RawWord as a Go int expression. It handles integer
// literals, $var references, and `[expr ...]` constant arithmetic.
func (tp *transpiler) intValueExpr(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	if n, err := strconv.Atoi(text); err == nil {
		return strconv.Itoa(n)
	}
	text = strings.TrimPrefix(text, "$")
	if false {
		goName := tclVarToGo(strings.TrimPrefix(text, "$"))
		if isValidGoIdent(goName) && tp.isVarDeclared(goName) {
			return "tclBlobInt(" + goName + ")"
		}
		return "0"
	}
	if strings.HasPrefix(text, "[expr ") && strings.HasSuffix(text, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(text, "[expr "), "]")
		inner = strings.TrimSpace(strings.Trim(inner, "{}"))
		// Try constant evaluation at transpile time (e.g. "5 + 3").
		if res, err := tcl.EvalExpr(inner, nil, nil); err == nil {
			if n, perr := strconv.Atoi(res); perr == nil {
				return strconv.Itoa(n)
			}
		}
		// Fall back to the generic runtime expression.
		exprVarNames, exprGo := tclExprToGo(inner, tp.vars)
		if len(exprVarNames) == 0 {
			return "tclBlobInt(" + fmt.Sprintf("%q", inner) + ")"
		}
		var parts []string
		for _, name := range exprVarNames {
			parts = append(parts, fmt.Sprintf("%q: %s", name, tp.exprVarValue(name)))
		}
		return "tclBlobInt(tclExprWith(" + fmt.Sprintf("%q", exprGo) + ", map[string]string{" + strings.Join(parts, ", ") + "}))"
	}
	return "tclBlobInt(" + fmt.Sprintf("%q", text) + ")"
}
