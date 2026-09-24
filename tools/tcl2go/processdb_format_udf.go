package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// Inline-format UDF emission (split from processdb.go for file-size
// hygiene): `db function <name> {format <literal> <arg>}` bodies.

// emitInlineFormatUDF emits a format-based closure for a `db function`
// whose inline TCL body is a single `format <literal-fmt> <arg>` command,
// reporting whether it handled the registration. A nil stub would null out
// every value the test stores through it (collate1-1.x: hex(45) must return
// "0x2D", not NULL).
func (tp *transpiler) emitInlineFormatUDF(name string, rest []tcl.RawWord) bool {
	if len(rest) < 2 {
		return false
	}
	body := strings.TrimSpace(rest[1].Text)
	body = strings.TrimPrefix(body, "{")
	body = strings.TrimSuffix(body, "}")
	cmds := tcl.ParseCommands(strings.TrimSpace(body))
	if len(cmds) != 1 || len(cmds[0]) < 2 || cmds[0][0].Text != "format" {
		return false
	}
	fmtLit := cmds[0][1].Text
	if strings.ContainsAny(fmtLit, "$[") {
		return false
	}
	tp.emitLine("// db function %s {format %s} (TCL format UDF)", name, fmtLit)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) == 0 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn tclFormat(%q, tclStr(args[0])), nil", fmtLit)
	tp.emitLine("}, 1, -1)")
	return true
}
