package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// Parser turns a token stream into AST nodes.
type Parser struct {
	tokens *Tokenizer
	cur    Token
	peek   Token
	err    error
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
		return p.parseInsert()
	case "REPLACE":
		// REPLACE INTO is equivalent to INSERT OR REPLACE INTO
		return p.parseInsert()
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
	// EXPLAIN QUERY PLAN is a variant
	if p.cur.Type == TokenKeyword && p.cur.Value == "QUERY" {
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "PLAN" {
			p.next()
		}
	}
	stmt := p.parseStatement()
	if stmt == nil {
		return nil
	}
	return &ExplainStmt{Statement: stmt}
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
	p.parseSelectOrderBy(s)
	p.parseSelectLimit(s)

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
		s.Union = p.parseSelect()
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
			s.Joins = append(s.Joins, JoinClause{Table: table, JoinType: "CROSS"})
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
// Stores the first returned expression in col and sets hasReturning to true.
func (p *Parser) parseReturningClause(col *SelectColumn, hasReturning *bool) {
	p.next() // skip RETURNING
	*hasReturning = true
	// Parse first expression
	if p.cur.Type == TokenStar {
		// RETURNING * — special case, not handled by parseExpr
		col.Expr = &ColumnRef{Name: "*"}
		p.next()
	} else if p.cur.Type != TokenKeyword || p.cur.Value != "ORDER" {
		col.Expr = p.parseExpr()
		// Optional alias
		if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				col.As = p.cur.Value
				p.next()
			}
		}
	}
	// Consume remaining expressions if any (ignore for now)
	for p.cur.Type == TokenComma {
		p.next()
		if p.cur.Type == TokenStar {
			p.next() // skip *
		} else {
			p.parseExpr()
			// Consume optional alias: AS alias
			if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
				p.next()
				if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
					p.next() // skip alias name
				}
			}
		}
	}
}

func (p *Parser) parseSelectWindow(s *SelectStmt) {
	// WINDOW name AS (window_spec), ...
	if p.cur.Type == TokenKeyword && p.cur.Value == "WINDOW" {
		p.next()
		for {
			// Window name
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				p.next()
			}
			if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
				p.next()
			}
			if p.cur.Type == TokenLParen {
				p.skipInlineWindowSpec()
			}
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
	ref.Name = p.readName()

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
		"USING", "SET", "RETURNING", "EXCEPT", "INTERSECT", "UNION":
		return true
	}
	return false
}

