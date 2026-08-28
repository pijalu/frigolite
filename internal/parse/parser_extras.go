// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar. This replaces the hand-written
// recursive-descent parser in internal/sql/parser.go.

package parse

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

func recoverFuncCallOrderBy(stmts []sql.Stmt) {
	for _, stmt := range stmts {
		var sel *sql.SelectStmt
		switch s := stmt.(type) {
		case *sql.SelectStmt:
			sel = s
		case *sql.InsertStmt:
			sel = s.Select
		case *sql.CreateViewStmt:
			sel = s.Select
			// The inner SELECT does not carry RawSQL (only the CREATE VIEW
			// wrapper does); propagate it so the fixup can scan the body.
			if sel != nil && sel.RawSQL == "" {
				sel.RawSQL = s.RawSQL
			}
		}
		if sel == nil || sel.RawSQL == "" {
			continue
		}
		recoverSelectFuncCallOrderBy(sel)
	}
}

// extractSavepointStatements replaces SAVEPOINT / RELEASE / ROLLBACK TO
// savepoint statements in input with comment placeholders, returning the
// rebuilt input, the extracted SavepointStmt nodes, and a parallel bool slice
// marking which positions were savepoint placeholders.

func recoverSelectFuncCallOrderBy(sel *sql.SelectStmt) {
	if sel == nil || sel.RawSQL == "" {
		return
	}
	raw := sel.RawSQL
	// Extract every function-call ORDER BY region from the raw SQL in
	// source order, keyed by the function name so matching FuncCalls can be
	// assigned in traversal order.
	byName := collectFuncCallOrderBy(raw)
	recoverSelectFuncCallOrderByAssigned(sel, byName)
}

// recoverSelectFuncCallOrderByAssigned walks a SELECT tree, assigning each
// FuncCall the next unused ORDER BY recovered for its name (in source order).
func recoverSelectFuncCallOrderByAssigned(sel *sql.SelectStmt, byName map[string][][]sql.OrderByTerm) {
	if sel == nil {
		return
	}
	for i := range sel.Columns {
		recoverExprFuncCallOrderByAssigned(sel.Columns[i].Expr, byName)
	}
	if sel.Where != nil {
		recoverExprFuncCallOrderByAssigned(sel.Where, byName)
	}
	if sel.Having != nil {
		recoverExprFuncCallOrderByAssigned(sel.Having, byName)
	}
}

// recoverExprFuncCallOrderByAssigned walks an expression, assigning each
// FuncCall the next recovered ORDER BY for its name and recursing into
// subqueries.
// recoverFuncCallOrderByAssigned assigns the next recovered ORDER BY for a
// function call (in source order) and recurses into its arguments.
func recoverFuncCallOrderByAssigned(v *sql.FuncCall, byName map[string][][]sql.OrderByTerm) {
	if len(v.OrderBy) == 0 {
		key := strings.ToUpper(v.Name)
		if lst := byName[key]; len(lst) > 0 {
			v.OrderBy = lst[0]
			byName[key] = lst[1:]
		}
	}
	for _, a := range v.Args {
		recoverExprFuncCallOrderByAssigned(a, byName)
	}
}

// recoverInListFuncCallOrderByAssigned recurses into an IN-list expression.
func recoverInListFuncCallOrderByAssigned(v *sql.InList, byName map[string][][]sql.OrderByTerm) {
	recoverExprFuncCallOrderByAssigned(v.Operand, byName)
	for _, item := range v.List {
		recoverExprFuncCallOrderByAssigned(item, byName)
	}
}

// recoverCaseFuncCallOrderByAssigned recurses into a CASE expression.
func recoverCaseFuncCallOrderByAssigned(v *sql.CaseExpr, byName map[string][][]sql.OrderByTerm) {
	recoverExprFuncCallOrderByAssigned(v.Operand, byName)
	for _, w := range v.Whens {
		recoverExprFuncCallOrderByAssigned(w.When, byName)
		recoverExprFuncCallOrderByAssigned(w.Then, byName)
	}
	recoverExprFuncCallOrderByAssigned(v.Else, byName)
}

