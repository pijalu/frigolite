// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar. This replaces the hand-written
// recursive-descent parser in internal/sql/parser.go.

package parse

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// parsePreprocess bundles the result of the SQL preprocessing pipeline that
// runs before LALR parsing: statement-ORDER/LIMIT rewriting, row-value paren
// rewriting, and SAVEPOINT extraction.
type parsePreprocess struct {
	input          string
	stmtClauses    []stmtOrderLimit
	hasStmtRewrite bool
	savepointStmts []sql.Stmt
	stmtKind       []bool
	parenSpans     []parenRewriteSpan
}

// preprocessInput applies the rewriting/ extraction steps ParseSQL needs
// before handing input to the LALR parser. Each step exists because the
// generated parse tables lack a SQLite extension or statement family.
func preprocessInput(input string) (*parsePreprocess, error) {
	// Collapse empty statements (consecutive semicolons with only whitespace
	// or comments between): SQLite treats them as no-ops, and the LALR tables
	// mis-parse `stmt;; stmt` by duplicating the trailing statement. Do this
	// FIRST so all later rewrites (which split on statements) see a clean
	// statement stream.
	input = collapseEmptyStatements(input)
	// Normalize generated-column storage syntax unsupported by legacy tables.
	input = strings.ReplaceAll(input, "GENERATED ALWAYS", "")
	input = strings.ReplaceAll(input, "generated always", "")
	input = strings.ReplaceAll(input, ") VIRTUAL", ")")
	input = strings.ReplaceAll(input, ") STORED", ")")

	// The LALR tables are generated from SQLite's grammar, which accepts
	// UPDATE ... ORDER BY ... LIMIT and DELETE ... ORDER BY ... LIMIT (SQLite
	// extensions). The tables used here predate that extension, so rewrite
	// such statements first: strip the trailing ORDER BY/LIMIT, parse the
	// remainder, then re-attach the clause to the resulting statement.
	rewritten, stmtClauses, hasStmtRewrite := rewriteStmtOrderLimit(input)
	if hasStmtRewrite {
		input = rewritten
	}

	// SAVEPOINT / RELEASE / ROLLBACK TO savepoint statements are not in the
	// LALR tables. Replace each with a comment placeholder (preserving the
	// statement's position), parse the rest normally, then interleave the
	// SavepointStmt nodes back in input order.
	var savepointStmts []sql.Stmt
	var stmtKind []bool // true = savepoint placeholder, false = regular
	input, savepointStmts, stmtKind = extractSavepointStatements(input)

	// The LALR tables (pre-generated from an older SQLite grammar) have a
	// shift/reduce conflict after a CREATE TRIGGER whose WHEN clause ends a
	// top-level `==` (or `=`) comparison expression: the following statement's
	// `=` (e.g. UPDATE SET) is then mis-read. SQLite resolves this with the
	// scanpt lookahead markers in the trigger_cmd grammar rules. The engine
	// works around it by parenthesizing the WHEN expression, which is
	// semantically a no-op and lets the tables reduce the comparison before
	// the statement boundary.
	input = rewriteTriggerWhenParenthesize(input)

	// UPDATE ... SET (c,d) = (SELECT y,z ...) (row-value assignment) is not
	// in the LALR tables either; rewrite it into per-column assignments. An
	// arity mismatch (N columns assigned M values) is reported here. Run LAST
	// so the recorded restore spans are in the final input coordinates.
	rewritten, parenSpans, parenErr := rewriteParenSet(input)
	if parenErr != nil {
		return nil, parenErr
	}
	input = rewritten

	return &parsePreprocess{
		input:          input,
		stmtClauses:    stmtClauses,
		hasStmtRewrite: hasStmtRewrite,
		savepointStmts: savepointStmts,
		stmtKind:       stmtKind,
		parenSpans:     parenSpans,
	}, nil
}

// emptyInputResult reports whether input is only whitespace/comments (or only
// SAVEPOINT placeholders) after preprocessing. When done is true the returned
// statements are the final result of ParseSQL.
func emptyInputResult(input string, savepointStmts []sql.Stmt) (stmts []sql.Stmt, done bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return savepointStmts, true
	}
	if strings.TrimSpace(stripSQLComments(trimmed)) == "" {
		return savepointStmts, true
	}
	// The rebuilt input may be only SAVEPOINT comment placeholders plus the
	// ";" separators (e.g. "/* __SAVEPOINT__ */;/* __SAVEPOINT__ */");
	// strip comments AND semicolons to detect that case.
	if len(savepointStmts) > 0 {
		noComments := strings.TrimSpace(stripSQLComments(trimmed))
		noComments = strings.ReplaceAll(noComments, ";", "")
		if strings.TrimSpace(noComments) == "" {
			return savepointStmts, true
		}
	}
	return nil, false
}

