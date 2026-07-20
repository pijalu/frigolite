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

func (p *Parser) expect(typ TokenType) bool {
	if p.cur.Type != typ {
		p.setErr("expected %v but got %v (%s)", typ, p.cur.Type, p.cur.Value)
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
	default:
		p.setErr("unexpected token: %v (%s)", p.cur.Type, p.cur.Value)
		return nil
	}
}

func (p *Parser) parseKeywordStmt() Stmt {
	switch p.cur.Value {
	case "SELECT":
		return p.parseSelect()
	case "INSERT":
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
	default:
		p.setErr("unexpected keyword: %s", p.cur.Value)
		return nil
	}
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
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
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

	// UNION
	if p.cur.Type == TokenKeyword && (p.cur.Value == "UNION" || p.cur.Value == "INTERSECT" || p.cur.Value == "EXCEPT") {
		s.UnionAll = p.cur.Value == "UNION" && p.peekType(TokenKeyword, "ALL")
		if s.UnionAll {
			p.next() // skip ALL
		}
		p.next() // skip UNION/INTERSECT/EXCEPT
		s.Union = p.parseSelect()
	}

	return s
}

func (p *Parser) parseSelectJoins(s *SelectStmt) {
	for {
		if p.cur.Type == TokenKeyword && (p.cur.Value == "JOIN" || p.cur.Value == "INNER" || p.cur.Value == "LEFT" || p.cur.Value == "RIGHT" || p.cur.Value == "CROSS" || p.cur.Value == "NATURAL") {
			j := p.parseJoinClause()
			s.Joins = append(s.Joins, j)
		} else {
			break
		}
	}
}

func (p *Parser) parseJoinClause() JoinClause {
	j := JoinClause{}
	switch p.cur.Value {
	case "INNER":
		p.next()
		p.expectKeyword("JOIN")
		j.JoinType = "INNER"
	case "LEFT":
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OUTER" {
			p.next()
		}
		p.expectKeyword("JOIN")
		j.JoinType = "LEFT"
	case "RIGHT":
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OUTER" {
			p.next()
		}
		p.expectKeyword("JOIN")
		j.JoinType = "RIGHT"
	case "CROSS":
		p.next()
		p.expectKeyword("JOIN")
		j.JoinType = "CROSS"
	case "NATURAL":
		p.next()
		if p.cur.Type == TokenKeyword && (p.cur.Value == "LEFT" || p.cur.Value == "RIGHT") {
			p.next()
		}
		p.expectKeyword("JOIN")
		j.JoinType = "NATURAL"
	default:
		p.expectKeyword("JOIN")
	}
	j.Table = p.parseTableRef()
	if p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.next()
		j.On = p.parseExpr()
	}
	return j
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
	if p.cur.Type == TokenKeyword && p.cur.Value == "LIMIT" {
		p.next()
		s.Limit = p.parseExpr()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OFFSET" {
			p.next()
			s.Offset = p.parseExpr()
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
				if p.cur.Type == TokenIdentifier {
					col.As = p.cur.Value
					p.next()
				}
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
	if p.cur.Type == TokenIdentifier {
		ref.Name = p.cur.Value
		p.next()
	} else if p.cur.Type == TokenKeyword {
		ref.Name = p.cur.Value
		p.next()
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier {
			ref.As = p.cur.Value
			p.next()
		}
	}
	return ref
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
	if !p.expectKeyword("INTO") {
		return nil
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Table = p.cur.Value
		p.next()
	}
	if p.cur.Type == TokenLParen {
		p.next()
		s.Columns = p.parseIdentList()
		p.expect(TokenRParen)
	}
	p.parseInsertSource(s)
	if p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		s.OnConflict = p.parseOnConflict()
	}
	return s
}

func (p *Parser) parseOnConflict() *OnConflictClause {
	oc := &OnConflictClause{}
	p.next() // skip ON
	p.expectKeyword("CONFLICT")

	// Optional conflict target: (column_name)
	if p.cur.Type == TokenLParen {
		p.next()
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			oc.ConflictColumn = p.cur.Value
			p.next()
		}
		p.expect(TokenRParen)
	}

	// WHERE clause not supported yet for conflict target

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
	return oc
}

