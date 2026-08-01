package sql

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Parser turns a token stream into AST nodes.
type Parser struct {
	tokens          *Tokenizer
	cur             Token
	peek            Token
	err             error
	windowSpecDepth int // recursion guard for window spec parsing
}

// NewParser creates a parser for the given SQL text.
func NewParser(input string) *Parser {
	p := &Parser{
		tokens: NewTokenizer(input),
	}
	p.next() // initialize cur
	p.next() // initialize peek
	return p
}

// Err returns any error encountered during parsing.
func (p *Parser) Err() error {
	return p.err
}

func (p *Parser) next() {
	p.cur = p.peek
	p.peek = p.tokens.Next()
}

// readName reads an identifier or keyword as a name (possibly schema-qualified).
// Also accepts string literals (deprecated SQL syntax for identifiers).
// Returns the name with schema prefix if present (e.g., "schema.table").
func (p *Parser) readName() string {
	if p.cur.Type != TokenIdentifier && p.cur.Type != TokenKeyword && p.cur.Type != TokenString {
		return ""
	}
	var name string
	if p.cur.Type == TokenString {
		// String literal used as identifier (e.g., UPDATE 'tablename' SET ...)
		name = p.cur.Value
	} else {
		name = p.cur.Value
	}
	p.next()
	// Check for schema-qualified name: schema.name
	if p.cur.Type == TokenDot {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenStar || p.cur.Type == TokenString {
			name = name + "." + p.cur.Value
			p.next()
		} else {
			// Dot without following identifier - put it back conceptually
			return name
		}
	}
	return name
}

// readNameWithInfo reads an identifier like readName but also returns the byte
// position of the last name token in the original SQL text. For schema-qualified
// names (schema.table), the TokenInfo covers only the last part (the table name),
// not the schema prefix. This is used by ALTER TABLE RENAME for token-level replacement.
func (p *Parser) readNameWithInfo() (string, TokenInfo) {
	if p.cur.Type != TokenIdentifier && p.cur.Type != TokenKeyword && p.cur.Type != TokenString {
		return "", TokenInfo{}
	}

	var name string
	if p.cur.Type == TokenString {
		name = p.cur.Value
	} else {
		name = p.cur.Value
	}

	// Compute end of current token. For quoted identifiers, the actual text in the
	// source includes the quoting characters; the Value only has the content.
	start, end := p.computeTokenBounds()
	p.next()

	// Handle dot (schema.table) — for qualified names, position covers just the LAST part
	if p.cur.Type == TokenDot {
		p.next() // consume dot
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenStar || p.cur.Type == TokenString {
			start = p.cur.Pos
			end = p.cur.Pos + len(p.cur.Value)
			if p.cur.Type == TokenIdentifier && start < len(p.tokens.input) && (p.tokens.input[start] == '"' || p.tokens.input[start] == '`' || p.tokens.input[start] == '[') {
				end += 2 // account for opening and closing quote/backtick/bracket
			}
			name = name + "." + p.cur.Value
			p.next()
		}
		// else: dot without following identifier — return position of first name only
	}

	return name, TokenInfo{Start: start, End: end}
}

// computeTokenBounds returns the byte range of the current token in the original SQL text.
// It accounts for quoted identifiers where the Value doesn't include the quoting chars.
func (p *Parser) computeTokenBounds() (int, int) {
	start := p.cur.Pos
	end := start + len(p.cur.Value)
	// For quoted identifiers, the source text includes opening and closing quotes.
	// Check the first character of the token in the source to determine quoting.
	if p.cur.Type == TokenIdentifier && start < len(p.tokens.input) {
		ch := p.tokens.input[start]
		if ch == '"' || ch == '`' || ch == '[' {
			end += 2 // opening and closing quote/backtick/bracket
		}
	}
	return start, end
}

func (p *Parser) expect(typ TokenType) bool {
	if p.cur.Type != typ {
		p.setErr("expected %s but got %s", tokenName(typ, ""), tokenName(p.cur.Type, p.cur.Value))
		return false
	}
	p.next()
	return true
}

func (p *Parser) expectKeyword(keyword string) bool {
	if p.cur.Type != TokenKeyword || p.cur.Value != keyword {
		p.setErr("expected keyword '%s' but got '%s'", keyword, p.cur.Value)
		return false
	}
	p.next()
	return true
}

func (p *Parser) setErr(format string, args ...interface{}) {
	if p.err == nil {
		p.err = fmt.Errorf(format, args...)
	}
}

// tokenName returns a human-readable name for a token type.
// If value is non-empty, it provides context for TokenKeyword.
func tokenName(typ TokenType, value string) string {
	if typ == TokenKeyword {
		if value != "" {
			return fmt.Sprintf("keyword '%s'", value)
		}
		return "keyword"
	}
	names := map[TokenType]string{
		TokenEOF:        "end of input",
		TokenError:      "error",
		TokenIdentifier: "identifier",
		TokenString:     "string",
		TokenNumber:     "number",
		TokenBlob:       "blob",
		TokenEq:         "'='",
		TokenNeq:        "'!=' or '<>'",
		TokenLt:         "'<'",
		TokenGt:         "'>'",
		TokenLe:         "'<='",
		TokenGe:         "'>='",
		TokenPlus:       "'+'",
		TokenMinus:      "'-'",
		TokenStar:       "'*'",
		TokenSlash:      "'/'",
		TokenArrow:      "'->'",
		TokenDoubleArrow: "'->>'",
		TokenMod:        "'%'",
		TokenBitAnd:     "'&'",
		TokenLParen:     "'('",
		TokenRParen:     "')'",
		TokenComma:      "','",
		TokenSemicolon:  "';'",
		TokenDot:        "'.'",
		TokenConcat:     "'||'",
		TokenParam:      "'?'",
	}
	if name, ok := names[typ]; ok {
		return name
	}
	return fmt.Sprintf("token %d", typ)
}

// Parse parses a list of statements.
func (p *Parser) Parse() StmtList {
	var stmts StmtList
	for p.cur.Type != TokenEOF {
		if p.cur.Type == TokenSemicolon {
			p.next()
			continue
		}
		stmt := p.parseStatement()
		if stmt == nil {
			break
		}
		stmts = append(stmts, stmt)
		if p.cur.Type == TokenSemicolon {
			p.next()
		}
	}
	return stmts
}

func (p *Parser) parseStatement() Stmt {
	switch p.cur.Type {
	case TokenKeyword:
		return p.parseKeywordStmt()
	case TokenParam:
		// $param or ? as a statement (e.g., $sql)
		p.next()
		return &RollbackStmt{} // placeholder
	case TokenLParen:
		// (SELECT ...) or (VALUES ...) as a top-level statement
		sel := p.parseSelectBody()
		return sel
	default:
		p.setErr("unexpected token: %s", tokenName(p.cur.Type, p.cur.Value))
		return nil
	}
}

func (p *Parser) parseKeywordStmt() Stmt {
	switch p.cur.Value {
	case "SELECT":
		return p.parseSelect()
	case "INSERT":
		return p.parseInsert(false)
	case "REPLACE":
		// REPLACE INTO is equivalent to INSERT OR REPLACE INTO
		return p.parseInsert(true)
	case "UPDATE":
		return p.parseUpdate()
	case "DELETE":
		return p.parseDelete()
	case "CREATE":
		return p.parseCreate()
	case "DROP":
		return p.parseDrop()
	case "ALTER":
		return p.parseAlter()
	case "WITH":
		return p.parseWithStatement()
	default:
		return p.parseKeywordStmtTail()
	}
}

func (p *Parser) parseKeywordStmtTail() Stmt {
	switch p.cur.Value {
	case "BEGIN":
		return p.parseBegin()
	case "COMMIT":
		return p.parseCommit()
	case "ROLLBACK":
		return p.parseRollback()
	case "PRAGMA":
		return p.parsePragma()
	case "ATTACH":
		return p.parseAttach()
	case "DETACH":
		return p.parseDetach()
	case "VACUUM":
		return p.parseVacuum()
	case "REINDEX":
		return p.parseReindex()
	case "SAVEPOINT":
		return p.parseSavepoint()
	case "RELEASE":
		return p.parseSavepoint()
	case "EXPLAIN":
		return p.parseExplain()
	case "ANALYZE":
		return p.parseAnalyze()
	case "END":
		return p.parseEndAsCommit()
	case "VALUES":
		return p.parseValuesSubquery()
	default:
		p.setErr("unexpected keyword: %s", p.cur.Value)
		return nil
	}
}

func (p *Parser) parseEndAsCommit() Stmt {
	p.next()
	if p.cur.Type == TokenKeyword && p.cur.Value == "TRANSACTION" {
		p.next()
	}
	return &CommitStmt{}
}  

func (p *Parser) parseExplain() Stmt {
	p.next() // skip EXPLAIN
	e := &ExplainStmt{}
	// EXPLAIN QUERY PLAN is a variant
	if p.cur.Type == TokenKeyword && p.cur.Value == "QUERY" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "PLAN" {
			p.next()
			e.QueryPlan = true
		}
	}
	stmt := p.parseStatement()
	if stmt == nil {
		return nil
	}
	e.Statement = stmt
	return e
}

func (p *Parser) parseAnalyze() Stmt {
	p.next() // skip ANALYZE
	s := &AnalyzeStmt{}
	if name := p.readName(); name != "" {
		s.Name = name
	}
	return s
}

// SELECT
func (p *Parser) parseSelect() *SelectStmt {
	s := &SelectStmt{}
	p.next() // skip SELECT

	if p.cur.Type == TokenKeyword && p.cur.Value == "DISTINCT" {
		s.Distinct = true
		p.next()
	}

	s.Columns = p.parseSelectColumns()
	p.parseSelectFrom(s)
	p.parseSelectJoins(s)
	p.parseSelectWhere(s)
	p.parseSelectGroupBy(s)

	// WINDOW clause: WINDOW name AS (window_spec), ...
	p.parseSelectWindow(s)

	// UNION / INTERSECT / EXCEPT
	if p.cur.Type == TokenKeyword && (p.cur.Value == "UNION" || p.cur.Value == "INTERSECT" || p.cur.Value == "EXCEPT") {
		switch p.cur.Value {
		case "UNION":
			s.SetOp = SetUnion
			s.UnionAll = p.peekType(TokenKeyword, "ALL")
			if s.UnionAll {
				p.next() // skip ALL
			}
		case "INTERSECT":
			s.SetOp = SetIntersect
		case "EXCEPT":
			s.SetOp = SetExcept
		}
		p.next() // skip UNION/INTERSECT/EXCEPT
		s.Union = p.parseSelectBody()
	}

	// ORDER BY and LIMIT apply to the outermost SELECT (or the compound result)
	p.parseSelectOrderBy(s)

	// If ORDER BY was consumed and there's another compound operator following,
	// the ORDER BY was in the wrong place (between compound operators).
	if len(s.OrderBy) > 0 && p.cur.Type == TokenKeyword &&
		(p.cur.Value == "UNION" || p.cur.Value == "INTERSECT" || p.cur.Value == "EXCEPT") {
		p.setErr("ORDER BY clause should come after %s not before", p.cur.Value)
		return nil
	}

	p.parseSelectLimit(s)

	return s
}

// parseSelectBody parses a SELECT statement body without consuming ORDER BY, LIMIT,
// or compound UNION/INTERSECT/EXCEPT operators. Used for compound SELECT sub-queries.
// It handles SELECT, VALUES(...), and parenthesized compound terms.
// It recursively handles compound operators to build the correct tree structure.
func (p *Parser) parseSelectBody() *SelectStmt {
	// Handle VALUES(...) as a compound term
	if p.cur.Type == TokenKeyword && p.cur.Value == "VALUES" {
		return p.parseValuesSubquery()
	}

	// Handle (SELECT ...) or (VALUES ...) as a compound term
	if p.cur.Type == TokenLParen {
		p.next()
		inner := p.parseSelectBody()
		p.expect(TokenRParen)

		// Check for compound operators after the closing paren
		if p.cur.Type == TokenKeyword && (p.cur.Value == "UNION" || p.cur.Value == "INTERSECT" || p.cur.Value == "EXCEPT") {
			s := &SelectStmt{}
			// Copy the inner select into the new compound select
			*s = *inner

			switch p.cur.Value {
			case "UNION":
				s.SetOp = SetUnion
				s.UnionAll = p.peekType(TokenKeyword, "ALL")
				if s.UnionAll {
					p.next() // skip ALL
				}
			case "INTERSECT":
				s.SetOp = SetIntersect
			case "EXCEPT":
				s.SetOp = SetExcept
			}
			p.next() // skip UNION/INTERSECT/EXCEPT
			s.Union = p.parseSelectBody()
			return s
		}

		return inner
	}

	s := &SelectStmt{}
	p.next() // skip SELECT

	// Handle DISTINCT
	if p.cur.Type == TokenKeyword && p.cur.Value == "DISTINCT" {
		s.Distinct = true
		p.next()
	}

	s.Columns = p.parseSelectColumns()
	p.parseSelectFrom(s)
	p.parseSelectJoins(s)
	p.parseSelectWhere(s)
	p.parseSelectGroupBy(s)
	p.parseSelectWindow(s)

	// Handle compound operators recursively (still without ORDER BY/LIMIT)
	if p.cur.Type == TokenKeyword && (p.cur.Value == "UNION" || p.cur.Value == "INTERSECT" || p.cur.Value == "EXCEPT") {
		switch p.cur.Value {
		case "UNION":
			s.SetOp = SetUnion
			s.UnionAll = p.peekType(TokenKeyword, "ALL")
			if s.UnionAll {
				p.next() // skip ALL
			}
		case "INTERSECT":
			s.SetOp = SetIntersect
		case "EXCEPT":
			s.SetOp = SetExcept
		}
		p.next() // skip UNION/INTERSECT/EXCEPT
		s.Union = p.parseSelectBody()
	}

	return s
}

func (p *Parser) parseSelectJoins(s *SelectStmt) {
	for {
		if p.cur.Type == TokenKeyword && (p.cur.Value == "JOIN" || p.cur.Value == "INNER" || p.cur.Value == "LEFT" || p.cur.Value == "RIGHT" || p.cur.Value == "CROSS" || p.cur.Value == "NATURAL" || p.cur.Value == "FULL") {
			j := p.parseJoinClause()
			s.Joins = append(s.Joins, j)
		} else if p.cur.Type == TokenComma {
			// Comma-separated table references: FROM t1, t2
			// Creates an implicit CROSS JOIN
			p.next()
			table := p.parseTableRef()
			s.Joins = append(s.Joins, JoinClause{Table: table, JoinType: "CROSS", CommaJoin: true})
		} else {
			break
		}
	}
}

func (p *Parser) parseJoinClause() JoinClause {
	j := JoinClause{}
	j.JoinType = p.parseJoinType()
	j.Table = p.parseTableRef()
	if p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.next()
		j.On = p.parseExpr()
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "USING" {
		j.On = p.parseUsingClause()
	}
	return j
}