// ensureTrailingSemicolon appends a statement terminator when missing. The
// LALR grammar requires SEMI as a statement terminator (ecmd ::= cmdx SEMI).
// Use "\n;" not ";": if the statement ends with a -- line comment, a bare
// ";" would be swallowed by the comment (skipLineComment runs to EOF) and
// the grammar would never see its SEMI terminator.
func ensureTrailingSemicolon(input string) string {
	input = strings.TrimRight(input, " \t\r\n")
	if input != "" && input[len(input)-1] != ';' {
		input += "\n;"
	}
	return input
}

// ParseSQL parses a SQL string using the go-lemon generated LALR(1) parser.
// Returns a list of statements compatible with Frigolite's AST types.
func ParseSQL(input string) ([]sql.Stmt, error) {
	return parseSQLMode(input, false)
}

// ParseSQLSchema parses a SQL string in schema-reload mode, relaxing the
// semantic checks SQLite only applies to freshly-authored SQL. Used when
// re-parsing stored sqlite_schema text (e.g. a writable_schema-modified
// CREATE TABLE whose FOREIGN KEY column list carries COLLATE or ASC/DESC).
func ParseSQLSchema(input string) ([]sql.Stmt, error) {
	return parseSQLMode(input, true)
}

func parseSQLMode(input string, schemaMode bool) ([]sql.Stmt, error) {
	pre, err := preprocessInput(input)
	if err != nil {
		return nil, err
	}
	// If input is only whitespace or comments, return no statements without
	// error (unless SAVEPOINT statements were extracted — they still need
	// returning).
	if stmts, done := emptyInputResult(pre.input, pre.savepointStmts); done {
		return stmts, nil
	}

	// The LALR grammar handles RETURNING clauses (SQLite 3.35+ syntax) with
	// full projection fidelity: INSERT/UPDATE/DELETE RETURNING populates the
	// AST's Returning/HasReturning fields (multi-expression RETURNING folds
	// into a RowValue). No RD fallback is needed for RETURNING.
	origLen := len(pre.input)
	stmts, err := runLALRParse(ensureTrailingSemicolon(pre.input), schemaMode, pre.parenSpans, origLen)
	if err != nil {
		return stmts, err
	}

	// WITH-clause (CTE) definitions are carried directly by the LALR grammar:
	// SELECT (rules 85/86), INSERT (rule 164), and CREATE VIEW bodies all
	// populate the AST's CTEs field. No RD re-parse merge is needed.
	// Recover function-call ORDER BY clauses that the LALR tables drop
	// (e.g. group_concat(a ORDER BY b)) by scanning the raw statement text.
	recoverFuncCallOrderBy(stmts)
	// INSERT/UPDATE/DELETE targets with a schema-qualified table plus an
	// AS alias (main.t1 AS t2) reduce to a malformed TableRef through the
	// LALR tables; recover the real table name and alias from the raw SQL.
	fixupDMLTableAlias(stmts)
	if pre.hasStmtRewrite {
		attachStmtOrderLimit(stmts, pre.stmtClauses)
	}
	// Re-insert extracted SAVEPOINT statements in their original input order
	// (interleaved with the regular statements).
	if len(pre.stmtKind) > 0 {
		stmts = interleaveSavepoints(stmts, pre.savepointStmts, pre.stmtKind)
	}
	return stmts, nil
}

// parseReducer accumulates completed statements during LALR reduction and
// captures the raw SQL text for each statement as it is completed.
type parseReducer struct {
	input       string
	stmts       []sql.Stmt
	pendingStmt sql.Stmt
	stmtStart   int // byte offset where the current statement begins
	parenSpans  []parenRewriteSpan
}

// reduce implements the parser's OnReduce callback: run the grammar action
// for the rule, collect completed statements, and set the LHS value.
func (r *parseReducer) reduce(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) {
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

	// Collect completed statements when the statement-root rules fire.
	r.collect(ruleNo, p, result)

	// Set the LHS value on the stack.
	// For empty rules, LHS overwrites current top position.
	lhsSlot := top
	if size > 0 {
		lhsSlot = top - size + 1
	}
	p.stack[lhsSlot].Minor = result
}

