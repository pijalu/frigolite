// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar. This replaces the hand-written
// recursive-descent parser in internal/sql/parser.go.

package parse

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func decodeBlobToken(hexStr string) sql.Expr {
	if len(hexStr) == 0 {
		return &sql.BlobLit{Value: []byte{}}
	}
	// Allow odd-length hex strings by padding with a leading zero.
	if len(hexStr)%2 == 1 {
		hexStr = "0" + hexStr
	}
	data, err := hex.DecodeString(hexStr)
	if err != nil {
		// Graceful fallback for malformed hex: keep the raw content as text.
		return &sql.StringLit{Value: "x" + hexStr}
	}
	return &sql.BlobLit{Value: data}
}

// --- Type-safe accessors for parser stack values ---

type setOpResult struct {
	Op  sql.SetOp
	All bool
}

// triggerDeclInfo carries the parsed CREATE TRIGGER declaration parts
// between the trigger_decl and cmd grammar rules.
type triggerDeclInfo struct {
	name       string
	schema     string
	time       string
	event      string
	table      string
	when       sql.Expr
	ifNotExist bool
}

// alterAddInfo is an intermediate value for the alter_add nonterminal
// (rule 294), carrying the table name, column name, and type token before
// they are combined with carglist constraints in rule 293.
type alterAddInfo struct {
	table string
	name  string
	typ   string
}

func getAlterAddInfo(v interface{}) *alterAddInfo {
	if ai, ok := v.(*alterAddInfo); ok {
		return ai
	}
	return &alterAddInfo{}
}

// mergeColumnDef merges non-zero fields from src into dst.
func mergeColumnDef(dst *sql.ColumnDef, src sql.ColumnDef) {
	if src.Type != "" {
		dst.Type = src.Type
	}
	if src.NotNull {
		dst.NotNull = true
	}
	if src.PrimaryKey {
		dst.PrimaryKey = true
	}
	if src.Unique {
		dst.Unique = true
	}
	if src.Default != nil {
		dst.Default = src.Default
	}
	if src.Check != nil {
		dst.Check = src.Check
	}
	if src.Generated != nil {
		dst.Generated = src.Generated
	}
	if src.References != "" {
		dst.References = src.References
	}
	if src.Collate != "" {
		dst.Collate = src.Collate
	}
	if src.AutoInc {
		dst.AutoInc = true
	}
}

func getString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if tok, ok := v.(sql.Token); ok {
		return tok.Value
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		return getString(util.UnwrapColumnValue(cv))
	}
	if cv, ok := v.(util.ColumnValue); ok {
		return getString(util.UnwrapColumnValue(&cv))
	}
	if nl, ok := v.(*sql.NumericLit); ok {
		return nl.Value
	}
	if sl, ok := v.(*sql.StringLit); ok {
		return sl.Value
	}
	return fmt.Sprintf("%v", v)
}

func getBool(v interface{}) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func getExpr(v interface{}) sql.Expr {
	if v == nil {
		return nil
	}
	if e, ok := v.(sql.Expr); ok {
		return e
	}
	return nil
}

func getStmt(v interface{}) sql.Stmt {
	if v == nil {
		return nil
	}
	if s, ok := v.(sql.Stmt); ok {
		return s
	}
	return nil
}

// getStmtList extracts a []sql.Stmt from a parser stack value (the
// trigger_cmd_list rules produce []sql.Stmt).
func getStmtList(v interface{}) []sql.Stmt {
	if v == nil {
		return nil
	}
	if l, ok := v.([]sql.Stmt); ok {
		return l
	}
	return nil
}

func getSelectStmt(v interface{}) *sql.SelectStmt {
	if v == nil {
		return nil
	}
	if s, ok := v.(*sql.SelectStmt); ok {
		return s
	}
	return nil
}

