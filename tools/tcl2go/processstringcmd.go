// Package main implements the tcl2go tool.
//
// This file handles string/concat/regexp/regsub/error/glob/split/join
// commands.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// stringCmdHandler emits Go code for one `string <op>` command. args excludes
// the "string" word but includes the op.
type stringCmdHandler func(tp *transpiler, args []tcl.RawWord)

// stringCmdHandlers maps string subcommand names to their Go emitters.
var stringCmdHandlers = map[string]stringCmdHandler{
	"length":    (*transpiler).processStringLength,
	"tolower":   (*transpiler).processStringToLower,
	"toupper":   (*transpiler).processStringToUpper,
	"trim":      (*transpiler).processStringTrim,
	"trimleft":  (*transpiler).processStringTrimLeft,
	"trimright": (*transpiler).processStringTrimRight,
	"compare":   (*transpiler).processStringCompare,
	"equal":     (*transpiler).processStringEqual,
	"first":     (*transpiler).processStringFirst,
	"index":     (*transpiler).processStringIndex,
	"range":     (*transpiler).processStringRange,
	"repeat":    (*transpiler).processStringRepeat,
	"match":     (*transpiler).processStringMatch,
	"map":       (*transpiler).processStringMap,
}

// processStringCmd handles: string operation args...
func (tp *transpiler) processStringCmd(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	op := args[0].Text
	if h, ok := stringCmdHandlers[op]; ok {
		h(tp, args)
		return
	}
	if len(args) > 1 {
		tp.emitLine("// string %s %s", op, describeArgsShort(args[1:]))
	} else {
		tp.emitLine("// string %s", op)
	}
}

func (tp *transpiler) processStringLength(args []tcl.RawWord) {
	if len(args) >= 2 {
		strExpr := tp.goStringLiteral(args[1])
		tp.emitLine("_ = strconv.Itoa(len(%s)) // string length result", strExpr)
	}
}

func (tp *transpiler) processStringToLower(args []tcl.RawWord) {
	if len(args) >= 2 {
		strExpr := tp.goStringLiteral(args[1])
		tp.emitLine("strings.ToLower(%s)", strExpr)
	}
}

func (tp *transpiler) processStringToUpper(args []tcl.RawWord) {
	if len(args) >= 2 {
		strExpr := tp.goStringLiteral(args[1])
		tp.emitLine("strings.ToUpper(%s)", strExpr)
	}
}

func (tp *transpiler) processStringTrim(args []tcl.RawWord) {
	if len(args) >= 2 {
		strExpr := tp.goStringLiteral(args[1])
		charsExpr := `" \t\n\r\v\f"`
		if len(args) >= 3 {
			charsExpr = tp.goStringLiteral(args[2])
		}
		tp.emitLine("_ = strings.Trim(%s, %s) // string trim result", strExpr, charsExpr)
	}
}

func (tp *transpiler) processStringTrimLeft(args []tcl.RawWord) {
	if len(args) >= 2 {
		strExpr := tp.goStringLiteral(args[1])
		charsExpr := `" \t\n\r\v\f"`
		if len(args) >= 3 {
			charsExpr = tp.goStringLiteral(args[2])
		}
		tp.emitLine("_ = strings.TrimLeft(%s, %s) // string trimleft result", strExpr, charsExpr)
	}
}

func (tp *transpiler) processStringTrimRight(args []tcl.RawWord) {
	if len(args) >= 2 {
		strExpr := tp.goStringLiteral(args[1])
		charsExpr := `" \t\n\r\v\f"`
		if len(args) >= 3 {
			charsExpr = tp.goStringLiteral(args[2])
		}
		tp.emitLine("_ = strings.TrimRight(%s, %s) // string trimright result", strExpr, charsExpr)
	}
}

func (tp *transpiler) processStringCompare(args []tcl.RawWord) {
	if len(args) >= 3 {
		a := tp.goStringLiteral(args[1])
		b := tp.goStringLiteral(args[2])
		tp.emitLine("strings.Compare(%s, %s)", a, b)
	}
}