func (p *Parser) parseAssignments() []Assignment {
	var assigns []Assignment
	for {
		var a Assignment
		if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			a.Column = p.cur.Value
			p.next()
		}
		p.expect(TokenEq)
		a.Value = p.parseExpr()
		assigns = append(assigns, a)
		if p.cur.Type != TokenComma {
			break
		}
		p.next()
	}
	return assigns
}

func (p *Parser) parseInsertSource(s *InsertStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "SELECT" {
		s.Select = p.parseSelect()
	} else if p.cur.Type == TokenKeyword && p.cur.Value == "DEFAULT" {
		p.next()
		p.expectKeyword("VALUES")
	} else {
		p.expectKeyword("VALUES")
		p.expect(TokenLParen)
		s.Values = p.parseExprList()
		p.expect(TokenRParen)
		for p.cur.Type == TokenComma {
			p.next()
			if p.cur.Type == TokenLParen {
				p.next()
				p.parseExprList()
				p.expect(TokenRParen)
			}
		}
	}
}

// UPDATE
func (p *Parser) parseUpdate() *UpdateStmt {
	s := &UpdateStmt{}
	p.next() // skip UPDATE

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Table = p.cur.Value
		p.next()
	}

	if !p.expectKeyword("SET") {
		return nil
	}

	for {
		if p.cur.Type != TokenIdentifier {
			p.setErr("expected column name in SET")
			break
		}
		col := p.cur.Value
		p.next()
		if !p.expect(TokenEq) {
			break
		}
		val := p.parseExpr()
		s.Assignments = append(s.Assignments, Assignment{Column: col, Value: val})
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}

	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}

	return s
}

// DELETE
func (p *Parser) parseDelete() *DeleteStmt {
	s := &DeleteStmt{}
	p.next() // skip DELETE

	if !p.expectKeyword("FROM") {
		return nil
	}

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Table = p.cur.Value
		p.next()
	}

	if p.cur.Type == TokenKeyword && p.cur.Value == "WHERE" {
		p.next()
		s.Where = p.parseExpr()
	}

	return s
}

// CREATE
func (p *Parser) parseCreate() Stmt {
	p.next() // skip CREATE

	if p.cur.Type == TokenKeyword && (p.cur.Value == "TEMP" || p.cur.Value == "TEMPORARY") {
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
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
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

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}

	if !p.expectKeyword("AS") {
		return nil
	}

	s.Select = p.parseSelect()
	return s
}

func (p *Parser) parseCreateTrigger() *CreateTriggerStmt {
	s := &CreateTriggerStmt{}
	p.next() // skip TRIGGER

	p.parseTriggerIfNotExists(s)

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}

	p.parseTriggerTiming(s)
	p.parseTriggerEvent(s)

	if !p.expectKeyword("ON") {
		return nil
	}

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Table = p.cur.Value
		p.next()
	}

	p.parseTriggerBody(s)
	return s
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
	if p.cur.Type == TokenKeyword {
		s.Time = p.cur.Value
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "OF" {
			s.Time += " OF"
			p.next()
		}
	}
}

func (p *Parser) parseTriggerEvent(s *CreateTriggerStmt) {
	if p.cur.Type == TokenKeyword {
		s.Event = p.cur.Value
		p.next()
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

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}

	if p.cur.Type == TokenLParen {
		p.next()
		s.Columns = p.parseColumnDefs()
		if !p.expect(TokenRParen) {
			return nil
		}
	}

	return s
}

func (p *Parser) parseCreateIndex() *CreateIndexStmt {
	s := &CreateIndexStmt{}
	p.next() // skip INDEX

	if p.cur.Type == TokenKeyword && p.cur.Value == "UNIQUE" {
		s.Unique = true
		p.next()
	}

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}

	if !p.expectKeyword("ON") {
		return nil
	}

	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Table = p.cur.Value
		p.next()
	}

	if p.cur.Type == TokenLParen {
		p.next()
		s.Columns = p.parseIndexColumns()
		if !p.expect(TokenRParen) {
			return nil
		}
	}

	return s
}