// parseJoinType reads the join type keyword (INNER, LEFT, RIGHT, CROSS, NATURAL, or plain JOIN).
func (p *Parser) parseJoinType() string {
	switch p.cur.Value {
	case "INNER":
		p.next()
		p.expectKeyword("JOIN")
		return "INNER"
	case "LEFT":
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OUTER" {
			p.next()
		}
		p.expectKeyword("JOIN")
		return "LEFT"
	case "RIGHT":
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OUTER" {
			p.next()
		}
		p.expectKeyword("JOIN")
		return "RIGHT"
	case "CROSS":
		p.next()
		p.expectKeyword("JOIN")
		return "CROSS"
	case "FULL":
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OUTER" {
			p.next()
		}
		p.expectKeyword("JOIN")
		return "FULL"
	case "NATURAL":
		return p.parseNaturalJoinType()
	default:
		p.expectKeyword("JOIN")
		return ""
	}
}

// parseNaturalJoinType handles NATURAL [LEFT|RIGHT|INNER|FULL|CROSS] [OUTER] JOIN.
func (p *Parser) parseNaturalJoinType() string {
	p.next()
	if p.cur.Type == TokenKeyword && (p.cur.Value == "LEFT" || p.cur.Value == "RIGHT" || p.cur.Value == "INNER" || p.cur.Value == "FULL" || p.cur.Value == "CROSS") {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OUTER" {
			p.next()
		}
	}
	p.expectKeyword("JOIN")
	return "NATURAL"
}

// parseUsingClause converts JOIN ... USING (col1, col2) into ON left.col = right.col AND ...
func (p *Parser) parseUsingClause() Expr {
	p.next() // skip USING
	if p.cur.Type != TokenLParen {
		return nil
	}
	p.next()
	var cols []string
	for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		cols = append(cols, p.cur.Value)
		p.next()
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	if p.cur.Type == TokenRParen {
		p.next()
	}
	if len(cols) == 0 {
		return nil
	}
	var onExpr Expr
	for _, col := range cols {
		leftRef := &ColumnRef{Name: col}
		rightRef := &ColumnRef{Name: col}
		eq := &BinaryOp{Left: leftRef, Right: rightRef, Operator: "="}
		if onExpr == nil {
			onExpr = eq
		} else {
			onExpr = &BinaryOp{Left: onExpr, Right: eq, Operator: "AND"}
		}
	}
	return onExpr
}

func (p *Parser) peekType(typ TokenType, val string) bool {
	return p.peek.Type == typ && p.peek.Value == val
}

func (p *Parser) parseSelectFrom(s *SelectStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "FROM" {
		p.next()
		s.From = p.parseTableRef()
	}
}

func (p *Parser) parseSelectWhere(s *SelectStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}
}

func (p *Parser) parseSelectGroupBy(s *SelectStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "GROUP" {
		p.next()
		p.expectKeyword("BY")
		s.GroupBy = p.parseExprList()
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "HAVING" {
		p.next()
		s.Having = p.parseExpr()
	}
}

func (p *Parser) parseSelectOrderBy(s *SelectStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
		p.next()
		p.expectKeyword("BY")
		s.OrderBy = p.parseOrderBy()
	}
}

func (p *Parser) parseSelectLimit(s *SelectStmt) {
	p.parseLimitOffset(&s.Limit, &s.Offset)
}

// parseLimitOffset parses LIMIT and OFFSET clauses.
// Handles: LIMIT x, LIMIT x OFFSET y, LIMIT x,y
func (p *Parser) parseLimitOffset(limit, offset *Expr) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "LIMIT" {
		p.next()
		*limit = p.parseExpr()
		if p.cur.Type == TokenComma {
			// LIMIT x,y → LIMIT y OFFSET x
			p.next()
			off := p.parseExpr()
			*offset = *limit
			*limit = off
		} else if p.cur.Type == TokenKeyword && p.cur.Value == "OFFSET" {
			p.next()
			*offset = p.parseExpr()
		}
	}
}

// parseReturningClause parses a RETURNING clause: RETURNING expr [, expr]...
// All expressions are collected; multiple expressions are wrapped in a RowValue.
func (p *Parser) parseReturningClause(col *SelectColumn, hasReturning *bool) {
	p.next() // skip RETURNING
	*hasReturning = true
	// Parse all expressions (handles *, expr, or *, expr combinations)
	var exprs []Expr
	for {
		// Handle * as a standalone expression in multi-expression RETURNING.
		// parseExpr does not accept * (it's a binary operator), so we handle it
		// directly here.
		if p.cur.Type == TokenStar {
			exprs = append(exprs, &ColumnRef{Name: "*"})
			p.next()
		} else {
			exprs = append(exprs, p.parseExpr())
		}
		// Optional alias on last expression (only meaningful for single-expr RETURNING)
		if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				col.As = p.cur.Value
				p.next()
			}
		}
		if p.cur.Type != TokenComma {
			break
		}
		p.next()
	}
	// Wrap in RowValue if multiple expressions
	if len(exprs) == 1 {
		col.Expr = exprs[0]
	} else {
		col.Expr = &RowValue{Values: exprs}
	}
}

func (p *Parser) parseOneWindowDef() WindowDef {
	wd := WindowDef{}
	// Window name
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		wd.Name = p.cur.Value
		p.next()
	}
	// AS keyword
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
	}
	// Window spec in parentheses
	if p.cur.Type == TokenLParen {
		p.next()
		var partitions []Expr
		for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
			if p.cur.Type == TokenKeyword && p.cur.Value == "PARTITION" {
				p.next()
				if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
					p.next()
				}
				// Parse partition expressions until ORDER or frame keyword
				for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
					if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
						break
					}
					if p.cur.Type == TokenKeyword &&
						(p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
						break
					}
					expr := p.parseExpr()
					partitions = append(partitions, expr)
					if p.cur.Type == TokenComma {
						p.next()
					} else {
						break
					}
				}
				wd.Partitions = partitions
			} else if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
				p.next() // consume ORDER
				if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
					p.next() // consume BY
				}
				wd.OrderBy = p.parseOrderBy()
			} else if p.cur.Type == TokenKeyword &&
				(p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
				p.skipFrameSpec()
			} else if p.cur.Type == TokenComma {
				p.next()
			} else {
					// Parse expressions (handles function calls like percent_rank() OVER w1)
					expr := p.parseExpr()
					if expr != nil {
						partitions = append(partitions, expr)
					}
				}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
	return wd
}

func (p *Parser) parseSelectWindow(s *SelectStmt) {
	// WINDOW name AS (window_spec), ...
	if p.cur.Type == TokenKeyword && p.cur.Value == "WINDOW" {
		p.next()
		for {
			wd := p.parseOneWindowDef()
			s.Windows = append(s.Windows, wd)
			if p.cur.Type == TokenComma {
				p.next()
			} else {
				break
			}
		}
	}
}

func (p *Parser) parseSelectColumns() []SelectColumn {
	var cols []SelectColumn
	for {
		if p.cur.Type == TokenStar {
			cols = append(cols, SelectColumn{
				Expr: &ColumnRef{Name: "*"},
			})
			p.next()
		} else {
			expr := p.parseExpr()
			// Handle COLLATE after complete expression (e.g., expr COLLATE nocase)
			expr = p.skipCollateExpr(expr)
			col := SelectColumn{Expr: expr}
			if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
				p.next()
				if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
					col.As = p.cur.Value
					p.next()
				}
			} else if p.cur.Type == TokenIdentifier {
				// Implicit alias without AS (e.g., a name)
				col.As = p.cur.Value
				p.next()
			}
			cols = append(cols, col)
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return cols
}

func (p *Parser) parseTableRef() TableRef {
	ref := TableRef{}

	// Subquery in FROM clause: (SELECT ...) AS alias
	if p.cur.Type == TokenLParen {
		return p.parseParenTableRef()
	}

	// Regular table name
	ref.Name, ref.NameTok = p.readNameWithInfo()

	// Table-valued function arguments: FROM tablename(args)
	// SQLite supports table-valued functions like pragma_table_info('t2')
	p.skipTableValuedFuncArgs()

	// Optional INDEXED BY / NOT INDEXED clause: FROM t1 INDEXED BY i1
	p.skipIndexedByClause()

	ref = p.parseTableRefAlias(ref)
	return ref
}

func isJoinKeyword(v string) bool {
	switch v {
	case "ON", "JOIN", "WHERE", "ORDER", "GROUP", "LIMIT", "HAVING",
		"CROSS", "INNER", "LEFT", "RIGHT", "NATURAL", "OUTER", "FULL",
		"USING", "SET", "RETURNING", "EXCEPT", "INTERSECT", "UNION",
		"WINDOW":
		return true
	}
	return false
}

// parseParenTableRef handles parenthesized table references in a FROM clause:
// subquery (SELECT ...), CTE subquery (WITH ... SELECT ...), or bare table name (t1).
func (p *Parser) parseParenTableRef() TableRef {
	ref := TableRef{}
	p.next() // skip (
	if p.cur.Type == TokenKeyword && (p.cur.Value == "SELECT" || p.cur.Value == "WITH" || p.cur.Value == "VALUES") {
		if p.cur.Value == "SELECT" {
			ref.Subquery = p.parseSelect()
		} else if p.cur.Value == "VALUES" {
			ref.Subquery = p.parseValuesSubquery()
		} else {
			sel := p.parseWithStatement()
			if s, ok := sel.(*SelectStmt); ok {
				ref.Subquery = s
			}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
		ref = p.parseTableRefAlias(ref)
		return ref
	}
	// Parenthesized table name: (t1) AS alias
	// Must be followed by ')' to distinguish from (t2 JOIN t3 ...)
	if (p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword) && p.peek.Type == TokenRParen {
		ref.Name = p.cur.Value
		p.next()
		if p.cur.Type == TokenRParen {
			p.next()
			ref = p.parseTableRefAlias(ref)
		}
		return ref
	}
	// Parenthesized join expression: (t2 JOIN t3 USING(a))
	// Just skip tokens until the matching ')' is found
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		// Record the first table name if possible (for error messages etc.)
		if p.cur.Type == TokenIdentifier || !isJoinKeyword(p.cur.Value) {
			ref.Name = p.cur.Value
		}
		p.skipParenContent()
		// Try to parse optional alias
		ref = p.parseTableRefAlias(ref)
		return ref
	}
	// Not recognized, return empty ref (content left unconsumed)
	return ref
}

// skipParenContent skips tokens until the matching ')' is found,
// properly handling nested parentheses. The opening '(' must already
// have been consumed. Used for parenthesized JOIN expressions
// and other content that doesn't need full parsing.
func (p *Parser) skipParenContent() {
	depth := 1
	for depth > 0 {
		if p.cur.Type == TokenLParen {
			depth++
		} else if p.cur.Type == TokenRParen {
			depth--
			if depth == 0 {
				p.next() // consume the closing )
				return
			}
		}
		if p.cur.Type == TokenEOF {
			return
		}
		p.next()
	}
}

// parseTableRefAlias parses optional AS alias or implicit alias for a table reference.
func (p *Parser) parseTableRefAlias(ref TableRef) TableRef {
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
			ref.As = p.cur.Value
			p.next()
		}
	} else if p.cur.Type == TokenIdentifier {
		ref.As = p.cur.Value
		p.next()
	} else if p.cur.Type == TokenKeyword && !isJoinKeyword(p.cur.Value) {
		ref.As = p.cur.Value
		p.next()
	}
	return ref
}

// skipTableValuedFuncArgs consumes optional parenthesized arguments after
// a table name in a FROM clause, e.g. FROM pragma_table_info('t2').
// Handles both empty args: pragma_func() and non-empty: pragma_func('arg').
func (p *Parser) skipTableValuedFuncArgs() {
	if p.cur.Type == TokenLParen {
		p.next()
		if p.cur.Type != TokenRParen {
			p.parseExpr()
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
}

// skipIndexedByClause consumes an optional INDEXED BY / NOT INDEXED clause
// after a table name: FROM t1 INDEXED BY i1 or FROM t1 NOT INDEXED.
func (p *Parser) skipIndexedByClause() {
	if p.cur.Type == TokenKeyword && p.cur.Value == "INDEXED" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				p.next()
			}
		}
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		if p.peekType(TokenKeyword, "INDEXED") {
			p.next()
			p.next()
		}
	}
}

func (p *Parser) parseOrderBy() []OrderByTerm {
	var terms []OrderByTerm
	for {
		expr := p.parseExpr()
		term := OrderByTerm{Expr: expr}
		if p.cur.Type == TokenKeyword && p.cur.Value == "ASC" {
			p.next()
		} else if p.cur.Type == TokenKeyword && p.cur.Value == "DESC" {
			term.Desc = true
			p.next()
		}
		// Optional NULLS FIRST/LAST
		if p.cur.Type == TokenKeyword && p.cur.Value == "NULLS" {
			p.next()
			if p.cur.Type == TokenKeyword && (p.cur.Value == "FIRST" || p.cur.Value == "LAST") {
				p.next()
			}
		}
		terms = append(terms, term)
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return terms
}

// INSERT
func (p *Parser) parseInsert(isReplace bool) *InsertStmt {
	s := &InsertStmt{IsReplace: isReplace}
	p.next()

	// INSERT OR REPLACE/ROLLBACK/ABORT/FAIL/IGNORE
	orConflict := ""
	if p.cur.Type == TokenKeyword && p.cur.Value == "OR" {
		p.next()
		if p.cur.Type == TokenKeyword {
			orConflict = p.cur.Value
			p.next()
		}
		if orConflict == "REPLACE" || orConflict == "ROLLBACK" || orConflict == "ABORT" ||
			orConflict == "FAIL" || orConflict == "IGNORE" {
			// Valid conflict resolution clause
			if orConflict == "REPLACE" {
				s.IsReplace = true
			}
		} else {
			p.setErr("expected OR conflict resolution keyword after OR")
			return nil
		}
	}

	if !p.expectKeyword("INTO") {
		return nil
	}
	if name := p.readName(); name != "" {
		s.Table = name
	}
	// Handle optional alias: INSERT INTO t1 AS alias(col1, col2) ...
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next() // skip alias name
		}
	}
	if p.cur.Type == TokenLParen {
		p.next()
		s.Columns = p.parseIdentList()
		p.expect(TokenRParen)
	}
	p.parseInsertSource(s)
	// Handle ON CONFLICT clause(s) - use loop for duplicate clauses
	for p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		s.OnConflict = p.parseOnConflict()
	}
	// Optional RETURNING clause
	if p.cur.Type == TokenKeyword && p.cur.Value == "RETURNING" {
		p.parseReturningClause(&s.Returning, &s.HasReturning)
	}
	return s
}

