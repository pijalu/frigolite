// Package main implements the tcl2go tool.
//
// This file transpiles the rtree test-suite fixture procs
// (ori/sqlite/test/rtree8.test and rtreeA.test) whose bodies were previously
// shadowed by the incrblob4 same-named hardcoded emitters.
package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// userProcEmitterFor fingerprints a file-local proc body and returns an
// emitter key for a faithful Go implementation, "" when unsupported.
func userProcEmitterFor(name, body string) string {
	switch name {
	case "populate_t1":
		switch {
		case strings.Contains(body, "DELETE FROM t1") && strings.Contains(body, "$i+2"):
			return "rtree8_populate" // rtree8.test: INSERT ($i,$i,$i+2)
		case strings.Contains(body, "BEGIN") && strings.Contains(body, "500"):
			return "rtreea_populate" // rtreeA.test: 500-row BEGIN/COMMIT fill
		}
	case "create_t1":
		if strings.Contains(body, "CREATE VIRTUAL TABLE t1 USING rtree(") {
			return "rtreea_create" // reopen fresh file + rtree t1
		}
	case "truncate_node":
		if strings.Contains(body, "string range") {
			return "rtreea_truncate"
		}
	}
	return ""
}

// goArgWords renders transpiler call arguments as Go string expressions:
// $var references become their Go variables, bare words become literals.
func goArgWords(args []tcl.RawWord) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		w := strings.TrimSpace(a.Text)
		if strings.HasPrefix(w, "$") && !strings.Contains(w, "(") {
			if gv := tclVarToGo(strings.TrimPrefix(w, "$")); gv != "" {
				out = append(out, gv)
				continue
			}
		}
		out = append(out, strconv.Quote(w))
	}
	return out
}

// emitUserProc emits one faithful fixture-proc implementation at the call
// site. goArgs carries rendered Go expressions for each TCL argument.
func (tp *transpiler) emitUserProc(key string, goArgs []string) {
	argAt := func(i int) string {
		if i < len(goArgs) {
			return goArgs[i]
		}
		return `""`
	}
	switch key {
	case "rtree8_populate":
		tp.emitLine("_res = %s.Exec(\"DELETE FROM t1\")", tp.dbVar)
		tp.emitLine("if _res.Error != nil { t.Errorf(\"exec error: %%v\", _res.Error) }")
		tp.emitLine("for _i := 1; _i <= tclInt(%s); _i++ {", argAt(0))
		tp.indent++
		tp.emitLine("_res = %s.Exec(fmt.Sprintf(\"INSERT INTO t1 VALUES(%%d, %%d, %%d)\", _i, _i, _i+2))", tp.dbVar)
		tp.emitLine("if _res.Error != nil { t.Errorf(\"exec error: %%v\", _res.Error) }")
		tp.indent--
		tp.emitLine("}")

	case "rtreea_create":
		// rtreeA.test create_t1: close, recreate the file at page_size 1024,
		// reopen and build the virtual table. Scoped so multiple invocations
		// don't redeclare scratch variables.
		tp.emitLine("{")
		tp.indent++
		tp.emitLine("%s.Close()", tp.dbVar)
		tp.emitLine("_ = os.Remove(\"test.db\")")
		tp.emitLine("_ndb, _nerr := frigolite.Open(\"test.db\")")
		tp.emitLine("if _nerr != nil { t.Fatal(_nerr) }")
		tp.emitLine("%s = _ndb", tp.dbVar)
		tp.emitLine("_res = %s.Exec(\"PRAGMA page_size=1024; CREATE VIRTUAL TABLE t1 USING rtree(id,x1,x2,y1,y2)\")", tp.dbVar)
		tp.emitLine("if _res.Error != nil {")
		tp.indent++
		tp.emitLine("t.Errorf(\"exec error: %%v\", _res.Error)")
		tp.indent--
		tp.emitLine("}")
		tp.indent--
		tp.emitLine("}")

	case "rtreea_populate":
		tp.emitLine("_res = %s.Exec(\"BEGIN\")", tp.dbVar)
		tp.emitLine("for _i := 0; _i < 500; _i++ {")
		tp.indent++
		tp.emitLine("_res = %s.Exec(fmt.Sprintf(\"INSERT INTO t1 VALUES(%%d, %%d, %%d, %%d, %%d)\", _i, _i, _i+5, _i, _i+5))", tp.dbVar)
		tp.emitLine("if _res.Error != nil { t.Errorf(\"exec error: %%v\", _res.Error) }")
		tp.indent--
		tp.emitLine("}")
		tp.emitLine("_res = %s.Exec(\"COMMIT\")", tp.dbVar)
		tp.emitLine("if _res.Error != nil { t.Errorf(\"exec error: %%v\", _res.Error) }")
		tp.emitLine("%s.SetDefensive(false)", tp.dbVar)

	case "rtreea_truncate":
		// truncate_node $nodeno $nTrunc: blob := that node's data; negative
		// nTrunc drops |n| trailing bytes ("end-N"); non-negative keeps the
		// first nTrunc+1 bytes (TCL string range is inclusive).
		tp.emitLine("{")
		tp.indent++
		tp.emitLine("_nodeno := tclInt(%s)", argAt(0))
		tp.emitLine("_trunc := tclInt(%s)", argAt(1))
		tp.emitLine("_rrows := %s.Query(\"SELECT data FROM t1_node WHERE nodeno=\" + strconv.Itoa(_nodeno)).Rows", tp.dbVar)
		tp.emitLine("if len(_rrows) == 1 {")
		tp.indent++
		tp.emitLine("_blob, _ := _rrows[0][0].([]byte)")
		tp.emitLine("if _trunc < 0 {")
		tp.indent++
		tp.emitLine("_blob = _blob[:len(_blob)+_trunc]")
		tp.indent--
		tp.emitLine("} else if _trunc+1 <= len(_blob) {")
		tp.indent++
		tp.emitLine("_blob = _blob[:_trunc+1]")
		tp.indent--
		tp.emitLine("}")
		tp.emitLine("_hex := make([]byte, 0, 48+len(_blob)*2)")
		tp.emitLine("_hex = append(_hex, []byte(\"UPDATE t1_node SET data=X'\")...)")
		tp.emitLine("const _hx = \"0123456789abcdef\"")
		tp.emitLine("for _, _b := range _blob { _hex = append(_hex, _hx[_b>>4], _hx[_b&0xf]) }")
		tp.emitLine("_hex = append(_hex, `' WHERE nodeno=`...)")
		tp.emitLine("_hex = append(_hex, []byte(strconv.Itoa(_nodeno))...)")
		tp.emitLine("_res = %s.Exec(string(_hex))", tp.dbVar)
		tp.emitLine("if _res.Error != nil { t.Errorf(\"exec error: %%v\", _res.Error) }")
		tp.indent--
		tp.emitLine("}")
		tp.indent--
		tp.emitLine("}")
	}
}

var _ = tcl.ParseCommands // anchored for future body-transpile extensions