func (p *Parser) parseColumnDefs() []ColumnDef {
	var cols []ColumnDef
	for {
		if p.cur.Type != TokenIdentifier {
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

// isTypeContinuation returns true if word is a multi-word type continuation
// (e.g. "FLOATING" in "FLOATING POINT").
func isTypeContinuation(word string) bool {
	switch word {
	case "UNSIGNED", "SIGNED", "CHARACTER", "VARYING", "PRECISION",
		"POINT", "NATIONAL", "DOUBLE":
		return true
	}
	return false
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
	if isConstraintStart(p.cur.Value) {
		return ""
	}

	parts := []string{p.cur.Value}
	p.next()
	for p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
		if isConstraintStart(p.cur.Value) {
			break
		}
		if !isTypeContinuation(p.cur.Value) {
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
		switch p.cur.Value {
		case "PRIMARY":
			p.parsePrimaryKeyConstraint(col)
		case "NOT":
			p.parseNotNullConstraint(col)
		case "DEFAULT":
			p.parseDefaultConstraint(col)
		case "UNIQUE":
			col.Unique = true
			p.next()
		case "CHECK":
			p.parseCheckConstraint(col)
		case "REFERENCES":
			p.parseReferencesConstraint(col)
		case "COLLATE":
			p.next()
			if p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
				col.Collate = p.cur.Value
				p.next()
			}
		default:
			return // not a constraint keyword
		}
	}
	// Optional ON CONFLICT clause after any constraint
	if p.cur.Type == TokenKeyword && p.cur.Value == "ON" {
		p.parseOnConflictColumnConstraint(col)
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
		p.parseExpr() // consume the check expression
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
}

func (p *Parser) parsePrimaryKeyConstraint(col *ColumnDef) {
	p.next()
	p.expectKeyword("KEY")
	col.PrimaryKey = true
	if p.cur.Type == TokenKeyword && p.cur.Value == "AUTOINCREMENT" {
		col.AutoInc = true
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
		if p.cur.Type != TokenIdentifier {
			break
		}
		col := IndexColumn{Name: p.cur.Value}
		p.next()
		if p.cur.Type == TokenKeyword && p.cur.Value == "ASC" {
			p.next()
		} else if p.cur.Type == TokenKeyword && p.cur.Value == "DESC" {
			col.Desc = true
			p.next()
		}
		cols = append(cols, col)
		if p.cur.Type == TokenComma {
			p.next()
		} else {
			break
		}
	}
	return cols
}

// DROP
func (p *Parser) parseDrop() Stmt {
	p.next() // skip DROP
	if p.cur.Type == TokenKeyword {
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
			p.setErr("expected TABLE, VIEW, TRIGGER, or INDEX after DROP, got %s", p.cur.Value)
			return nil
		}
	}
	p.setErr("expected TABLE, VIEW, TRIGGER, or INDEX after DROP")
	return nil
}

func (p *Parser) parseDropTable() Stmt {
	p.next()
	s := &DropTableStmt{}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
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
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
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
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}
	return s
}

func (p *Parser) parseDropIndex() Stmt {
	p.next()
	s := &DropTableStmt{Name: "index"}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}
	return s
}

// Transactions
func (p *Parser) parseBegin() *BeginStmt {
	p.next()
	return &BeginStmt{}
}

func (p *Parser) parseCommit() *CommitStmt {
	p.next()
	return &CommitStmt{}
}

func (p *Parser) parseRollback() *RollbackStmt {
	p.next()
	if p.cur.Type == TokenKeyword && p.cur.Value == "TO" {
		p.next()
		if p.cur.Type == TokenKeyword || p.cur.Type == TokenIdentifier {
			p.next()
		}
	}
	return &RollbackStmt{}
}

func (p *Parser) parsePragma() *PragmaStmt {
	s := &PragmaStmt{}
	p.next()
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Name = p.cur.Value
		p.next()
	}
	if p.cur.Type == TokenEq {
		p.next()
		if p.cur.Type == TokenNumber || p.cur.Type == TokenString || p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
			s.Value = p.cur.Value
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
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.Table = p.cur.Value
		p.next()
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
	}
	return s
}