func (p *Parser) parseOnConflict() *OnConflictClause {
	oc := &OnConflictClause{}
	p.next() // skip ON
	p.expectKeyword("CONFLICT")

	// Optional conflict target: (column_name), (expr), or (col1, col2)
	if p.cur.Type == TokenLParen {
		// Parse the parenthesized content as an expression list
		// to handle single expressions, multi-column targets, etc.
		p.skipParenExprList()
	}

	// Optional WHERE clause for partial index conflict target
	// e.g., ON CONFLICT(col) WHERE condition DO ...
	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		oc.Where = p.parseExpr()
	}

	p.expectKeyword("DO")

	if p.cur.Type == TokenKeyword && p.cur.Value == "NOTHING" {
		oc.Action = ConflictDoNothing
		p.next()
		return oc
	}

	p.expectKeyword("UPDATE")
	oc.Action = ConflictDoUpdate

	if !p.expectKeyword("SET") {
		return nil
	}
	oc.Assignments = p.parseAssignments()
	// Optional WHERE clause for DO UPDATE
	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		oc.Where = p.parseExpr()
	}
	return oc
}

func (p *Parser) parseAssignments() []Assignment {
	var assigns []Assignment
	for {
		var a Assignment
		if p.cur.Type == TokenLParen {
			// Parenthesized column list: (col1, col2) = (expr1, expr2)
			as := p.parseParenthesizedAssignments()
			if as == nil {
				return assigns
			}
			assigns = append(assigns, as...)
		} else if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			a.Column = p.cur.Value
			p.next()
			if !p.expect(TokenEq) {
				return assigns
			}
			a.Value = p.parseExpr()
			assigns = append(assigns, a)
		} else {
			break
		}
		if p.cur.Type != TokenComma {
			break
		}
		p.next()
	}
	return assigns
}

// parseParenthesizedAssignments handles SET (col1, col2) = (expr1, expr2) syntax.
func (p *Parser) parseParenthesizedAssignments() []Assignment {
	p.next() // skip (
	var cols []string
	for {
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			cols = append(cols, p.cur.Value)
			p.next()
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	if p.cur.Type == TokenRParen {
		p.next()
	}
	if !p.expect(TokenEq) {
		return nil
	}
	var assigns []Assignment
	if p.cur.Type == TokenLParen {
		p.next()
		for i, col := range cols {
			val := p.parseExpr()
			assigns = append(assigns, Assignment{Column: col, Value: val})
			if i < len(cols)-1 {
				if p.cur.Type == TokenComma {
					p.next()
				}
			}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
	return assigns
}

func (p *Parser) parseInsertSource(s *InsertStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
		s.Select = p.parseSelect()
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "DEFAULT" {
		p.next()
		p.expectKeyword("VALUES")
	} else if p.cur.Type == TokenParam {
		// TCL variable reference ($data1, $data2) - skip it
		p.next()
	} else {
		p.expectKeyword("VALUES")
		// First tuple
		p.expect(TokenLParen)
		s.Values = [][]Expr{p.parseExprList()}
		p.expect(TokenRParen)
		// Additional tuples
		for p.cur.Type == TokenComma {
			p.next()
			if p.cur.Type == TokenLParen {
				p.next()
				s.Values = append(s.Values, p.parseExprList())
				p.expect(TokenRParen)
			}
		}
	}
}

// UPDATE
func (p *Parser) parseUpdate() *UpdateStmt {
	s := &UpdateStmt{}
	p.next() // skip UPDATE

	p.parseUpdateOrConflict(s)
	if p.err != nil {
		return nil
	}

	if name := p.readName(); name != "" {
		s.Table = name
	}

	// Handle optional alias: UPDATE t1 AS alias SET ...
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next() // skip AS
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next() // skip alias name
		}
	}

	if !p.expectKeyword("SET") {
		return nil
	}

	p.parseUpdateAssignments(s)

	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}

	p.parseUpdateFromClause(s)

	// WHERE clause after UPDATE FROM: UPDATE t SET ... FROM t2 WHERE ...
	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}

	// Optional RETURNING clause
	if p.cur.Type == TokenKeyword && p.cur.Value == "RETURNING" {
		p.parseReturningClause(&s.Returning, &s.HasReturning)
	}

	// Optional ORDER BY
	if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
		p.next() // skip ORDER
		if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
			p.next()
			s.OrderBy = p.parseOrderBy()
		}
	}

	// Optional LIMIT / LIMIT x OFFSET y / LIMIT x,y
	p.parseLimitOffset(&s.Limit, &s.Offset)

	return s
}

func (p *Parser) parseUpdateOrConflict(s *UpdateStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "OR" {
		p.next()
		if p.cur.Type == TokenKeyword {
			switch p.cur.Value {
			case "ROLLBACK", "ABORT", "FAIL", "IGNORE", "REPLACE":
				p.next()
			default:
				p.setErr("expected conflict action after OR in UPDATE")
			}
		}
	}
}

func (p *Parser) parseUpdateAssignments(s *UpdateStmt) {
	for {
		if p.cur.Type == TokenLParen {
			p.parseParenthesizedUpdateAssignments(s)
		} else if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
			col := p.cur.Value
			p.next()
			if !p.expect(TokenEq) {
				break
			}
			val := p.parseExpr()
			s.Assignments = append(s.Assignments, Assignment{Column: col, Value: val})
		} else {
			p.setErr("expected column name in SET")
			break
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
}

func (p *Parser) parseUpdateFromClause(s *UpdateStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "FROM" {
		p.next()
		for {
			if !p.consumeUpdateFromTable() {
				break
			}
			if p.cur.Type == TokenComma {
				p.next()
			} else {
				break
			}
		}
	}
}

// consumeUpdateFromTable consumes one table reference from an UPDATE ... FROM
// clause: either a subquery (SELECT ...) or a table name with optional alias.
// Returns false when the loop should stop (end-of-clause keyword or EOF).
func (p *Parser) consumeUpdateFromTable() bool {
	if p.cur.Type == TokenLParen {
		p.next()
		p.parseSelect()
		if p.cur.Type == TokenRParen {
			p.next()
		}
		return true
	}
	if p.cur.Type != TokenIdentifier && p.cur.Type != TokenKeyword {
		return false
	}
	if p.cur.Type == TokenKeyword && isEndOfUpdateFrom(p.cur.Value) {
		return false
	}
	p.readName()
	// Optional alias: table_name AS alias
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next()
		}
	}
	// Handle JOIN continuations: NATURAL JOIN, LEFT JOIN, INNER JOIN, etc.
	for p.cur.Type == TokenKeyword && isUpdateJoinKeyword(p.cur.Value) {
		p.consumeUpdateFromJoin()
	}
	return true
}

// isUpdateJoinKeyword returns true if the keyword is a valid join modifier
// in an UPDATE ... FROM clause (not a clause-ending keyword).
func isUpdateJoinKeyword(v string) bool {
	switch v {
	case "NATURAL", "LEFT", "RIGHT", "CROSS", "FULL", "INNER", "OUTER", "JOIN":
		return true
	}
	return false
}

// consumeUpdateFromJoin consumes a single JOIN continuation from an UPDATE ... FROM
// clause, including the JOIN keyword, joined table, and optional ON/USING clause.
func (p *Parser) consumeUpdateFromJoin() {
	p.next() // skip join keyword (NATURAL, LEFT, etc.)
	if p.cur.Type == TokenKeyword && (p.cur.Value == "OUTER" || p.cur.Value == "INNER") {
		p.next()
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "JOIN" {
		p.next()
		p.consumeJoinTable()
	}
	// Consume optional ON or USING clause
	p.consumeJoinOnUsing()
}

// consumeJoinTable consumes the table reference after a JOIN keyword.
func (p *Parser) consumeJoinTable() {
	if p.cur.Type == TokenLParen {
		p.next()
		p.parseSelect()
		if p.cur.Type == TokenRParen {
			p.next()
		}
	} else if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		if !(p.cur.Type == TokenKeyword && isEndOfUpdateFrom(p.cur.Value)) {
			p.readName()
			if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
				p.next()
				if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
					p.next()
				}
			}
		}
	}
}

// consumeJoinOnUsing consumes an optional ON or USING clause after a JOIN.
func (p *Parser) consumeJoinOnUsing() {
	if p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.next()
		p.parseExpr()
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "USING" {
		p.next()
		if p.cur.Type == TokenLParen {
			p.next()
			for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				p.next()
				if p.cur.Type == TokenComma {
					p.next()
				}
			}
			if p.cur.Type == TokenRParen {
				p.next()
			}
		}
	}
}

// isEndOfUpdateFrom returns true if the keyword signals the end of
// an UPDATE ... FROM clause.
func isEndOfUpdateFrom(v string) bool {
	switch v {
	case "WHERE", "RETURNING", "ORDER", "LIMIT", "OFFSET":
		return true
	}
	return false
}

func (p *Parser) parseParenthesizedUpdateAssignments(s *UpdateStmt) {
	// Parenthesized column list: SET (col1, col2) = (expr1, expr2)
	p.next()
	var cols []string
	for {
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			cols = append(cols, p.cur.Value)
			p.next()
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	if p.cur.Type == TokenRParen {
		p.next()
	}
	if !p.expect(TokenEq) {
		return
	}
	if p.cur.Type == TokenLParen {
		// Use parseExpr which goes through parseParenExpr and handles
		// both subqueries (SELECT ...) and row value lists (expr, expr, ...).
		val := p.parseExpr()
		if rv, ok := val.(*RowValue); ok && len(rv.Values) == len(cols) {
			for i, col := range cols {
				s.Assignments = append(s.Assignments, Assignment{Column: col, Value: rv.Values[i]})
			}
		} else if val != nil {
			for _, col := range cols {
				s.Assignments = append(s.Assignments, Assignment{Column: col, Value: val})
			}
		}
		s.SetParenColumns = cols
	}
}

// DELETE
func (p *Parser) parseDelete() *DeleteStmt {
	s := &DeleteStmt{}
	p.next() // skip DELETE

	if !p.expectKeyword("FROM") {
		return nil
	}

	if name := p.readName(); name != "" {
		s.Table = name
	}

	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}

	// Optional RETURNING clause
	if p.cur.Type == TokenKeyword && p.cur.Value == "RETURNING" {
		p.parseReturningClause(&s.Returning, &s.HasReturning)
	}
	// Optional ORDER BY
	// Optional ORDER BY
	if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
		p.next() // skip ORDER
		if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
			p.next()
			s.OrderBy = p.parseOrderBy()
		}
	}

	// Optional LIMIT / LIMIT x OFFSET y / LIMIT x,y
	p.parseLimitOffset(&s.Limit, &s.Offset)

	return s
}

// CREATE
func (p *Parser) parseCreate() Stmt {
	p.next() // skip CREATE

	if p.cur.Type == TokenKeyword && (p.cur.Value == "TEMP" || p.cur.Value == "TEMPORARY") {
		p.next()
	}

	if p.cur.Type == TokenKeyword && p.cur.Value == "UNIQUE" {
		p.next()
	}

	if p.cur.Type == TokenKeyword {
		switch p.cur.Value {
		case "TABLE":
			return p.parseCreateTable()
		case "INDEX":
			return p.parseCreateIndex()
		case "VIEW":
			return p.parseCreateView()
		case "TRIGGER":
			return p.parseCreateTrigger()
		case "VIRTUAL":
			return p.parseCreateVirtualTable()
		default:
			p.setErr("expected TABLE, INDEX, VIEW, TRIGGER, or VIRTUAL after CREATE, got %s", p.cur.Value)
			return nil
		}
	}

	p.setErr("expected TABLE, INDEX, VIEW, TRIGGER, or VIRTUAL after CREATE")
	return nil
}

func (p *Parser) parseCreateVirtualTable() *CreateVirtualTableStmt {
	s := &CreateVirtualTableStmt{}
	p.next() // skip VIRTUAL
	if !p.expectKeyword("TABLE") {
		return nil
	}
	if name := p.readName(); name != "" {
		s.Name = name
	}
	if !p.expectKeyword("USING") {
		return nil
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Module = p.cur.Value
		p.next()
	}
	s.Args = p.parseVTabArgs()
	return s
}

func (p *Parser) parseVTabArgs() []string {
	var args []string
	if p.cur.Type != TokenLParen {
		return args
	}
	p.next()
	for {
		if p.cur.Type == TokenRParen {
			p.next()
			break
		}
		if p.cur.Type == TokenString || p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenNumber || p.cur.Type == TokenBlob {
			args = append(args, p.cur.Value)
			p.next()
			// Skip optional '=' between key and value in key=value pairs
			if p.cur.Type == TokenEq {
				p.next()
				if p.cur.Type == TokenString || p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenNumber || p.cur.Type == TokenBlob {
					args = append(args, p.cur.Value)
					p.next()
				}
			}
		} else {
			break
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else if p.cur.Type != TokenRParen {
			break
		}
	}
	return args
}

func (p *Parser) parseCreateView() *CreateViewStmt {
	s := &CreateViewStmt{}
	p.next() // skip VIEW

	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		if !p.expectKeyword("NOT") {
			return nil
		}
		if !p.expectKeyword("EXISTS") {
			return nil
		}
		// IF NOT EXISTS for views
	}

	if name, info := p.readNameWithInfo(); name != "" {
		s.Name = name
		s.NameTok = info
	}

	// Optional parenthesized column list: CREATE VIEW name (col1, col2) AS ...
	if p.cur.Type == TokenLParen {
		p.next()
		// Collect column names
		for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			s.Columns = append(s.Columns, p.cur.Value)
			p.next()
			if p.cur.Type == TokenComma {
				p.next()
			} else {
				break
			}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}

	if !p.expectKeyword("AS") {
		return nil
	}

	// Handle CREATE VIEW ... AS WITH ... SELECT (CTE in view body)
	if p.cur.Type == TokenKeyword && p.cur.Value == "WITH" {
		withStmt := p.parseWithStatement()
		if ss, ok := withStmt.(*SelectStmt); ok {
			s.Select = ss
		}
	} else {
		s.Select = p.parseSelect()
	}
	return s
}

func (p *Parser) parseCreateTrigger() *CreateTriggerStmt {
	s := &CreateTriggerStmt{}
	p.next() // skip TRIGGER

	p.parseTriggerIfNotExists(s)

	// Read trigger name. The name ALWAYS comes right after
	// CREATE TRIGGER [IF NOT EXISTS], regardless of whether it
	// could also be a timing keyword (e.g., "AFTER", "BEFORE").
	// The timing keyword (if any) comes AFTER the name.
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
		// Check for schema-qualified name
		if p.cur.Type == TokenDot {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				s.Name = s.Name + "." + p.cur.Value
				p.next()
			}
		}
	}

	p.parseTriggerTiming(s)
	p.parseTriggerEvent(s)

	if !p.expectKeyword("ON") {
		return nil
	}

	if tableName, info := p.readNameWithInfo(); tableName != "" {
		s.Table = tableName
		s.TableTok = info
	}

	p.parseTriggerWhenForEach(s)
	p.parseTriggerBody(s)
	return s
}