// checkCompoundSelect mirrors SQLite's parserDoubleLinkSelect semantic check:
// in a compound SELECT (UNION/INTERSECT/EXCEPT), ORDER BY and LIMIT may only
// appear on the final SELECT. If any earlier member has them, report the
// error "X clause should come after Y not before" (matching SQLite's message).
func checkCompoundSelect(p *Parser, sel *sql.SelectStmt) *sql.SelectStmt {
	if sel == nil || p.SemanticErr != nil {
		return sel
	}
	// Walk the chain built by rule 88: members are linked via Union.
	// The last member (Union == nil) may carry ORDER BY / LIMIT.
	members := []*sql.SelectStmt{}
	for cur := sel; cur != nil; cur = cur.Union {
		members = append(members, cur)
	}
	// members[len-1] is the final SELECT — allowed to have ORDER BY/LIMIT.
	// Any earlier member with ORDER BY or LIMIT is an error.
	for i := 0; i < len(members)-1; i++ {
		m := members[i]
		if len(m.OrderBy) > 0 {
			// The operator stored on this member links it to the next
			// member in the compound chain (set by rule 88), so it names
			// the operator that the ORDER BY should have come after.
			p.SemanticErr = fmt.Errorf("%s clause should come after %s not before", "ORDER BY", opNameOf(m))
			return sel
		}
		if m.Limit != nil {
			p.SemanticErr = fmt.Errorf("%s clause should come after %s not before", "LIMIT", opNameOf(m))
			return sel
		}
	}
	return sel
}

// opNameOf returns the SQL keyword for a compound-set operator.
func opNameOf(m *sql.SelectStmt) string {
	switch m.SetOp {
	case sql.SetExcept:
		return "EXCEPT"
	case sql.SetIntersect:
		return "INTERSECT"
	case sql.SetUnion:
		if m.UnionAll {
			return "UNION ALL"
		}
		return "UNION"
	default:
		return "UNION"
	}
}

func getSelectColumns(v interface{}) []sql.SelectColumn {
	if v == nil {
		return nil
	}
	if cols, ok := v.([]sql.SelectColumn); ok {
		return cols
	}
	return nil
}

func getColumnList(v interface{}) []sql.ColumnDef {
	if v == nil {
		return nil
	}
	if cols, ok := v.([]sql.ColumnDef); ok {
		return cols
	}
	return nil
}

func getColumnDef(v interface{}) sql.ColumnDef {
	if v == nil {
		return sql.ColumnDef{}
	}
	if c, ok := v.(sql.ColumnDef); ok {
		return c
	}
	return sql.ColumnDef{}
}

// mergeColumnConstraintFlags ORs the boolean constraint flags from c into dst.
func mergeColumnConstraintFlags(dst, c *sql.ColumnDef) {
	dst.NotNull = dst.NotNull || c.NotNull
	dst.PrimaryKey = dst.PrimaryKey || c.PrimaryKey
	dst.PKDesc = dst.PKDesc || c.PKDesc
	dst.AutoInc = dst.AutoInc || c.AutoInc
	dst.Unique = dst.Unique || c.Unique
}

// mergeColumnConstraintFields copies the non-empty constraint fields from c
// into dst (the last non-empty value wins).
func mergeColumnConstraintFields(dst, c *sql.ColumnDef) {
	if c.Collate != "" {
		dst.Collate = c.Collate
	}
	if c.ConstraintName != "" {
		dst.ConstraintName = c.ConstraintName
	}
	if c.References != "" {
		if strings.HasPrefix(c.References, " ") && dst.References != "" {
			// A defer_subclause marker (leading space) appends to the
			// preceding REFERENCES constraint.
			dst.References += c.References
		} else {
			dst.References = c.References
		}
	}
	if c.OnConflict != "" {
		dst.OnConflict = c.OnConflict
	}
	if c.Default != nil {
		dst.Default = c.Default
	}
	if c.Check != nil {
		dst.Check = c.Check
	}
	if c.Generated != nil {
		dst.Generated = c.Generated
	}
}

// mergeColumnConstraints merges per-column constraints (produced by the
// carglist/ccons rules) into the base column definition.
func mergeColumnConstraints(dst *sql.ColumnDef, cons []sql.ColumnDef) {
	if dst == nil {
		return
	}
	for _, c := range cons {
		mergeColumnConstraintFlags(dst, &c)
		mergeColumnConstraintFields(dst, &c)
	}
}

// rejectNullsInSortlist returns an error if any sortlist term carries an
// explicit NULLS FIRST/LAST clause. SQLite only allows NULLS FIRST/LAST in
// ORDER BY, not in index key, PRIMARY KEY, UNIQUE, or ON CONFLICT definitions
// (error: "unsupported use of NULLS FIRST/LAST").
func rejectNullsInSortlist(terms []sql.OrderByTerm) error {
	for _, t := range terms {
		if t.NullsFirst {
			return fmt.Errorf("unsupported use of NULLS FIRST")
		}
		if t.NullsLast {
			return fmt.Errorf("unsupported use of NULLS LAST")
		}
	}
	return nil
}