// recoverExprFuncCallOrderByAssigned walks an expression, assigning each
// FuncCall the next recovered ORDER BY for its name and recursing into
// subqueries.
func recoverExprFuncCallOrderByAssigned(expr sql.Expr, byName map[string][][]sql.OrderByTerm) {
	switch v := expr.(type) {
	case *sql.FuncCall:
		recoverFuncCallOrderByAssigned(v, byName)
	case *sql.Subquery:
		recoverSelectFuncCallOrderByAssigned(v.Select, byName)
	case *sql.ExistsExpr:
		recoverSelectFuncCallOrderByAssigned(v.Select, byName)
	case *sql.BinaryOp:
		recoverExprFuncCallOrderByAssigned(v.Left, byName)
		recoverExprFuncCallOrderByAssigned(v.Right, byName)
	case *sql.UnaryOp:
		recoverExprFuncCallOrderByAssigned(v.Operand, byName)
	case *sql.IsNull:
		recoverExprFuncCallOrderByAssigned(v.Operand, byName)
	case *sql.IsNotNull:
		recoverExprFuncCallOrderByAssigned(v.Operand, byName)
	case *sql.Between:
		recoverExprFuncCallOrderByAssigned(v.Operand, byName)
		recoverExprFuncCallOrderByAssigned(v.Low, byName)
		recoverExprFuncCallOrderByAssigned(v.High, byName)
	case *sql.InList:
		recoverInListFuncCallOrderByAssigned(v, byName)
	case *sql.CaseExpr:
		recoverCaseFuncCallOrderByAssigned(v, byName)
	}
}

// nextFuncCallParen scans raw (starting at i) for an identifier immediately
// followed by '(' and returns the index of the '(' plus the upper-cased
// identifier name. When no function-call-like match exists, returns -1.
// SQL keywords that can be followed by '(' (SELECT, WHERE, etc.) are skipped
// so "SELECT (...)" is not mistaken for a function call.
func nextFuncCallParen(raw, upper string, i int) (parenIdx int, name string) {
	if !isIdentStart(upper[i]) {
		return -1, ""
	}
	j := i
	for j < len(raw) && isIdentChar(upper[j]) {
		j++
	}
	name = upper[i:j]
	k := j
	for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
		k++
	}
	if k >= len(raw) || raw[k] != '(' {
		return j, ""
	}
	if isSQLKeywordFollowedByParen(name) {
		return k, ""
	}
	return k, name
}

// balancedParenEnd returns the index of the ')' matching the '(' at openIdx,
// or -1 if unbalanced.
func balancedParenEnd(raw string, openIdx int) int {
	depth := 0
	for m := openIdx; m < len(raw); m++ {
		if raw[m] == '(' {
			depth++
		} else if raw[m] == ')' {
			depth--
			if depth == 0 {
				return m
			}
		}
	}
	return -1
}

// funcCallOrderByTerms extracts the ORDER BY sortlist from a function-call
// argument text, returning nil when there is none.
func funcCallOrderByTerms(inner string) []sql.OrderByTerm {
	obIdx := strings.Index(strings.ToUpper(inner), "ORDER BY")
	if obIdx < 0 {
		return nil
	}
	obText := strings.TrimSpace(inner[obIdx+len("ORDER BY"):])
	if obText == "" {
		return nil
	}
	return parseSortlistText(obText)
}

// collectFuncCallOrderBy scans raw SQL for every "funcname( ... ORDER BY
// sortlist )" call and returns, keyed by upper-cased function name, the
// recovered sortlists in source order.
func collectFuncCallOrderBy(raw string) map[string][][]sql.OrderByTerm {
	result := make(map[string][][]sql.OrderByTerm)
	upper := strings.ToUpper(raw)
	for i := 0; i < len(raw); i++ {
		k, name := nextFuncCallParen(raw, upper, i)
		if k < 0 {
			continue
		}
		if name == "" {
			// No call here; advance past the identifier/keyword scan.
			i = k
			continue
		}
		end := balancedParenEnd(raw, k)
		if end < 0 {
			i = k
			continue
		}
		inner := raw[k+1 : end]
		if terms := funcCallOrderByTerms(inner); len(terms) > 0 {
			result[name] = append(result[name], terms)
		}
		i = end
	}
	return result
}