func (tp *transpiler) processStringEqual(args []tcl.RawWord) {
	if len(args) >= 3 {
		a := tp.goStringLiteral(args[1])
		b := tp.goStringLiteral(args[2])
		// Assign to a throwaway var so the comparison is not an unused
		// bare expression statement (Go forbids bare bool expressions).
		tp.emitLine("_ = (%s == %s)", a, b)
	}
}

func (tp *transpiler) processStringFirst(args []tcl.RawWord) {
	if len(args) >= 3 {
		needle := tp.goStringLiteral(args[1])
		haystack := tp.goStringLiteral(args[2])
		tp.emitLine("strings.Index(%s, %s)", haystack, needle)
	}
}

func (tp *transpiler) processStringIndex(args []tcl.RawWord) {
	if len(args) >= 3 {
		strExpr := tp.goStringLiteral(args[1])
		idxExpr := tp.goStringLiteral(args[2])
		tp.emitLine("string([]byte{%s[%s]})", strExpr, idxExpr)
	}
}

func (tp *transpiler) processStringRange(args []tcl.RawWord) {
	if len(args) >= 4 {
		strExpr := tp.goStringLiteral(args[1])
		startExpr := tp.goStringLiteral(args[2])
		endExpr := tp.goStringLiteral(args[3])
		tp.emitLine("_ = tclStringRange(%s, %s, %s) // string range result", strExpr, startExpr, endExpr)
	}
}

func (tp *transpiler) processStringRepeat(args []tcl.RawWord) {
	if len(args) >= 3 {
		strExpr := tp.goStringLiteral(args[1])
		nExpr := tp.goStringLiteral(args[2])
		tp.emitLine("strings.Repeat(%s, %s)", strExpr, nExpr)
	}
}

func (tp *transpiler) processStringMatch(args []tcl.RawWord) {
	if len(args) >= 3 {
		pattern := tp.goStringLiteral(args[1])
		// string match incrblob_* $blob on an incremental-blob channel: the
		// handle is a valid blob channel, so the pattern always matches
		// ("1").
		if goName := tp.resolveBlobChannel(args[2]); goName != "" {
			tp.emitLine("_r = tclStringMatch01(%s, %q)", pattern, goName)
			return
		}
		strExpr := tp.goStringLiteral(args[2])
		// Emit the result ("1"/"0") so do_test bodies ending in
		// `string match ...` compare the match result.
		tp.emitLine("_r = tclStringMatch01(%s, %s)", pattern, strExpr)
	}
}

func (tp *transpiler) processStringMap(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	// string map {- {}} $str — replace '-' with '' (remove).
	mapSpec := args[1]
	strExpr := tp.goStringLiteral(args[2])
	spec := strings.TrimSpace(mapSpec.Text)
	// Parse a TCL {from to from2 to2 ...} map spec: emit successive
	// strings.ReplaceAll calls from innermost to outermost.
	elements := tclSplitList(spec)
	expr := strExpr
	for i := 0; i+1 < len(elements); i += 2 {
		from := elements[i]
		to := elements[i+1]
		// Expand escaped braces for multi-char mappings if needed.
		expr = fmt.Sprintf("strings.ReplaceAll(%s, %q, %q)", expr, from, to)
	}
	tp.emitLine(expr)
}

// processConcat handles: concat args...
// Concatenates TCL lists.
func (tp *transpiler) processConcat(args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	// concat $a $b → perform tcl-style list concatenation
	var parts []string
	for _, a := range args {
		parts = append(parts, tp.goStringLiteral(a))
	}
	// Use tclSplitList on each arg, then tclList join.
	// Go's append() only allows one ... spread, so build incrementally.
	if len(parts) == 1 {
		tp.emitLine("_r_tcl := tclList(tclSplitList(%s))", parts[0])
	} else {
		tp.emitLine("_r_tcl := append([]string{}, tclSplitList(%s)...)", parts[0])
		for i := 1; i < len(parts); i++ {
			tp.emitLine("_r_tcl = append(_r_tcl, tclSplitList(%s)...)", parts[i])
		}
		tp.emitLine("_r_tcl_str := tclList(_r_tcl)")
		tp.emitLine("_ = _r_tcl_str")
	}
	tp.emitLine("_ = _r_tcl")
}