// collect appends a completed statement and captures its raw SQL text.
// Statements complete at the ecmd rules (after SEMI is consumed) to handle
// multi-statement input. Rules 352 (ecmd ::= cmdx SEMI) and 353
// (ecmd ::= explain cmdx SEMI) complete a statement.
func (r *parseReducer) collect(ruleNo int, p *Parser, result interface{}) {
	s, ok := result.(sql.Stmt)
	if !ok {
		return
	}
	r.pendingStmt = s
	if ruleNo != 352 && ruleNo != 353 {
		return
	}
	r.stmts = append(r.stmts, s)
	// Capture the raw statement text. The SEMI token's Pos is its byte
	// offset in input, so input[stmtStart:Pos] is the exact statement
	// (SQLite stores original DDL text verbatim).
	rhs := 2 // rule 352: cmdx SEMI
	if ruleNo == 353 {
		rhs = 3 // rule 353: explain cmdx SEMI
	}
	if semiTok, tokOK := getRHS(p, ruleNo, rhs).(sql.Token); tokOK {
		end := semiTok.Pos
		if end >= r.stmtStart && end <= len(r.input) {
			setStatementRawSQL(s, r.input[r.stmtStart:end], r.stmtStart, r.parenSpans)
			r.stmtStart = end + 1
		}
	}
}

// runLALRParse feeds input through the LALR parser and returns the completed
// statements. A trailing syntax error after a parseable prefix is returned
// together with the prefix statements (SQLite prepares/executes statements
// incrementally: the parseable prefix runs and its error takes precedence).
// schemaMode relaxes eidlist COLLATE/sortorder checks (see Parser.SchemaMode).
func runLALRParse(input string, schemaMode bool, parenSpans []parenRewriteSpan, origLen int) ([]sql.Stmt, error) {
	tables := GetParseTables()
	parser := NewParser(tables)
	parser.SchemaMode = schemaMode
	reducer := &parseReducer{input: input, parenSpans: parenSpans}
	parser.OnReduce(reducer.reduce)

	lalrErr := feedParserTokens(parser, input, origLen)
	parser.Finalize()
	if parser.SemanticErr != nil {
		return nil, parser.SemanticErr
	}
	if lalrErr != nil {
		return reducer.stmts, lalrErr
	}
	if len(reducer.stmts) == 0 {
		if reducer.pendingStmt != nil {
			// No statements were collected via ecmd (no SEMI in input).
			// Use pendingStmt as a fallback.
			reducer.stmts = append(reducer.stmts, reducer.pendingStmt)
		} else {
			return nil, fmt.Errorf("no statements parsed")
		}
	}
	return reducer.stmts, nil
}

// lexErrorToken builds the error for a lexer-level failure. An unrecognized
// token (e.g. 0x0MATCH, an unterminated string) reports SQLite's "unrecognized
// token" message rather than a generic syntax error.
func lexErrorToken(tok sql.Token) error {
	if tok.Type == sql.TokenUnrecognized {
		// SQLite formats the token with %T — the raw text wrapped in plain
		// quotes WITHOUT escaping (main-3.2.3: token "abc reports
		// unrecognized token: ""abc"). Go's %q would escape the embedded
		// quote and produce a different message.
		return fmt.Errorf("unrecognized token: \"%s\"", tok.Value)
	}
	return fmt.Errorf("near %q: syntax error", tok.Value)
}

// restoreKeywordCase restores the original input case for a keyword token that
// was mapped to TK_ID (unknown keyword treated as identifier). The RD lexer
// uppercases keyword values, but SQLite preserves identifier case.
func restoreKeywordCase(input string, code int, tok *sql.Token) {
	if code != TK_ID || tok.Type != sql.TokenKeyword {
		return
	}
	end := tok.Pos + len(tok.Value)
	if end <= len(input) {
		tok.Value = input[tok.Pos:end]
	}
}