func isTimingKeyword(s string) bool {
	return s == "BEFORE" || s == "AFTER" || s == "INSTEAD"
}

func isEventKeyword(s string) bool {
	return s == "DELETE" || s == "INSERT" || s == "UPDATE"
}

func (p *Parser) parseTriggerIfNotExists(s *CreateTriggerStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		if !p.expectKeyword("NOT") {
			return
		}
		p.expectKeyword("EXISTS")
		s.IfNotExists = true
	}
}

func (p *Parser) parseTriggerTiming(s *CreateTriggerStmt) {
	if p.cur.Type == TokenKeyword && isTimingKeyword(p.cur.Value) {
		s.Time = p.cur.Value
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OF" {
			s.Time += " OF"
			p.next()
		}
	}
}

func (p *Parser) parseTriggerEvent(s *CreateTriggerStmt) {
	if p.cur.Type == TokenKeyword && isEventKeyword(p.cur.Value) {
		s.Event = p.cur.Value
		p.next()
		// Consume optional OF column list (UPDATE OF col1, col2, ...)
		if s.Event == "UPDATE" && p.cur.Type == TokenKeyword && p.cur.Value == "OF" {
			p.next()
			for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				p.next()
				if p.cur.Type == TokenComma {
					p.next()
				} else {
					break
				}
			}
		}
	}
}

func (p *Parser) parseTriggerWhenForEach(s *CreateTriggerStmt) {
	// FOR EACH ROW / FOR EACH STATEMENT (optional, skip)
	for p.cur.Type == TokenKeyword && p.cur.Value == "FOR" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "EACH" {
			p.next()
			if p.cur.Type == TokenKeyword && (p.cur.Value == "ROW" || p.cur.Value == "STATEMENT") {
				p.next()
			}
		}
	}
	// WHEN expr (optional)
	if p.cur.Type == TokenKeyword && p.cur.Value == "WHEN" {
		p.next()
		s.When = p.parseExpr()
	}
}

func (p *Parser) parseTriggerBody(s *CreateTriggerStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "BEGIN" {
		p.next()
		for {
			if p.cur.Type == TokenKeyword && p.cur.Value == "END" {
				p.next()
				break
			}
			stmt := p.parseStatement()
			if stmt == nil {
				break
			}
			s.Statements = append(s.Statements, stmt)
			for p.cur.Type == TokenSemicolon {
				p.next()
			}
		}
	}
}

func (p *Parser) parseCreateTable() *CreateTableStmt {
	s := &CreateTableStmt{}
	p.next() // skip TABLE

	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		if !p.expectKeyword("NOT") {
			return nil
		}
		if !p.expectKeyword("EXISTS") {
			return nil
		}
		s.IfNotExists = true
	}

	if name, info := p.readNameWithInfo(); name != "" {
		s.Name = name
		s.NameTok = info
	}

	if p.cur.Type == TokenLParen {
		p.next()
		cols, constraints := p.parseColumnDefs()
		s.Columns = cols
		// Parse any remaining table-level constraints after all columns
		remaining := p.parseTableConstraints()
		s.Constraints = append(constraints, remaining...)
		if !p.expect(TokenRParen) {
			return nil
		}
	}

	// Table options: WITHOUT ROWID, STRICT
	p.parseTableOptions(s)

	// CREATE TABLE ... AS SELECT
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
			s.AsSelect = p.parseSelect()
		} else {
			p.setErr("expected SELECT after AS in CREATE TABLE")
			return nil
		}
	}

	return s
}

func (p *Parser) parseTableOptions(s *CreateTableStmt) {
	// Table options: WITHOUT ROWID, STRICT (in any order, with optional commas)
	for {
		if p.cur.Type == TokenComma {
			p.next()
			continue
		}
		if p.cur.Type == TokenKeyword && p.cur.Value == "WITHOUT" {
			p.next()
			if p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
				if !strings.EqualFold(p.cur.Value, "ROWID") {
					p.setErr("expected 'ROWID' but got '%s'", p.cur.Value)
					return
				}
				p.next()
				s.WithoutRowid = true
			}
			continue
		}
		if p.cur.Type == TokenKeyword && p.cur.Value == "STRICT" {
			p.next()
			continue
		}
		break
	}
}

func (p *Parser) parseWithStatement() Stmt {
	p.next() // skip WITH
	if p.cur.Type == TokenKeyword && p.cur.Value == "RECURSIVE" {
		p.next()
	}
	var ctes []CTEDef
	for {
		cte := p.parseOneCTE()
		if cte == nil {
			return nil
		}
		ctes = append(ctes, *cte)
		if p.cur.Type == TokenComma {
			p.next()
			continue
		}
		break
	}
	main := p.parseKeywordStmt()
	if main != nil {
		switch s := main.(type) {
		case *SelectStmt:
			s.CTEs = ctes
		case *InsertStmt:
			s.CTEs = ctes
			if s.Select != nil {
				s.Select.CTEs = ctes
			}
		}
	}
	return main
}

func (p *Parser) parseOneCTE() *CTEDef {
	cte := &CTEDef{}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		cte.Name = p.cur.Value
		p.next()
	}
	cte.Columns = p.parseCTEColumnList()
	if !p.expectKeyword("AS") {
		return nil
	}
	// Optional MATERIALIZED (or NOT MATERIALIZED) CTE optimization hint
	if p.cur.Type == TokenKeyword && p.cur.Value == "MATERIALIZED" {
		p.next()
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "MATERIALIZED" {
			p.next()
		}
	}
	if p.cur.Type == TokenLParen {
		p.next()
		cte.Select = p.parseCTEBody()
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
	return cte
}

func (p *Parser) parseCTEColumnList() []string {
	if p.cur.Type != TokenLParen {
		return nil
	}
	p.next()
	var cols []string
	for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		cols = append(cols, p.cur.Value)
		p.next()
		if p.cur.Type == TokenComma {
			p.next()
		}
	}
	if p.cur.Type == TokenRParen {
		p.next()
	}
	return cols
}

func (p *Parser) parseCTEBody() *SelectStmt {
	if p.cur.Type == TokenKeyword && p.cur.Value == "VALUES" {
		return p.parseValuesSubquery()
	}
	// Nested CTE: WITH ... SELECT ... inside a CTE body
	if p.cur.Type == TokenKeyword && p.cur.Value == "WITH" {
		sel := p.parseWithStatement()
		if s, ok := sel.(*SelectStmt); ok {
			return s
		}
		return nil
	}
	return p.parseSelect()
}

func (p *Parser) parseValuesSubquery() *SelectStmt {
	p.next() // skip VALUES
	vs := &SelectStmt{}
	// Parse one or more value rows: (expr, expr), (expr, expr), ...
	for p.cur.Type == TokenLParen {
		p.next()
		row := p.parseExprList()
		if len(vs.Columns) == 0 {
			// First row defines the columns
			for _, expr := range row {
				vs.Columns = append(vs.Columns, SelectColumn{Expr: expr})
			}
		}
		// Store additional rows as Values data
		// (Currently just parsing; execution stores rows differently)
		if p.cur.Type == TokenRParen {
			p.next()
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	if p.cur.Type == TokenKeyword && (p.cur.Value == "UNION" || p.cur.Value == "INTERSECT" || p.cur.Value == "EXCEPT") {
		if p.cur.Value == "UNION" {
			vs.SetOp = SetUnion
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "ALL" {
				vs.UnionAll = true
				p.next()
			}
		} else if p.cur.Value == "INTERSECT" {
			vs.SetOp = SetIntersect
			p.next()
		} else if p.cur.Value == "EXCEPT" {
			vs.SetOp = SetExcept
			p.next()
		}
		vs.Union = p.parseSelectBody()
	}
	// ORDER BY and LIMIT apply to the outermost compound result
	p.parseSelectOrderBy(vs)
	p.parseSelectLimit(vs)
	return vs
}

// skipTableConstraint consumes a table-level constraint expression.
func (p *Parser) skipTableConstraint() {
	switch p.cur.Value {
	case "PRIMARY":
		p.next()
		p.expectKeyword("KEY")
		p.skipParenExprList()
		p.skipOnConflictInConstraint()
	case "UNIQUE":
		p.next()
		p.skipParenExprList()
		p.skipOnConflictInConstraint()
	case "CHECK":
		p.next()
		p.skipParenExpr()
	case "FOREIGN":
		p.next()
		p.expectKeyword("KEY")
		p.skipParenExprList()
		p.expectKeyword("REFERENCES")
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next() // skip table name
		}
		// Optional parenthesized column list: REFERENCES t1(col1, col2)
		if p.cur.Type == TokenLParen {
			p.skipParenExprList()
		}
		// Optional ON DELETE/UPDATE clauses
		for p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
			p.parseReferencesOnAction()
		}
		// Optional MATCH clause
		if p.cur.Type == TokenKeyword && p.cur.Value == "MATCH" {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				p.next()
			}
		}
		// Optional DEFERRABLE clause: NOT DEFERRABLE, DEFERRABLE INITIALLY DEFERRED|IMMEDIATE
		p.skipDeferrableClause()
	case "CONSTRAINT":
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next()
		}
		p.skipTableConstraint()
	}
}

func (p *Parser) skipOnConflictInConstraint() {
	// Skip optional ON CONFLICT clause: ON CONFLICT REPLACE|ABORT|FAIL|ROLLBACK|IGNORE
	if p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "CONFLICT" {
			p.next()
			if p.cur.Type == TokenKeyword {
				switch p.cur.Value {
				case "REPLACE", "ABORT", "FAIL", "ROLLBACK", "IGNORE":
					p.next()
				}
			}
		}
	}
}

func (p *Parser) skipParenExprList() {
	if p.cur.Type == TokenLParen {
		p.next()
		for p.cur.Type != TokenRParen {
			if p.cur.Type == TokenEOF {
				return
			}
			p.parseExpr()
			// Consume optional ASC/DESC after each expression
			for p.cur.Type == TokenKeyword && (p.cur.Value == "ASC" || p.cur.Value == "DESC") {
				p.next()
			}
			if p.cur.Type == TokenComma {
				p.next()
			}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
}

func (p *Parser) skipParenExpr() {
	if p.cur.Type == TokenLParen {
		p.next()
		p.parseExpr()
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
}

// parseParenExpr parses (expr) and returns the expression.
func (p *Parser) parseParenExprOnly() Expr {
	if p.cur.Type == TokenLParen {
		p.next()
		expr := p.parseExpr()
		if p.cur.Type == TokenRParen {
			p.next()
		}
		return expr
	}
	return nil
}

// parseParenIdentList parses (ident1, ident2, ...) with optional COLLATE and ASC/DESC
// and returns the list of indexed columns.
func (p *Parser) parseParenIdentList() []IndexedColumn {
	if p.cur.Type == TokenLParen {
		p.next()
		var cols []IndexedColumn
		for p.cur.Type != TokenRParen {
			if p.cur.Type == TokenEOF {
				return cols
			}
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
					col := IndexedColumn{Name: p.cur.Value}
					p.next()
					// Optional COLLATE clause
					if p.cur.Type == TokenKeyword && p.cur.Value == "COLLATE" {
						p.next()
						if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
							col.Collate = p.cur.Value
							p.next()
						}
					}
					// Optional ASC/DESC
					if p.cur.Type == TokenKeyword && (p.cur.Value == "ASC" || p.cur.Value == "DESC") {
						if p.cur.Value == "DESC" {
							col.Desc = true
						}
						p.next()
					}
					cols = append(cols, col)
				} else {
					// Token doesn't match expected identifier/string — break to avoid infinite loop
					break
				}
			if p.cur.Type == TokenComma {
				p.next()
			}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
		return cols
	}
	return nil
}

// parseConstraintName parses the optional CONSTRAINT name prefix and returns the name.
func (p *Parser) parseConstraintName() string {
	if p.cur.Type == TokenKeyword && p.cur.Value == "CONSTRAINT" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
			name := p.cur.Value
			p.next()
			return name
		}
	}
	return ""
}

// parseTableConstraint parses a table-level constraint and returns it.
func (p *Parser) parseTableConstraint() TableConstraint {
	name := p.parseConstraintName()
	tc := TableConstraint{Name: name}

	switch p.cur.Value {
	case "PRIMARY":
		tc.Type = ConstraintPrimaryKey
		p.next()
		p.expectKeyword("KEY")
		tc.Columns = p.parseParenIdentList()
		p.skipOnConflictInConstraint()
	case "UNIQUE":
		tc.Type = ConstraintUnique
		p.next()
		tc.Columns = p.parseParenIdentList()
		p.skipOnConflictInConstraint()
	case "CHECK":
		tc.Type = ConstraintCheck
		p.next()
		tc.Expr = p.parseParenExprOnly()
	case "FOREIGN":
		tc.Type = ConstraintForeignKey
		p.next()
		p.expectKeyword("KEY")
		p.skipParenExprList() // (col1, col2)
		p.expectKeyword("REFERENCES")
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next() // table name
		}
		if p.cur.Type == TokenLParen {
			p.skipParenExprList() // (col1, col2)
		}
		p.skipForeignKeyClauses()
	}
	return tc
}

// parseTableConstraints parses table-level constraints and returns them.
func (p *Parser) parseTableConstraints() []TableConstraint {
	var constraints []TableConstraint
	for p.cur.Type == TokenKeyword && (p.cur.Value == "PRIMARY" || p.cur.Value == "UNIQUE" ||
		p.cur.Value == "CHECK" || p.cur.Value == "FOREIGN" || p.cur.Value == "CONSTRAINT") {
		constraints = append(constraints, p.parseTableConstraint())
		// Consume optional comma after constraint
		if p.cur.Type == TokenComma {
			p.next()
		}
	}
	return constraints
}

// skipForeignKeyClauses consumes optional ON DELETE/UPDATE/MATCH/DEFERRABLE clauses.
func (p *Parser) skipForeignKeyClauses() {
	for p.cur.Type == TokenKeyword && (p.cur.Value == "ON" || p.cur.Value == "MATCH" || p.cur.Value == "NOT" || p.cur.Value == "DEFERRABLE") {
		if p.cur.Value == "MATCH" {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				p.next()
			}
		} else if p.cur.Value == "NOT" || p.cur.Value == "DEFERRABLE" {
			if p.cur.Value == "NOT" {
				p.next()
			}
			p.expectKeyword("DEFERRABLE")
			if p.cur.Type == TokenKeyword && p.cur.Value == "INITIALLY" {
				p.next()
				p.next() // DEFERRED or IMMEDIATE
			}
		} else {
			p.next() // ON
			if p.cur.Type == TokenKeyword && (p.cur.Value == "DELETE" || p.cur.Value == "UPDATE") {
				p.next()
				p.parseReferencesOnAction()
			}
		}
	}
}
func (p *Parser) parseCreateIndex() *CreateIndexStmt {
	s := &CreateIndexStmt{}
	p.next() // skip INDEX

	if p.cur.Type == TokenKeyword && p.cur.Value == "UNIQUE" {
		s.Unique = true
		p.next()
	}

	if name := p.readName(); name != "" {
		s.Name = name
	}

	if !p.expectKeyword("ON") {
		return nil
	}

	if tableName, info := p.readNameWithInfo(); tableName != "" {
		s.Table = tableName
		s.TableTok = info
	}

	if p.cur.Type == TokenLParen {
		p.next()
		s.Columns = p.parseIndexColumns()
		if !p.expect(TokenRParen) {
			return nil
		}
	}

	// Optional WHERE clause for partial indexes
	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}

	return s
}