// seltablistAcc accumulates the FROM clause during seltablist reductions.
// It carries the first table and the list of joins (comma or explicit).
type seltablistAcc struct {
	First     sql.TableRef
	HasFirst  bool
	Joins     []sql.JoinClause
	PendingOp joinOp // join operator waiting for the next table
}

// joinOp describes a join operator between two tables.
type joinOp struct {
	Kind  string // "LEFT", "RIGHT", "INNER", "CROSS", "NATURAL", ""
	Outer bool   // "LEFT OUTER JOIN" etc.
	Comma bool   // comma join (FROM a, b)
}

// joinTypeOf maps a pending join operator's Kind to the JoinClause JoinType.
// SQLite's joinop nonterminal carries the raw keyword text; this normalizes it.
func joinTypeOf(kind string) string {
	switch kind {
	case "LEFT", "LEFT OUTER":
		return "LEFT"
	case "RIGHT", "RIGHT OUTER":
		return "RIGHT"
	case "FULL", "FULL OUTER":
		return "FULL"
	case "INNER":
		return "INNER"
	case "CROSS":
		return "CROSS"
	case "NATURAL":
		return "NATURAL"
	case "NATURAL LEFT", "NATURAL LEFT OUTER":
		return "NATURAL LEFT"
	case "NATURAL RIGHT", "NATURAL RIGHT OUTER":
		return "NATURAL RIGHT"
	case "NATURAL FULL", "NATURAL FULL OUTER":
		return "NATURAL FULL"
	case "NATURAL INNER":
		return "NATURAL INNER"
	case "NATURAL CROSS":
		return "NATURAL CROSS"
	}
	return "CROSS"
}

// appendTableWithOn adds a table, attaching an optional ON condition and/or
// USING column list to the join clause that links it to the previous table.
func (a *seltablistAcc) appendTableWithOn(ref sql.TableRef, on sql.Expr, using []string) *seltablistAcc {
	if !a.HasFirst {
		a.First = ref
		a.HasFirst = true
		return a
	}
	jc := sql.JoinClause{
		Table:     ref,
		CommaJoin: a.PendingOp.Comma,
		On:        on,
		Using:     using,
		JoinType:  joinTypeOf(a.PendingOp.Kind),
	}
	a.Joins = append(a.Joins, jc)
	a.PendingOp = joinOp{}
	return a
}

// appendJoin appends a pre-built JoinClause (used by parenthesized lists).
func (a *seltablistAcc) appendJoin(j sql.JoinClause) *seltablistAcc {
	a.Joins = append(a.Joins, j)
	return a
}

// firstTable returns the first table ref, or an empty one.
func (a *seltablistAcc) firstTable() sql.TableRef {
	if a == nil || !a.HasFirst {
		return sql.TableRef{}
	}
	return a.First
}

// hasExplicitJoins reports whether the accumulated list contains any non-comma
// JOIN clauses (e.g. (t1 JOIN t2 ON ...)), which must remain inside a derived
// table rather than being flattened into the outer query.
func (a *seltablistAcc) hasExplicitJoins() bool {
	if a == nil {
		return false
	}
	for _, j := range a.Joins {
		if !j.CommaJoin {
			return true
		}
	}
	return false
}

// appendSeltablistTable handles seltablist ::= stl_prefix nm dbnm as ... rules.
// posName/posSchema/posAlias are the 1-based RHS positions of the table name,
// schema (dbnm), and alias. posOn is the position of the on_using value.
func appendSeltablistTable(p *Parser, ruleNo, posName, posSchema, posAlias, posIndexedBy, posOn int) *seltablistAcc {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	tbl := getString(getRHS(p, ruleNo, posName))
	schema := getString(getRHS(p, ruleNo, posSchema))
	alias := getString(getRHS(p, ruleNo, posAlias))
	on, using := getOnUsing(getRHS(p, ruleNo, posOn))
	if schema != "" {
		tbl = tbl + "." + schema
	}
	ref := sql.TableRef{Name: tbl, As: alias}
	if posIndexedBy > 0 {
		if ib, ok := getRHS(p, ruleNo, posIndexedBy).(string); ok && ib != "" && !strings.EqualFold(ib, "NOT INDEXED") {
			ref.IndexedBy = ib
		}
	}
	return acc.appendTableWithOn(ref, on, using)
}

