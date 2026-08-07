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

// ParseSQL parses a SQL string using the go-lemon generated LALR(1) parser.
// Returns a list of statements compatible with Frigolite's AST types.
func ParseSQL(input string) ([]sql.Stmt, error) {
	// The LALR tables are generated from SQLite's grammar, which accepts
	// UPDATE ... ORDER BY ... LIMIT and DELETE ... ORDER BY ... LIMIT (SQLite
	// extensions). The tables used here predate that extension, so rewrite
	// such statements first: strip the trailing ORDER BY/LIMIT, parse the
	// remainder, then re-attach the clause to the resulting statement.
	rewritten, stmtClauses, hasStmtRewrite := rewriteStmtOrderLimit(input)
	if hasStmtRewrite {
		input = rewritten
	}

	// If input is only whitespace or comments, return no statements without error.
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, nil
	}
	if strings.TrimSpace(stripSQLComments(trimmed)) == "" {
		return nil, nil
	}
	// Append trailing semicolon if missing — the LALR grammar requires
	// SEMI as a statement terminator (ecmd ::= cmdx SEMI).
	// Use "\n;" not ";": if the statement ends with a -- line comment,
	// TrimRight above strips the comment's newline, so a bare ";" would
	// be swallowed by the comment (skipLineComment runs to EOF) and the
	// grammar would never see its SEMI terminator.
	input = strings.TrimRight(input, " \t\r\n")
	if input != "" && input[len(input)-1] != ';' {
		input += "\n;"
	}

	// The LALR grammar handles RETURNING clauses (SQLite 3.35+ syntax) with
	// full projection fidelity: INSERT/UPDATE/DELETE RETURNING populates the
	// AST's Returning/HasReturning fields (multi-expression RETURNING folds
	// into a RowValue). No RD fallback is needed for RETURNING.
	tables := GetParseTables()
	parser := NewParser(tables)

	tok := sql.NewTokenizer(input)
	var stmts []sql.Stmt
	var pendingStmt sql.Stmt
	stmtStart := 0 // byte offset where the current statement begins

	// OnReduce callback: handle grammar rule reductions
	parser.OnReduce(func(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) {
		t := p.tables
		top := p.pos
		nrhs := t.RuleInfoNRhs[ruleNo]
		size := -nrhs

		result := handleRule(ruleNo, p, lookahead, lookaheadToken)

		// Default: pass through first RHS value if handler returned nil
		// (Only for non-empty rules - empty rules have no RHS values)
		if result == nil && size > 0 {
			result = getRHS(p, ruleNo, 1)
		}

		// Collect completed statements when the statement-root rules fire
		if s, ok := result.(sql.Stmt); ok {
			pendingStmt = s
			// Collect at ecmd rules (after SEMI is consumed) to handle
			// multi-statement input. Rules 352 (ecmd ::= cmdx SEMI) and
			// 353 (ecmd ::= explain cmdx SEMI) complete a statement.
			if ruleNo == 352 || ruleNo == 353 {
				stmts = append(stmts, s)
				// Capture the raw statement text. The SEMI token's Pos is its
				// byte offset in input, so input[stmtStart:Pos] is the exact
				// statement (SQLite stores original DDL text verbatim).
				rhs := 2 // rule 352: cmdx SEMI
				if ruleNo == 353 {
					rhs = 3 // rule 353: explain cmdx SEMI
				}
				if semiTok, tokOK := getRHS(p, ruleNo, rhs).(sql.Token); tokOK {
					end := semiTok.Pos
					if end >= stmtStart && end <= len(input) {
						if ct, ctOK := s.(*sql.CreateTableStmt); ctOK {
							ct.RawSQL = strings.TrimSpace(input[stmtStart:end])
						} else if tr, trOK := s.(*sql.CreateTriggerStmt); trOK {
							tr.RawSQL = strings.TrimSpace(input[stmtStart:end])
						} else if vw, vwOK := s.(*sql.CreateViewStmt); vwOK {
							vw.RawSQL = strings.TrimSpace(input[stmtStart:end])
						} else if ci, ciOK := s.(*sql.CreateIndexStmt); ciOK {
							ci.RawSQL = strings.TrimSpace(input[stmtStart:end])
						} else if sel, selOK := s.(*sql.SelectStmt); selOK {
							sel.RawSQL = strings.TrimSpace(input[stmtStart:end])
						}
						stmtStart = end + 1
					}
				}
			}
		}

		// Set the LHS value on the stack
		// For empty rules, LHS overwrites current top position
		lhsSlot := top
		if size > 0 {
			lhsSlot = top - size + 1
		}
		p.stack[lhsSlot].Minor = result
	})

	// Feed tokens until EOF
	var lalrErr error
	for {
		tok := tok.Next()
		code := tokenCode(int(tok.Type), tok.Value)
		if code < 0 {
			lalrErr = fmt.Errorf("near %q: syntax error", tok.Value)
			break
		}
		// When a keyword token is mapped to TK_ID (unknown keyword treated as
		// identifier), restore the original case from the input text. The RD
		// lexer uppercases keyword values, but SQLite preserves identifier case.
		if code == TK_ID && tok.Type == sql.TokenKeyword {
			end := tok.Pos + len(tok.Value)
			if end <= len(input) {
				tok.Value = input[tok.Pos:end]
			}
		}

		result := parser.Parse(code, tok)
		if result == ParseError {
			lalrErr = fmt.Errorf("near %q: syntax error", tok.Value)
			break
		}
		if result == ParseAccept && code == 0 { // EOF
			break
		}
	}

	parser.Finalize()
	if parser.SemanticErr != nil {
		return nil, parser.SemanticErr
	}
	if lalrErr != nil || len(stmts) == 0 {
		if lalrErr != nil {
			if len(stmts) > 0 {
				// SQLite prepares/executes statements incrementally: the
				// parseable prefix runs and its error (if any) takes
				// precedence over the trailing syntax error. Return the
				// prefix with the error so callers can execute it.
				return stmts, lalrErr
			}
			return nil, lalrErr
		}
		if pendingStmt != nil {
			// No statements were collected via ecmd (no SEMI in input).
			// Use pendingStmt as a fallback.
			stmts = append(stmts, pendingStmt)
		} else {
			return nil, fmt.Errorf("no statements parsed")
		}
	}
	// WITH-clause (CTE) definitions are carried directly by the LALR grammar:
	// SELECT (rules 85/86), INSERT (rule 164), and CREATE VIEW bodies all
	// populate the AST's CTEs field. No RD re-parse merge is needed.
	// Recover function-call ORDER BY clauses that the LALR tables drop
	// (e.g. group_concat(a ORDER BY b)) by scanning the raw statement text.
	recoverFuncCallOrderBy(stmts)
	if hasStmtRewrite {
		attachStmtOrderLimit(stmts, stmtClauses)
	}
	return stmts, nil
}

// recoverFuncCallOrderBy re-attaches ORDER BY clauses inside aggregate
// function calls (e.g. group_concat(a ORDER BY b)) that the LALR parse
// tables drop. The parser reduces the function call without the ORDER BY,
// leaving the FuncCall.OrderBy empty; this pass scans each SELECT's raw
// statement text for "funcname( ... ORDER BY sortlist )" and attaches the
// recovered sortlist to the matching FuncCall.
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

// recoverSelectFuncCallOrderBy walks a SELECT's column expressions and
// attaches function-call ORDER BY recovered from raw SQL. It recurses into
// subqueries (whose own RawSQL is empty; their text is located within the
// enclosing statement's raw SQL).
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
func recoverExprFuncCallOrderByAssigned(expr sql.Expr, byName map[string][][]sql.OrderByTerm) {
	switch v := expr.(type) {
	case *sql.FuncCall:
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
	case *sql.Subquery:
		if sub := v.Select; sub != nil {
			recoverSelectFuncCallOrderByAssigned(sub, byName)
		}
	case *sql.ExistsExpr:
		if sub := v.Select; sub != nil {
			recoverSelectFuncCallOrderByAssigned(sub, byName)
		}
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
		recoverExprFuncCallOrderByAssigned(v.Operand, byName)
		for _, item := range v.List {
			recoverExprFuncCallOrderByAssigned(item, byName)
		}
	case *sql.CaseExpr:
		recoverExprFuncCallOrderByAssigned(v.Operand, byName)
		for _, w := range v.Whens {
			recoverExprFuncCallOrderByAssigned(w.When, byName)
			recoverExprFuncCallOrderByAssigned(w.Then, byName)
		}
		recoverExprFuncCallOrderByAssigned(v.Else, byName)
	}
}