func (p *Parser) parseColumnDefs() ([]ColumnDef, []TableConstraint) {
	var cols []ColumnDef
	var constraints []TableConstraint
	sawTableConstraint := false
	hasColumns := false
	for {
		// Parse table-level constraints instead of skipping them
		if p.cur.Type == TokenKeyword && (p.cur.Value == "PRIMARY" || p.cur.Value == "UNIQUE" ||
			p.cur.Value == "CHECK" || p.cur.Value == "FOREIGN" || p.cur.Value == "CONSTRAINT") {
			// SQLite rejects table-level constraints before any columns
			if !hasColumns {
				p.setErr("near %q: syntax error", p.cur.Value)
				return nil, nil
			}
			constraints = append(constraints, p.parseTableConstraint())
			sawTableConstraint = true
			if p.cur.Type == TokenComma {
				p.next()
			}
			continue
		}
		// Handle optional comma before column definition
		if p.cur.Type == TokenComma {
			p.next()
			continue
		}
		if p.cur.Type != TokenIdentifier && p.cur.Type != TokenKeyword {
			break
		}
		// After a table-level constraint, additional column definitions are invalid.
		// SQLite rejects: CREATE TABLE t(a, b, CONSTRAINT ck CHECK(a!=b), extra_col)
		if sawTableConstraint {
			p.setErr("near %q: syntax error", p.cur.Value)
			return nil, nil
		}
		col := ColumnDef{Name: p.cur.Value}
		p.next()
		col.Type = p.parseColumnType()
		p.parseColumnConstraints(&col)
		cols = append(cols, col)
		hasColumns = true
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return cols, constraints
}

// isConstraintStart returns true if word is a SQL keyword that starts
// a column constraint, not a type name.
func isConstraintStart(word string) bool {
	switch word {
	case "PRIMARY", "NOT", "DEFAULT", "UNIQUE", "CHECK", "REFERENCES",
		"COLLATE", "CONSTRAINT", "NULL":
		return true
	}
	return false
}

func (p *Parser) parseColumnType() string {
	if p.cur.Type != TokenIdentifier && p.cur.Type != TokenKeyword {
		return ""
	}
	if isConstraintStart(p.cur.Value) || p.cur.Value == "AS" {
		return ""
	}

	parts := []string{p.cur.Value}
	p.next()
	// SQLite accepts any sequence of identifiers/keywords as a column type.
	// Continue consuming tokens as long as they are valid type name parts
	// (i.e., not constraint-starting keywords like DEFAULT, NOT, PRIMARY, etc.).
	for p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
		if isConstraintStart(p.cur.Value) || p.cur.Value == "AS" {
			break
		}
		parts = append(parts, p.cur.Value)
		p.next()
	}

	// Optional type arguments: VARCHAR(123) or VARCHAR(123,456)
	if p.cur.Type == TokenLParen {
		p.next()
		skipParenValue(p)
		if p.cur.Type == TokenComma {
			p.next()
			skipParenValue(p)
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
	return strings.Join(parts, " ")
}

// skipParenValue skips a single token inside parenthesized type arguments.
func skipParenValue(p *Parser) {
	if p.cur.Type == TokenNumber || p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
		p.next()
	}
}

func (p *Parser) parseColumnConstraints(col *ColumnDef) {
	for {
		if p.cur.Type != TokenKeyword {
			break
		}
		if !p.dispatchColumnConstraint(col) {
			break
		}
	}
	// Optional ON CONFLICT clause after any constraint
	for p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.parseOnConflictColumnConstraint(col)
	}
}

func (p *Parser) dispatchColumnConstraint(col *ColumnDef) bool {
	switch p.cur.Value {
	case "PRIMARY":
		p.parsePrimaryKeyConstraint(col)
	case "NOT":
		p.parseNotNullConstraint(col)
	case "DEFAULT":
		p.parseDefaultConstraint(col)
	case "UNIQUE":
		// If next token is '(', it's a table-level UNIQUE(col) constraint
		if p.peek.Type == TokenLParen {
			return false
		}
		col.Unique = true
		p.next()
	case "CHECK":
		p.parseCheckConstraint(col)
	case "NULL":
		// NULL constraint means nullable (the default, but valid syntax)
		p.next()
	case "REFERENCES":
		p.parseReferencesConstraint(col)
	case "COLLATE":
		p.parseCollateColumnConstraint(col)
	case "CONSTRAINT":
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
			col.ConstraintName = p.cur.Value
			p.next()
		}
		// Parse the constraint that follows the name using the proper handler
		p.dispatchColumnConstraint(col)
	case "AS":
		p.parseGeneratedColumnAs(col)
	default:
		return false
	}
	return true
}

func (p *Parser) parseCollateColumnConstraint(col *ColumnDef) {
	p.next()
	if p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
		col.Collate = p.cur.Value
		p.next()
	}
}

//lint:ignore U1000 Parser utility for future use
func (p *Parser) skipConstraintName() {
	p.next()
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
		p.next() // skip constraint name
	}
}

func (p *Parser) parseGeneratedColumnAs(col *ColumnDef) {
	p.next() // skip AS
	if p.cur.Type == TokenLParen {
		p.next()
		col.Generated = p.parseExpr()
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
	// Optional STORED or VIRTUAL modifier after generated column expression
	if p.cur.Type == TokenKeyword && (p.cur.Value == "STORED" || p.cur.Value == "VIRTUAL") {
		p.next()
	}
}

func (p *Parser) parseOnConflictColumnConstraint(col *ColumnDef) {
	p.next() // skip ON
	if p.cur.Type == TokenKeyword && p.cur.Value == "CONFLICT" {
		p.next()
		if p.cur.Type == TokenKeyword {
			switch p.cur.Value {
			case "REPLACE", "ABORT", "FAIL", "ROLLBACK", "IGNORE":
				col.OnConflict = p.cur.Value
				p.next()
			}
		}
	}
}

func (p *Parser) parseCheckConstraint(col *ColumnDef) {
	p.next() // skip CHECK
	if p.cur.Type == TokenLParen {
		p.next()
		col.Check = p.parseExpr() // store the check expression
		p.expect(TokenRParen)
	}
}

func (p *Parser) parseReferencesConstraint(col *ColumnDef) {
	// Basic REFERENCES support - consume the clause
	p.next() // skip REFERENCES
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		col.References = p.cur.Value
		p.next()
	}
	// Optional parenthesized column list: REFERENCES t1(col1, col2)
	if p.cur.Type == TokenLParen {
		p.next()
		var cols []string
		for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			cols = append(cols, p.cur.Value)
			p.next()
			if p.cur.Type == TokenComma {
				p.next()
			} else {
				break
			}
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
		col.References += "(" + strings.Join(cols, ", ") + ")"
	}
	// Optional ON DELETE/UPDATE clauses
	for p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.parseReferencesOnAction()
	}
	// Optional MATCH clause
	if p.cur.Type == TokenKeyword && p.cur.Value == "MATCH" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next()
		}
	}
	// Optional DEFERRABLE clause: NOT DEFERRABLE, DEFERRABLE INITIALLY DEFERRED|IMMEDIATE
	p.skipDeferrableClause()
}

// parseReferencesOnAction consumes ON DELETE/UPDATE SET NULL|DEFAULT|CASCADE|RESTRICT|NO ACTION.
func (p *Parser) parseReferencesOnAction() {
	p.next() // skip ON
	if p.cur.Type == TokenKeyword && (p.cur.Value == "DELETE" || p.cur.Value == "UPDATE") {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "SET" {
			p.next()
			if p.cur.Type == TokenKeyword && (p.cur.Value == "NULL" || p.cur.Value == "DEFAULT") {
				p.next()
			}
		} else if p.cur.Type == TokenKeyword && (p.cur.Value == "CASCADE" || p.cur.Value == "RESTRICT") {
			p.next()
		} else if p.cur.Type == TokenKeyword && p.cur.Value == "NO" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "ACTION" {
				p.next()
			}
		}
	}
}

// skipDeferrableClause consumes an optional DEFERRABLE clause in a
// foreign key constraint: NOT DEFERRABLE, DEFERRABLE INITIALLY DEFERRED,
// or DEFERRABLE INITIALLY IMMEDIATE.
func (p *Parser) skipDeferrableClause() {
	if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "DEFERRABLE" {
			p.next()
		}
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "DEFERRABLE" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "INITIALLY" {
			p.next()
			if p.cur.Type == TokenKeyword && (p.cur.Value == "DEFERRED" || p.cur.Value == "IMMEDIATE") {
				p.next()
			}
		}
	}
}

func (p *Parser) parsePrimaryKeyConstraint(col *ColumnDef) {
	p.next()
	p.expectKeyword("KEY")
	col.PrimaryKey = true
	if p.cur.Type == TokenKeyword && p.cur.Value == "AUTOINCREMENT" {
		col.AutoInc = true
		p.next()
	}
	// Optional ASC/DESC sort order
	if p.cur.Type == TokenKeyword && (p.cur.Value == "ASC" || p.cur.Value == "DESC") {
		p.next()
	}
}

func (p *Parser) parseNotNullConstraint(col *ColumnDef) {
	p.next()
	if p.cur.Type == TokenKeyword && p.cur.Value == "NULL" {
		col.NotNull = true
		p.next()
	}
}

func (p *Parser) parseDefaultConstraint(col *ColumnDef) {
	p.next()
	col.Default = p.parseExpr()
}

func (p *Parser) parseIndexColumns() []IndexColumn {
	var cols []IndexColumn
	for {
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword ||
			p.cur.Type == TokenLParen || p.cur.Type == TokenNumber ||
			p.cur.Type == TokenString || p.cur.Type == TokenPlus ||
			p.cur.Type == TokenMinus || p.cur.Type == TokenBlob {
			expr := p.parseExpr()
			p.skipIndexColumnCollate()
			col := IndexColumn{}
			if colRef, ok := expr.(*ColumnRef); ok {
				col.Name = colRef.Name
			} else {
				// For expression-based index columns, store the expression text
				col.Name = ExprString(expr)
			}
			if p.cur.Type == TokenKeyword && p.cur.Value == "ASC" {
				p.next()
			} else if p.cur.Type == TokenKeyword && p.cur.Value == "DESC" {
				col.Desc = true
				p.next()
			}
			cols = append(cols, col)
		} else {
			break
		}
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return cols
}

// skipIndexColumnCollate skips an optional COLLATE clause in an index column
// definition (e.g., "COLLATE binary").
func (p *Parser) skipIndexColumnCollate() {
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLLATE" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next()
		}
	}
}

// DROP
func (p *Parser) parseDrop() Stmt {
	p.next() // skip DROP
	if p.cur.Type == TokenKeyword || p.cur.Type == TokenParam {
		switch p.cur.Value {
		case "TABLE":
			return p.parseDropTable()
		case "VIEW":
			return p.parseDropView()
		case "TRIGGER":
			return p.parseDropTrigger()
		case "INDEX":
			return p.parseDropIndex()
		default:
			// For $param or unknown keywords, just skip to next statement
			p.next()
			return &RollbackStmt{} // placeholder
		}
	}
	p.setErr("expected TABLE, VIEW, TRIGGER, or INDEX after DROP")
	return nil
}

func (p *Parser) parseDropTable() Stmt {
	p.next()
	s := &DropTableStmt{}
	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		p.expectKeyword("EXISTS")
		s.IfExists = true
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
		s.Name = p.cur.Value
		p.next()
		// Handle schema-qualified name (schema.table)
		if p.cur.Type == TokenDot {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
				s.Name = s.Name + "." + p.cur.Value
				p.next()
			}
		}
	}
	return s
}

func (p *Parser) parseDropView() Stmt {
	p.next()
	s := &DropViewStmt{}
	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		p.expectKeyword("EXISTS")
		s.IfExists = true
	}
	if name := p.readName(); name != "" {
		s.Name = name
	}
	return s
}

func (p *Parser) parseDropTrigger() Stmt {
	p.next()
	s := &DropTriggerStmt{}
	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		p.expectKeyword("EXISTS")
		s.IfExists = true
	}
	if name := p.readName(); name != "" {
		s.Name = name
	}
	return s
}

func (p *Parser) parseDropIndex() Stmt {
	p.next()
	s := &DropIndexStmt{}
	if p.cur.Type == TokenKeyword && p.cur.Value == "IF" {
		p.next()
		p.expectKeyword("EXISTS")
		s.IfExists = true
	}
	if name := p.readName(); name != "" {
		s.Name = name
	}
	return s
}

// Transactions
func (p *Parser) parseBegin() *BeginStmt {
	p.next()
	// Optional: DEFERRED/IMMEDIATE/EXCLUSIVE
	if p.cur.Type == TokenKeyword &&
		(p.cur.Value == "DEFERRED" || p.cur.Value == "IMMEDIATE" || p.cur.Value == "EXCLUSIVE") {
		p.next()
	}
	// Optional: TRANSACTION
	if p.cur.Type == TokenKeyword && p.cur.Value == "TRANSACTION" {
		p.next()
		// Optional transaction/savepoint name (identifier or string)
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenString {
			p.next()
		}
	}
	return &BeginStmt{}
}

func (p *Parser) parseCommit() *CommitStmt {
	p.next()
	// Optional: TRANSACTION
	if p.cur.Type == TokenKeyword && p.cur.Value == "TRANSACTION" {
		p.next()
		// Optional transaction/savepoint name (identifier or string)
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenString {
			p.next()
		}
	}
	return &CommitStmt{}
}

func (p *Parser) parseRollback() *RollbackStmt {
	p.next()
	// Optional: TRANSACTION
	if p.cur.Type == TokenKeyword && p.cur.Value == "TRANSACTION" {
		p.next()
	}
	// Optional transaction/savepoint name after TRANSACTION
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenString {
		p.next()
	}
	// Optional: TO SAVEPOINT savepoint_name
	if p.cur.Type == TokenKeyword && p.cur.Value == "TO" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenString {
			p.next()
		}
	}
	return &RollbackStmt{}
}

