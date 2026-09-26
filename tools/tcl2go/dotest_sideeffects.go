// SPDX-License-Identifier: GPL-3.0-or-later
package main

// Skipped-do_test side-effect emission: a skipped test still replays the
// SQL and file-manipulation statements later tests observe (extracted from
// dotest.go to keep both files under the 1000-line quality-gate limit).

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// skipSideEffect is one ordered side effect of a skipped do_test body: a
// SQL execution on a connection, or a pure file operation later tests rely on.
type skipSideEffect struct {
	kind    string // "sql", "close", "remove", "mkdir", "copy", "reopen"
	connVar string
	sqlExpr string
	goArgs  []string
}

func (tp *transpiler) emitSkippedDoTestSideEffects(name, reason string, args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	bodyCmds := tp.parseBracedBody(args, 1)

	// Walk the body once, in order: SQL-running commands (the DDL/DML side
	// effects) AND file-manipulation commands (delete/mkdir/copy layout for
	// later tests). File ordering matters — a later do_test opens a database
	// file that an earlier skipped test had to place (misc7-23.1's
	// forcecopy test.db tst/test.db before sqlite3 db tst/test.db).
	var effects []skipSideEffect
	for _, cmd := range bodyCmds {
		if connVar, sqlExpr, ok := tp.sqlSideEffectCmd(cmd); ok {
			effects = append(effects, skipSideEffect{kind: "sql", connVar: connVar, sqlExpr: sqlExpr})
			continue
		}
		if eff, ok := tp.fileSideEffectCmd(cmd); ok {
			effects = append(effects, eff)
			continue
		}
		// `sqlite3 db FILE` inside a skipped body re-binds the main
		// connection to FILE (pragma-3.19: `forcedelete test.db; sqlite3 db
		// test.db` — replaying the forcedelete without the reopen leaves db
		// writing to an unlinked inode and the following do_test fails with
		// "attempt to write a readonly database"). Skipped tests whose N-A
		// subject IS the reopen lifecycle (vtab1-1.1x's echo
		// reopen-unregister family: C unregisters a test module on reopen,
		// which the engine cannot model) opt out — replaying the reopen
		// there re-activates the very seam the skip adjudicated. Only the
		// pre-declared main connection is handled; secondary handles keep
		// their old conservative no-op.
		if eff, ok := tp.reopenSideEffectCmd(cmd, reason); ok {
			effects = append(effects, eff)
		}
	}
	if len(effects) == 0 {
		return false
	}
	// A reopen closes the connection itself; a preceding `db close` effect
	// for the same connection would emit a redundant (and confusing) second
	// Close.
	effects = dropCloseBeforeReopen(effects)
	hasSQL := false
	for _, eff := range effects {
		if eff.kind == "sql" {
			hasSQL = true
			break
		}
	}
	nameExpr := tp.goStringLiteral(tcl.RawWord{Text: name})
	label := "(file side effects only)"
	if hasSQL {
		label = "(SQL + file side effects only)"
	}
	tp.emitLine("{ // %s — skipped: %s %s", nameExpr, reason, label)
	tp.indent++
	for _, eff := range effects {
		tp.emitSkipEffect(eff)
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// dropCloseBeforeReopen removes `close` effects immediately superseded by a
// later `reopen` of the same connection (the reopen emits its own Close).
func dropCloseBeforeReopen(effects []skipSideEffect) []skipSideEffect {
	reopened := make(map[string]bool)
	for _, eff := range effects {
		if eff.kind == "reopen" {
			reopened[eff.connVar] = true
		}
	}
	if len(reopened) == 0 {
		return effects
	}
	out := make([]skipSideEffect, 0, len(effects))
	for _, eff := range effects {
		if eff.kind == "close" && reopened[eff.connVar] {
			continue
		}
		out = append(out, eff)
	}
	return out
}

// emitSkipEffect emits one side-effect statement of a skipped do_test body.
func (tp *transpiler) emitSkipEffect(eff skipSideEffect) {
	switch eff.kind {
	case "sql":
		tp.emitLine("_res = %s.Exec(%s)", eff.connVar, eff.sqlExpr)
		tp.emitLine("_ = _res.Error // tolerate unsupported-feature errors in skipped tests")
	case "close":
		tp.emitLine("%s.Close()", eff.connVar)
	case "reopen":
		tp.emitLine("%s.Close()", eff.connVar)
		tp.emitLine("%s, err = frigolite.Open(%s)", eff.connVar, eff.goArgs[0])
		tp.emitLine("if err != nil { t.Fatal(err) }")
		tp.emitLine("tclConnRegister(%q, %s)", eff.connVar, eff.connVar)
	case "remove":
		tp.emitLine("os.RemoveAll(%s)", eff.goArgs[0])
	case "mkdir":
		tp.emitLine("os.MkdirAll(%s, 0755)", eff.goArgs[0])
	case "copy":
		tp.emitLine("tclFileCopy(%s, %s)", eff.goArgs[0], eff.goArgs[1])
	}
}

// fileSideEffectCmd classifies one skipped-test body command as a pure file
// manipulation the later tests depend on: `dbN close`, `forcedelete PATH`,
// `file delete [-force] PATH`, `file mkdir PATH`, `file copy|forcecopy SRC
// DST`. Permission changes (`file attributes P -permissions M`) are NOT
// emitted: enforcing them is precisely the VFS capability under N/A, and a
// real chmod would break the engine's subsequent opens.
func (tp *transpiler) fileSideEffectCmd(cmd []tcl.RawWord) (skipSideEffect, bool) {
	kind, words := classifyFileCmd(cmd)
	if kind == "" {
		return skipSideEffect{}, false
	}
	if kind == "close" {
		return skipSideEffect{kind: "close", connVar: cmd[0].Text}, true
	}
	args, ok := tp.literalGoWords(words)
	if !ok {
		return skipSideEffect{}, false
	}
	switch kind {
	case "remove":
		if len(args) == 0 {
			return skipSideEffect{}, false
		}
		return skipSideEffect{kind: "remove", goArgs: args}, true
	case "mkdir":
		return skipSideEffect{kind: "mkdir", goArgs: args}, true
	case "copy":
		if len(args) > 0 && args[0] == `"-force"` {
			args = args[1:]
		}
		if len(args) < 2 {
			return skipSideEffect{}, false
		}
		return skipSideEffect{kind: "copy", goArgs: args[:2]}, true
	}
	return skipSideEffect{}, false
}

// reopenSideEffectCmd classifies a `sqlite3 db FILE` body command as a
// main-connection re-bind the later tests depend on. Only the main "db"
// connection is handled (db1..db9 reopen shapes differ in declaration
// status); the filename must be a string literal. Skips whose reason names
// the reopen-unregister lifecycle as the N-A subject do not replay it.
func (tp *transpiler) reopenSideEffectCmd(cmd []tcl.RawWord, reason string) (skipSideEffect, bool) {
	if strings.Contains(reason, "reopen-unregister") {
		return skipSideEffect{}, false
	}
	if len(cmd) < 3 || cmd[0].Text != "sqlite3" || cmd[1].Text != "db" {
		return skipSideEffect{}, false
	}
	filename := strings.TrimSpace(cmd[2].Text)
	if strings.HasPrefix(filename, "$") || strings.HasPrefix(filename, "[") {
		return skipSideEffect{}, false
	}
	return skipSideEffect{kind: "reopen", connVar: "db", goArgs: []string{tp.goStringLiteral(cmd[2])}}, true
}

// classifyFileCmd maps a body command to its side-effect kind and the
// argument words that carry the paths (empty for `db close`).
func classifyFileCmd(cmd []tcl.RawWord) (string, []tcl.RawWord) {
	switch {
	case len(cmd) == 2 && strings.HasPrefix(cmd[0].Text, "db") && cmd[1].Text == "close":
		return "close", nil
	case cmd[0].Text == "forcedelete" && len(cmd) >= 2:
		return "remove", cmd[1:]
	case cmd[0].Text == "file" && len(cmd) >= 3:
		return classifyFileSubCmd(cmd[1].Text, len(cmd)-2, cmd[2:])
	case cmd[0].Text == "forcecopy" && len(cmd) >= 3:
		return "copy", cmd[1:]
	}
	return "", nil
}

// classifyFileSubCmd classifies the `file SUB ...` family: only delete,
// mkdir and copy carry side effects the later tests depend on.
func classifyFileSubCmd(sub string, nargs int, words []tcl.RawWord) (string, []tcl.RawWord) {
	switch {
	case sub == "delete" && nargs >= 1, sub == "mkdir" && nargs >= 1:
		return sub, words
	case sub == "copy" && nargs >= 2:
		return "copy", words
	}
	return "", nil
}

// literalGoWords converts argument words to Go string literals; false when a
// word is not a literal path (a variable or command substitution).
func (tp *transpiler) literalGoWords(words []tcl.RawWord) ([]string, bool) {
	out := make([]string, 0, len(words))
	for _, w := range words {
		if strings.HasPrefix(w.Text, "$") || strings.HasPrefix(w.Text, "[") {
			return nil, false
		}
		out = append(out, tp.goStringLiteral(w))
	}
	return out, true
}

// sqlSideEffectCmd classifies one body command as a SQL side effect,
// reporting the connection variable and the SQL expression. Recognizes
// `dbN eval {SQL}` and plain `execsql {SQL}` (resolved through the
// alias/connection map, so `execsql {SQL} db2` lands on db2).
func (tp *transpiler) sqlSideEffectCmd(cmd []tcl.RawWord) (connVar, sqlExpr string, ok bool) {
	isDBEval := len(cmd) >= 3 && strings.HasPrefix(cmd[0].Text, "db") && cmd[1].Text == "eval"
	isExecsql := len(cmd) >= 2 && cmd[0].Text == "execsql"
	if !isDBEval && !isExecsql {
		return "", "", false
	}
	sqlIdx := 2
	connVar = cmd[0].Text
	if isExecsql {
		sqlIdx = 1
		connVar = tp.dbVar
		if conn := tp.resolveSQLConnection(cmd); conn != tp.dbVar {
			connVar = conn
		}
	}
	if len(cmd) <= sqlIdx {
		return "", "", false
	}
	return connVar, tp.collectSQLExpression(cmd[sqlIdx : sqlIdx+1]), true
}
