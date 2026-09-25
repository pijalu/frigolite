// Package main implements the tcl2go tool.
//
// This file transpiles fts5_common.tcl's meta-loop proc foreach_detail_mode,
// which runs its body once per fts5 detail mode after a reset_db. Without it
// the whole body is dropped as an unsupported command, and — worse — the
// per-iteration reset_db is lost, so later sections of the generated test
// collide with tables created before the loop (fts5simple3 4.0 "table t1
// already exists").
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// fts5DetailModes lists the detail modes foreach_detail_mode iterates, in
// fts5_common.tcl order.
var fts5DetailModes = []string{"full", "col", "none"}

// processForEachDetailMode transpiles fts5_common.tcl's foreach_detail_mode:
//
//	foreach_detail_mode PREFIX BODY
//
// TCL semantics (fts5_common.tcl:462): save ::testprefix; for each mode in
// {full col none}: set ::detail MODE, set ::testprefix "PREFIX-MODE",
// reset_db, then eval BODY. At the end ::testprefix is restored.
//
// BODY references %DETAIL% where the mode name must appear (inside SQL
// string literals), so the body is transpiled once per mode with the
// substitution done at transpile time; each copy is guarded by the runtime
// loop variable. detail_is_* predicates resolve at runtime against that
// variable, which keeps TCL `continue` semantics intact (a bare
// `if {[detail_is_none]} continue` inside the body skips to the next mode).
// ::detail itself is not mirrored; bodies read the mode via detail_is_*.
func (tp *transpiler) processForEachDetailMode(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	tp.fdmSeq++
	seq := tp.fdmSeq
	modeVar := fmt.Sprintf("_fdmMode%d", seq)
	prefixVar := fmt.Sprintf("_fdmPrefix%d", seq)

	prefixExpr := tp.buildStringExpr(args[0].Text)
	tp.emitLine("%s := %s // foreach_detail_mode %s", prefixVar, prefixExpr, sanitizeTCLComment(strings.TrimSpace(args[0].Text)))
	tp.emitLine("for _, %s := range []string{%s} {", modeVar, quotedJoin(fts5DetailModes))
	tp.indent++
	tp.emitLine("testprefix = %s + \"-\" + %s", prefixVar, modeVar)
	tp.emitLine("vtab.TclVarSet(\"testprefix\", \"\", testprefix)")
	// TCL resets the database before every iteration.
	tp.processResetDB()
	for _, mode := range fts5DetailModes {
		tp.emitLine("if %s == %q {", modeVar, mode)
		tp.indent++
		bodyText := strings.ReplaceAll(args[1].Text, "%DETAIL%", mode)
		bodyTP := tp.forkBodyTranspiler(tp.testPrefix + "-" + mode)
		bodyTP.processCommands(parseCommands(bodyText))
		tp.syncBodyTranspiler(bodyTP)
		tp.indent--
		tp.emitLine("}")
	}
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("testprefix = %s", prefixVar)
	tp.emitLine("vtab.TclVarSet(\"testprefix\", \"\", testprefix)")
}

// quotedJoin renders each string as a Go quoted literal joined by commas.
func quotedJoin(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

// forkBodyTranspiler returns a sub-transpiler sharing this transpiler's
// output buffer and state, with the given test prefix (foreach_detail_mode
// renames ::testprefix per mode, which also feeds skip-map lookups).
func (tp *transpiler) forkBodyTranspiler(testPrefix string) *transpiler {
	return &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: tp.catchMode, inDBEvalCb: tp.inDBEvalCb, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: testPrefix, preparedState: tp.preparedState, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, dbClosed: tp.dbClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps, currentTestFile: tp.currentTestFile, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, fdmSeq: tp.fdmSeq, dbAliases: tp.dbAliases, unsetVars: tp.unsetVars, queryVars: tp.queryVars, varCount: tp.varCount}
}

// syncBodyTranspiler copies mutable state back from a forked body
// sub-transpiler (same fields runIfBody syncs).
func (tp *transpiler) syncBodyTranspiler(bodyTP *transpiler) {
	tp.indent = bodyTP.indent
	tp.varCount = bodyTP.varCount
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	tp.dbClosed = bodyTP.dbClosed
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
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
	tp.fdmSeq = bodyTP.fdmSeq
}