// feedParserTokens feeds all tokens from input to the LALR parser until EOF
// or a lexer/parse error, returning the error (nil on clean accept).
//
// The pre-generated LALR tables can shift TK_GENERATED in the initial
// carglist state but not in the continuation state (after another column
// constraint has been reduced). Since "GENERATED ALWAYS AS (expr)" is
// semantically identical to "AS (expr)" in SQLite (both create a generated
// column; STORED/VIRTUAL is ignored by this engine), we transparently drop
// the GENERATED ALWAYS keywords from the token stream so the parser sees
// just AS, which works in every grammar position.
func feedParserTokens(parser *Parser, input string, origLen int) error {
	lexer := sql.NewTokenizer(input)
	skipAlways := false
	var prevCode int // code of the previously fed token (0 = TK_EOF / none)
	for {
		tok := lexer.Next()
		code := tokenCode(int(tok.Type), tok.Value)
		if code < 0 {
			// The engine appends "\n;" after the original input
			// (ensureTrailingSemicolon). An EOF-spanning illegal token must
			// not quote that appended terminator: clamp the token text to the
			// original input, matching SQLite (which tokenizes to NUL).
			if tok.Pos < origLen && tok.Pos+len(tok.Value) > origLen {
				tok.Value = input[tok.Pos:origLen]
			}
			return lexErrorToken(tok)
		}
		// OVER and WINDOW are context-sensitive in SQLite: they are keywords
		// only in window-function/window-clause positions, and identifiers
		// everywhere else (SELECT sum(x) over FROM over — `over` is an alias
		// and a table name). SQLite decides via analyzeOverKeyword/
		// analyzeWindowKeyword:
		//   OVER is a keyword iff the previous token was ')' and the next is
		//     '(' or an identifier;
		//   WINDOW is a keyword iff the next token is an identifier and the
		//     one after that is AS (i.e. "WINDOW <name> AS").
		// OVER/WINDOW/FILTER context rules apply to KEYWORD tokens only;
		// a quoted string such as 'filter' must stay TK_STRING (its value
		// matching a keyword must not turn the literal into an identifier).
		if tok.Type == sql.TokenKeyword {
			if strings.EqualFold(tok.Value, "OVER") {
				if !(prevCode == TK_RP && windowOverNextIsIdentOrLP(lexer)) {
					code = TK_ID
				}
			} else if strings.EqualFold(tok.Value, "WINDOW") {
				if !windowKeywordFollowedByIdentAS(lexer) {
					code = TK_ID
				}
			} else if strings.EqualFold(tok.Value, "FILTER") {
				// tokenize.c analyzeFilterKeyword: FILTER is a keyword only after
				// a closing paren and before an opening one (aggregate FILTER
				// clause); otherwise it is an ordinary identifier.
				if !(prevCode == TK_RP && lexer.Peek().Type == sql.TokenLParen) {
					code = TK_ID
				}
			}
		}
		// REPLACE is both INSERT syntax and a scalar function name. Inside
		// expression-call position, feed it as an identifier.
		if strings.EqualFold(tok.Value, "REPLACE") && lexer.Peek().Type == sql.TokenLParen {
			code = TK_ID
		}
		// Drop "GENERATED ALWAYS" when they precede AS and the trailing
		// VIRTUAL/STORED storage clause — see comment above. The lexer may
		// classify these as identifiers (TK_ID) or keywords, so we match by
		// uppercased token value for robustness. STORED/VIRTUAL is ignored
		// by this engine (all generated columns are virtual).
		if strings.EqualFold(tok.Value, "GENERATED") {
			peek := lexer.Peek()
			if strings.EqualFold(peek.Value, "ALWAYS") {
				skipAlways = true
				continue
			}
		}
		if skipAlways {
			skipAlways = false
			continue // drop the ALWAYS token
		}
		if strings.EqualFold(tok.Value, "VIRTUAL") || strings.EqualFold(tok.Value, "STORED") {
			// Only drop VIRTUAL/STORED when it follows a generated column
			// definition "... AS (expr) VIRTUAL/STORED". The previous token
			// would have been ')' closing the generated expression. For
			// CREATE VIRTUAL TABLE, VIRTUAL is a real keyword (TK_VIRTUAL)
			// and must not be dropped — but that VIRTUAL appears as
			// "CREATE VIRTUAL TABLE" with previous token CREATE/TABLE, not ")".
			if prevCode == TK_RP {
				continue // drop VIRTUAL/STORED storage clause
			}
		}
		restoreKeywordCase(input, code, &tok)

		result := parser.Parse(code, tok)
		if result == ParseError {
			if code == 0 || (origLen > 0 && tok.Pos >= origLen) {
				// SQLite reports "incomplete input" when a statement ends
				// mid-parse at EOF (with2 6.6: "DELETE FROM t2 WHERE" with
				// no terminator). The engine appends a trailing ";" so the
				// LALR grammar sees a terminator; an error on that appended
				// token means the input itself was incomplete.
				return fmt.Errorf("incomplete input")
			}
			return fmt.Errorf("near %q: syntax error", tok.Value)
		}
		if result == ParseAccept && code == 0 { // EOF
			return nil
		}
		prevCode = code
	}
}

