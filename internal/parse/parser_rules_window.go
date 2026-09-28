// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Code in this file is ORGANIZED BY GRAMMAR FUNCTION, one file per family;
// rule numbers are go-lemon table indices (see parser_ruleids.go).
//
// Window grammar: window definitions, window_clause, frame specs
// (RANGE/ROWS/GROUPS, bounds, EXCLUDE), FILTER and OVER clauses.

package parse

import (
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 318: windowdefn_list ::= windowdefn_list COMMA windowdefn
func ruleWindowdefnListWindowdefnListCommaWindowdefn(ruleNo int, p *Parser) interface{} {
	// The LALR tables reduce a single windowdefn to a *sql.WindowDef here
	// (rule 410's list shape is not used); accept both the list and the
	// single-definition shapes.
	var list []sql.WindowDef
	if l := getWindowDefList(getRHS(p, ruleNo, 1)); l != nil {
		list = l
	} else if wd := getWindowDef(getRHS(p, ruleNo, 1)); wd != nil {
		list = []sql.WindowDef{*wd}
	}
	wd := getWindowDef(getRHS(p, ruleNo, 3))
	if wd != nil {
		list = append(list, *wd)
	}
	return list

}

// Rule 319: windowdefn ::= nm AS LP window RP
func ruleWindowdefnNmAsLpWindowRp(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	inner := getWindowDef(getRHS(p, ruleNo, 4))
	if inner != nil {
		inner.Name = name
		return inner
	}
	// The LALR tables fold a frame-only window body (LP frame_opt RP) into a
	// *sql.WindowFrame value at RHS 4 (rule 411's shape is not used for the
	// parenthesized form). Wrap it in a WindowDef with the frame.
	if f := getWindowFrame(getRHS(p, ruleNo, 4)); f != nil {
		return &sql.WindowDef{Name: name, FrameSpec: frameFromStruct(f), Frame: f}
	}
	return &sql.WindowDef{Name: name}

}

// Rule 320: window ::= PARTITION BY nexprlist orderby_opt frame_opt
func ruleWindowPartitionByNexprlistOrderbyOptFrameOpt(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		Partitions: getExprList(getRHS(p, ruleNo, 3)),
		OrderBy:    getOrderByList(getRHS(p, ruleNo, 4)),
		FrameSpec:  getFrameOptSpec(getRHS(p, ruleNo, 5)),
		Frame:      getFrameOptFrame(getRHS(p, ruleNo, 5)),
	}

}

// Rule 321: window ::= nm PARTITION BY nexprlist orderby_opt frame_opt
func ruleWindowNmPartitionByNexprlistOrderbyOptFrameOpt(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		BaseName:   getString(getRHS(p, ruleNo, 1)),
		Partitions: getExprList(getRHS(p, ruleNo, 4)),
		OrderBy:    getOrderByList(getRHS(p, ruleNo, 5)),
		FrameSpec:  getFrameOptSpec(getRHS(p, ruleNo, 6)),
		Frame:      getFrameOptFrame(getRHS(p, ruleNo, 6)),
	}

}

// Rule 322: window ::= ORDER BY sortlist frame_opt
func ruleWindowOrderBySortlistFrameOpt(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		OrderBy:   getOrderByList(getRHS(p, ruleNo, 3)),
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 4)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 4)),
	}

}

// Rule 323: window ::= nm ORDER BY sortlist frame_opt
func ruleWindowNmOrderBySortlistFrameOpt(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{
		BaseName:  getString(getRHS(p, ruleNo, 1)),
		OrderBy:   getOrderByList(getRHS(p, ruleNo, 4)),
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 5)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 5)),
	}

}

// Rule 324: window ::= nm frame_opt
func ruleWindowNmFrameOpt(ruleNo int, p *Parser) interface{} {
	// window ::= nm frame_opt — a bare window-name reference (optionally with
	// an added frame). The nm is the referenced base window.
	return &sql.WindowDef{
		BaseName:  getString(getRHS(p, ruleNo, 1)),
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 2)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 2)),
	}

}