// collectFuncCallOrderBy scans raw SQL for every "funcname( ... ORDER BY
// sortlist )" call and returns, keyed by upper-cased function name, the
// recovered sortlists in source order.
func collectFuncCallOrderBy(raw string) map[string][][]sql.OrderByTerm {
	result := make(map[string][][]sql.OrderByTerm)
	upper := strings.ToUpper(raw)
	for i := 0; i < len(raw); i++ {
		// Match an identifier followed by '('.
		if !(isIdentStart(upper[i])) {
			continue
		}
		j := i
		for j < len(raw) && isIdentChar(upper[j]) {
			j++
		}
		name := upper[i:j]
		// Skip whitespace then expect '('.
		k := j
		for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
			k++
		}
		if k >= len(raw) || raw[k] != '(' {
			i = j
			continue
		}
		// Skip SQL keywords that can be followed by '(' (SELECT, WHERE, etc.)
		// so "SELECT (...)" is not mistaken for a function call.
		if isSQLKeywordFollowedByParen(name) {
			i = k
			continue
		}
		// Balance parens from k.
		depth := 0
		end := -1
		for m := k; m < len(raw); m++ {
			if raw[m] == '(' {
				depth++
			} else if raw[m] == ')' {
				depth--
				if depth == 0 {
					end = m
					break
				}
			}
		}
		if end < 0 {
			i = j
			continue
		}
		inner := raw[k+1 : end]
		if obIdx := strings.Index(strings.ToUpper(inner), "ORDER BY"); obIdx >= 0 {
			obText := strings.TrimSpace(inner[obIdx+len("ORDER BY"):])
			if obText != "" {
				if terms := parseSortlistText(obText); len(terms) > 0 {
					result[name] = append(result[name], terms)
				}
			}
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
// SQLite rejects ORDER BY/LIMIT combined with RETURNING on UPDATE and DELETE
// ("near \"RETURNING\": syntax error"), so a statement that carries both a
// clause and a RETURNING is left untouched and fails to parse.
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
	// UPDATE/DELETE statement that has no RETURNING.
	var clauseStarts []int // token index where the clause begins, per statement
	for _, sp := range spans {
		if !(isTopLevelStmt(toks, sp, "UPDATE") || isTopLevelStmt(toks, sp, "DELETE")) {
			continue
		}
		if hasStatementReturning(toks, sp) {
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

// hasStatementReturning reports whether a top-level statement span contains a
// RETURNING keyword at parenthesis depth 0. SQLite does not allow RETURNING
// together with ORDER BY/LIMIT on UPDATE or DELETE, so such statements must
// not be rewritten (they should fail to parse).
func hasStatementReturning(toks []sql.Token, sp stmtSpan) bool {
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
			if strings.EqualFold(toks[j].Value, "RETURNING") {
				return true
			}
		}
	}
	return false
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
		if t.Type == sql.TokenError {
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
	for j := sp.start; j < sp.end; j++ {
		if toks[j].Type == sql.TokenKeyword && strings.EqualFold(toks[j].Value, keyword) {
			return true
		}
		// Stop at the first meaningful token after the previous SEMI that is
		// not WITH (a non-matching statement).
		if toks[j].Type != sql.TokenIdentifier || !strings.EqualFold(toks[j].Value, "WITH") {
			return false
		}
	}
	return false
}

// findStatementOrderLimit finds the token index where the statement-level
// ORDER BY/LIMIT clause begins (the LAST top-level ORDER BY, or the last
// top-level LIMIT when no ORDER BY is present). SQLite requires LIMIT when
// ORDER BY is used on UPDATE or DELETE ("ORDER BY without LIMIT on UPDATE/DELETE"), so an ORDER
// BY without a following LIMIT is not rewritten. Returns ok=false when the
// statement has no usable clause.
func findStatementOrderLimit(toks []sql.Token, sp stmtSpan) (int, bool) {
	lastOB := -1
	lastLIMIT := -1
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
func getRHS(p *Parser, ruleNo, n int) interface{} {
	t := p.tables
	size := -t.RuleInfoNRhs[ruleNo]
	return p.stack[p.pos-size+n].Minor
}

// handleRule implements the action code for each grammar rule.
// Returns the semantic value for the LHS symbol.
func handleRule(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) interface{} {
	switch ruleNo {
	// Rule 0: explain ::= EXPLAIN
	case 0:
		return false // plain EXPLAIN (opcode dump)

	// Rule 1: explain ::= EXPLAIN QUERY PLAN
	case 1:
		return true // EXPLAIN QUERY PLAN (plan output)

	// Rule 2: cmdx ::= cmd
	case 2:
		return getRHS(p, ruleNo, 1)

	// Rule 3: cmd ::= BEGIN transtype trans_opt
	case 3:
		return &sql.BeginStmt{}

	// Rule 4: transtype ::= (empty)
	case 4:
		return nil

	// Rule 8: cmd ::= COMMIT|END trans_opt
	case 8:
		return &sql.CommitStmt{}

	// Rule 9: cmd ::= ROLLBACK trans_opt
	case 9:
		return &sql.RollbackStmt{}

	// Rule 13: create_table ::= createkw temp TABLE ifnotexists nm dbnm
	case 13:
		name := getString(getRHS(p, ruleNo, 5))
		schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
		if schema != "" {
			name = name + "." + schema
		}
		return &sql.CreateTableStmt{
			Name:        name,
			IfNotExists: getBool(getRHS(p, ruleNo, 4)),
			Columns:     nil, // will be filled by create_table_args
		}

	// Rule 14: createkw ::= CREATE
	case 14:
		return nil

	// Rule 15: ifnotexists ::=
	case 15:
		return false

	// Rule 16: ifnotexists ::= IF NOT EXISTS
	case 16:
		return true

	// Rule 17: temp ::= TEMP
	case 17:
		return true

	// Rule 18: temp ::=
	case 18:
		return false

	// Rule 19: create_table_args ::= LP columnlist conslist_opt RP table_option_set
	case 19:
		// This rule produces columns from a column definition list plus
		// table-level constraints (conslist_opt) and table options
		// (table_option_set). The create_table value isn't available here;
		// rule 359 combines them into the CreateTableStmt.
		cols := getColumnList(getRHS(p, ruleNo, 2))
		cons := getTableConstraints(getRHS(p, ruleNo, 3))
		opts := getTableOptions(getRHS(p, ruleNo, 5))
		return &createTableArgs{
			columns:      cols,
			constraints:  cons,
			withoutRowid: opts.withoutRowid,
			strict:       opts.strict,
		}

	// Rule 20: create_table_args ::= AS select
	case 20:
		sel := getSelectStmt(getRHS(p, ruleNo, 2))
		if sel != nil {
			// Wrap in CreateTableStmt with AS SELECT
			createStmt := &sql.CreateTableStmt{
				AsSelect: sel,
			}
			return createStmt
		}
		return nil

	// Rule 21: table_option_set ::=
	case 21:
		return &createTableArgs{}

	// Rule 22: table_option_set ::= table_option_set COMMA table_option
	case 22:
		acc := getTableOptions(getRHS(p, ruleNo, 1))
		opt := getTableOptions(getRHS(p, ruleNo, 3))
		acc.withoutRowid = acc.withoutRowid || opt.withoutRowid
		acc.strict = acc.strict || opt.strict
		return acc

	// Rule 23: table_option ::= WITHOUT nm
	case 23:
		// "WITHOUT ROWID" is the only valid WITHOUT option.
		return &createTableArgs{withoutRowid: true}

	// Rule 24: table_option ::= nm
	case 24:
		// A bare table option name: STRICT is the only one supported.
		opt := getString(getRHS(p, ruleNo, 1))
		return &createTableArgs{strict: strings.EqualFold(opt, "STRICT")}

	// Rule 25: columnname :: nm typemod
	case 25:
		name := getString(getRHS(p, ruleNo, 1))
		typeName := getString(getRHS(p, ruleNo, 2))
		return sql.ColumnDef{Name: name, Type: typeName}

	// Rule 26: typetoken ::=
	case 26:
		return ""

	// Rule 27: typetoken ::= typename LP signed RP
	// e.g., TEXT(50), VARCHAR(255), DECIMAL(10)
	case 27:
		typeName := getString(getRHS(p, ruleNo, 1))
		return fmt.Sprintf("%s(%s)", typeName, getString(getRHS(p, ruleNo, 3)))

	// Rule 28: typetoken ::= typename LP signed COMMA signed RP
	// e.g., DECIMAL(10,2)
	case 28:
		typeName := getString(getRHS(p, ruleNo, 1))
		return fmt.Sprintf("%s(%s, %s)", typeName,
			getString(getRHS(p, ruleNo, 3)), getString(getRHS(p, ruleNo, 5)))

	// Rule 29: typename ::= typename ID — multi-word type names.
	// SQLite permits multi-word type names (e.g. "NATIONAL CHARACTER",
	// "LONG INTEGER", "DOUBLE PRECISION"). The recursive rule accumulates
	// each additional identifier into the type name, joined by a space.
	case 29:
		return getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2))

	// Rule 32: ccons ::= CONSTRAINT nm
	case 32:
		return sql.ColumnDef{ConstraintName: getString(getRHS(p, ruleNo, 2))}

	// Rule 33: ccons ::= DEFAULT scantok term
	case 33:
		return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 3))}

	// Rule 34: ccons ::= DEFAULT LP expr RP
	case 34:
		return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 3))}

	// Rule 35: ccons ::= DEFAULT PLUS scantok term
	case 35:
		return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 4))}

	// Rule 36: ccons ::= DEFAULT MINUS scantok term
	case 36:
		// Fold -9223372036854775808 into math.MinInt64 (SQLite special case),
		// mirroring the unary-minus handling in rule 216.
		if nl, ok := getExpr(getRHS(p, ruleNo, 4)).(*sql.NumericLit); ok && nl.Value == "9223372036854775808" {
			return sql.ColumnDef{Default: &sql.NumericLit{Value: "-9223372036854775808"}}
		}
		return sql.ColumnDef{Default: &sql.UnaryOp{Operand: getExpr(getRHS(p, ruleNo, 4)), Operator: "-"}}

	// Rule 37: ccons ::= DEFAULT scantok ID
	// SQLite's "DEFAULT ID" rule: an unquoted identifier becomes a string
	// literal (sqlite3AddDefaultValue converts TK_ID to TK_STRING), except
	// the unquoted keywords TRUE/FALSE which are boolean literals (1/0).
	case 37:
		if tok, ok := getRHS(p, ruleNo, 3).(sql.Token); ok {
			if !tok.QuotedIdent && strings.EqualFold(tok.Value, "TRUE") {
				return sql.ColumnDef{Default: &sql.NumericLit{Value: "1"}}
			}
			if !tok.QuotedIdent && strings.EqualFold(tok.Value, "FALSE") {
				return sql.ColumnDef{Default: &sql.NumericLit{Value: "0"}}
			}
			return sql.ColumnDef{Default: &sql.StringLit{Value: tok.Value}}
		}
		if s, ok := getRHS(p, ruleNo, 3).(string); ok {
			return sql.ColumnDef{Default: &sql.StringLit{Value: s}}
		}
		return sql.ColumnDef{}

	// Rule 38: ccons ::= NOT NULL onconf
	case 38:
		cd := sql.ColumnDef{NotNull: true}
		cd.OnConflict = getString(getRHS(p, ruleNo, 3))
		return cd

	// Rule 39: ccons ::= PRIMARY KEY sortorder onconf autoinc
	case 39:
		cd := sql.ColumnDef{PrimaryKey: true}
		cd.OnConflict = getString(getRHS(p, ruleNo, 4))
		if getBool(getRHS(p, ruleNo, 5)) {
			cd.AutoInc = true
		}
		return cd

	// Rule 40: ccons ::= UNIQUE onconf
	case 40:
		cd := sql.ColumnDef{Unique: true}
		cd.OnConflict = getString(getRHS(p, ruleNo, 2))
		return cd

	// Rule 41: ccons ::= CHECK LP expr RP
	case 41:
		return sql.ColumnDef{Check: getExpr(getRHS(p, ruleNo, 3))}

	// Rule 42: ccons ::= REFERENCES nm eidlist_opt refargs
	case 42:
		cd := sql.ColumnDef{References: getString(getRHS(p, ruleNo, 2))}
		if cols := getStringList(getRHS(p, ruleNo, 3)); len(cols) > 0 {
			cd.References += "(" + strings.Join(cols, ", ") + ")"
		}
		if ra := getString(getRHS(p, ruleNo, 4)); ra != "" {
			cd.References += " " + ra
		}
		return cd

	// Rule 43: ccons ::= defer_subclause
	// Produces a References marker carrying the DEFERRABLE clause so the
	// merge can append it to a preceding REFERENCES constraint.
	case 43:
		if d, ok := getRHS(p, ruleNo, 1).(string); ok {
			return sql.ColumnDef{References: " " + d}
		}
		return sql.ColumnDef{}

	// Rule 44: ccons ::= COLLATE ids
	case 44:
		return sql.ColumnDef{Collate: getString(getRHS(p, ruleNo, 2))}

	// Rule 45: generated ::= LP expr RP
	case 45:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 61: defer_subclause ::= DEFERRABLE init_deferred_pred_opt
	case 61:
		suffix := ""
		if d, ok := getRHS(p, ruleNo, 2).(string); ok {
			suffix = d
		}
		return "DEFERRABLE" + suffix

	// Rule 62: init_deferred_pred_opt ::= (empty)
	case 62:
		return ""

	// Rule 63: init_deferred_pred_opt ::= INITIALLY DEFERRED
	case 63:
		return " INITIALLY DEFERRED"

	// Rule 64: init_deferred_pred_opt ::= INITIALLY IMMEDIATE
	case 64:
		return " INITIALLY IMMEDIATE"

	// Rule 46: generated ::= LP expr RP ID
	case 46:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 371: ccons ::= AS generated
	case 371:
		return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 2))}

	// Rule 372: ccons ::= GENERATED ALWAYS AS generated
	case 372:
		return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 4))}

	// Rule 373: ccons ::= AS generated
	case 373:
		return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 2))}

	// Rule 47: autoinc ::=
	case 47:
		return false

	// Rules 51-59: refarg — FK reference actions (ON DELETE CASCADE, etc.)
	// Accumulated as strings into refargs.
	case 51:
		return "MATCH " + getString(getRHS(p, ruleNo, 2))
	case 52:
		return "ON INSERT " + getString(getRHS(p, ruleNo, 3))
	case 53:
		return "ON DELETE " + getString(getRHS(p, ruleNo, 3))
	case 54:
		return "ON UPDATE " + getString(getRHS(p, ruleNo, 3))
	case 55:
		return "SET NULL"
	case 56:
		return "SET DEFAULT"
	case 57:
		return "CASCADE"
	case 58:
		return "RESTRICT"
	case 59:
		return "NO ACTION"

	// Rule 48: autoinc ::= AUTOINCREMENT
	case 48:
		return true

	// Rule 49: refargs ::= (empty)
	case 49:
		return ""

	// Rule 50: refargs ::= refargs refarg
	// Accumulates FK reference actions as a space-separated string.
	case 50:
		return getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2))

	// Rule 65: conslist_opt ::= (empty)
	case 65:
		return ([]sql.TableConstraint)(nil)

	// Rule 66: tconscomma ::= COMMA
	case 66:
		return nil

	// Rule 67: tcons ::= CONSTRAINT nm
	case 67:
		return sql.TableConstraint{Type: "", Name: getString(getRHS(p, ruleNo, 2))}

	// Rule 68: tcons ::= PRIMARY KEY LP sortlist autoinc RP onconf
	case 68:
		sortlist := getOrderByList(getRHS(p, ruleNo, 4))
		if err := rejectNullsInSortlist(sortlist); err != nil {
			p.SemanticErr = err
			return nil
		}
		return sql.TableConstraint{
			Type:       sql.ConstraintPrimaryKey,
			Columns:    indexColumnsFromSortlist(getRHS(p, ruleNo, 4)),
			OnConflict: getString(getRHS(p, ruleNo, 6)),
		}

	// Rule 69: tcons ::= UNIQUE LP sortlist RP onconf
	case 69:
		sortlist := getOrderByList(getRHS(p, ruleNo, 3))
		if err := rejectNullsInSortlist(sortlist); err != nil {
			p.SemanticErr = err
			return nil
		}
		return sql.TableConstraint{
			Type:       sql.ConstraintUnique,
			Columns:    indexColumnsFromSortlist(getRHS(p, ruleNo, 3)),
			OnConflict: getString(getRHS(p, ruleNo, 5)),
		}

	// Rule 70: tcons ::= CHECK LP expr RP onconf
	case 70:
		return sql.TableConstraint{
			Type:       sql.ConstraintCheck,
			Expr:       getExpr(getRHS(p, ruleNo, 3)),
			OnConflict: getString(getRHS(p, ruleNo, 5)),
		}

	// Rule 71: tcons ::= FOREIGN KEY LP eidlist RP REFERENCES nm eidlist_opt refargs defer_subclause_opt
	case 71:
		refTable := getString(getRHS(p, ruleNo, 7))
		refCols := getStringList(getRHS(p, ruleNo, 8))
		refAction := ""
		if ra := getString(getRHS(p, ruleNo, 9)); ra != "" {
			refAction = strings.TrimSpace(ra)
		}
		deferred := false
		if d, ok := getRHS(p, ruleNo, 10).(string); ok {
			deferred = strings.Contains(strings.ToUpper(d), "DEFERRABLE") && strings.Contains(strings.ToUpper(d), "INITIALLY DEFERRED")
		}
		return sql.TableConstraint{
			Type:      sql.ConstraintForeignKey,
			Columns:   fkColumnsFromEidlist(getRHS(p, ruleNo, 4)),
			RefTable:  refTable,
			RefCols:   refCols,
			RefAction: refAction,
			Deferred:  deferred,
		}

	// Rule 72: defer_subclause_opt ::=
	case 72:
		return ""

	// Rule 73: onconf ::=
	case 73:
		return ""

	// Rule 74: onconf ::= ON CONFLICT orconf
	case 74:
		return getString(getRHS(p, ruleNo, 3))

	// Rule 75: orconf ::=
	case 75:
		return ""

	// Rule 76: orconf ::= OR resolvel
	case 76:
		return getString(getRHS(p, ruleNo, 2))

	// Rule 77: resolvel ::= IGNORE
	case 77:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 78: resolvel ::= REPLACE
	case 78:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 379: resolvel ::= ROLLBACK|ABORT|FAIL
	case 379:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 79: cmd ::= DROP TABLE ifexists fullname
	case 79:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropTableStmt{Name: name, IfExists: ifExists}

	// Rule 80: ifexists ::= IF EXISTS
	case 80:
		return true

	// Rule 81: ifexists ::=
	case 81:
		return false

	// Rule 82: cmd ::= createkw temp VIEW ifnotexists nm dbnm eidlist_opt AS select
	case 82:
		name := getString(getRHS(p, ruleNo, 5))
		schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
		if schema != "" {
			name = name + "." + schema
		}
		sel := getSelectStmt(getRHS(p, ruleNo, 9))
		cols := getStringList(getRHS(p, ruleNo, 7))
		return &sql.CreateViewStmt{
			Name:    name,
			Columns: cols,
			Select:  sel,
		}

	// Rule 83: cmd ::= DROP VIEW ifexists fullname
	case 83:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropViewStmt{Name: name, IfExists: ifExists}

	// Rule 84: cmd ::= select
	case 84:
		return getSelectStmt(getRHS(p, ruleNo, 1))

	// Rule 85: select ::= WITH wqlist selectnowith
	case 85:
		sel := getSelectStmt(getRHS(p, ruleNo, 3))
		if sel != nil {
			sel.CTEs = getCTEDefs(getRHS(p, ruleNo, 2))
		}
		return checkCompoundSelect(p, sel)

	// Rule 86: select ::= WITH RECURSIVE wqlist selectnowith
	case 86:
		sel := getSelectStmt(getRHS(p, ruleNo, 4))
		if sel != nil {
			sel.CTEs = getCTEDefs(getRHS(p, ruleNo, 3))
		}
		return checkCompoundSelect(p, sel)

	// Rule 87: select ::= selectnowith
	case 87:
		return checkCompoundSelect(p, getSelectStmt(getRHS(p, ruleNo, 1)))

	// Rule 88: selectnowith ::= selectnowith multiselect_op oneselect
	case 88:
		left := getSelectStmt(getRHS(p, ruleNo, 1))
		right := getSelectStmt(getRHS(p, ruleNo, 3))
		if left == nil || right == nil {
			return left
		}
		// multiselect_op = getRHS(p, ruleNo, 2) - returns (SetOp, bool for ALL)
		op := getSetOp(getRHS(p, ruleNo, 2))
		all := false
		if sr, ok := getRHS(p, ruleNo, 2).(setOpResult); ok {
			all = sr.All
		}
		left.AppendUnion(right, op, all)
		return left

	// Rule 89: multiselect_op ::= UNION
	case 89:
		return setOpResult{Op: sql.SetUnion, All: false}

	// Rule 90: multiselect_op ::= UNION ALL
	case 90:
		return setOpResult{Op: sql.SetUnion, All: true}

	// Rule 91: multiselect_op ::= EXCEPT|INTERSECT
	case 91:
		// Distinguish EXCEPT vs INTERSECT from the RHS token value. The
		// lookahead at reduce time is the NEXT token (e.g. SELECT), not the
		// operator being reduced, so it cannot be used to tell them apart.
		op := sql.SetExcept // default
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && strings.EqualFold(tok.Value, "INTERSECT") {
			op = sql.SetIntersect
		}
		return setOpResult{Op: op, All: false}

	// Rule 92: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt orderby_opt limit_opt
	case 92:
		distinct := getBool(getRHS(p, ruleNo, 2))
		cols := getSelectColumns(getRHS(p, ruleNo, 3))
		from, joins := fromValue(getRHS(p, ruleNo, 4))
		where := getExpr(getRHS(p, ruleNo, 5))
		groupBy := getExprList(getRHS(p, ruleNo, 6))
		having := getExpr(getRHS(p, ruleNo, 7))
		orderBy := getOrderByList(getRHS(p, ruleNo, 8))
		lc := getLimitClause(getRHS(p, ruleNo, 9))

		return &sql.SelectStmt{
			Distinct: distinct,
			Columns:  cols,
			From:     from,
			Joins:    joins,
			Where:    where,
			GroupBy:  groupBy,
			Having:   having,
			OrderBy:  orderBy,
			Limit:    lc.limit,
			Offset:   lc.offset,
		}

	// Rule 93: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt window_clause orderby_opt limit_opt
	case 93:
		// Same as 92 but with window_clause before orderby_opt
		distinct := getBool(getRHS(p, ruleNo, 2))
		cols := getSelectColumns(getRHS(p, ruleNo, 3))
		from, joins := fromValue(getRHS(p, ruleNo, 4))
		where := getExpr(getRHS(p, ruleNo, 5))
		groupBy := getExprList(getRHS(p, ruleNo, 6))
		having := getExpr(getRHS(p, ruleNo, 7))
		windows := getWindowDefList(getRHS(p, ruleNo, 8))
		orderBy := getOrderByList(getRHS(p, ruleNo, 9))
		lc := getLimitClause(getRHS(p, ruleNo, 10))

		return &sql.SelectStmt{
			Distinct: distinct,
			Columns:  cols,
			From:     from,
			Joins:    joins,
			Where:    where,
			GroupBy:  groupBy,
			Having:   having,
			Windows:  windows,
			OrderBy:  orderBy,
			Limit:    lc.limit,
			Offset:   lc.offset,
		}

	// Rule 94: values ::= VALUES LP nexprlist RP
	case 94:
		exprs := getExprList(getRHS(p, ruleNo, 3))
		cols := make([]sql.SelectColumn, len(exprs))
		for i, expr := range exprs {
			cols[i] = sql.SelectColumn{Expr: expr}
		}
		return &sql.SelectStmt{
			Columns: cols,
		}

	// Rule 95: oneselect ::= mvalues
	case 95:
		sel := getSelectStmt(getRHS(p, ruleNo, 1))
		if sel != nil {
			sel.ValuesChain = true
		}
		return sel

	// Rule 96: mvalues ::= values COMMA LP nexprlist RP
	case 96:
		first := getSelectStmt(getRHS(p, ruleNo, 1))
		secondExprs := getExprList(getRHS(p, ruleNo, 4))
		secondCols := make([]sql.SelectColumn, len(secondExprs))
		for i, expr := range secondExprs {
			secondCols[i] = sql.SelectColumn{Expr: expr}
		}
		second := &sql.SelectStmt{Columns: secondCols}
		if first != nil {
			if len(first.Columns) != len(secondExprs) {
				p.SemanticErr = fmt.Errorf("all VALUES must have the same number of terms")
			}
			first.AppendUnion(second, sql.SetUnion, true)
		}
		return first

	// Rule 97: mvalues ::= mvalues COMMA LP nexprlist RP
	case 97:
		acc := getSelectStmt(getRHS(p, ruleNo, 1))
		exprs := getExprList(getRHS(p, ruleNo, 4))
		cols := make([]sql.SelectColumn, len(exprs))
		for i, expr := range exprs {
			cols[i] = sql.SelectColumn{Expr: expr}
		}
		last := &sql.SelectStmt{Columns: cols}
		if acc != nil {
			if len(acc.Columns) != len(exprs) {
				p.SemanticErr = fmt.Errorf("all VALUES must have the same number of terms")
			}
			acc.AppendUnion(last, sql.SetUnion, true)
		}
		return acc

	// Rule 98: distinct ::= DISTINCT
	case 98:
		return true

	// Rule 99: distinct ::= ALL
	case 99:
		return false

	// Rule 100: distinct ::=
	case 100:
		return false

	// Rule 102: selcollist ::= sclp scanpt expr scanpt as
	case 102:
		expr := getExpr(getRHS(p, ruleNo, 3))
		alias := getString(getRHS(p, ruleNo, 5))

		// Prepend the accumulated list from sclp (RHS 1). sclp holds the
		// columns collected before the COMMA (via rule 382).
		prev := getSelectColumns(getRHS(p, ruleNo, 1))
		return append(prev, sql.SelectColumn{Expr: expr, As: alias})

	// Rule 103: selcollist ::= sclp scanpt STAR
	case 103:
		prev := getSelectColumns(getRHS(p, ruleNo, 1))
		return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Name: "*"}})

	// Rule 104: selcollist ::= sclp scanpt nm DOT STAR
	case 104:
		tbl := getString(getRHS(p, ruleNo, 3))
		prev := getSelectColumns(getRHS(p, ruleNo, 1))
		return append(prev, sql.SelectColumn{Expr: &sql.ColumnRef{Table: tbl, Name: "*"}})

	// Rule 105: as ::= AS nm
	case 105:
		return getString(getRHS(p, ruleNo, 2))

	// Rule 106: as ::=
	case 106:
		return ""

	// Rule 107: from ::=
	case 107:
		return sql.TableRef{}

	// Rule 108: from ::= FROM seltablist
	case 108:
		return getRHS(p, ruleNo, 2)

	// Rule 109: stl_prefix ::= seltablist joinop
	// Combine the accumulated seltablist with the join operator that follows.
	// The joinop (COMMA or JOIN) marks how the NEXT table will be joined.
	case 109:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		op := getJoinOp(getRHS(p, ruleNo, 2))
		acc.PendingOp = op
		return acc

	// Rule 110: stl_prefix ::=
	case 110:
		return &seltablistAcc{}

	// Rule 111: seltablist ::= stl_prefix nm dbnm as on_using
	case 111:
		return appendSeltablistTable(p, ruleNo, 2, 3, 4, 5)

	// Rule 112: seltablist ::= stl_prefix nm dbnm as indexed_by on_using
	case 112:
		return appendSeltablistTable(p, ruleNo, 2, 3, 4, 6)

	// Rule 113: seltablist ::= stl_prefix nm dbnm LP exprlist RP as on_using
	// Table-valued function in FROM: pragma_table_info('t1').
	case 113:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		tbl := getString(getRHS(p, ruleNo, 2))
		schema := getString(getRHS(p, ruleNo, 3))
		args := getExprList(getRHS(p, ruleNo, 5))
		alias := getString(getRHS(p, ruleNo, 7))
		on, using := getOnUsing(getRHS(p, ruleNo, 8))
		if schema != "" {
			tbl = tbl + "." + schema
		}
		return acc.appendTableWithOn(sql.TableRef{Name: tbl, As: alias, Args: args}, on, using)

	// Rule 114: seltablist ::= stl_prefix LP select RP as on_using
	case 114:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		sel := getSelectStmt(getRHS(p, ruleNo, 3))
		alias := getString(getRHS(p, ruleNo, 5))
		on, using := getOnUsing(getRHS(p, ruleNo, 6))
		ref := sql.TableRef{Subquery: sel, As: alias}
		return acc.appendTableWithOn(ref, on, using)

	// Rule 115: seltablist ::= stl_prefix LP seltablist RP as on_using
	// Parenthesized table list: FROM (t1) or FROM (t1, t2).
	// A parenthesized comma list is flattened into the outer query (SQLite
	// treats (t1, t2) as a group). A parenthesized JOIN group — FROM
	// (t1 JOIN t2 ON ...) — is a derived table (subquery): its joins must
	// stay inside the parens so an outer join sees the group as one unit
	// (e.g. FROM t2 LEFT JOIN (dual JOIN t1 ON true) ON b=c).
	case 115:
		acc := getSeltablist(getRHS(p, ruleNo, 1))
		inner := getSeltablist(getRHS(p, ruleNo, 3))
		alias := getString(getRHS(p, ruleNo, 5))
		on, using := getOnUsing(getRHS(p, ruleNo, 6))
		// A parenthesized JOIN group (explicit JOIN keywords, not a comma list)
		// is always kept as a derived table when it is a JOIN operand: the
		// outer ON/USING applies to the group as a unit and may reference the
		// group's inner tables (e.g. FROM t1 INNER JOIN (t2 CROSS JOIN t0) ON
		// (t0.c0<t0.c1)), which flattening would break. Only parenthesized
		// comma lists are flattened (SQLite treats (t1, t2) as a group).
		if inner.hasExplicitJoins() && acc.HasFirst {
			// A parenthesized JOIN group must stay a derived table when the
			// outer join is OUTER, OR when the group itself contains an OUTER
			// join: flattening a group with an inner FULL JOIN would let
			// later joins leak into it (e.g. FROM t4 INNER JOIN (t5 FULL JOIN
			// t6 USING(id)) USING(id) must keep the FULL JOIN scoped).
			sub := &sql.SelectStmt{
				From:  inner.First,
				Joins: inner.Joins,
				Columns: []sql.SelectColumn{
					{Expr: &sql.ColumnRef{Name: "*"}},
				},
			}
			ref := sql.TableRef{Subquery: sub, As: alias}
			return acc.appendTableWithOn(ref, on, using)
		}
		ref := inner.firstTable()
		if alias != "" {
			ref.As = alias
		}
		// A parenthesized comma list (t1, t2) contributes its joins. The
		// trailing ON/USING of the parenthesized group binds to the first
		// table contributed by the group (SQLite: FROM t1 JOIN (t2 JOIN t3
		// USING(a)) USING(a) applies the outer USING to the group's first
		// table t2).
		acc = acc.appendTableWithOn(ref, on, using)
		for _, j := range inner.Joins {
			acc = acc.appendJoin(j)
		}
		return acc

	// Rule 116: dbnm ::=
	case 116:
		return ""

	// Rule 117: dbnm ::= DOT nm
	case 117:
		return getString(getRHS(p, ruleNo, 2))

	// Rule 118: fullname ::= nm
	case 118:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 119: fullname ::= nm DOT nm
	case 119:
		a := getString(getRHS(p, ruleNo, 1))
		b := getString(getRHS(p, ruleNo, 3))
		return a + "." + b

	// Rule 124: joinop ::= COMMA|JOIN
	case 124:
		// Rule 124: joinop ::= COMMA|JOIN — the multiterminal covers both a
		// comma join (FROM a, b) and a plain JOIN keyword (INNER JOIN).
		// Distinguish by the token value: "," is a comma join, "JOIN" is INNER.
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && tok.Value == "," {
			return joinOp{Comma: true}
		}
		return joinOp{Kind: "INNER"}

	// Rule 121: xfullname ::= nm DOT nm (schema-qualified table name used by
	// INSERT/UPDATE/DELETE, e.g. "temp.t2"). Produces "schema.table".
	case 121:
		a := getString(getRHS(p, ruleNo, 1))
		b := getString(getRHS(p, ruleNo, 3))
		return a + "." + b

	// Rule 122: fullname ::= nm AS nm — table alias. The value is the
	// TABLE NAME (the alias is consumed); the join-op productions are
	// separate (rules 124+).
	case 122:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 123: joinop ::= JOIN_KW nm JOIN
	case 123:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1))}

	// Rule 125: joinop ::= JOIN_KW JOIN
	case 125:
		return joinOp{Kind: joinKind(getRHS(p, ruleNo, 1)), Outer: true}

	// Rule 126: joinop ::= JOIN_KW nm JOIN
	// "NATURAL LEFT JOIN" has JOIN_KW=NATURAL and nm=LEFT; the nm join type
	// must be preserved so exec can NULL-fill the correct side (SQLite's
	// sqlite3JoinType ORs JT_NATURAL with the JOIN_KW/nm flags).
	case 126:
		kw := joinKind(getRHS(p, ruleNo, 1))
		nm := joinKind(getRHS(p, ruleNo, 2))
		return joinOp{Kind: combineNaturalJoin(kw, nm), Outer: true}

	// Rule 127: joinop ::= JOIN_KW nm OUTER JOIN
	case 127:
		kw := joinKind(getRHS(p, ruleNo, 1))
		nm := joinKind(getRHS(p, ruleNo, 2))
		return joinOp{Kind: combineNaturalJoin(kw, nm), Outer: true}

	// Rule 128: joinop ::= JOIN_KW nm JOIN
	case 128:
		// Rule 128: on_using ::= ON expr — the ON condition for a JOIN.
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 129: on_using ::= USING LP idlist RP — the USING column list.
	case 129:
		return getStringList(getRHS(p, ruleNo, 3))

	// Rule 130: on_using ::=
	case 130:
		return nil

	// Rule 131: on_using ::=
	case 131:
		return nil

	// Rule 132: indexed_by ::= INDEXED BY nm
	// Returns the index name. Consumers currently ignore indexed_by.
	case 132:
		return getString(getRHS(p, ruleNo, 3))

	// Rule 133: indexed_by ::= NOT INDEXED
	// Marks the table reference as NOT INDEXED (no index hints).
	// Consumers currently ignore indexed_by; this returns a non-nil marker
	// so the rule does not fall through to a nil passthrough.
	case 133:
		return "NOT INDEXED"

	// Rule 134: orderby_opt ::=
	case 134:
		return ([]sql.OrderByTerm)(nil)

	// Rule 135: orderby_opt ::= ORDER BY sortlist
	case 135:
		return getOrderByList(getRHS(p, ruleNo, 3))

	// Rule 136: sortlist ::= sortlist COMMA expr sortorder nulls
	case 136:
		acc := getOrderByList(getRHS(p, ruleNo, 1))
		expr := getExpr(getRHS(p, ruleNo, 3))
		desc := getBool(getRHS(p, ruleNo, 4))
		nf, nl := getNullsOrder(getRHS(p, ruleNo, 5))
		return append(acc, sql.OrderByTerm{Expr: expr, Desc: desc, NullsFirst: nf, NullsLast: nl})

	// Rule 137: sortlist ::= expr sortorder nulls
	case 137:
		expr := getExpr(getRHS(p, ruleNo, 1))
		desc := getBool(getRHS(p, ruleNo, 2))
		nf, nl := getNullsOrder(getRHS(p, ruleNo, 3))
		return []sql.OrderByTerm{{Expr: expr, Desc: desc, NullsFirst: nf, NullsLast: nl}}

	// Rule 138: sortorder ::= ASC
	case 138:
		return false

	// Rule 139: sortorder ::= DESC
	case 139:
		return true

	// Rule 140: sortorder ::=
	case 140:
		return false

	// Rule 141: nulls ::= NULLS FIRST
	case 141:
		return nullsOrder{first: true}

	// Rule 142: nulls ::= NULLS LAST
	case 142:
		return nullsOrder{last: true}

	// Rule 143: nulls ::=
	case 143:
		return nullsOrder{}

	// Rule 144: groupby_opt ::=
	case 144:
		return ([]sql.Expr)(nil)

	// Rule 145: groupby_opt ::= GROUP BY nexprlist
	case 145:
		return getExprList(getRHS(p, ruleNo, 3))

	// Rule 146: having_opt ::=
	case 146:
		return nil

	// Rule 147: having_opt ::= HAVING expr
	case 147:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 148: limit_opt ::=
	case 148:
		return nil

	// Rule 149: limit_opt ::= LIMIT expr
	case 149:
		return &limitClause{limit: getExpr(getRHS(p, ruleNo, 2))}

	// Rule 150: limit_opt ::= LIMIT expr OFFSET expr
	case 150:
		return &limitClause{
			limit:  getExpr(getRHS(p, ruleNo, 2)),
			offset: getExpr(getRHS(p, ruleNo, 4)),
		}

	// Rule 151: limit_opt ::= LIMIT expr COMMA expr
	case 151:
		// SQLite's LIMIT expr, expr form: first expr is the OFFSET.
		return &limitClause{
			offset: getExpr(getRHS(p, ruleNo, 2)),
			limit:  getExpr(getRHS(p, ruleNo, 4)),
		}

	// Rule 152: cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret
	case 152:
		tbl := getString(getRHS(p, ruleNo, 4))
		wr := getWhereRet(getRHS(p, ruleNo, 6))
		stmt := &sql.DeleteStmt{Table: tbl}
		if wr != nil {
			stmt.Where = wr.where
			if len(wr.returning) > 0 {
				stmt.Returning = foldReturning(wr.returning)
				stmt.HasReturning = true
			}
		}
		return stmt

	// Rule 159: cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt_ret
	case 159:
		tbl := getString(getRHS(p, ruleNo, 4))
		setlist := getAssignments(getRHS(p, ruleNo, 7))
		wr := getWhereRet(getRHS(p, ruleNo, 9))
		stmt := &sql.UpdateStmt{
			Table:       tbl,
			OnConflict:  getString(getRHS(p, ruleNo, 3)),
			Assignments: setlist,
		}
		if wr != nil {
			stmt.Where = wr.where
			if len(wr.returning) > 0 {
				stmt.Returning = foldReturning(wr.returning)
				stmt.HasReturning = true
			}
		}
		return stmt

	// Rule 160: setlist ::= setlist COMMA nm EQ expr
	case 160:
		acc := getAssignments(getRHS(p, ruleNo, 1))
		col := getString(getRHS(p, ruleNo, 3))
		val := getExpr(getRHS(p, ruleNo, 5))
		return append(acc, sql.Assignment{Column: col, Value: val})

	// Rule 162: setlist ::= nm EQ expr
	case 162:
		col := getString(getRHS(p, ruleNo, 1))
		val := getExpr(getRHS(p, ruleNo, 3))
		return []sql.Assignment{{Column: col, Value: val}}

	// Rule 153: where_opt ::=
	case 153:
		return nil

	// Rule 154: where_opt ::= WHERE expr
	case 154:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 155: where_opt_ret ::=
	case 155:
		return &whereRet{}

	// Rule 156: where_opt_ret ::= WHERE expr
	case 156:
		return &whereRet{where: getExpr(getRHS(p, ruleNo, 2))}

	// Rule 157: where_opt_ret ::= RETURNING selcollist
	case 157:
		return &whereRet{returning: getSelectColumns(getRHS(p, ruleNo, 2))}

	// Rule 158: where_opt_ret ::= WHERE expr RETURNING selcollist
	case 158:
		return &whereRet{
			where:     getExpr(getRHS(p, ruleNo, 2)),
			returning: getSelectColumns(getRHS(p, ruleNo, 4)),
		}

	// Rule 164: cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert
	case 164:
		table := getString(getRHS(p, ruleNo, 4))
		columns := getStringList(getRHS(p, ruleNo, 5))
		sel := getSelectStmt(getRHS(p, ruleNo, 6))
		// A VALUES insert (INSERT INTO t VALUES(...),(...)) parses as a SELECT
		// with no FROM. Convert it into s.Values tuples and clear s.Select so
		// the engine uses the VALUES path (insertRow); a real INSERT...SELECT
		// (even one without a FROM clause) keeps s.Select.
		var values [][]sql.Expr
		if sel != nil && sel.ValuesChain {
			values = valuesFromSelect(sel)
			sel = nil
		}
		// insert_cmd is "INSERT" or "REPLACE" (rules 173/174); the orconf
		// resolution type ("IGNORE", "REPLACE", ...) arrives in that string.
		cmd := getString(getRHS(p, ruleNo, 2))
		stmt := &sql.InsertStmt{
			Table:     table,
			Columns:   columns,
			Values:    values,
			Select:    sel,
			IsReplace: strings.EqualFold(cmd, "REPLACE"),
			OrIgnore:  strings.EqualFold(cmd, "IGNORE"),
			CTEs:      getCTEDefs(getRHS(p, ruleNo, 1)),
		}
		// The upsert nonterminal (RHS 7) carries an ON CONFLICT clause and/or
		// a RETURNING projection.
		if uv := getUpsertVal(getRHS(p, ruleNo, 7)); uv != nil {
			stmt.OnConflict = uv.onConflict
			if len(uv.returning) > 0 {
				stmt.Returning = foldReturning(uv.returning)
				stmt.HasReturning = true
			}
		}

		return stmt

	// Rule 165: cmd ::= with insert_cmd INTO xfullname idlist_opt DEFAULT VALUES returning
	case 165:
		table := getString(getRHS(p, ruleNo, 4))
		columns := getStringList(getRHS(p, ruleNo, 5))
		cmd := getString(getRHS(p, ruleNo, 2))
		stmt := &sql.InsertStmt{
			Table:     table,
			Columns:   columns,
			IsReplace: strings.EqualFold(cmd, "REPLACE"),
			OrIgnore:  strings.EqualFold(cmd, "IGNORE"),
		}
		// The returning nonterminal (RHS 8) is either nil (rule 166) or a
		// []sql.SelectColumn from `RETURNING selcollist` (rule 167).
		if cols, ok := getRHS(p, ruleNo, 8).([]sql.SelectColumn); ok && len(cols) > 0 {
			stmt.Returning = foldReturning(cols)
			stmt.HasReturning = true
		}
		return stmt

	// Rule 166: upsert ::=
	case 166:
		return &upsertVal{}

	// Rule 167: upsert ::= RETURNING selcollist
	case 167:
		return &upsertVal{returning: getSelectColumns(getRHS(p, ruleNo, 2))}

	// Rule 172: returning ::= RETURNING selcollist
	case 172:
		return getSelectColumns(getRHS(p, ruleNo, 2))

	// Rule 385: returning ::=
	case 385:
		return nil

	// Rule 168: upsert ::= ON CONFLICT LP sortlist RP where_opt
	//                       DO UPDATE SET setlist where_opt upsert
	case 168:
		target := getOrderByList(getRHS(p, ruleNo, 4))
		// NULLS FIRST/LAST is not supported in an ON CONFLICT target.
		if err := rejectNullsInSortlist(target); err != nil {
			p.SemanticErr = err
			return nil
		}
		oc := &sql.OnConflictClause{
			Action:         sql.ConflictDoUpdate,
			ConflictColumn: conflictTargetColumn(target),
			// The DO UPDATE WHERE (RHS 11) is the update condition; the
			// conflict-target WHERE (RHS 6) is the partial-index predicate.
			Where:       getExpr(getRHS(p, ruleNo, 11)),
			Assignments: getAssignments(getRHS(p, ruleNo, 10)),
		}
		// A chained ON CONFLICT clause: SQLite gives the last clause
		// precedence, so surface it instead of the first.
		if chained, ok := getRHS(p, ruleNo, 12).(*upsertVal); ok && chained != nil && chained.onConflict != nil {
			return chained
		}
		uv := &upsertVal{onConflict: oc}
		if chained, ok := getRHS(p, ruleNo, 12).(*upsertVal); ok && chained != nil {
			uv.returning = chained.returning
		}
		return uv

	// Rule 169: upsert ::= ON CONFLICT LP sortlist RP where_opt DO NOTHING upsert
	case 169:
		target := getOrderByList(getRHS(p, ruleNo, 4))
		oc := &sql.OnConflictClause{
			Action:         sql.ConflictDoNothing,
			ConflictColumn: conflictTargetColumn(target),
		}
		if chained, ok := getRHS(p, ruleNo, 9).(*upsertVal); ok && chained != nil && chained.onConflict != nil {
			return chained
		}
		uv := &upsertVal{onConflict: oc}
		if chained, ok := getRHS(p, ruleNo, 9).(*upsertVal); ok && chained != nil {
			uv.returning = chained.returning
		}
		return uv

	// Rule 170: upsert ::= ON CONFLICT DO NOTHING returning
	case 170:
		return &upsertVal{
			onConflict: &sql.OnConflictClause{
				Action: sql.ConflictDoNothing,
			},
			returning: getSelectColumns(getRHS(p, ruleNo, 5)),
		}

	// Rule 171: upsert ::= ON CONFLICT DO UPDATE SET setlist where_opt returning
	case 171:
		return &upsertVal{
			onConflict: &sql.OnConflictClause{
				Action:      sql.ConflictDoUpdate,
				Where:       getExpr(getRHS(p, ruleNo, 7)),
				Assignments: getAssignments(getRHS(p, ruleNo, 6)),
			},
			returning: getSelectColumns(getRHS(p, ruleNo, 8)),
		}

	case 173:
		// Return the orconf resolution type ("", "IGNORE", "REPLACE", ...).
		return getString(getRHS(p, ruleNo, 2))

	// Rule 174: insert_cmd ::= REPLACE
	case 174:
		return "REPLACE"

	// Rule 175: idlist_opt ::=
	case 175:
		return ([]string)(nil)

	// Rule 176: idlist_opt ::= LP idlist RP
	case 176:
		return getStringList(getRHS(p, ruleNo, 2))

	// Rule 177: idlist ::= idlist COMMA nm
	case 177:
		acc := getStringList(getRHS(p, ruleNo, 1))
		return append(acc, getString(getRHS(p, ruleNo, 3)))

	// Rule 178: idlist ::= nm
	case 178:
		return []string{getString(getRHS(p, ruleNo, 1))}

	// Rule 179: expr ::= LP expr RP
	case 179:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 180: expr ::= ID|INDEXED|JOIN_KW (column reference)
	case 180:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			// Keep the Quoted flag on all double-quoted identifiers (including
			// the empty "") so resolution can apply SQLite's DQS rules: with
			// DQS enabled an unmatched double-quoted identifier becomes a
			// string literal; with DQS disabled it is a "no such column"
			// error hinting at single-quoted strings.
			return &sql.ColumnRef{Name: tok.Value, Quoted: tok.QuotedIdent}
		}
		if s, ok := getRHS(p, ruleNo, 1).(string); ok {
			return &sql.ColumnRef{Name: s}
		}
		return &sql.ColumnRef{Name: fmt.Sprintf("%v", getRHS(p, ruleNo, 1))}

	// Rule 181: expr ::= nm DOT nm (schema.table)
	case 181:
		schema := getString(getRHS(p, ruleNo, 1))
		col := getString(getRHS(p, ruleNo, 3))
		return &sql.ColumnRef{Table: schema, Name: col}

	// Rule 182: expr ::= nm DOT nm DOT nm (schema.table.column)
	case 182:
		schema := getString(getRHS(p, ruleNo, 1))
		table := getString(getRHS(p, ruleNo, 3))
		col := getString(getRHS(p, ruleNo, 5))
		return &sql.ColumnRef{Table: schema + "." + table, Name: col}

	// Rule 183: term ::= NULL|FLOAT|BLOB
	case 183:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			if strings.EqualFold(tok.Value, "NULL") {
				return &sql.NullLit{}
			}
			// Hex blob literal X'...' / x'...': decode the hex content so
			// the value keeps its blob type instead of becoming a number.
			if tok.Type == sql.TokenBlob {
				return decodeBlobToken(tok.Value)
			}
			return &sql.NumericLit{Value: tok.Value}
		}
		return &sql.NullLit{}

	// Rule 184: term ::= STRING
	case 184:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return &sql.StringLit{Value: tok.Value}
		}
		if s, ok := getRHS(p, ruleNo, 1).(string); ok {
			return &sql.StringLit{Value: s}
		}
		return &sql.StringLit{}

	// Rule 185: term ::= INTEGER
	case 185:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return &sql.NumericLit{Value: tok.Value}
		}
		if s, ok := getRHS(p, ruleNo, 1).(string); ok {
			return &sql.NumericLit{Value: s}
		}
		return &sql.NumericLit{}

	// Rule 186: expr ::= VARIABLE
	// A parameter placeholder (? or $name). Frigolite does not support bound
	// parameters; it evaluates to NULL, but is kept distinct from a NULL
	// literal so CREATE TABLE can reject it in non-constant DEFAULT
	// expressions.
	case 186:
		param := &sql.ParameterExpr{}
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			param.Name = tok.Value
		}
		return param

	// Rule 187: expr ::= expr COLLATE ID|STRING
	case 187:
		expr := getExpr(getRHS(p, ruleNo, 1))
		collation := getString(getRHS(p, ruleNo, 3))
		// COLLATE is an operator that wraps the expression
		return &sql.BinaryOp{
			Left:     expr,
			Operator: "COLLATE",
			Right:    &sql.StringLit{Value: collation},
		}

	// Rule 188: expr ::= CAST LP expr AS typetoken RP
	case 188:
		return &sql.CastExpr{
			Operand: getExpr(getRHS(p, ruleNo, 3)),
			AsType:  getString(getRHS(p, ruleNo, 5)),
		}

	// Rule 189: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP (function call)
	case 189:
		name := getString(getRHS(p, ruleNo, 1))
		distinct := getBool(getRHS(p, ruleNo, 3))
		args := getExprList(getRHS(p, ruleNo, 4))
		return &sql.FuncCall{
			Name:     name,
			Args:     args,
			Distinct: distinct,
		}

	// Rule 191: expr ::= ID|INDEXED|JOIN_KW LP STAR RP (function(star))
	case 191:
		name := getString(getRHS(p, ruleNo, 1))
		return &sql.FuncCall{
			Name: name,
			Args: []sql.Expr{&sql.ColumnRef{Name: "*"}}, // COUNT(*) — star as a column ref
		}

	// Rule 190: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist ORDER BY sortlist RP
	// (function call with internal ORDER BY, e.g. group_concat(x ORDER BY y))
	case 190:
		name := getString(getRHS(p, ruleNo, 1))
		distinct := getBool(getRHS(p, ruleNo, 3))
		args := getExprList(getRHS(p, ruleNo, 4))
		orderBy := getOrderByList(getRHS(p, ruleNo, 6))
		return &sql.FuncCall{
			Name:     name,
			Args:     args,
			Distinct: distinct,
			OrderBy:  orderBy,
		}

	// Rule 192: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP filter_over
	case 192:
		name := getString(getRHS(p, ruleNo, 1))
		distinct := getBool(getRHS(p, ruleNo, 3))
		args := getExprList(getRHS(p, ruleNo, 4))
		wf := getWindowFilter(getRHS(p, ruleNo, 6))
		var over *sql.WindowDef
		var filter sql.Expr
		if wf != nil {
			over = wf.over
			filter = wf.filter
		}
		return &sql.FuncCall{
			Name:     name,
			Args:     args,
			Distinct: distinct,
			Filter:   filter,
			Over:     over,
		}

	// Rule 193: expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist ORDER BY sortlist RP filter_over
	case 193:
		name := getString(getRHS(p, ruleNo, 1))
		distinct := getBool(getRHS(p, ruleNo, 3))
		args := getExprList(getRHS(p, ruleNo, 4))
		orderBy := getOrderByList(getRHS(p, ruleNo, 6))
		wf := getWindowFilter(getRHS(p, ruleNo, 9))
		var over *sql.WindowDef
		var filter sql.Expr
		if wf != nil {
			over = wf.over
			filter = wf.filter
		}
		return &sql.FuncCall{
			Name:     name,
			Args:     args,
			Distinct: distinct,
			OrderBy:  orderBy,
			Filter:   filter,
			Over:     over,
		}

	// Rule 194: expr ::= ID|INDEXED|JOIN_KW LP STAR RP filter_over (window function)
	case 194:
		name := getString(getRHS(p, ruleNo, 1))
		wf := getWindowFilter(getRHS(p, ruleNo, 5))
		var over *sql.WindowDef
		var filter sql.Expr
		if wf != nil {
			over = wf.over
			filter = wf.filter
		}
		return &sql.FuncCall{
			Name:   name,
			Args:   []sql.Expr{&sql.ColumnRef{Name: "*"}}, // COUNT(*) — star as a column ref
			Filter: filter,
			Over:   over,
		}

	// Rule 196: expr ::= LP exprlist COMMA expr RP (row value / vector)
	// A parenthesized list of two or more expressions is a row value used
	// in comparisons like (a, b) = ('x', 'y'). The grammar splits the list
	// as (exprlist, expr) with exprlist holding all but the last element.
	case 196:
		exprs := getExprList(getRHS(p, ruleNo, 2))
		last := getExpr(getRHS(p, ruleNo, 4))
		exprs = append(exprs, last)
		return &sql.RowValue{Values: exprs}

	// Rule 197: expr ::= expr AND expr
	case 197:
		return &sql.BinaryOp{
			Left:     getExpr(getRHS(p, ruleNo, 1)),
			Operator: "AND",
			Right:    getExpr(getRHS(p, ruleNo, 3)),
		}

	// Rule 198: expr ::= expr OR expr
	case 198:
		return &sql.BinaryOp{
			Left:     getExpr(getRHS(p, ruleNo, 1)),
			Operator: "OR",
			Right:    getExpr(getRHS(p, ruleNo, 3)),
		}

	// Rule 199: expr ::= expr LT|GT|GE|LE expr
	case 199:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		// Read the operator from the RHS token value (the lookahead at reduce
		// time is the NEXT token, not the operator being reduced).
		op := "<"
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
			switch strings.ToUpper(tok.Value) {
			case ">":
				op = ">"
			case ">=":
				op = ">="
			case "<=":
				op = "<="
			}
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 200: expr ::= expr EQ|NE expr
	case 200:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		// Read the operator from the RHS token value (lookahead is the NEXT
		// token, so it cannot distinguish = from != / <>).
		op := "="
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
			if tok.Value == "!=" || tok.Value == "<>" {
				op = "<>"
			}
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 201: expr ::= expr BITAND|BITOR|LSHIFT|RSHIFT expr
	case 201:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		// Read the operator from the RHS token value (lookahead is the NEXT
		// token at reduce time).
		op := "&"
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
			switch tok.Value {
			case "|":
				op = "|"
			case "<<":
				op = "<<"
			case ">>":
				op = ">>"
			}
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 202: expr ::= expr PLUS|MINUS expr
	case 202:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		op := "+"
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value == "-" {
			op = "-"
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 203: expr ::= expr STAR|SLASH|REM expr
	case 203:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		op := "*"
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok {
			switch tok.Value {
			case "/":
				op = "/"
			case "%":
				op = "%"
			}
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

	// Rule 204: expr ::= expr CONCAT expr
	case 204:
		return &sql.BinaryOp{
			Left:     getExpr(getRHS(p, ruleNo, 1)),
			Operator: "||",
			Right:    getExpr(getRHS(p, ruleNo, 3)),
		}

		// Rule 206: expr ::= expr likeop expr (LIKE/GLOB/REGEXP/MATCH)
	case 206:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		op := "LIKE"
		if s, ok := getRHS(p, ruleNo, 2).(string); ok && s != "" {
			op = s
		}
		return &sql.BinaryOp{Left: left, Operator: op, Right: right}

		// Rule 207: expr ::= expr likeop expr ESCAPE expr
	case 207:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		escape := getExpr(getRHS(p, ruleNo, 5))
		return &sql.BinaryOp{
			Left:     left,
			Operator: "LIKE",
			Right:    right,
			Escape:   getString(escape),
		}

	// Rule 208: expr ::= expr ISNULL|NOTNULL
	case 208:
		operand := getExpr(getRHS(p, ruleNo, 1))
		// Read the operator from the RHS token value (lookahead at reduce
		// time is the NEXT token, not the ISNULL/NOTNULL keyword).
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && tok.Value != "ISNULL" {
			return &sql.IsNotNull{Operand: operand}
		}
		return &sql.IsNull{Operand: operand}

	// Rule 210: expr ::= expr IS expr
	case 210:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 3))
		// IS TRUE / IS FALSE predicates. The right side may be wrapped in a
		// COLLATE operator (e.g. `x IS TRUE COLLATE NOCASE`), which SQLite
		// parses as the IS TRUE predicate with a no-op collation on the
		// result; unwrap it so the predicate is still recognized.
		boolExpr := right
		if bo, ok := boolExpr.(*sql.BinaryOp); ok && bo.Operator == "COLLATE" {
			boolExpr = bo.Left
		}
		if name, ok := boolLitName(boolExpr); ok {
			if name == "TRUE" {
				return &sql.IsTrue{Operand: left}
			}
			return &sql.IsFalse{Operand: left}
		}
		return &sql.BinaryOp{Left: left, Operator: "IS", Right: right}

	// Rule 211: expr ::= expr IS NOT expr
	case 211:
		left := getExpr(getRHS(p, ruleNo, 1))
		right := getExpr(getRHS(p, ruleNo, 4))
		// IS NOT TRUE / IS NOT FALSE predicates (unwrap a COLLATE wrapper on
		// the right side, mirroring rule 210).
		boolExpr := right
		if bo, ok := boolExpr.(*sql.BinaryOp); ok && bo.Operator == "COLLATE" {
			boolExpr = bo.Left
		}
		if name, ok := boolLitName(boolExpr); ok {
			if name == "TRUE" {
				return &sql.IsTrue{Operand: left, Negated: true}
			}
			return &sql.IsFalse{Operand: left, Negated: true}
		}
		return &sql.BinaryOp{Left: left, Operator: "IS NOT", Right: right}

	// Rule 212: expr ::= expr IS NOT DISTINCT FROM expr (6 RHS symbols)
	case 212:
		return &sql.IsNotDistinctFrom{
			Left:  getExpr(getRHS(p, ruleNo, 1)),
			Right: getExpr(getRHS(p, ruleNo, 6)),
		}

	// Rule 213: expr ::= expr IS DISTINCT FROM expr (5 RHS symbols)
	case 213:
		return &sql.IsDistinctFrom{
			Left:  getExpr(getRHS(p, ruleNo, 1)),
			Right: getExpr(getRHS(p, ruleNo, 5)),
		}

	// Rule 214: expr ::= NOT expr
	case 214:
		return &sql.UnaryOp{
			Operand:  getExpr(getRHS(p, ruleNo, 2)),
			Operator: "NOT",
		}

	// Rule 215: expr ::= BITNOT expr
	case 215:
		return &sql.UnaryOp{
			Operand:  getExpr(getRHS(p, ruleNo, 2)),
			Operator: "~",
		}

	// Rule 216: expr ::= PLUS|MINUS expr (unary)
	case 216:
		operand := getExpr(getRHS(p, ruleNo, 2))
		// Read the operator from the RHS token value (lookahead is the NEXT
		// token at reduce time, so it cannot distinguish + from -).
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok && tok.Value == "-" {
			// SQLite special case: -9223372036854775808 is the minimum int64.
			// The positive literal 9223372036854775808 does not fit in int64
			// (it is 2^63), so SQLite folds the unary minus into the literal
			// to produce math.MinInt64 as an INTEGER (not a REAL).
			if nl, ok := operand.(*sql.NumericLit); ok && nl.Value == "9223372036854775808" {
				return &sql.NumericLit{Value: "-9223372036854775808"}
			}
			// SQLite folds the sign into hex literals too, so the "hex
			// literal too big" error message carries the minus sign
			// (e.g. "-0x08000000000000000").
			if nl, ok := operand.(*sql.NumericLit); ok && isHexLiteral(nl.Value) {
				return &sql.NumericLit{Value: "-" + nl.Value}
			}
			return &sql.UnaryOp{Operand: operand, Operator: "-"}
		}
		// Unary + is a no-op at parse level (SQLite semantics: +expr is
		// equivalent to expr but the result has NO affinity).
		return &sql.UnaryOp{Operand: operand, Operator: "+"}

	// Rule 220: expr ::= expr between_op expr AND expr
	case 220:
		// between_op is the raw keyword token: BETWEEN (or NOT for
		// "NOT BETWEEN"). SQLite's grammar reduces between_op to an
		// int flag; here the token itself sits on the stack.
		negated := false
		if tok, ok := getRHS(p, ruleNo, 2).(sql.Token); ok && strings.EqualFold(tok.Value, "NOT") {
			negated = true
		}
		return &sql.Between{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			Low:     getExpr(getRHS(p, ruleNo, 3)),
			High:    getExpr(getRHS(p, ruleNo, 5)),
			Negated: negated,
		}

	// Rule 221: in_op ::= IN
	case 221:
		return false

	// Rule 222: in_op ::= NOT IN
	case 222:
		return true

	// Rule 223: expr ::= expr in_op LP exprlist RP
	case 223:
		negated := getBool(getRHS(p, ruleNo, 2))
		return &sql.InList{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			List:    getExprList(getRHS(p, ruleNo, 4)),
			Negated: negated,
		}

	// Rule 224: expr ::= LP select RP
	case 224:
		return &sql.Subquery{
			Select: getSelectStmt(getRHS(p, ruleNo, 2)),
		}

	// Rule 225: expr ::= expr in_op LP select RP
	case 225:
		negated := getBool(getRHS(p, ruleNo, 2))
		return &sql.InList{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			List:    []sql.Expr{&sql.Subquery{Select: getSelectStmt(getRHS(p, ruleNo, 4))}},
			Negated: negated,
		}

	// Rule 226: expr ::= expr in_op nm dbnm paren_exprlist
	// SQLite extension: `expr IN table-name` is equivalent to
	// `expr IN (SELECT * FROM table-name)`. The optional paren_exprlist is
	// the argument list of a table-valued function in the FROM clause.
	case 226:
		negated := getBool(getRHS(p, ruleNo, 2))
		tbl := getString(getRHS(p, ruleNo, 3))
		schema := getString(getRHS(p, ruleNo, 4))
		if schema != "" {
			tbl = tbl + "." + schema
		}
		args := getExprList(getRHS(p, ruleNo, 5))
		sub := &sql.Subquery{Select: &sql.SelectStmt{
			Columns: []sql.SelectColumn{{Expr: &sql.ColumnRef{Name: "*"}}},
			From:    sql.TableRef{Name: tbl, Args: args},
		}}
		return &sql.InList{
			Operand: getExpr(getRHS(p, ruleNo, 1)),
			List:    []sql.Expr{sub},
			Negated: negated,
		}

	// Rule 227: expr ::= EXISTS LP select RP
	case 227:
		return &sql.ExistsExpr{
			Select:  getSelectStmt(getRHS(p, ruleNo, 3)),
			Negated: false,
		}

	// Rule 228: expr ::= CASE case_operand case_exprlist case_else END
	case 228:
		operand := getExpr(getRHS(p, ruleNo, 2))
		whenList := getWhenClauses(getRHS(p, ruleNo, 3))
		elseExpr := getExpr(getRHS(p, ruleNo, 4))
		return &sql.CaseExpr{
			Operand: operand,
			Whens:   whenList,
			Else:    elseExpr,
		}

	// Rule 229: case_exprlist ::= case_exprlist WHEN expr THEN expr
	case 229:
		acc := getWhenClauses(getRHS(p, ruleNo, 1))
		whenExpr := getExpr(getRHS(p, ruleNo, 3))
		thenExpr := getExpr(getRHS(p, ruleNo, 5))
		return append(acc, sql.WhenClause{When: whenExpr, Then: thenExpr})

	// Rule 230: case_exprlist ::= WHEN expr THEN expr
	case 230:
		whenExpr := getExpr(getRHS(p, ruleNo, 2))
		thenExpr := getExpr(getRHS(p, ruleNo, 4))
		return []sql.WhenClause{{When: whenExpr, Then: thenExpr}}

	// Rule 231: case_else ::= ELSE expr
	case 231:
		return getExpr(getRHS(p, ruleNo, 2))

	// Rule 232: case_else ::=
	case 232:
		return nil

	// Rule 233: case_operand ::=
	case 233:
		return nil

	// Rule 234: exprlist ::=
	case 234:
		return ([]sql.Expr)(nil)

	// Rule 235: nexprlist ::= nexprlist COMMA expr
	case 235:
		acc := getExprList(getRHS(p, ruleNo, 1))
		return append(acc, getExpr(getRHS(p, ruleNo, 3)))

	// Rule 236: nexprlist ::= expr
	case 236:
		return []sql.Expr{getExpr(getRHS(p, ruleNo, 1))}

	// Rule 237: paren_exprlist ::=
	case 237:
		return ([]sql.Expr)(nil)

	// Rule 238: paren_exprlist ::= LP exprlist RP
	case 238:
		return getExprList(getRHS(p, ruleNo, 2))

	// Rule 239: cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt
	case 239:
		name := getString(getRHS(p, ruleNo, 5))
		table := getString(getRHS(p, ruleNo, 8))
		sortlist := getOrderByList(getRHS(p, ruleNo, 10))
		// NULLS FIRST/LAST is only valid in ORDER BY, not in index key
		// definitions (SQLite: "unsupported use of NULLS FIRST/LAST").
		if err := rejectNullsInSortlist(sortlist); err != nil {
			p.SemanticErr = err
			return nil
		}
		where := getExpr(getRHS(p, ruleNo, 12))
		// uniqueflag is RHS[2]: empty or "UNIQUE".
		unique := strings.EqualFold(strings.TrimSpace(getString(getRHS(p, ruleNo, 2))), "UNIQUE")
		// The sortlist is []OrderByTerm; convert to []IndexColumn for the
		// engine's key population, while retaining the full term expressions
		// (Terms) for DDL validation and ALTER DROP COLUMN checks.
		// A plain identifier becomes a column reference. A numeric literal
		// is a 1-based column position (SQLite allows "CREATE INDEX ON
		// t1(1)" meaning "on the first column"); record it by its numeric
		// text so the engine can resolve it against the table columns.
		// A bare string literal index key is converted to an identifier
		// (SQLite sqlite3StringToId: CREATE INDEX t1('b') indexes column b).
		// Other expressions (e.g. "a+b") are not supported as index keys.
		var cols []sql.IndexColumn
		for _, term := range sortlist {
			switch ex := term.Expr.(type) {
			case *sql.ColumnRef:
				cols = append(cols, sql.IndexColumn{Name: ex.Name, Desc: term.Desc})
			case *sql.StringLit:
				cols = append(cols, sql.IndexColumn{Name: ex.Value, Desc: term.Desc})
			case *sql.NumericLit:
				cols = append(cols, sql.IndexColumn{Name: ex.Value, Desc: term.Desc})
			}
		}
		return &sql.CreateIndexStmt{
			Name:    name,
			Table:   table,
			Columns: cols,
			Terms:   sortlist,
			Unique:  unique,
			Where:   where,
		}

	// Rule 242: eidlist_opt ::=
	case 242:
		return ([]string)(nil)

	// Rule 243: eidlist_opt ::= LP eidlist RP
	case 243:
		return getStringList(getRHS(p, ruleNo, 2))

	// Rule 244: eidlist ::= eidlist COMMA nm collate sortorder
	case 244:
		acc := getStringList(getRHS(p, ruleNo, 1))
		return append(acc, getString(getRHS(p, ruleNo, 3)))

	// Rule 245: eidlist ::= nm collate sortorder
	case 245:
		return []string{getString(getRHS(p, ruleNo, 1))}

	// Rule 248: cmd ::= DROP INDEX ifexists fullname
	case 248:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropIndexStmt{Name: name, IfExists: ifExists}

	// Rule 249: cmd ::= VACUUM into_opt
	// VACUUM with an optional INTO <file> clause (into_opt: empty, rule 252,
	// or "INTO ids", rule 251). The exec handler is a no-op, so the INTO
	// target is not retained.
	case 249:
		return &sql.VacuumStmt{}

	// Rule 253: cmd ::= PRAGMA nm dbnm
	case 253:
		name := getString(getRHS(p, ruleNo, 2))
		return &sql.PragmaStmt{
			Name:  name,
			Value: "",
		}

	// Rule 254: cmd ::= PRAGMA nm dbnm = pragma_value
	case 254:
		name := getString(getRHS(p, ruleNo, 2))
		value := getString(getRHS(p, ruleNo, 5))
		return &sql.PragmaStmt{
			Name:  name,
			Value: value,
		}

	// Rule 255: cmd ::= PRAGMA nm dbnm LP pragma_value RP
	// Rule 257: cmd ::= PRAGMA nm dbnm LP minus_num RP
	case 255, 257:
		return &sql.PragmaStmt{
			Name:  getString(getRHS(p, ruleNo, 2)),
			Value: getString(getRHS(p, ruleNo, 5)),
		}

	// Rule 256: cmd ::= PRAGMA nm dbnm LP RP
	case 256:
		return &sql.PragmaStmt{
			Name: getString(getRHS(p, ruleNo, 2)),
		}

	// Rule 260: cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END
	case 260:
		decl, _ := getRHS(p, ruleNo, 2).(*triggerDeclInfo)
		if decl == nil {
			return nil
		}
		stmts := getStmtList(getRHS(p, ruleNo, 4))
		return &sql.CreateTriggerStmt{
			Name:        decl.name,
			Table:       decl.table,
			Event:       decl.event,
			Time:        decl.time,
			When:        decl.when,
			Statements:  stmts,
			IfNotExists: decl.ifNotExist,
		}

	// Rule 261: trigger_decl ::= temp TRIGGER ifnotexists nm dbnm trigger_time
	//            trigger_event ON fullname foreach_clause when_clause
	case 261:
		return &triggerDeclInfo{
			name:       getString(getRHS(p, ruleNo, 4)),
			schema:     getString(getRHS(p, ruleNo, 5)),
			time:       getString(getRHS(p, ruleNo, 6)),
			event:      getString(getRHS(p, ruleNo, 7)),
			table:      getString(getRHS(p, ruleNo, 9)),
			when:       getExpr(getRHS(p, ruleNo, 11)),
			ifNotExist: getBool(getRHS(p, ruleNo, 3)),
		}

	// Rule 270: trigger_cmd_list ::= trigger_cmd_list trigger_cmd SEMI
	case 270:
		list := getStmtList(getRHS(p, ruleNo, 1))
		stmt := getStmt(getRHS(p, ruleNo, 2))
		if stmt != nil {
			list = append(list, stmt)
		}
		return list

	// Rule 271: trigger_cmd_list ::= trigger_cmd SEMI
	case 271:
		stmt := getStmt(getRHS(p, ruleNo, 1))
		if stmt == nil {
			return []sql.Stmt(nil)
		}
		return []sql.Stmt{stmt}

	// Rule 274: trigger_cmd ::= UPDATE orconf nm indexed_opt SET setlist from where_opt
	case 274:
		return &sql.UpdateStmt{
			Table:       getString(getRHS(p, ruleNo, 3)),
			Assignments: getAssignments(getRHS(p, ruleNo, 6)),
			Where:       getExpr(getRHS(p, ruleNo, 8)),
		}

		// Rule 275: trigger_cmd ::= with insert_cmd INTO nm idlist_opt select upsert
	case 275:
		cmd := getString(getRHS(p, ruleNo, 2))
		table := getString(getRHS(p, ruleNo, 4))
		columns := getStringList(getRHS(p, ruleNo, 5))
		sel := getSelectStmt(getRHS(p, ruleNo, 6))
		var values [][]sql.Expr
		if sel != nil && sel.ValuesChain {
			values = valuesFromSelect(sel)
			sel = nil
		}
		stmt := &sql.InsertStmt{
			Table:     table,
			Columns:   columns,
			Values:    values,
			Select:    sel,
			IsReplace: strings.EqualFold(cmd, "REPLACE"),
		}
		// The upsert nonterminal (RHS 7) carries an ON CONFLICT clause.
		if uv := getUpsertVal(getRHS(p, ruleNo, 7)); uv != nil {
			stmt.OnConflict = uv.onConflict
			if len(uv.returning) > 0 {
				stmt.HasReturning = true
				stmt.Returning = foldReturning(uv.returning)
			}
		}
		return stmt

	// Rule 276: trigger_cmd ::= DELETE FROM xfullname tridxby where_opt scanpt
	case 276:
		return &sql.DeleteStmt{
			Table: getString(getRHS(p, ruleNo, 3)),
			Where: getExpr(getRHS(p, ruleNo, 5)),
		}

	// Rule 277: trigger_cmd ::= scanpt select scanpt
	// A bare SELECT as a trigger body. scanpt markers are empty (nil).
	case 277:
		return getRHS(p, ruleNo, 2)

	// Rule 278: expr ::= RAISE LP IGNORE RP
	case 278:
		return &sql.RaiseExpr{Kind: "IGNORE"}

	// Rule 279: expr ::= RAISE LP raisetype COMMA expr RP — RAISE(ABORT,msg) etc.
	case 279:
		return &sql.RaiseExpr{
			Kind:    getString(getRHS(p, ruleNo, 3)),
			Message: getExpr(getRHS(p, ruleNo, 5)),
		}

	// Rules 280-282: raisetype ::= ROLLBACK | ABORT | FAIL
	case 280:
		return "ROLLBACK"
	case 281:
		return "ABORT"
	case 282:
		return "FAIL"

	// Rule 283: cmd ::= DROP TRIGGER ifexists fullname
	case 283:
		ifExists := getBool(getRHS(p, ruleNo, 3))
		name := getString(getRHS(p, ruleNo, 4))
		return &sql.DropTriggerStmt{Name: name, IfExists: ifExists}

	// Rule 284: cmd ::= ATTACH database_kw_opt expr AS expr key_opt
	case 284:
		pathExpr := getExpr(getRHS(p, ruleNo, 3))
		schemaExpr := getExpr(getRHS(p, ruleNo, 5))
		path := ""
		if lit, ok := pathExpr.(*sql.StringLit); ok {
			path = lit.Value
		}
		schema := ""
		if lit, ok := schemaExpr.(*sql.StringLit); ok {
			schema = lit.Value
		} else if ref, ok := schemaExpr.(*sql.ColumnRef); ok {
			schema = ref.Name
		}
		return &sql.AttachStmt{Path: path, PathExpr: pathExpr, Schema: schema}

	// Rule 285: cmd ::= DETACH database_kw_opt expr
	// DETACH is a separate production from ATTACH (rule 284); the optional
	// DATABASE keyword is database_kw_opt and the database name arrives as an
	// expr (rule 180 yields a *sql.ColumnRef for a bare name).
	case 285:
		schema := ""
		if ref, ok := getRHS(p, ruleNo, 3).(*sql.ColumnRef); ok {
			schema = ref.Name
		} else {
			schema = getString(getRHS(p, ruleNo, 3))
		}
		return &sql.AttachStmt{IsDetach: true, Schema: schema}

	// Rule 288: cmd ::= REINDEX
	case 288:
		return &sql.ReindexStmt{}

	// Rule 289: cmd ::= REINDEX nm dbnm
	// REINDEX with an optional [schema.]name target (dbnm may be empty, rule
	// 116, or ".name", rule 117). The exec handler is a no-op, so the name is
	// not retained.
	case 289:
		return &sql.ReindexStmt{}

	// Rule 290: cmd ::= ANALYZE
	case 290:
		return &sql.AnalyzeStmt{}

	// Rule 291: cmd ::= ANALYZE nm dbnm
	// ANALYZE with a table/index name (and optional schema qualifier). The
	// name is the 2nd RHS element; a schema qualifier is the 3rd.
	case 291:
		name := getString(getRHS(p, ruleNo, 2))
		if schema := getString(getRHS(p, ruleNo, 3)); schema != "" {
			name = schema + "." + name
		}
		return &sql.AnalyzeStmt{Name: name}

	// Rule 292: cmd ::= ALTER TABLE fullname RENAME TO nm
	case 292:
		return &sql.AlterTableStmt{
			Table:   getString(getRHS(p, ruleNo, 3)),
			Action:  "RENAME",
			NewName: getString(getRHS(p, ruleNo, 6)),
		}

	// Rule 293: cmd ::= alter_add carglist
	// ALTER TABLE ... ADD COLUMN: combine the column name/type from alter_add
	// with the constraints from carglist into a full ColumnDef.
	case 293:
		ai := getAlterAddInfo(getRHS(p, ruleNo, 1))
		cols := getColumnList(getRHS(p, ruleNo, 2))
		cd := sql.ColumnDef{Name: ai.name, Type: ai.typ}
		for _, c := range cols {
			mergeColumnDef(&cd, c)
		}
		return &sql.AlterTableStmt{
			Table:  ai.table,
			Action: "ADD",
			ColDef: cd,
		}

	// Rule 294: alter_add ::= ALTER TABLE fullname ADD kwcolumn_opt nm typetoken
	case 294:
		return &alterAddInfo{
			table: getString(getRHS(p, ruleNo, 3)),
			name:  getString(getRHS(p, ruleNo, 6)),
			typ:   getString(getRHS(p, ruleNo, 7)),
		}

	// Rule 295: cmd ::= ALTER TABLE fullname DROP kwcolumn_opt nm
	case 295:
		return &sql.AlterTableStmt{
			Table:  getString(getRHS(p, ruleNo, 3)),
			Action: "DROP",
			Column: getString(getRHS(p, ruleNo, 6)),
		}

	// Rule 296: cmd ::= ALTER TABLE fullname RENAME kwcolumn_opt nm TO nm
	case 296:
		return &sql.AlterTableStmt{
			Table:   getString(getRHS(p, ruleNo, 3)),
			Action:  "RENAME",
			Column:  getString(getRHS(p, ruleNo, 6)),
			NewName: getString(getRHS(p, ruleNo, 8)),
		}

	// Rule 297: cmd ::= ALTER TABLE fullname DROP CONSTRAINT nm
	case 297:
		return &sql.AlterTableStmt{
			Table:   getString(getRHS(p, ruleNo, 3)),
			Action:  "DROP",
			Column:  "CONSTRAINT",
			NewName: getString(getRHS(p, ruleNo, 6)),
		}

	// Rules 298-299: ALTER COLUMN DROP/SET NOT NULL
	case 298:
		return &sql.AlterTableStmt{
			Table:          getString(getRHS(p, ruleNo, 3)),
			Action:         "ALTER",
			Column:         getString(getRHS(p, ruleNo, 6)),
			AlterColAction: "DROP NOT NULL",
		}
	case 299:
		return &sql.AlterTableStmt{
			Table:          getString(getRHS(p, ruleNo, 3)),
			Action:         "ALTER",
			Column:         getString(getRHS(p, ruleNo, 6)),
			AlterColAction: "SET NOT NULL",
		}

		// Rules 300-301: ALTER TABLE ADD [CONSTRAINT nm] CHECK(expr)
	// Rule 300: cmd ::= ALTER TABLE fullname ADD CONSTRAINT nm CHECK LP expr RP onconf
	case 300:
		return &sql.AlterTableStmt{
			Table:  getString(getRHS(p, ruleNo, 3)),
			Action: "ADD",
			NewConstraint: &sql.TableConstraint{
				Type: sql.ConstraintCheck,
				Name: getString(getRHS(p, ruleNo, 6)),
				Expr: getExpr(getRHS(p, ruleNo, 9)),
			},
		}
	// Rule 301: cmd ::= ALTER TABLE fullname ADD CHECK LP expr RP onconf
	case 301:
		return &sql.AlterTableStmt{
			Table:  getString(getRHS(p, ruleNo, 3)),
			Action: "ADD",
			NewConstraint: &sql.TableConstraint{
				Type: sql.ConstraintCheck,
				Expr: getExpr(getRHS(p, ruleNo, 7)),
			},
		}

	// Rule 302: cmd ::= create_vtab
	case 302:
		return getRHS(p, ruleNo, 1)

	// Rule 303: cmd ::= create_vtab LP vtabarglist RP
	case 303:
		vt, _ := getRHS(p, ruleNo, 1).(*sql.CreateVirtualTableStmt)
		if vt != nil {
			vt.Args = getStringList(getRHS(p, ruleNo, 3))
		}
		return getRHS(p, ruleNo, 1)

	// Rule 304: create_vtab ::= createkw VIRTUAL TABLE ifnotexists nm dbnm USING nm
	case 304:
		name := getString(getRHS(p, ruleNo, 5))
		module := getString(getRHS(p, ruleNo, 8))
		return &sql.CreateVirtualTableStmt{
			Name:   name,
			Module: module,
		}

	// Rule 305: vtabarg ::= (empty) — the base of the token-accumulating
	// vtabarg nonterminal. Empty so multi-token arguments build up.
	case 305:
		return ""

	// Rule 306: token ::= ID (a single virtual-table argument token)
	case 306:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 403: vtabarglist ::= vtabarg
	case 403:
		return []string{getString(getRHS(p, ruleNo, 1))}

	// Rule 404: vtabarglist ::= vtabarglist COMMA vtabarg
	case 404:
		head := getStringList(getRHS(p, ruleNo, 1))
		arg := getString(getRHS(p, ruleNo, 3))
		return append(head, arg)

	// Rule 405: vtabarg ::= vtabarg token
	case 405:
		return strings.TrimSpace(getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2)))

	// Rule 348: input ::= cmdlist
	case 348:
		return nil

	// Rule 349: cmdlist ::= cmdlist ecmd
	case 349:
		return getRHS(p, ruleNo, 1)

	// Rule 350: cmdlist ::= ecmd
	case 350:
		return getRHS(p, ruleNo, 1)

	// Rule 351: ecmd ::= SEMI
	case 351:
		return nil

	// Rule 352: ecmd ::= cmdx SEMI
	case 352:
		return getRHS(p, ruleNo, 1)

	// Rule 353: ecmd ::= explain cmdx SEMI (EXPLAIN)
	case 353:
		queryPlan := false
		if b, ok := getRHS(p, ruleNo, 1).(bool); ok {
			queryPlan = b
		}
		return &sql.ExplainStmt{
			Statement: getStmt(getRHS(p, ruleNo, 2)),
			QueryPlan: queryPlan,
		}

	// Rule 354: trans_opt ::=
	case 354:
		return nil

	// Rule 355: trans_opt ::= TRANSACTION
	case 355:
		return nil

	// Rule 359: cmd ::= create_table create_table_args
	case 359:
		ct, _ := getRHS(p, ruleNo, 1).(*sql.CreateTableStmt)
		args := getRHS(p, ruleNo, 2)
		if ct != nil {
			if cta, ok := args.(*createTableArgs); ok {
				ct.Columns = cta.columns
				ct.Constraints = cta.constraints
				ct.WithoutRowid = cta.withoutRowid
				ct.Strict = cta.strict
			} else if cols, ok := args.([]sql.ColumnDef); ok {
				ct.Columns = cols
			} else if ct2, ok := args.(*sql.CreateTableStmt); ok && ct2 != nil {
				ct.Columns = ct2.Columns
				ct.AsSelect = ct2.AsSelect
			}
		}
		if ct != nil {
			return ct
		}
		return getRHS(p, ruleNo, 1)

	// Rule 360: table_option_set ::= table_option
	case 360:
		return getRHS(p, ruleNo, 1)

	// Rule 361: columnlist ::= columnlist COMMA columnname carglist
	case 361:
		acc := getColumnList(getRHS(p, ruleNo, 1))
		col := getColumnDef(getRHS(p, ruleNo, 3))
		mergeColumnConstraints(&col, getColumnList(getRHS(p, ruleNo, 4)))
		return append(acc, col)

	// Rule 362: columnlist ::= columnname carglist
	case 362:
		col := getColumnDef(getRHS(p, ruleNo, 1))
		mergeColumnConstraints(&col, getColumnList(getRHS(p, ruleNo, 2)))
		return []sql.ColumnDef{col}

	// Rule 363: nm ::= ID|INDEXED|JOIN_KW
	case 363:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 364: nm ::= STRING
	case 364:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 365: typetoken ::= typename
	case 365:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 366: typename ::= ID|STRING
	case 366:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 369: carglist ::= carglist ccons
	case 369:
		acc := getColumnList(getRHS(p, ruleNo, 1))
		if c, ok := getRHS(p, ruleNo, 2).(sql.ColumnDef); ok {
			acc = append(acc, c)
		}
		return acc

	// Rule 370: carglist ::=
	case 370:
		return nil

	// Rule 374: conslist_opt ::= COMMA conslist
	case 374:
		return getConstraintSlice(getRHS(p, ruleNo, 2))

	// Rule 375: conslist ::= conslist tconscomma tcons
	case 375:
		acc := getConstraintSlice(getRHS(p, ruleNo, 1))
		tc, _ := getRHS(p, ruleNo, 3).(sql.TableConstraint)
		// Attach a preceding CONSTRAINT-name marker.
		if len(acc) > 0 && acc[len(acc)-1].Type == "" && tc.Type != "" {
			tc.Name = acc[len(acc)-1].Name
			acc = acc[:len(acc)-1]
		}
		if tc.Type != "" || tc.Name != "" {
			acc = append(acc, tc)
		}
		return acc

	// Rule 376: conslist ::= tcons
	case 376:
		return getConstraintSlice(getRHS(p, ruleNo, 1))

	// Rule 377: tconscomma ::= (empty)
	case 377:
		return nil

	// Rule 380: selectnowith ::= oneselect (already handled, but keep for pass-through)
	case 380:
		return nil

	// Rule 381: oneselect ::= values
	case 381:
		sel := getSelectStmt(getRHS(p, ruleNo, 1))
		if sel != nil {
			sel.ValuesChain = true
		}
		return sel

	// Rule 383: as ::= ID|STRING
	case 383:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return tok.Value
		}
		return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

	// Rule 386: expr ::= term
	case 386:
		return getRHS(p, ruleNo, 1)

		// Rule 387: likeop ::= LIKE_KW|MATCH
	case 387:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			switch strings.ToUpper(tok.Value) {
			case "MATCH":
				return "MATCH"
			case "GLOB":
				return "GLOB"
			case "REGEXP":
				return "REGEXP"
			}
		}
		return "LIKE"

	// Rule 389: exprlist ::= nexprlist
	case 389:
		return getExprList(getRHS(p, ruleNo, 1))

	// Rule 395: plus_num ::= INTEGER|FLOAT
	case 395:
		if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
			return &sql.NumericLit{Value: tok.Value}
		}
		return getRHS(p, ruleNo, 1)

	// Rule 309: with ::= WITH wqlist
	// The wqlist value is []sql.CTEDef; propagate it as the with value so
	// INSERT (rule 164) can attach the CTEs.
	case 309:
		return getCTEDefs(getRHS(p, ruleNo, 2))

	// Rule 310: with ::= WITH RECURSIVE wqlist
	// Mark every CTE as recursive (WITH RECURSIVE applies to the whole list).
	case 310:
		defs := getCTEDefs(getRHS(p, ruleNo, 3))
		for i := range defs {
			defs[i].Recursive = true
		}
		return defs

	// Rule 311: wqas ::= AS
	// The materialization hint (MATERIALIZED / NOT MATERIALIZED) is not
	// modeled; pass through a marker value.
	case 311:
		return true

	// Rule 314: wqitem ::= withnm eidlist_opt wqas LP select RP
	case 314:
		name := getString(getRHS(p, ruleNo, 1))
		cols := getStringList(getRHS(p, ruleNo, 2))
		sel := getSelectStmt(getRHS(p, ruleNo, 5))
		return sql.CTEDef{Name: name, Columns: cols, Select: sel}

	// Rule 315: withnm ::= nm
	case 315:
		return getRHS(p, ruleNo, 1)

	// Rule 316: wqlist ::= wqitem
	case 316:
		if d, ok := getRHS(p, ruleNo, 1).(sql.CTEDef); ok {
			return []sql.CTEDef{d}
		}
		return nil

	// Rule 317: wqlist ::= wqlist COMMA wqitem
	case 317:
		defs := getCTEDefs(getRHS(p, ruleNo, 1))
		if d, ok := getRHS(p, ruleNo, 3).(sql.CTEDef); ok {
			return append(defs, d)
		}
		return defs

	// Rule 409: with ::=
	case 409:
		return nil

	// Rule 318: windowdefn_list ::= windowdefn_list COMMA windowdefn
	case 318:
		list := getWindowDefList(getRHS(p, ruleNo, 1))
		wd := getWindowDef(getRHS(p, ruleNo, 3))
		if wd != nil {
			list = append(list, *wd)
		}
		return list

	// Rule 319: windowdefn ::= nm AS LP window RP
	case 319:
		name := getString(getRHS(p, ruleNo, 1))
		inner := getWindowDef(getRHS(p, ruleNo, 4))
		if inner != nil {
			inner.Name = name
			return inner
		}
		return &sql.WindowDef{Name: name}

	// Rule 320: window ::= PARTITION BY nexprlist orderby_opt frame_opt
	case 320:
		return &sql.WindowDef{
			Partitions: getExprList(getRHS(p, ruleNo, 3)),
			OrderBy:    getOrderByList(getRHS(p, ruleNo, 4)),
			FrameSpec:  getString(getRHS(p, ruleNo, 5)),
		}

	// Rule 321: window ::= nm PARTITION BY nexprlist orderby_opt frame_opt
	case 321:
		return &sql.WindowDef{
			Name:       getString(getRHS(p, ruleNo, 1)),
			Partitions: getExprList(getRHS(p, ruleNo, 4)),
			OrderBy:    getOrderByList(getRHS(p, ruleNo, 5)),
			FrameSpec:  getString(getRHS(p, ruleNo, 6)),
		}

	// Rule 322: window ::= ORDER BY sortlist frame_opt
	case 322:
		return &sql.WindowDef{
			OrderBy:   getOrderByList(getRHS(p, ruleNo, 3)),
			FrameSpec: getString(getRHS(p, ruleNo, 4)),
		}

	// Rule 323: window ::= nm ORDER BY sortlist frame_opt
	case 323:
		return &sql.WindowDef{
			Name:      getString(getRHS(p, ruleNo, 1)),
			OrderBy:   getOrderByList(getRHS(p, ruleNo, 4)),
			FrameSpec: getString(getRHS(p, ruleNo, 5)),
		}

	// Rule 324: window ::= nm frame_opt
	case 324:
		return &sql.WindowDef{
			Name:      getString(getRHS(p, ruleNo, 1)),
			FrameSpec: getString(getRHS(p, ruleNo, 2)),
		}

	// Rule 325: frame_opt ::=
	case 325:
		return ""

	// Rule 326: frame_opt ::= range_or_rows frame_bound_s frame_exclude_opt
	case 326:
		return frameSpecFromParts(
			getString(getRHS(p, ruleNo, 1)),
			getString(getRHS(p, ruleNo, 2)),
			getString(getRHS(p, ruleNo, 3)),
		)

	// Rule 327: frame_opt ::= range_or_rows BETWEEN frame_bound_s AND frame_bound_e frame_exclude_opt
	case 327:
		spec := frameSpecFromParts(
			getString(getRHS(p, ruleNo, 1)),
			"BETWEEN",
			getString(getRHS(p, ruleNo, 3)),
			"AND",
			getString(getRHS(p, ruleNo, 5)),
		)
		if excl := getString(getRHS(p, ruleNo, 6)); excl != "" {
			spec += " " + excl
		}
		return spec

	// Rule 328: range_or_rows ::= RANGE|ROWS|GROUPS
	case 328:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 329: frame_bound_s ::= frame_bound
	case 329:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 330: frame_bound_s ::= UNBOUNDED PRECEDING
	case 330:
		return "UNBOUNDED PRECEDING"

	// Rule 331: frame_bound_e ::= frame_bound
	case 331:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 332: frame_bound_e ::= UNBOUNDED FOLLOWING
	case 332:
		return "UNBOUNDED FOLLOWING"

	// Rule 333: frame_bound ::= expr PRECEDING|FOLLOWING
	case 333:
		expr := getExpr(getRHS(p, ruleNo, 1))
		dir := getString(getRHS(p, ruleNo, 2))
		return sql.ExprString(expr) + " " + dir

	// Rule 334: frame_bound ::= CURRENT ROW
	case 334:
		return "CURRENT ROW"

	// Rule 335: frame_exclude_opt ::=
	case 335:
		return ""

	// Rule 336: frame_exclude_opt ::= EXCLUDE frame_exclude
	case 336:
		return "EXCLUDE " + getString(getRHS(p, ruleNo, 2))

	// Rule 337: frame_exclude ::= NO OTHERS
	case 337:
		return "NO OTHERS"

	// Rule 338: frame_exclude ::= CURRENT ROW
	case 338:
		return "CURRENT ROW"

	// Rule 339: frame_exclude ::= GROUP|TIES
	case 339:
		return getString(getRHS(p, ruleNo, 1))

	// Rule 340: window_clause ::= WINDOW windowdefn_list
	case 340:
		return getWindowDefList(getRHS(p, ruleNo, 2))

	// Rule 341: filter_over ::= filter_clause over_clause
	case 341:
		return &windowFilter{
			filter: getExpr(getRHS(p, ruleNo, 1)),
			over:   getWindowDef(getRHS(p, ruleNo, 2)),
		}

	// Rule 342: filter_over ::= over_clause
	case 342:
		return &windowFilter{
			over: getWindowDef(getRHS(p, ruleNo, 1)),
		}

	// Rule 343: filter_over ::= filter_clause
	case 343:
		return &windowFilter{
			filter: getExpr(getRHS(p, ruleNo, 1)),
		}

	// Rule 344: over_clause ::= OVER LP window RP
	case 344:
		// The LALR tables fold the empty window (OVER ()) into
		// "OVER LP frame_opt RP": rh3 is then a frame-spec string
		// rather than a *sql.WindowDef (rule 411 never reduces for the
		// empty case). Accept both shapes.
		if wd := getWindowDef(getRHS(p, ruleNo, 3)); wd != nil {
			return wd
		}
		return &sql.WindowDef{FrameSpec: getString(getRHS(p, ruleNo, 3))}

	// Rule 345: over_clause ::= OVER nm
	case 345:
		return &sql.WindowDef{Name: getString(getRHS(p, ruleNo, 2))}

	// Rule 346: filter_clause ::= FILTER LP WHERE expr RP
	case 346:
		return getExpr(getRHS(p, ruleNo, 4))

	// Rule 410: windowdefn_list ::= windowdefn
	case 410:
		wd := getWindowDef(getRHS(p, ruleNo, 1))
		if wd != nil {
			return []sql.WindowDef{*wd}
		}
		return nil

	// Rule 411: window ::= frame_opt
	case 411:
		return &sql.WindowDef{FrameSpec: getString(getRHS(p, ruleNo, 1))}

	default:
		// For unhandled rules, pass through the first RHS value only if the rule has RHS symbols
		if p.pos >= 1 {
			t := p.tables
			if ruleNo < len(t.RuleInfoNRhs) && t.RuleInfoNRhs[ruleNo] != 0 {
				return getRHS(p, ruleNo, 1)
			}
		}
		return nil
	}
}