func (p *Parser) parsePragma() *PragmaStmt {
	s := &PragmaStmt{}
	p.next()
	if name := p.readName(); name != "" {
		s.Name = name
	}
	if p.cur.Type == TokenEq {
		// PRAGMA name = value
		p.next()
		if p.cur.Type == TokenNumber || p.cur.Type == TokenString || p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			s.Value = p.cur.Value
			p.next()
		}
	} else if p.cur.Type == TokenLParen {
		// PRAGMA name(value) — function-call syntax
		p.next()
		// Handle negative values like (-25)
		if p.cur.Type == TokenMinus && p.peek.Type == TokenNumber {
			s.Value = "-" + p.peek.Value
			p.next() // skip minus
			p.next() // skip number
		} else if p.cur.Type == TokenNumber || p.cur.Type == TokenString || p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			s.Value = p.cur.Value
			p.next()
		}
		if p.cur.Type == TokenRParen {
			p.next()
		}
	}
	return s
}

func (p *Parser) parseAlter() *AlterTableStmt {
	s := &AlterTableStmt{}
	p.next()
	if !p.expectKeyword("TABLE") {
		return nil
	}
	if name, info := p.readNameWithInfo(); name != "" {
		s.Table = name
		s.TableTok = info
	}
	if p.cur.Type == TokenKeyword {
		s.Action = p.cur.Value
		p.next()
	}
	if s.Action == "RENAME" {
		p.parseAlterRename(s)
	} else if s.Action == "ADD" {
		p.parseAlterAdd(s)
	} else if s.Action == "DROP" {
		p.parseAlterDrop(s)
	} else if s.Action == "ALTER" {
		p.parseAlterAlter(s)
	}
	return s
}

func (p *Parser) parseAlterAlter(s *AlterTableStmt) {
	// ALTER TABLE ... ALTER COLUMN SET NOT NULL / DROP DEFAULT / etc.
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN" {
		p.next()
	}
	if p.cur.Type == TokenIdentifier {
		s.Column = p.cur.Value
		p.next()
	}
	// Capture SET/DROP action
	if p.cur.Type == TokenKeyword && p.cur.Value == "SET" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "NULL" {
				p.next()
				s.AlterColAction = "SET NOT NULL"
			}
		}
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "DROP" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "NULL" {
				p.next()
				s.AlterColAction = "DROP NOT NULL"
			}
		}
	}
}

func (p *Parser) parseAlterRename(s *AlterTableStmt) {
	// ALTER TABLE t RENAME TO newname         — rename table
	// ALTER TABLE t RENAME column TO newname   — rename column
	// ALTER TABLE t RENAME COLUMN column TO newname
	if p.cur.Type == TokenIdentifier || (p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN") {
		// Column rename: RENAME [COLUMN] old_name TO new_name
		if p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN" {
			p.next()
		}
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			s.Column = p.cur.Value
			p.next()
		}
	}
	if !p.expectKeyword("TO") {
		return
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
		s.NewName = p.cur.Value
		p.next()
	}
}

func (p *Parser) parseAlterAdd(s *AlterTableStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN" {
		p.next()
	}

	// ALTER TABLE ... ADD CONSTRAINT name ... (table constraint)
	if p.cur.Type == TokenKeyword && p.cur.Value == "CONSTRAINT" {
		// This is a table-level constraint, not a column definition.
		// Store the constraint keyword and skip it.
		s.Column = "CONSTRAINT"
		p.skipTableConstraint()
		return
	}

	// Column name: can be identifier or keyword
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		start, end := p.computeTokenBounds()
		s.Column = p.cur.Value
		s.ColumnTok = TokenInfo{Start: start, End: end}
		p.next()
	}
	// Column type (optional): use parseColumnType which already handles
	// isConstraintStart to avoid reading constraint keywords as type names.
	s.ColDef.Type = p.parseColumnType()
	// Parse column constraints (DEFAULT, NOT NULL, PRIMARY KEY, REFERENCES, CHECK, etc.)
	p.parseColumnConstraints(&s.ColDef)
}

func (p *Parser) parseAlterDrop(s *AlterTableStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN" {
		p.next()
	}
	// DROP CONSTRAINT name
	if p.cur.Type == TokenKeyword && p.cur.Value == "CONSTRAINT" {
		p.next()
		s.Column = "CONSTRAINT"
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			s.NewName = p.cur.Value // store the constraint name
			p.next()
		}
		return
	}
	if p.cur.Type == TokenIdentifier {
		s.Column = p.cur.Value
		p.next()
	}
}

func (p *Parser) parseAttach() *AttachStmt {
	s := &AttachStmt{}
	p.next()
	// DATABASE keyword is optional for ATTACH
	if p.cur.Type == TokenKeyword && p.cur.Value == "DATABASE" {
		p.next()
	}
	if p.cur.Type == TokenString || p.cur.Type == TokenParam {
		s.Path = p.cur.Value
		p.next()
	} else {
		// Try to parse an expression for the database path
		// (e.g., ATTACH printf('file:%09000x/x.db',1) AS aux1;)
		expr := p.parseExpr()
		if expr != nil {
			s.PathExpr = expr
		}
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString || p.cur.Type == TokenParam {
			// Preserve original case for schema names (lexer uppercases keywords)
			if p.cur.Type == TokenKeyword {
				// Extract original text from input using position and value length
				s.Schema = p.tokens.input[p.cur.Pos : p.cur.Pos+len(p.cur.Value)]
			} else {
				s.Schema = p.cur.Value
			}
			p.next()
		}
	}
	return s
}

func (p *Parser) parseDetach() *AttachStmt {
	s := &AttachStmt{IsDetach: true}
	p.next()
	// DATABASE keyword is optional for DETACH
	if p.cur.Type == TokenKeyword && p.cur.Value == "DATABASE" {
		p.next()
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString || p.cur.Type == TokenParam {
		// Preserve original case for schema names (lexer uppercases keywords)
		if p.cur.Type == TokenKeyword {
			// Extract original text from input using position and value length
			s.Schema = p.tokens.input[p.cur.Pos : p.cur.Pos+len(p.cur.Value)]
		} else {
			s.Schema = p.cur.Value
		}
		p.next()
	}
	return s
}

func (p *Parser) parseVacuum() *VacuumStmt {
	p.next()
	return &VacuumStmt{}
}

func (p *Parser) parseReindex() *ReindexStmt {
	p.next()
	// Consume optional name or schema-qualified name
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		p.readName()
	}
	return &ReindexStmt{}
}

func (p *Parser) parseSavepoint() *SavepointStmt {
	s := &SavepointStmt{}
	s.Type = p.cur.Value
	p.next()
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}
	return s
}

// Expression parsing (simplified recursive descent)
func (p *Parser) parseExpr() Expr {
	return p.parseOrExpr()
}

func (p *Parser) parseOrExpr() Expr {
	left := p.parseAndExpr()
	for p.cur.Type == TokenKeyword && p.cur.Value == "OR" {
		op := p.cur.Value
		p.next()
		right := p.parseAndExpr()
		left = &BinaryOp{Left: left, Right: right, Operator: op}
	}
	// Handle COLLATE after complete expression (e.g., expr COLLATE nocase)
	left = p.skipCollateExpr(left)
	return left
}

func (p *Parser) parseAndExpr() Expr {
	left := p.parseNotExpr()
	for p.cur.Type == TokenKeyword && p.cur.Value == "AND" {
		op := p.cur.Value
		p.next()
		right := p.parseNotExpr()
		left = &BinaryOp{Left: left, Right: right, Operator: op}
	}
	return left
}

func (p *Parser) parseNotExpr() Expr {
	if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		return &UnaryOp{Operand: p.parseCompareExpr(), Operator: "NOT"}
	}
	return p.parseCompareExpr()
}

func (p *Parser) parseCompareExpr() Expr {
	left := p.parseAddExpr()
	for {
		next := p.tryCompareOp(left)
		if next == nil {
			return left
		}
		left = next
	}
}

func (p *Parser) tryCompareOp(left Expr) Expr {
	if p.cur.Type == TokenEq || p.cur.Type == TokenNeq ||
		p.cur.Type == TokenLt || p.cur.Type == TokenGt ||
		p.cur.Type == TokenLe || p.cur.Type == TokenGe {
		// Handle << (left shift) - two consecutive < tokens
		if p.cur.Type == TokenLt && p.peek.Type == TokenLt {
			p.next() // skip first <
			p.next() // skip second <
			right := p.parseMulExpr()
			return &BinaryOp{Left: left, Right: right, Operator: "<<"}
		}
		// Handle >> (right shift) - two consecutive > tokens
		if p.cur.Type == TokenGt && p.peek.Type == TokenGt {
			p.next() // skip first >
			p.next() // skip second >
			right := p.parseMulExpr()
			return &BinaryOp{Left: left, Right: right, Operator: ">>"}
		}
		return p.binaryOp(left)
	}
	return p.tryCompareKeywordOp(left)
}

func (p *Parser) tryCompareKeywordOp(left Expr) Expr {
	if p.cur.Type != TokenKeyword {
		return nil
	}
	switch p.cur.Value {
	case "IS":
		return p.parseIsOp(left)
	case "NOT":
		return p.tryNotOp(left)
	case "IN":
		return p.parseInOp(left)
	case "BETWEEN":
		return p.parseBetweenOp(left)
	case "LIKE":
		return p.parseLikeOp(left)
	case "GLOB":
		return p.parseGlobOp(left)
	case "REGEXP":
		return p.parseRegexpOp(left)
	case "MATCH":
		return p.parseMatchOp(left)
	case "NOTNULL":
		p.next()
		return &IsNotNull{Operand: left}
	case "ISNULL":
		p.next()
		return &IsNull{Operand: left}
	default:
		return nil
	}
}

func (p *Parser) tryNotOp(left Expr) Expr {
	saved := p.cur
	p.next()
	switch {
	case p.cur.Type == TokenKeyword && p.cur.Value == "IN":
		return p.parseNegatedInOp(left)
	case p.cur.Type == TokenKeyword && p.cur.Value == "BETWEEN":
		expr := p.parseBetweenOp(left)
		if b, ok := expr.(*Between); ok {
			b.Negated = true
		}
		return expr
	case p.cur.Type == TokenKeyword && p.cur.Value == "LIKE":
		p.next()
		right := p.parseAddExpr()
		return &BinaryOp{Left: left, Right: right, Operator: "NOT LIKE"}
	case p.cur.Type == TokenKeyword && p.cur.Value == "GLOB":
		p.next()
		right := p.parseAddExpr()
		return &BinaryOp{Left: left, Right: right, Operator: "NOT GLOB"}
	case p.cur.Type == TokenKeyword && p.cur.Value == "REGEXP":
		p.next()
		right := p.parseAddExpr()
		return &BinaryOp{Left: left, Right: right, Operator: "NOT REGEXP"}
	case p.cur.Type == TokenKeyword && p.cur.Value == "MATCH":
		p.next()
		right := p.parseAddExpr()
		return &BinaryOp{Left: left, Right: right, Operator: "NOT MATCH"}
	case p.cur.Type == TokenKeyword && p.cur.Value == "NULL":
		p.next()
		return &IsNotNull{Operand: left}
	default:
		p.cur = saved
		return nil
	}
}

func (p *Parser) binaryOp(left Expr) Expr {
	op := p.cur.Value
	p.next()
	right := p.parseAddExpr()
	return &BinaryOp{Left: left, Right: right, Operator: op}
}

func (p *Parser) parseIsOp(left Expr) Expr {
	p.next() // skip IS
	if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "NULL" {
			p.next()
			return &IsNotNull{Operand: left}
		}
		// IS NOT DISTINCT FROM
		if p.cur.Type == TokenKeyword && p.cur.Value == "DISTINCT" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "FROM" {
				p.next()
				right := p.parseExpr()
				return &IsNotDistinctFrom{Left: left, Right: right}
			}
		}
		// IS NOT TRUE or IS NOT FALSE
		if isTrueFalseToken(p.cur) {
			if strings.EqualFold(p.cur.Value, "TRUE") {
				p.next()
				return &IsTrue{Operand: left, Negated: true}
			}
			p.next()
			return &IsFalse{Operand: left, Negated: true}
		}
		p.consumeIsRightOperand()
		return left
	}
	// IS DISTINCT FROM
	if p.cur.Type == TokenKeyword && p.cur.Value == "DISTINCT" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "FROM" {
			p.next()
			right := p.parseExpr()
			return &IsDistinctFrom{Left: left, Right: right}
		}
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "NULL" {
		p.next()
		return &IsNull{Operand: left}
	}
	// IS TRUE or IS FALSE
	if isTrueFalseToken(p.cur) {
		if strings.EqualFold(p.cur.Value, "TRUE") {
			p.next()
			return &IsTrue{Operand: left}
		}
		p.next()
		return &IsFalse{Operand: left}
	}
	p.consumeIsRightOperand()
	return left
}

// isTrueFalseToken checks if the current token is TRUE or FALSE (case-insensitive).
func isTrueFalseToken(tok Token) bool {
	if tok.Type == TokenKeyword || tok.Type == TokenIdentifier {
		if strings.EqualFold(tok.Value, "TRUE") || strings.EqualFold(tok.Value, "FALSE") {
			return true
		}
	}
	return false
}

// consumeIsRightOperand consumes the right operand expression after IS or IS NOT,
// handling TRUE/FALSE as identifiers as well as any expression type.
func (p *Parser) consumeIsRightOperand() {
	if p.cur.Type == TokenIdentifier && (strings.EqualFold(p.cur.Value, "TRUE") || strings.EqualFold(p.cur.Value, "FALSE")) {
		p.next()
		return
	}
	if p.cur.Type == TokenNumber || p.cur.Type == TokenString || p.cur.Type == TokenIdentifier ||
		p.cur.Type == TokenKeyword || p.cur.Type == TokenLParen || p.cur.Type == TokenBlob ||
		p.cur.Type == TokenPlus || p.cur.Type == TokenMinus {
		p.parseExpr()
	}
}

func (p *Parser) parseInOp(left Expr) Expr {
	p.next()
	// SQLite allows IN tablename as shorthand for IN (SELECT * FROM tablename)
	// Only accept identifiers (not keywords) to avoid consuming clause markers.
	if p.cur.Type == TokenIdentifier {
		tableName := p.cur.Value
		p.next()
		sel := &SelectStmt{
			Columns: []SelectColumn{{Expr: &ColumnRef{Name: "*"}}},
			From:    TableRef{Name: tableName},
		}
		return &InList{Operand: left, List: []Expr{&Subquery{Select: sel}}}
	}
	if !p.expect(TokenLParen) {
		return left
	}
	// Check for empty IN () — valid in SQLite as IN (empty-list)
	if p.cur.Type == TokenRParen {
		p.next() // consume )
		return &InList{Operand: left, List: []Expr{}}
	}
	// Check for subquery: IN (SELECT ...)
	if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
		sel := p.parseSelect()
		if !p.expect(TokenRParen) {
			return left
		}
		// For IN with subquery, evaluate the subquery as a list
		// Store the select expression; the executor will handle it
		return &InList{Operand: left, List: []Expr{&Subquery{Select: sel}}}
	}
	list := p.parseExprList()
	if !p.expect(TokenRParen) {
		return left
	}
	return &InList{Operand: left, List: list}
}