func (p *Parser) parseAlterRename(s *AlterTableStmt) {
	if !p.expectKeyword("TO") {
		return
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.NewName = p.cur.Value
		p.next()
	}
}

func (p *Parser) parseAlterAdd(s *AlterTableStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN" {
		p.next()
	}
	if p.cur.Type == TokenIdentifier {
		s.Column = p.cur.Value
		p.next()
	}
	if p.cur.Type == TokenIdentifier || p.cur.Type == TokenKeyword {
		s.ColDef.Type = p.cur.Value
		p.next()
	}
}

func (p *Parser) parseAlterDrop(s *AlterTableStmt) {
	if p.cur.Type == TokenKeyword && p.cur.Value == "COLUMN" {
		p.next()
	}
	if p.cur.Type == TokenIdentifier {
		s.Column = p.cur.Value
		p.next()
	}
}

func (p *Parser) parseAttach() *AttachStmt {
	s := &AttachStmt{}
	p.next()
	if !p.expectKeyword("DATABASE") {
		return nil
	}
	if p.cur.Type == TokenString {
		s.Path = p.cur.Value
		p.next()
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "AS" {
		p.next()
		if p.cur.Type == TokenIdentifier {
			s.Schema = p.cur.Value
			p.next()
		}
	}
	return s
}

func (p *Parser) parseVacuum() *VacuumStmt {
	p.next()
	return &VacuumStmt{}
}

func (p *Parser) parseReindex() *ReindexStmt {
	p.next()
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
	p.next()
	if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		p.expectKeyword("NULL")
		return &IsNotNull{Operand: left}
	}
	p.expectKeyword("NULL")
	return &IsNull{Operand: left}
}

func (p *Parser) parseInOp(left Expr) Expr {
	p.next()
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
	return &BinaryOp{Left: left, Right: right, Operator: "LIKE"}
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
		default:
			return left
		}
	}
}

func (p *Parser) parseUnaryExpr() Expr {
	if p.cur.Type == TokenMinus {
		p.next()
		return &UnaryOp{Operand: p.parsePrimaryExpr(), Operator: "-"}
	}
	if p.cur.Type == TokenKeyword && p.cur.Value == "NOT" {
		p.next()
		return &UnaryOp{Operand: p.parsePrimaryExpr(), Operator: "NOT"}
	}
	return p.parsePrimaryExpr()
}

func (p *Parser) parsePrimaryExpr() Expr {
	switch p.cur.Type {
	case TokenNumber:
		lit := &NumericLit{Value: p.cur.Value}
		p.next()
		return lit

	case TokenString:
		lit := &StringLit{Value: p.cur.Value}
		p.next()
		return lit

	case TokenIdentifier:
		name := p.cur.Value
		p.next()

		// Function call
		if p.cur.Type == TokenLParen {
			p.next()
			// Handle COUNT(*) - * as function argument
			if p.cur.Type == TokenStar {
				args := []Expr{&ColumnRef{Name: "*"}}
				p.next()
				p.expect(TokenRParen)
				return &FuncCall{Name: name, Args: args}
			}
			args := p.parseExprList()
			p.expect(TokenRParen)
			return &FuncCall{Name: name, Args: args}
		}

		// Qualified name (table.column)
		if p.cur.Type == TokenDot {
			p.next()
			if p.cur.Type == TokenIdentifier {
				col := &ColumnRef{Table: name, Name: p.cur.Value}
				p.next()
				return col
			}
			if p.cur.Type == TokenStar {
				col := &ColumnRef{Table: name, Name: "*"}
				p.next()
				return col
			}
			return &ColumnRef{Name: name}
		}

		return &ColumnRef{Name: name}

	case TokenLParen:
		p.next()
		return p.parseParenExpr()

	case TokenKeyword:
		return p.parseKeywordExpr()

	default:
		p.setErr("unexpected token in expression: %s", p.cur.Value)
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
	expr := p.parseExpr()
	p.expect(TokenRParen)
	return expr
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