// windowOverNextIsIdentOrLP reports whether the token after an OVER keyword
// position is '(' or an identifier (SQLite's analyzeOverKeyword second
// condition). SQLite's getToken() classifies TK_OVER/TK_WINDOW/TK_JOIN_KW and
// fallback-to-ID keywords as identifiers here, so `OVER over` (a named window
// called over) is an OVER keyword.
func windowOverNextIsIdentOrLP(lexer *sql.Tokenizer) bool {
	peek := lexer.Peek()
	if peek.Type == sql.TokenLParen {
		return true
	}
	return windowTokenIsIdentLike(tokenCode(int(peek.Type), peek.Value))
}

// windowKeywordFollowedByIdentAS reports whether the tokens after a WINDOW
// keyword position are "<identifier> AS" (SQLite's analyzeWindowKeyword:
// WINDOW is a keyword iff the next token is an identifier and the one after
// that is AS). It saves/restores the tokenizer position so the main loop sees
// the tokens again.
func windowKeywordFollowedByIdentAS(lexer *sql.Tokenizer) bool {
	pos := lexer.Position()
	tok1 := lexer.Next()
	if tok1.Type == sql.TokenEOF {
		return false
	}
	tok2 := lexer.Next()
	lexer.SetPosition(pos)
	if !windowTokenIsIdentLike(tokenCode(int(tok1.Type), tok1.Value)) {
		return false
	}
	return strings.EqualFold(tok2.Value, "AS")
}

// windowTokenIsIdentLike reports whether a token code counts as an identifier
// in SQLite's window-keyword lookahead (getToken()): TK_ID, quoted strings,
// join keywords, and the OVER/WINDOW keywords themselves all classify as
// identifiers so `OVER over` and `WINDOW window AS ...` parse as named-window
// references.
func windowTokenIsIdentLike(code int) bool {
	return code == TK_ID || code == TK_STRING || code == TK_JOIN_KW ||
		code == TK_OVER || code == TK_WINDOW
}

// interleaveSavepoints merges extracted SAVEPOINT statements back into the
// regular statement list in their original input order.
func interleaveSavepoints(stmts, savepointStmts []sql.Stmt, stmtKind []bool) []sql.Stmt {
	var ordered []sql.Stmt
	spIdx := 0
	regIdx := 0
	for _, isSavepoint := range stmtKind {
		if isSavepoint {
			if spIdx < len(savepointStmts) {
				ordered = append(ordered, savepointStmts[spIdx])
				spIdx++
			}
		} else {
			if regIdx < len(stmts) {
				ordered = append(ordered, stmts[regIdx])
				regIdx++
			}
		}
	}
	ordered = append(ordered, stmts[regIdx:]...)
	return ordered
}

// recoverFuncCallOrderBy re-attaches ORDER BY clauses inside aggregate
// function calls (e.g. group_concat(a ORDER BY b)) that the LALR parse
// tables drop. The parser reduces the function call without the ORDER BY,
// leaving the FuncCall.OrderBy empty; this pass scans each SELECT's raw
// statement text for "funcname( ... ORDER BY sortlist )" and attaches the
// recovered sortlist to the matching FuncCall.

func extractSavepointStatements(input string) (string, []sql.Stmt, []bool) {
	var savepointStmts []sql.Stmt
	var stmtKind []bool // true = savepoint placeholder, false = regular
	if !hasSavepointStatements(input) {
		return input, savepointStmts, stmtKind
	}
	var rebuilt []string
	for _, stmtText := range splitSQLStatements(input) {
		if sp, ok := parseSavepointStatement(stmtText); ok {
			savepointStmts = append(savepointStmts, sp)
			stmtKind = append(stmtKind, true)
			rebuilt = append(rebuilt, "/* __SAVEPOINT__ */")
		} else {
			stmtKind = append(stmtKind, false)
			rebuilt = append(rebuilt, stmtText)
		}
	}
	return strings.Join(rebuilt, ";"), savepointStmts, stmtKind
}