// valuesFromSelect converts a VALUES-select (a SelectStmt with no FROM) into
// a list of value tuples, one per VALUES row. Multi-row VALUES is a compound
// (UNION ALL) chain: each member's columns form one tuple.
func valuesFromSelect(sel *sql.SelectStmt) [][]sql.Expr {
	var values [][]sql.Expr
	for cur := sel; cur != nil; cur = cur.Union {
		if len(cur.Columns) > 0 {
			tuple := make([]sql.Expr, len(cur.Columns))
			for i, col := range cur.Columns {
				tuple[i] = col.Expr
			}
			values = append(values, tuple)
		}
	}
	return values
}

// getOnUsing extracts an ON condition expr and/or a USING column list from an
// on_using value. The value is either an Expr (ON expr) or a []string (USING).
func getOnUsing(v interface{}) (sql.Expr, []string) {
	if e, ok := v.(sql.Expr); ok {
		return e, nil
	}
	if s, ok := v.([]string); ok {
		return nil, s
	}
	return nil, nil
}

// getCTEDefs extracts a []sql.CTEDef from a with-clause value.
func getCTEDefs(v interface{}) []sql.CTEDef {
	if v == nil {
		return nil
	}
	if defs, ok := v.([]sql.CTEDef); ok {
		return defs
	}
	if d, ok := v.(sql.CTEDef); ok {
		return []sql.CTEDef{d}
	}
	return nil
}

// getSeltablist extracts a seltablistAcc from a stack value, creating an empty
// one if the value is a plain TableRef (backward compat for rules that return
// TableRef directly).
// getFromInfo extracts the first table and join list from a `from`
// nonterminal value (a *seltablistAcc). Returns nil for an empty FROM.
func getFromInfo(v interface{}) *fromInfo {
	acc, ok := v.(*seltablistAcc)
	if !ok || acc == nil || !acc.HasFirst {
		return nil
	}
	info := &fromInfo{first: acc.First}
	info.joins = append(info.joins, acc.Joins...)
	return info
}

// fromInfo carries the parsed UPDATE ... FROM tables and their JOIN clauses
// (join type and ON condition preserved so the executor can apply JOIN
// semantics).
type fromInfo struct {
	first sql.TableRef
	joins []sql.JoinClause
}

func getSeltablist(v interface{}) *seltablistAcc {
	switch t := v.(type) {
	case *seltablistAcc:
		return t
	case sql.TableRef:
		return &seltablistAcc{First: t, HasFirst: true}
	default:
		return &seltablistAcc{}
	}
}

// getJoinOp extracts a joinOp from a stack value.
func getJoinOp(v interface{}) joinOp {
	if op, ok := v.(joinOp); ok {
		return op
	}
	return joinOp{}
}

// joinKind maps a JOIN_KW token value to a join type keyword.
func joinKind(v interface{}) string {
	s := getString(v)
	switch strings.ToUpper(s) {
	case "LEFT":
		return "LEFT"
	case "RIGHT":
		return "RIGHT"
	case "FULL":
		return "FULL"
	case "INNER":
		return "INNER"
	case "CROSS":
		return "CROSS"
	case "NATURAL":
		return "NATURAL"
	default:
		return ""
	}
}

// combineNaturalJoin merges a JOIN_KW with an optional nm join-type keyword,
// mirroring SQLite's sqlite3JoinType bitmask OR: "NATURAL LEFT" keeps both
// flags, and "LEFT RIGHT" ORs to FULL (JT_LEFT|JT_RIGHT). The result is a
// normalized join-type string the exec layer understands.
func combineNaturalJoin(kw, nm string) string {
	mask := joinKindMask(kw) | joinKindMask(nm)
	switch {
	case mask&(jtLeft|jtRight) == jtLeft|jtRight:
		// FULL (LEFT|RIGHT), possibly NATURAL
		if mask&jtNatural != 0 {
			return "NATURAL FULL"
		}
		return "FULL"
	case mask&jtLeft != 0:
		if mask&jtNatural != 0 {
			return "NATURAL LEFT"
		}
		return "LEFT"
	case mask&jtRight != 0:
		if mask&jtNatural != 0 {
			return "NATURAL RIGHT"
		}
		return "RIGHT"
	case mask&jtCross != 0:
		if mask&jtNatural != 0 {
			return "NATURAL CROSS"
		}
		return "CROSS"
	case mask&jtInner != 0:
		if mask&jtNatural != 0 {
			return "NATURAL INNER"
		}
		return "INNER"
	default:
		return kw
	}
}

// Join-type bitmask constants mirroring SQLite's JT_* flags.
const (
	jtInner   = 1 << iota // INNER/CROSS base
	jtCross               // CROSS
	jtNatural             // NATURAL
	jtLeft                // LEFT
	jtRight               // RIGHT
)