// Rule 325: frame_opt ::=
func ruleFrameOpt(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 326: frame_opt ::= range_or_rows frame_bound_s frame_exclude_opt
func ruleFrameOptRangeOrRowsFrameBoundSFrameExcludeOpt(ruleNo int, p *Parser) interface{} {
	bound := getFrameBound(getRHS(p, ruleNo, 2))
	if bound == nil {
		return frameSpecFromParts(
			getString(getRHS(p, ruleNo, 1)),
			getString(getRHS(p, ruleNo, 2)),
			getString(getRHS(p, ruleNo, 3)),
		)
	}
	// Shorthand form: "ROWS 2 PRECEDING" == "ROWS BETWEEN 2 PRECEDING AND CURRENT ROW".
	// SQLite validates the shorthand boundary in the engine.
	frame := &sql.WindowFrame{
		Type:  getString(getRHS(p, ruleNo, 1)),
		Start: *bound,
	}
	if excl := getString(getRHS(p, ruleNo, 3)); excl != "" {
		frame.Exclude = excl
	}
	return frame
}

// Rule 327: frame_opt ::= range_or_rows BETWEEN frame_bound_s AND frame_bound_e frame_exclude_opt
func ruleFrameOptBetween(ruleNo int, p *Parser) interface{} {
	spec := frameSpecFromParts(
		getString(getRHS(p, ruleNo, 1)),
		"BETWEEN",
		getString(getRHS(p, ruleNo, 3)),
		"AND",
		getString(getRHS(p, ruleNo, 5)),
	)
	frame := &sql.WindowFrame{
		Type:    getString(getRHS(p, ruleNo, 1)),
		Between: true,
	}
	if b := getFrameBound(getRHS(p, ruleNo, 3)); b != nil {
		frame.Start = *b
	}
	if b := getFrameBound(getRHS(p, ruleNo, 5)); b != nil {
		frame.End = *b
	}
	if excl := getString(getRHS(p, ruleNo, 6)); excl != "" {
		frame.Exclude = excl
		spec += " EXCLUDE " + excl
	}
	return frame
}

// Rule 328: range_or_rows ::= RANGE|ROWS|GROUPS
func ruleRangeOrRowsRangeRowsGroups(ruleNo int, p *Parser) interface{} {
	// SQLite's grammar selects the frame type by TOKEN TYPE (TK_ROWS etc.),
	// which is case-insensitive; normalize the keyword text so downstream
	// switch statements see the canonical uppercase form.
	return strings.ToUpper(getString(getRHS(p, ruleNo, 1)))

}

// Rule 329: frame_bound_s ::= frame_bound
func ruleFrameBoundSFrameBound(ruleNo int, p *Parser) interface{} {
	return getFrameBound(getRHS(p, ruleNo, 1))

}

// Rule 330: frame_bound_s ::= UNBOUNDED PRECEDING
func ruleFrameBoundSUnboundedPreceding(ruleNo int, p *Parser) interface{} {
	return &sql.FrameBound{Kind: "UNBOUNDED PRECEDING"}

}

// Rule 331: frame_bound_e ::= frame_bound
func ruleFrameBoundEFrameBound(ruleNo int, p *Parser) interface{} {
	return getFrameBound(getRHS(p, ruleNo, 1))

}

// Rule 332: frame_bound_e ::= UNBOUNDED FOLLOWING
func ruleFrameBoundEUnboundedFollowing(ruleNo int, p *Parser) interface{} {
	return &sql.FrameBound{Kind: "UNBOUNDED FOLLOWING"}

}

// Rule 333: frame_bound ::= expr PRECEDING|FOLLOWING
func ruleFrameBoundExprPrecedingFollowing(ruleNo int, p *Parser) interface{} {
	expr := getExpr(getRHS(p, ruleNo, 1))
	// PRECEDING/FOLLOWING are keyword tokens (case-insensitive in SQLite's
	// grammar); normalize the direction text to the canonical form.
	dir := strings.ToUpper(getString(getRHS(p, ruleNo, 2)))
	return &sql.FrameBound{Kind: dir, Expr: expr}

}

// Rule 334: frame_bound ::= CURRENT ROW
func ruleFrameBoundCurrentRow(ruleNo int, p *Parser) interface{} {
	return &sql.FrameBound{Kind: "CURRENT ROW"}

}

// Rule 335: frame_exclude_opt ::=
func ruleFrameExcludeOpt(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 336: frame_exclude_opt ::= EXCLUDE frame_exclude
func ruleFrameExcludeOptExcludeFrameExclude(ruleNo int, p *Parser) interface{} {
	// Return the bare exclude value; the caller adds the "EXCLUDE " prefix
	// when building the frame spec text.
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 337: frame_exclude ::= NO OTHERS
func ruleFrameExcludeNoOthers(ruleNo int, p *Parser) interface{} {
	return "NO OTHERS"

}

// Rule 338: frame_exclude ::= CURRENT ROW
func ruleFrameExcludeCurrentRow(ruleNo int, p *Parser) interface{} {
	return "CURRENT ROW"

}

// Rule 339: frame_exclude ::= GROUP|TIES
func ruleFrameExcludeGroupTies(ruleNo int, p *Parser) interface{} {
	// Keyword tokens are case-insensitive; normalize (see rule 328).
	return strings.ToUpper(getString(getRHS(p, ruleNo, 1)))

}

// Rule 340: window_clause ::= WINDOW windowdefn_list
func ruleWindowClause(ruleNo int, p *Parser) interface{} {
	// window_clause ::= WINDOW windowdefn_list. The LALR tables reduce a
	// single windowdefn to a *sql.WindowDef here (the list shape is only used
	// when there are two or more, rule 318); accept both shapes.
	if list := getWindowDefList(getRHS(p, ruleNo, 2)); list != nil {
		return list
	}
	if wd := getWindowDef(getRHS(p, ruleNo, 2)); wd != nil {
		return []sql.WindowDef{*wd}
	}
	return nil

}

// Rule 341: filter_over ::= filter_clause over_clause
func ruleFilterOverFilterClauseOverClause(ruleNo int, p *Parser) interface{} {
	return &windowFilter{
		filter: getExpr(getRHS(p, ruleNo, 1)),
		over:   getWindowDef(getRHS(p, ruleNo, 2)),
	}

}

// Rule 342: filter_over ::= over_clause
func ruleFilterOverOverClause(ruleNo int, p *Parser) interface{} {
	return &windowFilter{
		over: getWindowDef(getRHS(p, ruleNo, 1)),
	}

}

// Rule 343: filter_over ::= filter_clause
func ruleFilterOverFilterClause(ruleNo int, p *Parser) interface{} {
	return &windowFilter{
		filter: getExpr(getRHS(p, ruleNo, 1)),
	}

}

// Rule 344: over_clause ::= OVER LP window RP
func ruleOverClauseOverLpWindowRp(ruleNo int, p *Parser) interface{} {
	// The LALR tables fold the empty window (OVER ()) into
	// "OVER LP frame_opt RP": rh3 is then a frame-spec string
	// rather than a *sql.WindowDef (rule 411 never reduces for the
	// empty case). Accept both shapes.
	if wd := getWindowDef(getRHS(p, ruleNo, 3)); wd != nil {
		return wd
	}
	return &sql.WindowDef{
		FrameSpec: getFrameOptSpec(getRHS(p, ruleNo, 3)),
		Frame:     getFrameOptFrame(getRHS(p, ruleNo, 3)),
	}

}

// Rule 345: over_clause ::= OVER nm
func ruleOverClauseOverNm(ruleNo int, p *Parser) interface{} {
	return &sql.WindowDef{Name: getString(getRHS(p, ruleNo, 2))}

}

// Rule 346: filter_clause ::= FILTER LP WHERE expr RP
func ruleFilterClauseFilterLpWhereExprRp(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 4))

}

// Rule 410: windowdefn_list ::= windowdefn
func ruleWindowdefnListWindowdefn(ruleNo int, p *Parser) interface{} {
	wd := getWindowDef(getRHS(p, ruleNo, 1))
	if wd != nil {
		return []sql.WindowDef{*wd}
	}
	return nil

}

// Rule 411: window ::= frame_opt
func ruleWindowFrameOpt(ruleNo int, p *Parser) interface{} {
	// window ::= frame_opt — a bare frame spec with no PARTITION BY / ORDER BY.
	// The frame_opt value may be a *sql.WindowFrame (structured) or a string
	// (empty frame_opt, rule 325).
	if f := getWindowFrame(getRHS(p, ruleNo, 1)); f != nil {
		return &sql.WindowDef{FrameSpec: frameFromStruct(f), Frame: f}
	}
	return &sql.WindowDef{FrameSpec: getString(getRHS(p, ruleNo, 1))}

}