// isSQLKeywordFollowedByParen reports whether a keyword token can legally be
// followed by '(' in a way that is NOT a function call (SELECT, WHERE,
// HAVING, IN, etc.). Such keywords are skipped by the function-call scanner.
func isSQLKeywordFollowedByParen(name string) bool {
	switch name {
	case "SELECT", "WHERE", "HAVING", "IN", "EXISTS", "NOT", "AND", "OR", "FROM", "GROUP", "ORDER", "BY", "CASE", "WHEN", "THEN", "ELSE", "END", "VALUES", "WITH", "AS", "ON", "JOIN", "LEFT", "RIGHT", "FULL", "INNER", "CROSS", "UNION", "ALL", "INTERSECT", "EXCEPT", "LIMIT", "OFFSET", "DISTINCT", "COLLATE", "ASC", "DESC", "NULLS", "FIRST", "LAST", "IS", "BETWEEN", "LIKE", "GLOB", "MATCH", "REGEXP", "ESCAPE", "CAST", "OVER", "FILTER", "PRECEDING", "FOLLOWING", "CURRENT", "ROW", "RANGE", "UNBOUNDED", "PARTITION", "WINDOW", "RETURNING":
		return true
	}
	return false
}

// isIdentStart reports whether c can start a SQL identifier.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isIdentChar reports whether c can appear in a SQL identifier.
func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// parseSortlistText parses an ORDER BY sortlist text by re-parsing
// "SELECT 1 ORDER BY sortlist" and returning the OrderByTerms.
func parseSortlistText(text string) []sql.OrderByTerm {
	stmts, err := ParseSQL("SELECT 1 ORDER BY " + text)
	if err != nil || len(stmts) == 0 {
		return nil
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		return sel.OrderBy
	}
	return nil
}

// extractFuncCallOrderBy finds "funcname( ... ORDER BY sortlist )" in raw SQL
// and parses the sortlist into OrderByTerms by re-parsing "SELECT sortlist".
// Returns nil if no ORDER BY is present inside the call.
// stmtOrderLimit records the ORDER BY/LIMIT clause stripped from a top-level
// UPDATE or DELETE statement so it can be re-attached after LALR parsing. The
// clause index matches the statement's position in the parsed statement list.
type stmtOrderLimit struct {
	orderBy []sql.OrderByTerm
	limit   sql.Expr
	offset  sql.Expr
}

// rewriteStmtOrderLimit scans the input for top-level UPDATE or DELETE
// statements that carry a trailing ORDER BY/LIMIT clause (a SQLite extension
// not accepted by the LALR tables), strips the clause text, and returns the
// rewritten SQL plus the extracted clauses (indexed by statement order). It
// returns hasRewrite=false when no statement needs rewriting.
//
// SQLite allows ORDER BY/LIMIT combined with RETURNING on UPDATE and DELETE
// (e.g. DELETE FROM t1 RETURNING x ORDER BY x LIMIT 5), so such statements
// must also be rewritten: the LALR where_opt_ret rules (155-158) accept
// RETURNING selcollist but not a trailing ORDER BY/LIMIT.
//
// The scan operates on token boundaries with parenthesis tracking so that
// ORDER BY/LIMIT inside subqueries (e.g. SET x=(SELECT ... ORDER BY ... LIMIT
// 1)) is not mistaken for the statement-level clause.
func rewriteStmtOrderLimit(input string) (string, []stmtOrderLimit, bool) {
	toks, ok := tokenizeInput(input)
	if !ok {
		return input, nil, false
	}
	spans := splitTopLevelStatements(toks)

	// Find the statement-level ORDER BY/LIMIT clause of each top-level
	// UPDATE/DELETE statement (with or without RETURNING).
	var clauseStarts []int // token index where the clause begins, per statement
	for _, sp := range spans {
		if !(isTopLevelStmt(toks, sp, "UPDATE") || isTopLevelStmt(toks, sp, "DELETE")) {
			continue
		}
		if start, ok := findStatementOrderLimit(toks, sp); ok {
			clauseStarts = append(clauseStarts, start)
		}
	}
	if len(clauseStarts) == 0 {
		return input, nil, false
	}

	// Convert each clause start to a byte splice and parse the clause text.
	var splices []stmtSplice
	for _, tokStart := range clauseStarts {
		clauseText, from, to := clauseTextForToken(toks, input, tokStart)
		ob, limit, offset := parseOrderLimitClause(clauseText)
		if ob == nil && limit == nil {
			continue
		}
		splices = append(splices, stmtSplice{
			from:   from,
			to:     to,
			clause: stmtOrderLimit{orderBy: ob, limit: limit, offset: offset},
		})
	}
	if len(splices) == 0 {
		return input, nil, false
	}

	// The clause order in splices matches statement order; the parsed
	// statements will be in the same order (the rewritten input preserves
	// statement boundaries), so build the return slice in statement order.
	var result []stmtOrderLimit
	for _, s := range splices {
		result = append(result, s.clause)
	}
	return applyStmtSplices(input, splices), result, true
}