// joinKindMask maps a join keyword to its bitmask (SQLite's sqlite3JoinType
// aKeyword[] codes: LEFT|OUTER, RIGHT|OUTER, FULL=LEFT|RIGHT|OUTER,
// INNER, CROSS=INNER|CROSS, NATURAL).
func joinKindMask(kw string) int {
	switch kw {
	case "LEFT":
		return jtLeft
	case "RIGHT":
		return jtRight
	case "FULL":
		return jtLeft | jtRight
	case "INNER":
		return jtInner
	case "CROSS":
		return jtInner | jtCross
	case "NATURAL":
		return jtNatural
	default:
		return 0
	}
}

// fromValue extracts the From TableRef and Joins list from a `from` nonterminal
// value. The value is either a TableRef (old path) or a seltablistAcc.
func fromValue(v interface{}) (sql.TableRef, []sql.JoinClause) {
	if acc, ok := v.(*seltablistAcc); ok {
		return acc.First, acc.Joins
	}
	if t, ok := v.(sql.TableRef); ok {
		return t, nil
	}
	return sql.TableRef{}, nil
}

func getExprList(v interface{}) []sql.Expr {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.Expr); ok {
		return list
	}
	return nil
}

// getOrderByList extracts an ORDER BY / ON CONFLICT sortlist value from the
// parser stack.
func getOrderByList(v interface{}) []sql.OrderByTerm {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.OrderByTerm); ok {
		return list
	}
	return nil
}

// nullsOrder records the NULLS FIRST / NULLS LAST clause on an ORDER BY term.
type nullsOrder struct {
	first bool
	last  bool
}

// limitClause carries both the LIMIT and OFFSET expressions from a
// limit_opt reduction (OFFSET may be absent).
type limitClause struct {
	limit  sql.Expr
	offset sql.Expr
}

// getLimitClause extracts a limitClause from a parser stack value, returning
// an empty clause when absent.
func getLimitClause(v interface{}) *limitClause {
	if lc, ok := v.(*limitClause); ok {
		return lc
	}
	return &limitClause{}
}

// getNullsOrder extracts the NULLS FIRST/LAST marker from a parser stack
// value, returning (first, last).
func getNullsOrder(v interface{}) (bool, bool) {
	if no, ok := v.(nullsOrder); ok {
		return no.first, no.last
	}
	return false, false
}

// conflictTargetColumns extracts the conflict target column list from an
// ON CONFLICT (...) sortlist. Returns (column names, raw expression texts).
// A plain column reference contributes its name and leaves the expression
// text empty; any other expression (COLLATE, arithmetic, function call,
// subquery) contributes only its SQL text (used to match expression indexes
// textually, as SQLite does).
func conflictTargetColumns(terms []sql.OrderByTerm) ([]string, []string) {
	var names []string
	var exprs []string
	for _, t := range terms {
		if ref, ok := t.Expr.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
			exprs = append(exprs, "")
			continue
		}
		// Keep the two lists parallel: an expression term contributes an
		// empty name slot so indexKeyTerms/targetKeyTerms can pair them.
		names = append(names, "")
		exprs = append(exprs, exprText(t.Expr))
	}
	return names, exprs
}

// exprText renders an expression back to SQL text for conflict-target
// expression matching (SQLite matches expression index targets textually).
func exprText(e sql.Expr) string {
	return sql.ExprString(e)
}

// windowFilter carries the optional FILTER expression and OVER window
// definition produced by the filter_over grammar nonterminal. A FuncCall
// built from a filter_over rule consumes both fields.
type windowFilter struct {
	filter sql.Expr
	over   *sql.WindowDef
}

// getWindowFilter extracts a *windowFilter from a parser stack value.
func getWindowFilter(v interface{}) *windowFilter {
	if v == nil {
		return nil
	}
	if wf, ok := v.(*windowFilter); ok {
		return wf
	}
	return nil
}

// getWindowDef extracts a *sql.WindowDef from a parser stack value.
func getWindowDef(v interface{}) *sql.WindowDef {
	if v == nil {
		return nil
	}
	if wd, ok := v.(*sql.WindowDef); ok {
		return wd
	}
	return nil
}

// getWindowFrame extracts a *sql.WindowFrame from a parser stack value.
func getWindowFrame(v interface{}) *sql.WindowFrame {
	if v == nil {
		return nil
	}
	if wf, ok := v.(*sql.WindowFrame); ok {
		return wf
	}
	return nil
}

// getFrameBound extracts a *sql.FrameBound from a parser stack value.