func (p *Parser) parseNegatedInOp(left Expr) Expr {
	p.next() // skip IN
	// SQLite allows NOT IN tablename as shorthand for NOT IN (SELECT * FROM tablename)
	if p.cur.Type == TokenIdentifier {
		tableName := p.cur.Value
		p.next()
		sel := &SelectStmt{
			Columns: []SelectColumn{{Expr: &ColumnRef{Name: "*"}}},
			From:    TableRef{Name: tableName},
		}
		return &InList{Operand: left, List: []Expr{&Subquery{Select: sel}}, Negated: true}
	}
	if !p.expect(TokenLParen) {
		return left
	}
	// Check for empty IN () — valid in SQLite as NOT IN (empty-list)
	if p.cur.Type == TokenRParen {
		p.next() // consume )
		return &InList{Operand: left, List: []Expr{}, Negated: true}
	}
	// Check for subquery: NOT IN (SELECT ...)
	if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
		sel := p.parseSelect()
		if !p.expect(TokenRParen) {
			return left
		}
		return &InList{Operand: left, List: []Expr{&Subquery{Select: sel}}, Negated: true}
	}
	list := p.parseExprList()
	if !p.expect(TokenRParen) {
		return left
	}
	return &InList{Operand: left, List: list, Negated: true}
}

func (p *Parser) parseBetweenOp(left Expr) Expr {
	p.next()
	low := p.parseAddExpr()
	p.expectKeyword("AND")
	high := p.parseAddExpr()
	return &Between{Operand: left, Low: low, High: high}
}

func (p *Parser) parseLikeOp(left Expr) Expr {
	p.next()
	right := p.parseAddExpr()
	// Optional ESCAPE clause
	escape := ""
	if p.cur.Type == TokenKeyword && p.cur.Value == "ESCAPE" {
		p.next()
		if p.cur.Type == TokenString {
			escape = p.cur.Value
			p.next()
		}
	}
	return &BinaryOp{Left: left, Right: right, Operator: "LIKE", Escape: escape}
}

func (p *Parser) parseGlobOp(left Expr) Expr {
	p.next()
	right := p.parseAddExpr()
	return &BinaryOp{Left: left, Right: right, Operator: "GLOB"}
}

func (p *Parser) parseRegexpOp(left Expr) Expr {
	p.next()
	right := p.parseAddExpr()
	return &BinaryOp{Left: left, Right: right, Operator: "REGEXP"}
}

func (p *Parser) parseMatchOp(left Expr) Expr {
	p.next()
	right := p.parseAddExpr()
	return &BinaryOp{Left: left, Right: right, Operator: "MATCH"}
}

func (p *Parser) parseAddExpr() Expr {
	left := p.parseMulExpr()
	for {
		switch {
		case p.cur.Type == TokenPlus:
			p.next()
			right := p.parseMulExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "+"}
		case p.cur.Type == TokenMinus:
			p.next()
			right := p.parseMulExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "-"}
		case p.cur.Type == TokenConcat:
			p.next()
			right := p.parseMulExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "||"}
		default:
			return left
		}
	}
}

func (p *Parser) parseMulExpr() Expr {
	left := p.parseUnaryExpr()
	for {
		switch {
		case p.cur.Type == TokenStar:
			p.next()
			right := p.parseUnaryExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "*"}
		case p.cur.Type == TokenSlash:
			p.next()
			right := p.parseUnaryExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "/"}
		case p.cur.Type == TokenMod:
			p.next()
			right := p.parseUnaryExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "%"}
		case p.cur.Type == TokenBitAnd:
			p.next()
			right := p.parseUnaryExpr()
			left = &BinaryOp{Left: left, Right: right, Operator: "&"}
		default:
			return left
		}
	}
}

func (p *Parser) parseUnaryExpr() Expr {
	if p.cur.Type == TokenPlus {
		p.next()
		return &UnaryOp{Operand: p.parseUnaryExpr(), Operator: "+"}
	}
	if p.cur.Type == TokenMinus {
		p.next()
		return &UnaryOp{Operand: p.parseUnaryExpr(), Operator: "-"}
	}
	if p.cur.Type == TokenTilde {
		p.next()
		return &UnaryOp{Operand: p.parseUnaryExpr(), Operator: "~"}
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		return &UnaryOp{Operand: p.parseUnaryExpr(), Operator: "NOT"}
	}
	return p.parsePrimaryExpr()
}

func (p *Parser) parsePrimaryExpr() Expr {
	result := p.parsePrimaryExprInner()
	if result != nil {
		result = p.skipCollateExpr(result)
	}
	// Handle JSON operators: -> and ->>
	for p.cur.Type == TokenArrow || p.cur.Type == TokenDoubleArrow {
		op := "->"
		if p.cur.Type == TokenDoubleArrow {
			op = "->>"
		}
		p.next()
		right := p.parsePrimaryExpr()
		result = &BinaryOp{Left: result, Right: right, Operator: op}
	}
	return result
}

func (p *Parser) parsePrimaryExprInner() Expr {
	switch p.cur.Type {
	case TokenNumber:
		lit := &NumericLit{Value: p.cur.Value}
		p.next()
		return lit

	case TokenString:
		lit := &StringLit{Value: p.cur.Value}
		p.next()
		return lit

	case TokenBlob:
		lit, err := decodeBlobLiteral(p.cur.Value)
		if err != nil {
			// If decoding fails, treat as string (graceful fallback)
			return &StringLit{Value: "x" + p.cur.Value}
		}
		p.next()
		return lit

	case TokenIdentifier:
		nameVal := p.cur.Value
		nameStart, nameEnd := p.computeTokenBounds()
		p.next()

		// Function call
		if p.cur.Type == TokenLParen {
			return p.parseFunctionCall(nameVal)
		}

		// Handle dot (table.column or schema.table.column or table.*)
		tableName := ""
		var tableTok TokenInfo
		for p.cur.Type == TokenDot {
			p.next() // consume dot
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenStar {
				if tableName == "" {
					tableName = nameVal
					tableTok = TokenInfo{Start: nameStart, End: nameEnd}
					nameVal = p.cur.Value
					nameStart, nameEnd = p.computeTokenBounds()
				} else {
					// Three-part name: schema.table.column
					if p.cur.Type == TokenStar {
						return &ColumnRef{
							Table:    tableName + "." + nameVal,
							Name:     p.cur.Value,
							TableTok: tableTok,
							NameTok:  TokenInfo{Start: nameStart, End: nameEnd},
						}
					}
					// For three-part name, table becomes "schema.table" and name becomes "column"
					tableName = tableName + "." + nameVal
					nameVal = p.cur.Value
					nameStart, nameEnd = p.computeTokenBounds()
				}
				p.next()
			} else {
				return &ColumnRef{Name: nameVal, NameTok: TokenInfo{Start: nameStart, End: nameEnd}}
			}
		}
		if tableName != "" && nameVal == "*" {
			return &ColumnRef{Table: tableName, Name: "*", TableTok: tableTok}
		}
		return &ColumnRef{Table: tableName, Name: nameVal, TableTok: tableTok, NameTok: TokenInfo{Start: nameStart, End: nameEnd}}

	case TokenLParen:
		p.next()
		return p.parseParenExpr()

	case TokenKeyword:
		return p.parseKeywordExpr()

	case TokenParam:
		p.next()
		// Handle array-style parameter access: $::x(1)
		// In SQLite this references an array parameter element;
		// consume the parenthesized index to avoid parse errors.
		if p.cur.Type == TokenLParen {
			p.skipParenExpr()
		}
		return &NullLit{}

	default:
		p.setErr("unexpected token in expression: %s", tokenName(p.cur.Type, p.cur.Value))
		return nil
	}
}

func (p *Parser) parseParenExpr() Expr {
	// Subquery: (SELECT ...)
	if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
		sel := p.parseSelect()
		if !p.expect(TokenRParen) {
			return nil
		}
		return &Subquery{Select: sel}
	}
	// Scalar subquery with CTE: (WITH ... SELECT ...)
	if p.cur.Type == TokenKeyword && p.cur.Value == "WITH" {
		sel := p.parseWithStatement()
		if s, ok := sel.(*SelectStmt); ok {
			if !p.expect(TokenRParen) {
				return nil
			}
			return &Subquery{Select: s}
		}
		return nil
	}
	expr := p.parseExpr()
	// Row value: (a, b, c) — comma-separated list of expressions
	if p.cur.Type == TokenComma {
		values := []Expr{expr}
		for p.cur.Type == TokenComma {
			p.next()
			values = append(values, p.parseExpr())
		}
		p.expect(TokenRParen)
		return &RowValue{Values: values}
	}
	p.expect(TokenRParen)
	return &ParenExpr{Expr: expr}
}

func (p *Parser) parseFunctionCall(name string) Expr {
	p.next() // skip (
	// Check for DISTINCT keyword inside function call
	distinct := false
	if p.cur.Type == TokenKeyword && p.cur.Value == "DISTINCT" {
		distinct = true
		p.next()
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "ALL" {
		// ALL keyword (default behavior, just consume it)
		p.next()
	}
	// Handle COUNT(*) - * as function argument
	if p.cur.Type == TokenStar {
		args := []Expr{&ColumnRef{Name: "*"}}
		p.next()
		p.expect(TokenRParen)
		fc := &FuncCall{Name: name, Args: args, Distinct: distinct}
		fc.Over, fc.Filter = p.parseWindowClause()
		return fc
	}
	// Handle empty argument list with ORDER BY: count(ORDER BY x)
	var args []Expr
	var fc *FuncCall
	if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
		// Empty args, just ORDER BY
		orderBy := p.parseFunctionOrderBy()
		fc = &FuncCall{Name: name, Args: args, Distinct: distinct, OrderBy: orderBy}
	} else {
		args = p.parseExprList()
		// Parse optional ORDER BY inside function call: string_agg(x ORDER BY y)
		var orderBy []OrderByTerm
		if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
			orderBy = p.parseFunctionOrderBy()
		}
		fc = &FuncCall{Name: name, Args: args, Distinct: distinct, OrderBy: orderBy}
	}
	p.expect(TokenRParen)
	fc.Over, fc.Filter = p.parseWindowClause()
	return fc
}

func (p *Parser) parseFunctionOrderBy() []OrderByTerm {
	var terms []OrderByTerm
	p.next() // skip ORDER
	if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
		p.next() // skip BY
		for {
			expr := p.parseExpr()
			if expr == nil {
				break
			}
			desc := false
			// Optional ASC/DESC
			if p.cur.Type == TokenKeyword && (p.cur.Value == "ASC" || p.cur.Value == "DESC") {
				desc = p.cur.Value == "DESC"
				p.next()
			}
			// Optional NULLS FIRST/LAST
			if p.cur.Type == TokenKeyword && p.cur.Value == "NULLS" {
				p.next()
				if p.cur.Type == TokenKeyword && (p.cur.Value == "FIRST" || p.cur.Value == "LAST") {
					p.next()
				}
			}
			terms = append(terms, OrderByTerm{Expr: expr, Desc: desc})
			if p.cur.Type == TokenComma {
				p.next()
			} else {
				break
			}
		}
	}
	return terms
}

func (p *Parser) parseKeywordExpr() Expr {
	kw := p.cur.Value
	start, end := p.computeTokenBounds()
	p.next()

	switch kw {
	case "NULL":
		return &NullLit{}
	case "TRUE":
		// TRUE can be used as identifier (column name) in dot notation
		if p.cur.Type == TokenDot {
			return p.parseKeywordDotSuffix(kw, start, end)
		}
		return &NumericLit{Value: "1"}
	case "FALSE":
		// FALSE can be used as identifier (column name) in dot notation
		if p.cur.Type == TokenDot {
			return p.parseKeywordDotSuffix(kw, start, end)
		}
		return &NumericLit{Value: "0"}
	case "CASE":
		return p.parseCaseExpr()
	case "CAST":
		return p.parseCastExpr()
	case "EXISTS":
		return p.parseExistsExpr()
	default:
		// Could be a keyword used as identifier or function name
		if p.cur.Type == TokenLParen {
			p.next()
			args := p.parseExprList()
			p.expect(TokenRParen)
			return &FuncCall{Name: kw, Args: args}
		}
		return &ColumnRef{Name: kw, NameTok: TokenInfo{Start: start, End: end}}
	}
}

// parseKeywordDotSuffix handles table.column syntax when the table name
// is a keyword (e.g., true.column or false.column).
func (p *Parser) parseKeywordDotSuffix(tableName string, tableStart, tableEnd int) Expr {
	p.next() // consume dot
	tableTok := TokenInfo{Start: tableStart, End: tableEnd}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		nameVal := p.cur.Value
		nameStart, nameEnd := p.computeTokenBounds()
		p.next()
		return &ColumnRef{
			Table:    tableName,
			Name:     nameVal,
			TableTok: tableTok,
			NameTok:  TokenInfo{Start: nameStart, End: nameEnd},
		}
	} else if p.cur.Type == TokenStar {
		p.next()
		return &ColumnRef{Table: tableName, Name: "*", TableTok: tableTok}
	}
	return &ColumnRef{Name: tableName, NameTok: tableTok}
}

func (p *Parser) parseExistsExpr() Expr {
	if !p.expect(TokenLParen) {
		return nil
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
		sel := p.parseSelect()
		p.expect(TokenRParen)
		return &ExistsExpr{Select: sel}
	}
	p.expect(TokenRParen)
	return nil
}

func (p *Parser) parseCaseExpr() Expr {
	c := &CaseExpr{}
	// CASE x WHEN ... (optional operand)
	if p.cur.Type != TokenKeyword || p.cur.Value != "WHEN" {
		c.Operand = p.parseExpr()
	}
	for p.cur.Type == TokenKeyword && p.cur.Value == "WHEN" {
		p.next()
		w := WhenClause{}
		w.When = p.parseExpr()
		if !p.expectKeyword("THEN") {
			break
		}
		w.Then = p.parseExpr()
		c.Whens = append(c.Whens, w)
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "ELSE" {
		p.next()
		c.Else = p.parseExpr()
	}
	if !p.expectKeyword("END") {
		return nil
	}
	return c
}

func (p *Parser) parseCastExpr() Expr {
	if !p.expect(TokenLParen) {
		return nil
	}
	operand := p.parseExpr()
	if !p.expectKeyword("AS") {
		return nil
	}
	asType := ""
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		asType = p.cur.Value
		p.next()
		// Handle type arguments: AS VARCHAR(50)
		if p.cur.Type == TokenLParen {
			p.skipParenExpr()
		}
	}
	if !p.expect(TokenRParen) {
		return nil
	}
	return &CastExpr{Operand: operand, AsType: asType}
}