// setStatementRawSQL captures the raw statement text (input[stmtStart:end])
// on the statement types that store original DDL text verbatim. Paren-set
// rewrite spans are restored so SQLite's verbatim storage is preserved
// (altertab2-4.x stores "SET (c,d)=(a,b)" exactly as written).

func setStatementRawSQL(s sql.Stmt, stmtText string, stmtStart int, parenSpans []parenRewriteSpan) {
	raw := restoreParenSetSpans(stmtText, stmtStart, parenSpans)
	raw = strings.TrimSpace(raw)
	if ct, ok := s.(*sql.CreateTableStmt); ok {
		ct.RawSQL = raw
	} else if tr, ok := s.(*sql.CreateTriggerStmt); ok {
		tr.RawSQL = raw
	} else if vw, ok := s.(*sql.CreateViewStmt); ok {
		vw.RawSQL = raw
	} else if ci, ok := s.(*sql.CreateIndexStmt); ok {
		ci.RawSQL = raw
	} else if vt, ok := s.(*sql.CreateVirtualTableStmt); ok {
		vt.RawSQL = raw
	} else if sel, ok := s.(*sql.SelectStmt); ok {
		sel.RawSQL = raw
	} else if ins, ok := s.(*sql.InsertStmt); ok {
		ins.RawSQL = raw
	} else if upd, ok := s.(*sql.UpdateStmt); ok {
		upd.RawSQL = raw
	} else if del, ok := s.(*sql.DeleteStmt); ok {
		del.RawSQL = raw
	}
}

// fixupDMLTableAlias recovers the real table name and optional AS alias for
// INSERT/UPDATE/DELETE statements whose LALR reduction lost them. The grammar
// handles "t AS alias" and "schema.t", but "schema.t AS alias" reduces to a
// malformed value; the raw statement text is re-scanned to recover the target.
// Also attaches the alias for the plain "t AS alias" form (rule 122 consumes
// the alias), so DO UPDATE / SET / WHERE expressions can resolve it.
func fixupDMLTableAlias(stmts []sql.Stmt) {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *sql.InsertStmt:
			if s.RawSQL == "" {
				continue
			}
			if tbl, alias := extractTargetAlias(s.RawSQL, "INSERT INTO", "ON CONFLICT"); tbl != "" {
				s.Table = tbl
				s.Alias = alias
			}
		case *sql.UpdateStmt:
			if s.RawSQL == "" {
				continue
			}
			if tbl, alias := extractTargetAlias(stripUpdateOrClause(s.RawSQL), "UPDATE", "SET"); tbl != "" {
				s.Table = tbl
				s.Alias = alias
			}
		case *sql.DeleteStmt:
			if s.RawSQL == "" {
				continue
			}
			if tbl, alias := extractTargetAlias(s.RawSQL, "DELETE FROM", "WHERE"); tbl != "" {
				s.Table = tbl
				s.Alias = alias
			}
		}
	}
}

// stripUpdateOrClause removes an UPDATE ... OR conflict-resolution clause
// ("OR REPLACE", "OR IGNORE", "OR FAIL", "OR ABORT", "OR ROLLBACK") from the
// start of an UPDATE statement so extractTargetAlias reads the real table
// name. Without this, "UPDATE OR REPLACE t1 SET ..." would yield table "OR".
func stripUpdateOrClause(sqlText string) string {
	upper := strings.ToUpper(sqlText)
	idx := strings.Index(upper, "UPDATE")
	if idx < 0 {
		return sqlText
	}
	rest := sqlText[idx+len("UPDATE"):]
	trimmed := strings.TrimSpace(rest)
	u := strings.ToUpper(trimmed)
	for _, kw := range []string{"REPLACE", "IGNORE", "FAIL", "ABORT", "ROLLBACK"} {
		if strings.HasPrefix(u, "OR "+kw) && (len(u) == len("OR "+kw) || isSpaceByte(u[len("OR "+kw)])) {
			return sqlText[:idx+len("UPDATE")] + " " + trimmed[len("OR "+kw):]
		}
	}
	return sqlText
}

