// Package main implements the tcl2go tool.
//
// This file holds the foreach loop emitters: the `$list break` destructuring
// unpack, the generic single/multi-variable loop bodies, and the
// "array get" map-range loop.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// emitBreakUnpack handles `foreach {v1 v2 ...} $list break` — unpack the first
// list element into the variables and exit immediately.
func (tp *transpiler) emitBreakUnpack(args []tcl.RawWord, varNames []string, listExpr string) bool {
	if len(args) < 3 || args[2].Braced || strings.TrimSpace(args[2].Text) != "break" || len(varNames) <= 1 {
		return false
	}
	itemsVar := fmt.Sprintf("_items%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclSplitList(%s)", itemsVar, listExpr)
	tp.emitLine("if len(%s) >= %d {", itemsVar, len(varNames))
	tp.indent++
	for i, vn := range varNames {
		goVN := tclVarToGo(vn)
		if !tp.isVarDeclared(goVN) && !isPreDeclaredDB(goVN) && goVN != tp.dbVar {
			tp.emitLine("var %s string", goVN)
			tp.vars = append(tp.vars, goVN)
		}
		tp.emitLine("%s = %s[%d]", goVN, itemsVar, i)
		tp.emitLine("_ = %s // suppress unused warning", goVN)
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitForeachLoop emits the generic foreach loop (single-var range or
// multi-var index unpack) with the body transpiled in a fresh sub-transpiler.
func (tp *transpiler) emitForeachLoop(args []tcl.RawWord, varNames []string, listExpr, splitExpr string, bodyCmds [][]tcl.RawWord) {
	if len(varNames) == 1 {
		tp.emitSingleVarForeach(varNames[0], listExpr, splitExpr)
	} else {
		tp.emitMultiVarForeach(varNames, listExpr)
	}
	_ = listExpr // suppress unused warning if body is empty

	tp.indent++
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		// A foreach loop has no increment clause: continue targets this loop,
		// so the innermost entry is empty (plain Go continue).
		forIncrs:   append(tp.forIncrs, nil),
		testPrefix: tp.testPrefix, preparedState: tp.preparedState,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		collateGoFuncs:      tp.collateGoFuncs,
		collateEmittedProcs: tp.collateEmittedProcs,
		procBodies:          tp.procBodies,
		collateDtorVars:     tp.collateDtorVars,
		varConstValues:      tp.varConstValues,
		foreachLitValues:    tp.foreachLitValues,
		varsetLoopVars:      tp.varsetLoopVars,
		dbConnVars:          tp.dbConnVars,
		runtimeConnVars:     tp.runtimeConnVars,
		varRenames:          tp.varRenames,
		blobChans:           tp.blobChans,
		blobChannelVars:     tp.blobChannelVars,
		blobVarNames:        tp.blobVarNames,
		usedChannels:        tp.usedChannels,
		blobSeq:             tp.blobSeq,
		testDir:             tp.testDir,
		genesisPreamble:     tp.genesisPreamble,
		ftsBuildPreamble:    tp.ftsBuildPreamble,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.varConstValues = bodyTP.varConstValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
	if len(bodyTP.blobChans) > 0 {
		tp.blobChans = bodyTP.blobChans
	}
	if len(bodyTP.blobChannelVars) > 0 {
		tp.blobChannelVars = bodyTP.blobChannelVars
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	tp.blobSeq = bodyTP.blobSeq
	tp.indent--
	tp.emitLine("}")
}

// emitArrayGetForeach transpiles `foreach {k v} "array get ARR" {BODY}` — the
// TCL idiom that iterates a dynamic-key array's key/value pairs. The
// transpiler represents such arrays as Go maps (arrayMapVars), so the loop
// becomes a Go map range with k and v bound to the key and value. Returns true
// when the pattern matched and the loop was emitted.
func (tp *transpiler) emitArrayGetForeach(args []tcl.RawWord, varNames []string, rawList string) bool {
	trimmed := strings.TrimSpace(rawList)
	trimmed = strings.TrimPrefix(trimmed, "[")
	trimmed = strings.TrimSuffix(trimmed, "]")
	trimmed = strings.TrimPrefix(trimmed, `"`)
	trimmed = strings.TrimSuffix(trimmed, `"`)
	fields := strings.Fields(trimmed)
	if len(fields) != 3 || fields[0] != "array" || fields[1] != "get" {
		return false
	}
	base := strings.TrimPrefix(fields[2], "::")
	if !isArrayMapBacked(tp, base) {
		return false
	}
	mapVar := tclVarToGo(base) + "Map"
	keyVar := tclVarToGo(varNames[0])
	valVar := tclVarToGo(varNames[1])
	if !isValidGoIdent(keyVar) || !isValidGoIdent(valVar) {
		return false
	}
	tp.emitLine("// foreach {%s} %s", strings.Join(varNames, " "), trimmed)
	tp.emitLine("for %s, %s := range %s {", keyVar, valVar, mapVar)
	tp.indent++
	bodyCmds := tp.parseBracedBody(args, 2)
	if bodyCmds != nil {
		bodyTP := &transpiler{
			sb:            tp.sb,
			indent:        tp.indent,
			dbVar:         tp.dbVar,
			t:             tp.t,
			varCount:      tp.varCount,
			vars:          append(append([]string{}, tp.vars...), keyVar, valVar),
			arrayKeys:     tp.arrayKeys,
			arrayMapVars:  tp.arrayMapVars,
			forIncrs:      append(tp.forIncrs, nil),
			testPrefix:    tp.testPrefix,
			preparedState: tp.preparedState,
			queryFuncs:    tp.queryFuncs,
			specialFuncs:  tp.specialFuncs, procStringMaps: tp.procStringMaps,
			collateGoFuncs:      tp.collateGoFuncs,
			collateEmittedProcs: tp.collateEmittedProcs,
			procBodies:          tp.procBodies,
			collateDtorVars:     tp.collateDtorVars,
			varConstValues:      tp.varConstValues,
			foreachLitValues:    tp.foreachLitValues,
			varsetLoopVars:      tp.varsetLoopVars,
			dbConnVars:          tp.dbConnVars,
			runtimeConnVars:     tp.runtimeConnVars,
			varRenames:          tp.varRenames,
			blobChans:           tp.blobChans,
			blobChannelVars:     tp.blobChannelVars,
			blobVarNames:        tp.blobVarNames,
			usedChannels:        tp.usedChannels,
			blobSeq:             tp.blobSeq,
			testDir:             tp.testDir,
			genesisPreamble:     tp.genesisPreamble,
			ftsBuildPreamble:    tp.ftsBuildPreamble,
		}
		bodyTP.processCommands(bodyCmds)
		tp.varCount = bodyTP.varCount
		tp.indent = bodyTP.indent
		tp.varConstValues = bodyTP.varConstValues
		tp.foreachLitValues = bodyTP.foreachLitValues
		tp.varsetLoopVars = bodyTP.varsetLoopVars
		tp.dbConnVars = bodyTP.dbConnVars
		tp.runtimeConnVars = bodyTP.runtimeConnVars
		tp.varRenames = bodyTP.varRenames
		tp.genesisPreamble = bodyTP.genesisPreamble
		tp.ftsBuildPreamble = bodyTP.ftsBuildPreamble
	}
	tp.indent--
	tp.emitLine("}")
	return true
}