// parseParenTableRef handles parenthesized table references in a FROM clause:
// subquery (SELECT ...), CTE subquery (WITH ... SELECT ...), or bare table name (t1).
func (p *Parser) parseParenTableRef() TableRef {
	ref := TableRef{}
	p.next() // skip (
	if p.cur.Type == TokenKeyword && (p.cur.Value == "SELECT" || p.cur.Value == "WITH") {
		if p.cur.Value == "SELECT" {
			ref.Subquery = p.parseSelect()
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
		p.skipParenContent()
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
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
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
func (p *Parser) parseInsert() *InsertStmt {
	s := &InsertStmt{}
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
		p.next()
		for i, col := range cols {
			val := p.parseExpr()
			s.Assignments = append(s.Assignments, Assignment{Column: col, Value: val})
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
		if p.cur.Type == TokenString || p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenNumber {
			args = append(args, p.cur.Value)
			p.next()
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

	if name := p.readName(); name != "" {
		s.Name = name
	}

	// Optional parenthesized column list: CREATE VIEW name (col1, col2) AS ...
	if p.cur.Type == TokenLParen {
		p.next()
		// Skip column names
		for p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
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

	// Try to read trigger name. The name might be a keyword that doubles
	// as a timing keyword (e.g., "BEFORE", "AFTER"), so we need to peek
	// ahead to disambiguate.
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		name := p.cur.Value
		// Peek: if next token is a keyword that could be a timing keyword,
		// then the current token might be the name (as identifier).
		// If next token is a known event keyword, then current might be timing.
		if isTimingKeyword(name) && p.peek.Type == TokenKeyword {
			// name looks like timing; check if peek is an event keyword
			if isEventKeyword(p.peek.Value) || p.peek.Value == "ON" {
				// name is actually timing, not the trigger name
				s.Time = name
				p.next()
				p.parseTriggerEvent(s)
				if !p.expectKeyword("ON") {
					return nil
				}
				if tableName := p.readName(); tableName != "" {
					s.Table = tableName
				}
				p.parseTriggerWhenForEach(s)
				p.parseTriggerBody(s)
				return s
			}
		}
		// Read as trigger name
		s.Name = name
		p.next()
		// Check for schema-qualified name
		if p.cur.Type == TokenDot {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
				s.Name = name + "." + p.cur.Value
				p.next()
			}
		}
	}

	p.parseTriggerTiming(s)
	p.parseTriggerEvent(s)

	if !p.expectKeyword("ON") {
		return nil
	}

	if tableName := p.readName(); tableName != "" {
		s.Table = tableName
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

	if name := p.readName(); name != "" {
		s.Name = name
	}

	if p.cur.Type == TokenLParen {
		p.next()
		s.Columns = p.parseColumnDefs()
		p.skipTableConstraints()
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

func (p *Parser) skipTableConstraints() {
	for {
		if p.cur.Type == TokenComma {
			p.next()
		}
		if p.cur.Type == TokenKeyword && (p.cur.Value == "PRIMARY" || p.cur.Value == "UNIQUE" ||
			p.cur.Value == "CHECK" || p.cur.Value == "FOREIGN" || p.cur.Value == "CONSTRAINT") {
			p.skipTableConstraint()
		} else {
			break
		}
	}
}



// parseWithStatement handles WITH ... SELECT (CTE support).
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
		vs.Union = p.parseSelect()
	}
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

	if tableName := p.readName(); tableName != "" {
		s.Table = tableName
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
		p.parseExpr() // skip the WHERE expression
	}

	return s
}

func (p *Parser) parseColumnDefs() []ColumnDef {
	var cols []ColumnDef
	for {
		// Skip table-level constraints (PRIMARY KEY, UNIQUE, CHECK, FOREIGN KEY)
		if p.cur.Type == TokenKeyword && (p.cur.Value == "PRIMARY" || p.cur.Value == "UNIQUE" ||
			p.cur.Value == "CHECK" || p.cur.Value == "FOREIGN" || p.cur.Value == "CONSTRAINT") {
			p.skipTableConstraint()
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
		col := ColumnDef{Name: p.cur.Value}
		p.next()
		col.Type = p.parseColumnType()
		p.parseColumnConstraints(&col)
		cols = append(cols, col)
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return cols
}

// isConstraintStart returns true if word is a SQL keyword that starts
// a column constraint, not a type name.
func isConstraintStart(word string) bool {
	switch word {
	case "PRIMARY", "NOT", "DEFAULT", "UNIQUE", "CHECK", "REFERENCES",
		"COLLATE", "CONSTRAINT":
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
	case "REFERENCES":
		p.parseReferencesConstraint(col)
	case "COLLATE":
		p.parseCollateColumnConstraint(col)
	case "CONSTRAINT":
		p.skipConstraintName()
	case "AS":
		p.skipGeneratedColumnAs()
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

func (p *Parser) skipConstraintName() {
	p.next()
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString {
		p.next() // skip constraint name
	}
}

func (p *Parser) skipGeneratedColumnAs() {
	p.next() // skip AS
	if p.cur.Type == TokenLParen {
		p.skipParenExpr()
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
			col := IndexColumn{Name: ""}
			if colRef, ok := expr.(*ColumnRef); ok {
				col.Name = colRef.Name
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
	if name := p.readName(); name != "" {
		s.Table = name
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
	// Skip SET/DROP and their arguments
	if p.cur.Type == TokenKeyword && (p.cur.Value == "SET" || p.cur.Value == "DROP") {
		p.next()
		// Skip NOT NULL, DEFAULT, etc.
		if p.cur.Type == TokenKeyword && (p.cur.Value == "NOT" || p.cur.Value == "DEFAULT") {
			p.next()
			if p.cur.Type == TokenKeyword && p.cur.Value == "NULL" {
				p.next()
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
		s.Column = p.cur.Value
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
	if (p.cur.Type == TokenString) || (p.cur.Type == TokenParam && p.cur.Value == "?") {
		s.Path = p.cur.Value
		p.next()
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString || p.cur.Type == TokenParam {
			s.Schema = p.cur.Value
			p.next()
		}
	}
	return s
}

func (p *Parser) parseDetach() *AttachStmt {
	s := &AttachStmt{}
	p.next()
	// DATABASE keyword is optional for DETACH
	if p.cur.Type == TokenKeyword && p.cur.Value == "DATABASE" {
		p.next()
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword || p.cur.Type == TokenString || p.cur.Type == TokenParam {
		s.Schema = p.cur.Value
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
	p.consumeIsRightOperand()
	return left
}

// consumeIsRightOperand consumes the right operand expression after IS or IS NOT,
// handling TRUE/FALSE as identifiers as well as any expression type.
func (p *Parser) consumeIsRightOperand() {
	if p.cur.Type == TokenIdentifier && (p.cur.Value == "TRUE" || p.cur.Value == "FALSE") {
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
		lit := &StringLit{Value: p.cur.Value}
		p.next()
		return lit

	case TokenIdentifier:
		name := p.cur.Value
		p.next()

		// Function call
		if p.cur.Type == TokenLParen {
			return p.parseFunctionCall(name)
		}

		// Handle dot (table.column or schema.table.column or table.*)
		tableName := ""
		for p.cur.Type == TokenDot {
			p.next()
			if p.cur.Type == TokenIdentifier || p.cur.Type == TokenStar {
				if tableName == "" {
					tableName = name
					name = p.cur.Value
				} else {
					// Three-part name: schema.table.column
					// Combine first two as table name
					if p.cur.Type == TokenStar {
						// schema.table.* - not common but handle
						return &ColumnRef{Table: tableName + "." + name, Name: p.cur.Value}
					}
					// For three-part name, table becomes "schema.table" and name becomes "column"
					tableName = tableName + "." + name
					name = p.cur.Value
				}
				p.next()
			} else {
				return &ColumnRef{Name: name}
			}
		}
		if tableName != "" && name == "*" {
			return &ColumnRef{Table: tableName, Name: "*"}
		}
		return &ColumnRef{Table: tableName, Name: name}

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
	return expr
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
		p.skipWindowClause()
		return fc
	}
	// Handle empty argument list with ORDER BY: count(ORDER BY x)
	var args []Expr
	if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
		// Empty args, just ORDER BY
		p.skipFunctionOrderBy()
	} else {
		args = p.parseExprList()
		// Skip optional ORDER BY inside function call: string_agg(x ORDER BY y)
		if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
			p.skipFunctionOrderBy()
		}
	}
	p.expect(TokenRParen)
	fc := &FuncCall{Name: name, Args: args, Distinct: distinct}
	p.skipWindowClause()
	return fc
}

func (p *Parser) skipFunctionOrderBy() {
	p.next() // skip ORDER
	if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
		p.next() // skip BY
		for {
			expr := p.parseExpr()
			if expr == nil {
				break
			}
			// Optional ASC/DESC
			if p.cur.Type == TokenKeyword && (p.cur.Value == "ASC" || p.cur.Value == "DESC") {
				p.next()
			}
			// Optional NULLS FIRST/LAST
			if p.cur.Type == TokenKeyword && p.cur.Value == "NULLS" {
				p.next()
				if p.cur.Type == TokenKeyword && (p.cur.Value == "FIRST" || p.cur.Value == "LAST") {
					p.next()
				}
			}
			if p.cur.Type == TokenComma {
				p.next()
			} else {
				break
			}
		}
	}
}

func (p *Parser) parseKeywordExpr() Expr {
	kw := p.cur.Value
	p.next()

	switch kw {
	case "NULL":
		return &NullLit{}
	case "TRUE":
		return &NumericLit{Value: "1"}
	case "FALSE":
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
		return &ColumnRef{Name: kw}
	}
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
	for p.cur.Type == TokenIdentifier {
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
		return ExprString(v.Operand) + " IS NOT NULL"
	case *IsDistinctFrom:
		return ExprString(v.Left) + " IS DISTINCT FROM " + ExprString(v.Right)
	case *IsNotDistinctFrom:
		return ExprString(v.Left) + " IS NOT DISTINCT FROM " + ExprString(v.Right)
	case *Between:
		return formatBetween(v)
	case *InList:
		return formatInList(v)
	case *FuncCall:
		return formatFuncCall(v)
	default:
		return "?"
	}
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
	return v.Name + "(" + strings.Join(args, ", ") + ")"
}

// skipWindowClause skips window function clauses (OVER, FILTER, WITHIN GROUP)
// after a function call. This is a stub: the window clause is parsed but not
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

func (p *Parser) skipWindowSpec() {
	if p.cur.Type == TokenLParen {
		p.skipInlineWindowSpec()
	} else if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		// Named window: OVER windowName
		p.next()
	}
}

func (p *Parser) skipInlineWindowSpec() {
	// Inline window specification: OVER (PARTITION BY ... ORDER BY ...)
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
	p.next()
	// UNBOUNDED PRECEDING, expr PRECEDING, CURRENT ROW
	for p.cur.Type != TokenKeyword || p.cur.Value != "AND" {
		if p.cur.Type == TokenEOF || p.cur.Type == TokenRParen {
			return
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
	for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
		if p.cur.Type == TokenKeyword &&
			(p.cur.Value == "ORDER" || p.cur.Value == "PARTITION" || p.cur.Value == "BY") {
			return
		}
		p.next()
	}
}

// skipCollateExpr handles COLLATE and other post-expression suffixes.
func (p *Parser) skipCollateExpr(left Expr) Expr {
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLLATE" {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			p.next() // skip collation name
		}
	}
	return left
}