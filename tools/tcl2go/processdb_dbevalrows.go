// SPDX-License-Identifier: GPL-3.0-or-later
package main

// The `db eval SQL ARRAYVAR {BODY}` per-row array-capture emitter (split
// from processdb.go for file-size hygiene).

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// emitDBEvalArrayRows handles `db eval SQL ARRAYVAR {BODY}`: each result row
// binds ARRAYVAR(column) to the row's cell values, then BODY runs — once per
// row (fts3sort.test's per-row array capture).
func (tp *transpiler) emitDBEvalArrayRows(arrName string, rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest[:1])
	if sqlExpr == `""` {
		return
	}
	bodyText := strings.TrimSpace(rest[2].Text)
	bodyText = strings.TrimSuffix(strings.TrimPrefix(bodyText, "{"), "}")

	// Collect the ARRAY(key) references the body reads so per-row scalar
	// bindings can be pre-declared.
	keys := dbevalArrayKeys(arrName, bodyText)

	arrStar := tclVarToGo(arrName + "(*)")
	if !tp.isVarDeclared(arrStar) {
		tp.emitLine("var %s string", arrStar)
		tp.vars = append(tp.vars, arrStar)
		tp.emitLine("_ = %s // suppress unused warning", arrStar)
	}
	for _, k := range keys {
		kv := tclVarToGo(arrName + "(" + k + ")")
		if tp.isVarDeclared(kv) || !isValidGoIdent(kv) {
			continue
		}
		tp.emitLine("var %s string", kv)
		tp.vars = append(tp.vars, kv)
		tp.emitLine("_ = %s // suppress unused warning", kv)
	}

	rowsVar := fmt.Sprintf("_dbevalRows%d", tp.varCount)
	tp.varCount++
	flatVar := fmt.Sprintf("_%sFlat%d", arrName, tp.varCount)
	tp.varCount++
	if tp.rowFlatVars == nil {
		tp.rowFlatVars = make(map[string]string)
	}
	tp.rowFlatVars[arrName] = flatVar
	defer func() { delete(tp.rowFlatVars, arrName) }()
	tp.emitLine("%s := db.Query(%s)", rowsVar, sqlExpr)
	tp.emitLine("if %s.Error == nil {", rowsVar)
	tp.indent++
	// Active-read wrapper: the scanned SELECT is a RUN-state VM for the whole
	// callback loop upstream (db->nVdbeRead) — DDL in the body hits the
	// OP_Destroy interlock.
	tp.emitLine("db.BeginActiveStatement()")
	arrStarAssign := tclVarToGo(arrName + "(*)")
	tp.emitLine("%s = strings.Join(%s.Columns, \" \")", arrStarAssign, rowsVar)
	// TCL's db eval sets A(*) to the column list; sync the tclvar registry
	// so a later `set A(*)` reads it even when the read goes through the
	// registry store (with1-17.2).
	tp.emitLine("vtab.TclVarSet(%q, \"*\", %s)", arrName, arrStarAssign)
	tp.emitLine("for _ri := 0; _ri < len(%s.Rows); _ri++ {", rowsVar)
	tp.indent++
	tp.emitLine("%s := tclRowFlatPairs(%s.Columns, %s.Rows[_ri])", flatVar, rowsVar, rowsVar)
	tp.emitLine("_ = %s", flatVar)
	tp.emitDBEvalRowBindings(arrName, keys, rowsVar)
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		forIncrs:     tp.forIncrs,
		testPrefix:   tp.testPrefix,
		queryVars:    tp.queryVars,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		collateGoFuncs:      tp.collateGoFuncs,
		collateEmittedProcs: tp.collateEmittedProcs,
		procBodies:          tp.procBodies,
		preparedState:       tp.preparedState,
		varConstValues:      tp.varConstValues,
		sqlVarValues:        tp.sqlVarValues,
		foreachLitValues:    tp.foreachLitValues,
		rowFlatVars:         tp.rowFlatVars,
	}
	bodyTP.processCommands(parseCommands(bodyText))
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("db.EndActiveStatement()")
	tp.indent--
	tp.emitLine("}")
}

// dbevalArrayKeys collects the distinct ARRAY(key) references a db-eval body
// reads (pre-declared as per-row scalar bindings).
func dbevalArrayKeys(arrName, bodyText string) []string {
	keyRe := regexp.MustCompile(`\$` + regexp.QuoteMeta(arrName) + `\(([A-Za-z0-9_]+)\)`)
	var keys []string
	seen := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(bodyText, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	return keys
}

// emitDBEvalRowBindings emits the per-row switch binding ARRAYVAR(column) to
// each result cell.
func (tp *transpiler) emitDBEvalRowBindings(arrName string, keys []string, rowsVar string) {
	tp.emitLine("for _ci := 0; _ci < len(%s.Columns); _ci++ {", rowsVar)
	tp.indent++
	tp.emitLine("switch %s.Columns[_ci] {", rowsVar)
	tp.indent++
	for _, k := range keys {
		kv := tclVarToGo(arrName + "(" + k + ")")
		if !isValidGoIdent(kv) {
			continue
		}
		tp.emitLine("case %q:", k)
		tp.indent++
		tp.emitLine("%s = tclStr(%s.Rows[_ri][_ci])", kv, rowsVar)
		tp.indent--
	}
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}