// stmtSplice records a byte range of the input to remove and the clause it
// contained.
type stmtSplice struct {
	from, to int // byte offsets
	clause   stmtOrderLimit
}

// tokenizeInput tokenizes the input, returning false on a tokenizer error.
func tokenizeInput(input string) ([]sql.Token, bool) {
	tok := sql.NewTokenizer(input)
	var toks []sql.Token
	for {
		t := tok.Next()
		if t.Type == sql.TokenEOF {
			return toks, true
		}
		if t.Type == sql.TokenError || t.Type == sql.TokenUnrecognized {
			return nil, false
		}
		toks = append(toks, t)
	}
}

// stmtSpan is a half-open token range [start, end) for one top-level statement.
type stmtSpan struct {
	start, end int
}

// splitTopLevelStatements splits the token stream into statements on SEMI at
// parenthesis depth 0.
func splitTopLevelStatements(toks []sql.Token) []stmtSpan {
	var spans []stmtSpan
	depth := 0
	start := 0
	for i, t := range toks {
		switch {
		case t.Type == sql.TokenLParen:
			depth++
		case t.Type == sql.TokenRParen:
			if depth > 0 {
				depth--
			}
		case t.Type == sql.TokenSemicolon && depth == 0:
			spans = append(spans, stmtSpan{start: start, end: i})
			start = i + 1
		}
	}
	if start < len(toks) {
		spans = append(spans, stmtSpan{start: start, end: len(toks)})
	}
	return spans
}

// isTopLevelStmt reports whether the statement span starts with the given
// keyword (optionally after WITH, which SQLite does not allow on UPDATE or
// DELETE — the check is conservative so a WITH ... statement is simply not
// rewritten).
func isTopLevelStmt(toks []sql.Token, sp stmtSpan, keyword string) bool {
	d := 0
	for j := sp.start; j < sp.end; j++ {
		t := toks[j]
		switch {
		case t.Type == sql.TokenLParen:
			d++
		case t.Type == sql.TokenRParen:
			if d > 0 {
				d--
			}
		case d == 0 && t.Type == sql.TokenKeyword:
			if strings.EqualFold(t.Value, keyword) {
				return true
			}
			// A WITH prefix (WITH name AS (...) name2 AS (...) ...) is skipped:
			// WITH, AS, and COMMA separators between CTE definitions are all
			// depth-0 keywords that precede the DML keyword. Anything else
			// before the target keyword means this is not a matching statement.
			if !strings.EqualFold(t.Value, "WITH") &&
				!strings.EqualFold(t.Value, "AS") &&
				!strings.EqualFold(t.Value, "RECURSIVE") &&
				!strings.EqualFold(t.Value, ",") {
				return false
			}
		}
	}
	return false
}