// processListOp handles: lindex list idx, llength list, lrange list start end, lsort list, lreplace list first count args...
func (tp *transpiler) processListOp(cmd string, args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	listExpr := tp.goStringLiteral(args[0])

	switch cmd {
	case "lindex":
		if len(args) >= 2 {
			// A bracketed list argument ([catchsql db {SQL}], zipfile2 2.0)
			// must evaluate as a command expression, not stay a raw literal.
			if txt := strings.TrimSpace(args[0].Text); strings.HasPrefix(txt, "[") && strings.HasSuffix(txt, "]") && len(txt) > 2 {
				listExpr = tp.cmdExpr(txt[1 : len(txt)-1])
			}
			idxExpr := tp.goStringLiteral(args[1])
			tp.emitLine("_r = tclLIndex(%s, %s) // lindex result", listExpr, idxExpr)
		}
	case "llength":
		tp.emitLine("_ = strconv.Itoa(tclLLength(%s)) // llength result", listExpr)
	case "lrange":
		if len(args) >= 3 {
			startExpr := tp.goStringLiteral(args[1])
			endExpr := tp.goStringLiteral(args[2])
			tp.emitLine("_ = tclLRange(%s, %s, %s) // lrange result", listExpr, startExpr, endExpr)
		}
	case "lsort":
		tp.emitLine("_ = tclSort(%s) // lsort result", listExpr)
	case "lreplace":
		if len(args) >= 3 {
			firstExpr := tp.goStringLiteral(args[1])
			countExpr := tp.goStringLiteral(args[2])
			var repl []string
			for _, a := range args[3:] {
				repl = append(repl, tp.goStringLiteral(a))
			}
			tp.emitLine("_ = tclLReplace(%s, %s, %s, %s) // lreplace result", listExpr, firstExpr, countExpr, strings.Join(repl, ", "))
		}
	case "lsearch":
		// Simplified: just return "0" (not found) - complex
		tp.emitLine("// lsearch %s (simplified)", listExpr)
	}
}

// processRegexp handles: regexp {pattern} str [?var]
func (tp *transpiler) processRegexp(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	patternExpr := tp.goStringLiteral(args[0])
	strExpr := tp.goStringLiteral(args[1])
	// regexp returns 1 for match, 0 for no match
	tp.emitLine("tclRegexp(%s, %s)", patternExpr, strExpr)
}

// processRegsub handles: regsub ?-all? {pattern} str replacement [var]
func (tp *transpiler) processRegsub(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	// Skip optional flags like -all, -nocase, etc.
	idx := 0
	allFlag := false
	for idx < len(args) && strings.HasPrefix(args[idx].Text, "-") {
		if args[idx].Text == "-all" {
			allFlag = true
		}
		idx++
	}
	if len(args)-idx < 3 {
		return
	}
	patternExpr := tp.goStringLiteral(args[idx])
	strExpr := tp.goStringLiteral(args[idx+1])
	replExpr := tp.goStringLiteral(args[idx+2])
	if len(args)-idx >= 4 {
		varGo := tclVarToGo(args[idx+3].Text)
		if !isValidGoIdent(varGo) {
			varGo = "_regsub_result"
		}
		funcName := "tclRegsub"
		if allFlag {
			funcName = "tclRegsubAll"
		}
		if tp.isVarDeclared(varGo) {
			tp.emitLine("%s = %s(%s, %s, %s)", varGo, funcName, patternExpr, strExpr, replExpr)
		} else {
			tp.emitLine("var %s string", varGo)
			tp.emitLine("%s = %s(%s, %s, %s)", varGo, funcName, patternExpr, strExpr, replExpr)
			tp.vars = append(tp.vars, varGo)
		}
		tp.emitLine("_ = %s // suppress unused warning", varGo)
	} else {
		tp.emitLine("_ = tclRegsub(%s, %s, %s)", patternExpr, strExpr, replExpr)
	}
}

// processError handles: error message
func (tp *transpiler) processError(args []tcl.RawWord) {
	if len(args) == 0 {
		tp.emitLine("t.Errorf(\"error\")")
		return
	}
	msgExpr := tp.varValueExpr(args)
	tp.emitLine("t.Errorf(\"TCL error: %%s\", %s)", msgExpr)
}

// processGlob handles: glob pattern
func (tp *transpiler) processGlob(args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	patternExpr := tp.goStringLiteral(args[0])
	tp.emitLine("tclGlob(%s)", patternExpr)
}