func (p *Parser) parseExprList() []Expr {
	var list []Expr
	if p.cur.Type == TokenRParen {
		return list
	}
	for {
		expr := p.parseExpr()
		list = append(list, expr)
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return list
}

func (p *Parser) parseIdentList() []string {
	var list []string
	for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		list = append(list, p.cur.Value)
		p.next()
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return list
}

// EvalNumber evaluates an expression to a number (for LIMIT/offset).
func EvalNumber(e Expr) (int64, bool) {
	switch v := e.(type) {
	case *NumericLit:
		n, err := strconv.ParseInt(v.Value, 10, 64)
		if err != nil {
			f, err := strconv.ParseFloat(v.Value, 64)
			if err != nil {
				return 0, false
			}
			return int64(f), true
		}
		return n, true
	case *UnaryOp:
		if v.Operator == "-" {
			inner, ok := EvalNumber(v.Operand)
			if !ok {
				return 0, false
			}
			return -inner, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// EvalString evaluates an expression to a string.
func EvalString(e Expr) (string, bool) {
	switch v := e.(type) {
	case *StringLit:
		return v.Value, true
	case *ColumnRef:
		return v.Name, true
	default:
		return "", false
	}
}

// ExprString converts an Expr back to its SQL text representation.
// Used for serializing CHECK constraints and other expressions.
func ExprString(e Expr) string {
	if e == nil {
		return ""
	}
	switch v := e.(type) {
	case *NumericLit:
		return v.Value
	case *StringLit:
		return "'" + strings.ReplaceAll(v.Value, "'", "''") + "'"
	case *NullLit:
		return "NULL"
	case *ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Name
		}
		return v.Name
	case *BinaryOp:
		return ExprString(v.Left) + " " + v.Operator + " " + ExprString(v.Right)
	case *UnaryOp:
		return v.Operator + " " + ExprString(v.Operand)
	case *IsNull:
		return ExprString(v.Operand) + " IS NULL"
	case *IsNotNull:
		return ExprString(v.Operand) + " NOT NULL"
	case *IsDistinctFrom:
		return ExprString(v.Left) + " IS DISTINCT FROM " + ExprString(v.Right)
	case *IsNotDistinctFrom:
		return ExprString(v.Left) + " IS NOT DISTINCT FROM " + ExprString(v.Right)
	case *IsTrue:
		if v.Negated {
			return ExprString(v.Operand) + " IS NOT TRUE"
		}
		return ExprString(v.Operand) + " IS TRUE"
	case *IsFalse:
		if v.Negated {
			return ExprString(v.Operand) + " IS NOT FALSE"
		}
		return ExprString(v.Operand) + " IS FALSE"
	case *BlobLit:
		return "x'" + hex.EncodeToString(v.Value) + "'"
	case *Subquery:
		if v.Select != nil {
			return "(" + selectStmtToString(v.Select) + ")"
		}
		return "(?)"
	case *ExistsExpr:
		s := "EXISTS "
		if v.Negated {
			s = "NOT EXISTS "
		}
		if v.Select != nil {
			return s + "(" + selectStmtToString(v.Select) + ")"
		}
		return s + "(?)"
	case *Between:
		return formatBetween(v)
	case *InList:
		return formatInList(v)
	case *FuncCall:
		return formatFuncCall(v)
	case *RowValue:
		result := "("
		for i, val := range v.Values {
			if i > 0 {
				result += ", "
			}
			result += ExprString(val)
		}
		return result + ")"
	case *ParenExpr:
		return "(" + ExprString(v.Expr) + ")"
	default:
		return "?"
	}
}

// selectStmtToString converts a SelectStmt back to SQL text for use in ExprString.
// This is a simplified serialization that handles common patterns needed for
// trigger body serialization. It is intentionally kept in the sql package to
// avoid circular dependencies with exec.selectStmtToString.
func selectStmtToString(s *SelectStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder

	// CTEs
	if len(s.CTEs) > 0 {
		b.WriteString("WITH ")
		for i, cte := range s.CTEs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(cte.Name)
			if len(cte.Columns) > 0 {
				b.WriteString("(")
				for j, col := range cte.Columns {
					if j > 0 {
						b.WriteString(", ")
					}
					b.WriteString(col)
				}
				b.WriteString(")")
			}
			b.WriteString(" AS (")
			if cte.Select != nil {
				b.WriteString(selectStmtToString(cte.Select))
			}
			b.WriteString(")")
		}
		b.WriteString(" ")
	}

	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	for i, col := range s.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(ExprString(col.Expr))
		if col.As != "" {
			b.WriteString(" AS ")
			b.WriteString(col.As)
		}
	}
	if s.From.Name != "" {
		b.WriteString(" FROM ")
		b.WriteString(s.From.Name)
		if s.From.As != "" {
			b.WriteString(" AS ")
			b.WriteString(s.From.As)
		}
	}
	// JOINs
	for _, join := range s.Joins {
		b.WriteString(" ")
		b.WriteString(join.JoinType)
		b.WriteString(" JOIN ")
		b.WriteString(join.Table.Name)
		if join.Table.As != "" {
			b.WriteString(" AS ")
			b.WriteString(join.Table.As)
		}
		if join.On != nil {
			b.WriteString(" ON ")
			b.WriteString(ExprString(join.On))
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(ExprString(s.Where))
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY ")
		for i, gb := range s.GroupBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ExprString(gb))
		}
	}
	if s.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(ExprString(s.Having))
	}
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, ob := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ExprString(ob.Expr))
			if ob.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(ExprString(s.Limit))
	}
	if s.Offset != nil {
		b.WriteString(" OFFSET ")
		b.WriteString(ExprString(s.Offset))
	}
	// UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		switch s.SetOp {
		case SetUnion:
			b.WriteString(" UNION ")
			if s.UnionAll {
				b.WriteString("ALL ")
			}
		case SetIntersect:
			b.WriteString(" INTERSECT ")
		case SetExcept:
			b.WriteString(" EXCEPT ")
		}
		b.WriteString(selectStmtToString(s.Union))
	}
	return b.String()
}

func formatBetween(v *Between) string {
	s := ExprString(v.Operand) + " BETWEEN " + ExprString(v.Low) + " AND " + ExprString(v.High)
	if v.Negated {
		s = "NOT (" + s + ")"
	}
	return s
}

func formatInList(v *InList) string {
	var items []string
	for _, item := range v.List {
		items = append(items, ExprString(item))
	}
	s := ExprString(v.Operand)
	if v.Negated {
		s += " NOT IN ("
	} else {
		s += " IN ("
	}
	s += strings.Join(items, ", ") + ")"
	return s
}

func formatFuncCall(v *FuncCall) string {
	var args []string
	for _, arg := range v.Args {
		args = append(args, ExprString(arg))
	}
	result := v.Name + "(" + strings.Join(args, ", ") + ")"
	if v.Filter != nil {
		result += " FILTER (WHERE " + ExprString(v.Filter) + ")"
	}
	if v.Over != nil {
		result += " OVER " + v.Over.String()
	}
	return result
}

// parseWindowClause parses OVER and FILTER clauses after a function call.
// Returns the OVER clause WindowDef and an optional FILTER expression.
// Handles both orderings: FILTER (...) OVER (...) and OVER (...) FILTER (...).
func (p *Parser) parseWindowClause() (over *WindowDef, filter Expr) {
	// Prevent recursive window spec parsing. Window functions cannot be
	// nested inside window definitions (OVER clauses), so skip window
	// clause processing when already inside a window spec.
	if p.windowSpecDepth > 0 {
		return nil, nil
	}
	var ov *WindowDef
	var flt Expr
	// Loop to handle both orderings: FILTER...OVER or OVER...FILTER
	for {
		if p.cur.Type == TokenKeyword && p.cur.Value == "OVER" && ov == nil {
			p.next() // skip OVER
			// Named window: OVER windowName
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				ov = &WindowDef{Name: p.cur.Value}
				p.next()
			} else if p.cur.Type == TokenLParen {
				// Inline window spec: parse and store
				ov = p.parseInlineWindowSpec()
			}
			// If neither, OVER was incomplete (e.g., "OVER ,") — just skip
			continue
		}
		if p.cur.Type == TokenKeyword && p.cur.Value == "FILTER" && flt == nil {
			p.next() // skip FILTER
			if p.cur.Type == TokenLParen {
				p.next()
				if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
					p.next()
					flt = p.parseExpr()
				}
				p.expect(TokenRParen)
			}
			continue
		}
		if p.cur.Type == TokenKeyword && p.cur.Value == "WITHIN" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "GROUP" {
				p.next()
				if p.expect(TokenLParen) {
					p.parseOrderBy()
					p.expect(TokenRParen)
				}
			}
			continue
		}
		break
	}
	return ov, flt
}

// skipWindowClause skips window function clauses (OVER, FILTER, WITHIN GROUP)
// after a function call. This is a stub: the window clause is parsed but not
//lint:ignore U1000 Parser utility for future use
// semantically analyzed.
 func (p *Parser) skipWindowClause() {
 	if p.cur.Type == TokenKeyword && p.cur.Value == "OVER" {
 		p.next() // skip OVER
 		p.skipWindowSpec()
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "FILTER" {
		p.next() // skip FILTER
		if p.cur.Type == TokenLParen {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
				p.next()
				p.parseExpr()
			}
			p.expect(TokenRParen)
		}
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "WITHIN" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "GROUP" {
			p.next()
			if p.expect(TokenLParen) {
				p.parseOrderBy()
				p.expect(TokenRParen)
			}
		}
	}
}

//lint:ignore U1000 Parser utility for future use
func (p *Parser) skipWindowSpec() {
	if p.cur.Type == TokenLParen {
		p.skipInlineWindowSpec()
	} else if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		// Named window: OVER windowName
		p.next()
	}
}

// parseInlineWindowSpec parses an inline window specification
// (PARTITION BY ... ORDER BY ... frame_spec) inside parentheses
// and returns a populated *WindowDef. Unlike skipInlineWindowSpec,
// this function stores the parsed results rather than discarding them.
func (p *Parser) parseInlineWindowSpec() *WindowDef {
	p.windowSpecDepth++
	defer func() { p.windowSpecDepth-- }()
	wd := &WindowDef{}
	p.next() // skip (
	for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
		if p.cur.Type == TokenKeyword && p.cur.Value == "PARTITION" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
				p.next()
			}
			var partitions []Expr
			for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
				if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
					break
				}
				if p.cur.Type == TokenKeyword &&
					(p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
					break
				}
				expr := p.parseExpr()
				partitions = append(partitions, expr)
				if p.cur.Type == TokenComma {
					p.next()
				} else {
					break
				}
			}
			wd.Partitions = partitions
		} else if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
			p.next() // consume ORDER
			if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
				p.next() // consume BY
			}
			wd.OrderBy = p.parseOrderBy()
		} else if p.cur.Type == TokenKeyword &&
			(p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
			p.skipFrameSpec()
		} else if p.cur.Type == TokenComma {
			p.next()
		} else {
			// Parse expressions (handles function calls, identifiers, etc.)
			p.parseExpr()
		}
	}
	if p.cur.Type == TokenRParen {
		p.next()
	}
	return wd
}

func (p *Parser) skipInlineWindowSpec() {
	// Inline window specification: OVER (PARTITION BY ... ORDER BY ...)
	p.windowSpecDepth++
	defer func() { p.windowSpecDepth-- }()
	p.next()
	for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
		if p.cur.Type == TokenKeyword && p.cur.Value == "PARTITION" {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
				p.next()
			}
		} else if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
			p.parseOrderBy()
		} else if p.cur.Type == TokenKeyword &&
			(p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
			p.skipFrameSpec()
		} else if p.cur.Type == TokenComma {
			p.next()
		} else {
			// Parse expressions (handles function calls, identifiers, etc.)
			p.parseExpr()
		}
	}
	if p.cur.Type == TokenRParen {
		p.next()
	}
}

func (p *Parser) skipFrameSpec() {
	// RANGE/ROWS/GROUPS BETWEEN ... AND ... or just RANGE/ROWS/GROUPS ...
	p.next()
	if p.cur.Type == TokenKeyword && p.cur.Value == "BETWEEN" {
		p.skipBetweenFrame()
	} else {
		p.skipSimpleFrame()
	}
}

func (p *Parser) skipBetweenFrame() {
	p.next() // skip BETWEEN
	depth := 0
	// UNBOUNDED PRECEDING, expr PRECEDING, CURRENT ROW
	for p.cur.Type != TokenKeyword || p.cur.Value != "AND" || depth > 0 {
		if p.cur.Type == TokenEOF {
			return
		}
		if p.cur.Type == TokenLParen {
			depth++
		} else if p.cur.Type == TokenRParen {
			if depth == 0 {
				// No matching open paren — this ')' closes the frame spec
				return
			}
			depth--
		}
		p.next()
	}
	p.next() // skip AND
	// expr PRECEDING/FOLLOWING, UNBOUNDED FOLLOWING, CURRENT ROW
	p.skipUntilFrameEnd()
}

func (p *Parser) skipSimpleFrame() {
	// Simple frame: ROWS/ROWS/GROUPS expr PRECEDING/FOLLOWING or CURRENT ROW
	p.skipUntilFrameEnd()
}

func (p *Parser) skipUntilFrameEnd() {
	depth := 0
	for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF || depth > 0 {
		if p.cur.Type == TokenEOF {
			return
		}
		if p.cur.Type == TokenKeyword &&
			(p.cur.Value == "ORDER" || p.cur.Value == "PARTITION" || p.cur.Value == "BY") {
			if depth == 0 {
				return
			}
		}
		if p.cur.Type == TokenLParen {
			depth++
		} else if p.cur.Type == TokenRParen {
			if depth == 0 {
				return
			}
			depth--
		}
		p.next()
	}
}

// skipCollateExpr handles COLLATE and other post-expression suffixes.
func (p *Parser) skipCollateExpr(left Expr) Expr {
	if p.cur.Type == TokenKeyword && strings.ToUpper(p.cur.Value) == "COLLATE" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			collation := p.cur.Value
			p.next()
			return &BinaryOp{Operator: "COLLATE", Left: left, Right: &StringLit{Value: collation}}
		}
	}
	return left
}

// decodeBlobLiteral decodes a hex blob literal string (e.g., "00AB") into a BlobLit.
func decodeBlobLiteral(hexStr string) (*BlobLit, error) {
	if len(hexStr) == 0 {
		return &BlobLit{Value: []byte{}}, nil
	}
	// Allow odd-length hex strings by padding with leading zero
	if len(hexStr)%2 == 1 {
		hexStr = "0" + hexStr
	}
	data, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, err
	}
	return &BlobLit{Value: data}, nil
}