// lastOrderLimitIdx scans a statement span and returns the last top-level
// ORDER BY token index and the last top-level LIMIT token index.
func lastOrderLimitIdx(toks []sql.Token, sp stmtSpan) (lastOB, lastLIMIT int) {
	lastOB, lastLIMIT = -1, -1
	d := 0
	for j := sp.start; j < sp.end; j++ {
		switch {
		case toks[j].Type == sql.TokenLParen:
			d++
		case toks[j].Type == sql.TokenRParen:
			if d > 0 {
				d--
			}
		case toks[j].Type == sql.TokenKeyword && d == 0:
			kw := strings.ToUpper(toks[j].Value)
			if kw == "ORDER" && j+1 < sp.end &&
				toks[j+1].Type == sql.TokenKeyword && strings.EqualFold(toks[j+1].Value, "BY") {
				lastOB = j
			}
			if kw == "LIMIT" {
				lastLIMIT = j
			}
		}
	}
	return lastOB, lastLIMIT
}

// findStatementOrderLimit finds the token index where the statement-level
// ORDER BY/LIMIT clause begins (the LAST top-level ORDER BY, or the last
// top-level LIMIT when no ORDER BY is present). SQLite requires LIMIT when
// ORDER BY is used on UPDATE or DELETE ("ORDER BY without LIMIT on UPDATE/DELETE"), so an ORDER
// BY without a following LIMIT is not rewritten. Returns ok=false when the
// statement has no usable clause.
func findStatementOrderLimit(toks []sql.Token, sp stmtSpan) (int, bool) {
	lastOB, lastLIMIT := lastOrderLimitIdx(toks, sp)
	// SQLite only allows ORDER BY/LIMIT AFTER the RETURNING clause on
	// UPDATE/DELETE (DELETE FROM t RETURNING x ORDER BY x). An ORDER BY/LIMIT
	// BEFORE RETURNING is a syntax error ("near RETURNING: syntax error"); do
	// not rewrite it so the parser rejects the invalid statement.
	if hasReturningAfter(toks, sp, lastOB, lastLIMIT) {
		return 0, false
	}
	switch {
	case lastOB >= 0:
		if lastLIMIT > lastOB {
			return lastOB, true
		}
	case lastLIMIT >= 0:
		return lastLIMIT, true
	}
	return 0, false
}

// hasReturningAfter reports whether a top-level RETURNING token appears after
// the given ORDER BY / LIMIT positions (an invalid clause order).
func hasReturningAfter(toks []sql.Token, sp stmtSpan, lastOB, lastLIMIT int) bool {
	clausePos := lastOB
	if lastLIMIT > clausePos {
		clausePos = lastLIMIT
	}
	if clausePos < 0 {
		return false
	}
	d := 0
	for j := sp.start; j < sp.end; j++ {
		switch {
		case toks[j].Type == sql.TokenLParen:
			d++
		case toks[j].Type == sql.TokenRParen:
			if d > 0 {
				d--
			}
		case toks[j].Type == sql.TokenKeyword && d == 0 && j > clausePos:
			if strings.EqualFold(toks[j].Value, "RETURNING") {
				return true
			}
		}
	}
	return false
}

// clauseTextForToken returns the clause text, its starting byte offset, and
// the byte offset of the statement's terminating SEMI (or end of input).
func clauseTextForToken(toks []sql.Token, input string, tokStart int) (string, int, int) {
	from := toks[tokStart].Pos
	to := len(input)
	for i := tokStart; i < len(toks); i++ {
		if toks[i].Type == sql.TokenSemicolon {
			to = toks[i].Pos
			break
		}
	}
	return strings.TrimSpace(input[from:to]), from, to
}

// applyStmtSplices rebuilds the input with each clause's text replaced by a
// single space (so adjacent tokens do not merge).
func applyStmtSplices(input string, splices []stmtSplice) string {
	var out strings.Builder
	out.WriteString(input[:splices[0].from])
	for i, s := range splices {
		if i > 0 {
			out.WriteString(input[splices[i-1].to:s.from])
		}
		// Leave a space where the clause was so tokens do not merge.
		out.WriteString(" ")
	}
	out.WriteString(input[splices[len(splices)-1].to:])
	return out.String()
}