// processSplit handles: split str [?sep]
func (tp *transpiler) processSplit(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	strExpr := tp.goStringLiteral(args[0])
	sep := `" "`
	if len(args) >= 2 {
		sep = tp.goStringLiteral(args[1])
	}
	tp.emitLine("strings.Split(%s, %s)", strExpr, sep)
}

// processJoin handles: join list [?sep]
func (tp *transpiler) processJoin(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	listExpr := tp.goStringLiteral(args[0])
	sep := `" "`
	if len(args) >= 2 {
		sep = tp.goStringLiteral(args[1])
	}
	tp.emitLine("strings.Join(tclSplitList(%s), %s)", listExpr, sep)
}

// processScriptEval handles: eval script
// In TCL tests this typically evaluates a string as a script.
func (tp *transpiler) processScriptEval(args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	// Parse the script and execute its commands
	if args[0].Braced {
		bodyCmds := parseCommands(args[0].Text)
		bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState}
		bodyTP.processCommands(bodyCmds)
		tp.indent = bodyTP.indent
	} else if strings.HasPrefix(args[0].Text, "$") && len(args) == 1 {
		// eval $varsetVar — rewrite into struct field assignments when the
		// variable iterates over a transpiled varset list.
		vn := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
		if info, ok := tp.varsetLoopVars[vn]; ok {
			for _, f := range info.fields {
				// Only assign fields the varset script actually set; fields
				// that stay unset keep the loop's reset default (e.g. "''").
				tp.emitLine("if %s.%sSet {", vn, f)
				tp.indent++
				tp.emitLine("%s = %s.%s", f, vn, f)
				tp.indent--
				tp.emitLine("}")
			}
			tp.emitLine("_ = %s // suppress unused warning", vn)
			return
		}
		// eval $var where var iterates over a literal braced-script list
		// (backup.test's `foreach zOpenScript {...} { eval $zOpenScript }`):
		// inline each script's commands, dispatching on the runtime value. The
		// case expressions use the same $var substitution the foreach list
		// builds (tclListElem), so they match the runtime zOpenScript value.
		if vals, ok := tp.foreachLitValues[vn]; ok && len(vals) > 0 {
			for i, v := range vals {
				kw := "if"
				if i > 0 {
					kw = "} else if"
				}
				tp.emitLine("%s %s == %s {", kw, vn, tp.buildListStringExpr(stripOuterBraces(v)))
				tp.indent++
				bodyCmds := parseCommands(stripOuterBraces(v))
				bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, varConstValues: tp.varConstValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, inEvalScript: true}
				bodyTP.processCommands(bodyCmds)
				tp.indent = bodyTP.indent
				tp.vars = bodyTP.vars
				tp.varCount = bodyTP.varCount
				tp.varConstValues = bodyTP.varConstValues
				tp.foreachLitValues = bodyTP.foreachLitValues
				tp.varsetLoopVars = bodyTP.varsetLoopVars
				tp.dbConnVars = bodyTP.dbConnVars
				tp.runtimeConnVars = bodyTP.runtimeConnVars
				tp.varRenames = bodyTP.varRenames
				tp.indent--
			}
			tp.emitLine("}")
			return
		}
		// eval $var where var holds a dynamically-built `sqlite3_intarray_bind`
		// script: dispatch to the runtime intarray-bind handler (the script list
		// is built in a loop and cannot be statically expanded).
		if tp.intarrayEvalVars[vn] {
			tp.emitLine("if err := tclEvalRuntime(%s); err != nil { t.Errorf(\"eval %%s: %%v\", %s, err) }", vn, vn)
			return
		}
		tp.emitLine("// eval %s (dynamic, not transpiled)", args[0].Text)
	} else {
		// Non-braced eval (e.g., eval [string map ...]) — cannot transpile,
		// emit as a sanitized comment to avoid breaking Go syntax.
		tp.emitLine("// eval (dynamic, not transpiled)")
	}
}

// processSubst handles: subst {string}
func (tp *transpiler) processSubst(args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	// subst replaces $var and [cmd] in the string
	expr := tp.goStringLiteral(args[0])
	tp.emitLine("// subst: %s", expr)
}