// extractTargetAlias scans the beginning of a DML statement for the target
// table and an optional AS alias. Returns (table, alias).
func extractTargetAlias(sqlText, kw, endKw string) (string, string) {
	upper := strings.ToUpper(sqlText)
	kwIdx := strings.Index(upper, kw)
	if kwIdx < 0 {
		return "", ""
	}
	rest := strings.TrimSpace(sqlText[kwIdx+len(kw):])
	table, consumed := readDMLIdent(rest)
	if table == "" {
		return "", ""
	}
	after := strings.TrimSpace(rest[consumed:])
	if len(after) >= 2 && strings.EqualFold(after[:2], "AS") &&
		(len(after) == 2 || isSpaceByte(after[2])) {
		alias, _ := readDMLIdent(strings.TrimSpace(after[2:]))
		return table, alias
	}
	_ = endKw
	return table, ""
}

// readDMLIdent reads the leading identifier (table name or alias) from s,
// stopping at whitespace, a paren, or a comma. A single-quoted, double-
// quoted, backtick, or bracket-quoted identifier is returned without its
// outer quotes (matching how the grammar's nm rule produces the plain name —
// single-quoted strings as identifiers are SQLite's legacy identifier syntax,
// e.g. 'p 1 "parent one"' in e_fkey 56.x). Doubled-quote escapes inside
// the identifier are collapsed to one quote. The second return is the number
// of bytes consumed from s (including any quotes), so callers can locate the
// remainder.
func readDMLIdent(s string) (string, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0
	}
	i := 0
	if c := s[0]; c == '\'' || c == '"' || c == '`' || c == '[' {
		closer := byte(']')
		if c == '\'' {
			closer = '\''
		} else if c != '[' {
			closer = c
		}
		i = 1
		var inner []byte
		for i < len(s) {
			if s[i] == closer {
				if closer != ']' && i+1 < len(s) && s[i+1] == closer {
					inner = append(inner, closer)
					i += 2
					continue
				}
				i++
				break
			}
			inner = append(inner, s[i])
			i++
		}
		if i > len(s) {
			i = len(s)
		}
		if c == '\'' || c == '"' || c == '`' {
			return string(inner), i
		}
		return s[1 : i-1], i
	}
	for i < len(s) {
		c := s[i]
		if c == '(' || c == ',' || isSpaceByte(c) {
			break
		}
		i++
	}
	return s[:i], i
}

// isSpaceByte reports whether c is ASCII whitespace.
func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// restoreParenSetSpans replaces each paren-set rewrite span in stmtText (the
// statement's text as a slice of the rewritten input beginning at stmtStart)
// with the original paren-set text, so RawSQL matches what the caller wrote.
// Spans are sorted by outStart; applying them in reverse keeps earlier spans'
// offsets valid. Spans outside [stmtStart, stmtStart+len(stmtText)) belong to
// other statements and are skipped.
func restoreParenSetSpans(stmtText string, stmtStart int, spans []parenRewriteSpan) string {
	out := stmtText
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		start := sp.outStart - stmtStart
		end := sp.outEnd - stmtStart
		if start < 0 || end > len(out) {
			continue
		}
		out = out[:start] + sp.origText + out[end:]
	}
	return out
}

// recoverSelectFuncCallOrderBy walks a SELECT's column expressions and
// attaches function-call ORDER BY recovered from raw SQL. It recurses into
// subqueries (whose own RawSQL is empty; their text is located within the
// enclosing statement's raw SQL).

// copyQuoted copies a quoted string from s starting at i (s[i] is the quote)
// into b, honoring backslash escapes. Returns the index after the closing
// quote (or len(s) if unterminated).
func copyQuoted(b *strings.Builder, s string, i int) int {
	q := s[i]
	b.WriteByte(s[i])
	i++
	for i < len(s) && s[i] != q {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i])
			i++
		}
		b.WriteByte(s[i])
		i++
	}
	if i < len(s) {
		b.WriteByte(s[i])
		i++
	}
	return i
}

// skipLineComment returns the index after a -- comment (i points at '-').
func skipLineComment(s string, i int) int {
	i += 2
	for i < len(s) && s[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment returns the index after a /* */ comment (i points at '/').
func skipBlockComment(s string, i int) int {
	i += 2
	for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
		i++
	}
	return i + 2
}

func stripSQLComments(s string) string {
	var b strings.Builder
	i := 0
	n := len(s)
	for i < n {
		if s[i] == '\'' || s[i] == '"' {
			i = copyQuoted(&b, s, i)
			continue
		}
		if i+1 < n && s[i] == '-' && s[i+1] == '-' {
			i = skipLineComment(s, i)
			continue
		}
		if i+1 < n && s[i] == '/' && s[i+1] == '*' {
			i = skipBlockComment(s, i)
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