// decodeBlobToken decodes the hex content of a blob literal token
// (e.g., "00AB" from X'00AB') into a BlobLit. Empty and odd-length hex
// strings are handled like the hand-written parser's decodeBlobLiteral.
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

// mergeColumnConstraints merges per-column constraints (produced by the
// carglist/ccons rules) into the base column definition.
func mergeColumnConstraints(dst *sql.ColumnDef, cons []sql.ColumnDef) {
	if dst == nil {
		return
	}
	for _, c := range cons {
		dst.NotNull = dst.NotNull || c.NotNull
		dst.PrimaryKey = dst.PrimaryKey || c.PrimaryKey
		dst.AutoInc = dst.AutoInc || c.AutoInc
		dst.Unique = dst.Unique || c.Unique
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
}

func getTableRef(v interface{}) sql.TableRef {
	if v == nil {
		return sql.TableRef{}
	}
	if t, ok := v.(sql.TableRef); ok {
		return t
	}
	return sql.TableRef{}
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

// appendTable adds a table to the accumulator. If a join operator is pending,
// the new table becomes a JoinClause; otherwise it becomes the first table.
func (a *seltablistAcc) appendTable(ref sql.TableRef) *seltablistAcc {
	return a.appendTableWithOn(ref, nil, nil)
}

// appendTableWithOn adds a table, attaching an optional ON condition and/or
// USING column list to the join clause that links it to the previous table.
func (a *seltablistAcc) appendTableWithOn(ref sql.TableRef, on sql.Expr, using []string) *seltablistAcc {
	if !a.HasFirst {
		a.First = ref
		a.HasFirst = true
		return a
	}
	jc := sql.JoinClause{Table: ref, CommaJoin: a.PendingOp.Comma, On: on, Using: using}
	switch a.PendingOp.Kind {
	case "LEFT", "LEFT OUTER":
		jc.JoinType = "LEFT"
	case "RIGHT", "RIGHT OUTER":
		jc.JoinType = "RIGHT"
	case "FULL", "FULL OUTER":
		jc.JoinType = "FULL"
	case "INNER":
		jc.JoinType = "INNER"
	case "CROSS":
		jc.JoinType = "CROSS"
	case "NATURAL":
		jc.JoinType = "NATURAL"
	case "NATURAL LEFT", "NATURAL LEFT OUTER":
		jc.JoinType = "NATURAL LEFT"
	case "NATURAL RIGHT", "NATURAL RIGHT OUTER":
		jc.JoinType = "NATURAL RIGHT"
	case "NATURAL FULL", "NATURAL FULL OUTER":
		jc.JoinType = "NATURAL FULL"
	case "NATURAL INNER":
		jc.JoinType = "NATURAL INNER"
	case "NATURAL CROSS":
		jc.JoinType = "NATURAL CROSS"
	default:
		jc.JoinType = "CROSS"
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
func appendSeltablistTable(p *Parser, ruleNo, posName, posSchema, posAlias, posOn int) *seltablistAcc {
	acc := getSeltablist(getRHS(p, ruleNo, 1))
	tbl := getString(getRHS(p, ruleNo, posName))
	schema := getString(getRHS(p, ruleNo, posSchema))
	alias := getString(getRHS(p, ruleNo, posAlias))
	on, using := getOnUsing(getRHS(p, ruleNo, posOn))
	if schema != "" {
		tbl = tbl + "." + schema
	}
	return acc.appendTableWithOn(sql.TableRef{Name: tbl, As: alias}, on, using)
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

// getCTEDef extracts a single sql.CTEDef from a wqitem value.
func getCTEDef(v interface{}) sql.CTEDef {
	if v == nil {
		return sql.CTEDef{}
	}
	if d, ok := v.(sql.CTEDef); ok {
		return d
	}
	return sql.CTEDef{}
}

// getSeltablist extracts a seltablistAcc from a stack value, creating an empty
// one if the value is a plain TableRef (backward compat for rules that return
// TableRef directly).
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

// conflictTargetColumn extracts the single-column conflict target from an
// ON CONFLICT (...) sortlist, joining multi-column targets with commas.
func conflictTargetColumn(terms []sql.OrderByTerm) string {
	var names []string
	for _, t := range terms {
		if ref, ok := t.Expr.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
		}
	}
	return strings.Join(names, ",")
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

// getWindowDefList extracts a []sql.WindowDef from a parser stack value.
func getWindowDefList(v interface{}) []sql.WindowDef {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.WindowDef); ok {
		return list
	}
	return nil
}

// frameSpecFromParts joins frame clause parts into a single frame spec
// string, skipping empty optional parts.
func frameSpecFromParts(parts ...string) string {
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(p)
	}
	return sb.String()
}

// boolLitName returns "TRUE" or "FALSE" if the expression is a boolean
// literal column reference (the LALR parser represents TRUE/FALSE keywords as
// ColumnRef{Name:"TRUE"} / ColumnRef{Name:"FALSE"}), and whether it matched.
func boolLitName(e sql.Expr) (string, bool) {
	ref, ok := e.(*sql.ColumnRef)
	if !ok {
		return "", false
	}
	if ref.Name == "TRUE" || ref.Name == "FALSE" {
		return ref.Name, true
	}
	return "", false
}

// getAssignments extracts a []sql.Assignment from a stack value.
func getAssignments(v interface{}) []sql.Assignment {
	if v == nil {
		return nil
	}
	if a, ok := v.([]sql.Assignment); ok {
		return a
	}
	return nil
}

// getStringList extracts a []string from a stack value.
func getStringList(v interface{}) []string {
	if v == nil {
		return nil
	}
	if list, ok := v.([]string); ok {
		return list
	}
	return nil
}

func getSetOp(v interface{}) sql.SetOp {
	if v == nil {
		return sql.SetNone
	}
	if s, ok := v.(setOpResult); ok {
		return s.Op
	}
	return sql.SetNone
}

func getWhenClauses(v interface{}) []sql.WhenClause {
	if v == nil {
		return nil
	}
	if w, ok := v.([]sql.WhenClause); ok {
		return w
	}
	return nil
}

func getCreateTable(v interface{}) *sql.CreateTableStmt {
	if v == nil {
		return nil
	}
	if ct, ok := v.(*sql.CreateTableStmt); ok {
		return ct
	}
	return nil
}

func getCreateTableArgs(v interface{}) interface{} {
	return v
}

// stripSQLComments removes SQL block and line comments, preserving strings.
func stripSQLComments(s string) string {
	var b strings.Builder
	i := 0
	n := len(s)
	for i < n {
		if s[i] == '\'' || s[i] == '"' {
			q := s[i]
			b.WriteByte(s[i])
			i++
			for i < n && s[i] != q {
				if s[i] == '\\' && i+1 < n {
					b.WriteByte(s[i])
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			if i < n {
				b.WriteByte(s[i])
				i++
			}
			continue
		}
		if i+1 < n && s[i] == '-' && s[i+1] == '-' {
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < n && s[i] == '/' && s[i+1] == '*' {
			i += 2
			for i+1 < n && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// createTableArgs is the semantic value of the create_table_args nonterminal
// (rule 19). It carries the column definitions plus any table-level
// constraints (conslist) and table options (WITHOUT ROWID / STRICT) so that
// rule 359 can fold them all into the CreateTableStmt.
type createTableArgs struct {
	columns      []sql.ColumnDef
	constraints  []sql.TableConstraint
	withoutRowid bool
	strict       bool
}

// whereRet carries the WHERE expression and optional RETURNING projection for
// DELETE and UPDATE statements. The where_opt_ret nonterminal (rules 155-158)
// produces this value, and the DELETE/UPDATE cmd rules (152, 159) consume it.
// RETURNING columns are folded into a single sql.SelectColumn (multi-expression
// RETURNING becomes a RowValue), matching the AST's single-Returning field.
type whereRet struct {
	where     sql.Expr
	returning []sql.SelectColumn
}

// upsertVal carries the ON CONFLICT clause and/or RETURNING projection that an
// INSERT ... upsert nonterminal (rules 166-171) produces. INSERT statements can
// have both ON CONFLICT ... DO ... and RETURNING (e.g.
// "INSERT ... ON CONFLICT DO UPDATE SET ... RETURNING *"), so the upsert value
// carries both into rule 164's cmd handler.
type upsertVal struct {
	onConflict *sql.OnConflictClause
	returning  []sql.SelectColumn
}

// getUpsertVal extracts an *upsertVal semantic value.
func getUpsertVal(v interface{}) *upsertVal {
	if v == nil {
		return nil
	}
	if u, ok := v.(*upsertVal); ok {
		return u
	}
	return nil
}

// getWhereRet extracts a *whereRet semantic value.
func getWhereRet(v interface{}) *whereRet {
	if v == nil {
		return nil
	}
	if w, ok := v.(*whereRet); ok {
		return w
	}
	return nil
}

// foldReturning folds a slice of SELECT columns into a single sql.SelectColumn
// with a RowValue for multi-expression RETURNING, or nil when empty.
func foldReturning(cols []sql.SelectColumn) sql.SelectColumn {
	if len(cols) == 0 {
		return sql.SelectColumn{}
	}
	if len(cols) == 1 {
		return cols[0]
	}
	exprs := make([]sql.Expr, len(cols))
	for i, c := range cols {
		exprs[i] = c.Expr
	}
	return sql.SelectColumn{Expr: &sql.RowValue{Values: exprs}}
}

// getTableConstraints extracts a []sql.TableConstraint semantic value.
func getTableConstraints(v interface{}) []sql.TableConstraint {
	if v == nil {
		return nil
	}
	if list, ok := v.([]sql.TableConstraint); ok {
		return list
	}
	return nil
}

// getConstraintsCons coerces a single sql.TableConstraint into a one-element
// slice, for use by rule 376 (conslist ::= tcons).
func getConstraintsCons(v interface{}) []sql.TableConstraint {
	if tc, ok := v.(sql.TableConstraint); ok {
		if tc.Type == "" && tc.Name == "" {
			return ([]sql.TableConstraint)(nil)
		}
		return []sql.TableConstraint{tc}
	}
	return ([]sql.TableConstraint)(nil)
}

// getConsTConstraints coerces a value that may be either a single
// sql.TableConstraint or a []sql.TableConstraint into a slice.
func getConstraintSlice(v interface{}) []sql.TableConstraint {
	if list := getTableConstraints(v); list != nil {
		return list
	}
	return getConstraintsCons(v)
}

// getTableOptions extracts the *createTableArgs carry value produced by the
// table_option_set / table_option rules, returning a zero value if absent.
func getTableOptions(v interface{}) *createTableArgs {
	if opts, ok := v.(*createTableArgs); ok {
		return opts
	}
	return &createTableArgs{}
}

// indexColumnsFromSortlist converts a sortlist ([]sql.OrderByTerm) into the
// []sql.IndexedColumn list for a PRIMARY KEY / UNIQUE table constraint.
func indexColumnsFromSortlist(v interface{}) []sql.IndexedColumn {
	terms := getOrderByList(v)
	if terms == nil {
		return nil
	}
	out := make([]sql.IndexedColumn, 0, len(terms))
	for _, t := range terms {
		name, collate := indexedColumnName(t.Expr)
		out = append(out, sql.IndexedColumn{
			Name:    name,
			Collate: collate,
			Desc:    t.Desc,
		})
	}
	return out
}

// indexedColumnName extracts the column name (and optional COLLATE) from an
// expression used in a PRIMARY KEY / UNIQUE constraint column list.
func indexedColumnName(e sql.Expr) (string, string) {
	e = sql.UnwrapParenExpr(e)
	if bo, ok := e.(*sql.BinaryOp); ok && bo.Operator == "COLLATE" {
		if sl, ok := bo.Right.(*sql.StringLit); ok {
			n, _ := indexedColumnName(bo.Left)
			return n, sl.Value
		}
	}
	if ref, ok := e.(*sql.ColumnRef); ok {
		return ref.Name, ""
	}
	// SQLite sqlite3StringToId: a bare single-quoted string literal in an
	// index/constraint key becomes an identifier ("PRIMARY KEY('x' ASC)"
	// indexes column x). CREATE INDEX (rule 239) already applies this rule;
	// table-level PRIMARY KEY / UNIQUE constraints must do the same.
	if sl, ok := e.(*sql.StringLit); ok {
		return sl.Value, ""
	}
	return "", ""
}

// fkColumnsFromEidlist converts an eidlist (FOREIGN KEY column list) into
// []sql.IndexedColumn. Only the column names are meaningful for FK purposes.
func fkColumnsFromEidlist(v interface{}) []sql.IndexedColumn {
	names := getStringList(v)
	if names == nil {
		return nil
	}
	out := make([]sql.IndexedColumn, 0, len(names))
	for _, n := range names {
		out = append(out, sql.IndexedColumn{Name: n})
	}
	return out
}