// parseOrderLimitClause parses a trailing "ORDER BY sortlist LIMIT n [OFFSET m]"
// (or "LIMIT n [OFFSET m]") clause text into OrderByTerms and limit/offset
// expressions. Returns nil orderBy and nil limit when the text is empty or
// unparseable.
func parseOrderLimitClause(text string) ([]sql.OrderByTerm, sql.Expr, sql.Expr) {
	upper := strings.ToUpper(text)
	if strings.HasPrefix(upper, "ORDER BY") {
		rest := strings.TrimSpace(text[len("ORDER BY"):])
		// Split off a trailing LIMIT clause.
		limitText := ""
		if li := strings.Index(upper[len("ORDER BY"):], "LIMIT"); li >= 0 {
			abs := len("ORDER BY") + li
			limitText = strings.TrimSpace(text[abs:])
			rest = strings.TrimSpace(text[len("ORDER BY"):abs])
		}
		ob := parseOrderBySortlist(rest)
		var lim, off sql.Expr
		if limitText != "" {
			lim, off = parseLimitClause(limitText)
		}
		return ob, lim, off
	}
	if strings.HasPrefix(upper, "LIMIT") {
		lim, off := parseLimitClause(text)
		return nil, lim, off
	}
	return nil, nil, nil
}

// parseOrderBySortlist parses an ORDER BY sortlist into OrderByTerms by
// re-parsing it as a SELECT. Returns nil on any parse failure.
func parseOrderBySortlist(sortlist string) []sql.OrderByTerm {
	if strings.TrimSpace(sortlist) == "" {
		return nil
	}
	sub, err := ParseSQL("SELECT 1 ORDER BY " + sortlist)
	if err != nil || len(sub) == 0 {
		return nil
	}
	subSel, ok := sub[0].(*sql.SelectStmt)
	if !ok || len(subSel.OrderBy) == 0 {
		return nil
	}
	return subSel.OrderBy
}

// parseLimitClause parses "LIMIT n [OFFSET m]" or "LIMIT n, m" into limit and
// offset expressions. Returns nil expressions on parse failure.
func parseLimitClause(text string) (sql.Expr, sql.Expr) {
	// Reuse the LALR SELECT limit grammar via a wrapper statement.
	sub, err := ParseSQL("SELECT 1 " + text)
	if err != nil || len(sub) == 0 {
		return nil, nil
	}
	subSel, ok := sub[0].(*sql.SelectStmt)
	if !ok {
		return nil, nil
	}
	return subSel.Limit, subSel.Offset
}

// attachStmtOrderLimit applies the ORDER BY/LIMIT clauses stripped by
// rewriteStmtOrderLimit to the parsed statements in order.
func attachStmtOrderLimit(stmts []sql.Stmt, clauses []stmtOrderLimit) {
	ci := 0
	for _, st := range stmts {
		switch upd := st.(type) {
		case *sql.UpdateStmt:
			if ci >= len(clauses) {
				break
			}
			upd.OrderBy = clauses[ci].orderBy
			upd.Limit = clauses[ci].limit
			upd.Offset = clauses[ci].offset
			ci++
		case *sql.DeleteStmt:
			if ci >= len(clauses) {
				break
			}
			upd.OrderBy = clauses[ci].orderBy
			upd.Limit = clauses[ci].limit
			upd.Offset = clauses[ci].offset
			ci++
		}
	}
}

// (optionally with a leading + or - sign), e.g. "0x1A" or "-0xFF".
func isHexLiteral(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	return i+2 <= len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') && i+2 < len(s)
}

// getRHS returns the Nth RHS symbol value (1-indexed) for the current rule.
// In C lemon convention: yymsp[-(N-n)] where N = RHS count.
// In Go stack: stack[pos - size + n] where size = -RuleInfoNRhs[ruleNo].
