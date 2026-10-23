// SPDX-License-Identifier: GPL-3.0-or-later
package fts

import (
	"fmt"
	"strconv"
	"strings"
)

// The FTS3 query expression parser (ext/fts3/fts3_expr.c).
//
// The parser builds a tree of expression nodes from a MATCH query string:
// a phrase (one or more tokens), or a binary operator (AND, OR, NOT, NEAR)
// with two operands. The tree is what the FTS3 evaluator walks.
//
// The test build exposes the parser through the scalar functions
// fts3_exprtest() and fts3_exprtest_rebalance(): they parse an expression
// with a given tokenizer and return a text rendering of the tree, so a test
// can check the parser's structure. fts3_exprtest() returns the tree as
// parsed; fts3_exprtest_rebalance() returns the tree after the rebalancing
// that keeps a large expression's depth bounded.
//
// This file is a port of ext/fts3/fts3_expr.c: the node structure, the
// recursive-descent parser, the operator precedence, the rebalancing
// algorithm and the rendering all follow the C code.

// The node types (fts3Int.h FTSQUERY_*).
const (
	fts3QueryNear   = 1
	fts3QueryNot    = 2
	fts3QueryAnd    = 3
	fts3QueryOr     = 4
	fts3QueryPhrase = 5
)

// fts3ExprNode is one node of a parsed FTS3 query expression.
type fts3ExprNode struct {
	eType  int
	nNear  int
	parent *fts3ExprNode
	left   *fts3ExprNode
	right  *fts3ExprNode
	phrase *fts3Phrase
}

// fts3Phrase is the token list of a phrase node.
type fts3Phrase struct {
	column int
	tokens []fts3PhraseToken
}

// fts3PhraseToken is one token of a phrase.
type fts3PhraseToken struct {
	text     string
	isPrefix bool
	bFirst   bool
}

// fts3ParseCtx is the parser's state (fts3_expr.c's ParseContext).
type fts3ParseCtx struct {
	tokenizer   Tokenizer
	columns     []string
	defaultCol  int
	bFts4       bool
	isNot       bool
	nest        int
	enableParen bool

	// The tokens of the whole expression, with their byte offsets. The C
	// code re-tokenizes the remaining buffer for every token it reads,
	// which is quadratic in the length of the expression — the 1.5 MB
	// expression of fts3expr3.test's create_toggle_tree 17 then needs
	// billions of byte scans. The tokens are scanned once here instead.
	expr      string
	allTokens []OffsetToken
	tokIdx    int // the next token to read
}

// fts3ExprError is a parse error (SQLite returns "Error parsing expression"
// or "malformed MATCH expression: [...]" to the caller).
type fts3ExprError struct{ msg string }

func (e *fts3ExprError) Error() string { return e.msg }

// fts3ExprParseResult is the outcome of parsing an expression.
type fts3ExprParseResult struct {
	expr     *fts3ExprNode
	consumed int
}

// ExprTest parses an FTS3 query expression with the named tokenizer and
// returns the text rendering of the parse tree — the behaviour of SQLite's
// fts3_exprtest() (rebalance false) and fts3_exprtest_rebalance()
// (rebalance true) test functions.
//
// columns names the columns the expression may refer to ("col1", "col2",
// ...); the default column is the last one, as in SQLite.
func ExprTest(tokenizer, expr string, columns []string, rebalance bool) (string, error) {
	if len(columns) == 0 {
		return "", fmt.Errorf("fts3_exprtest: no columns")
	}
	tok, err := NewTokenizerFromSpec(tokenizer, nil)
	if err != nil {
		return "", err
	}
	ctx := &fts3ParseCtx{
		tokenizer: tok,
		columns:   columns,
		expr:      expr,
		allTokens: tok.TokenizeOffsets(expr),
		// SQLite passes nCol as the default column (fts3_expr.c's
		// fts3ExprTestCommon: fts3ExprParseUnbalanced(..., nCol, nCol, ...)),
		// so the rendering of an unqualified phrase names column nCol.
		defaultCol:  len(columns),
		enableParen: fts3ParenthesesEnabled(),
	}
	root, err := fts3ExprParseUnbalanced(ctx, expr)
	if err != nil {
		return "", err
	}
	if rebalance && root != nil {
		root, err = fts3ExprBalance(root, fts3MaxExprDepth)
		if err != nil {
			return "", err
		}
		if err = fts3ExprCheckDepth(root, fts3MaxExprDepth); err != nil {
			return "", err
		}
	}
	return fts3ExprToString(root), nil
}

// ExprTestFunc returns the SQL function that parses an FTS3 query expression
// and returns the rendering of its parse tree — the behaviour of SQLite's
// fts3_exprtest() (rebalance false) and fts3_exprtest_rebalance()
// (rebalance true).
func ExprTestFunc(rebalance bool) func(args []interface{}) (interface{}, error) {
	return func(args []interface{}) (interface{}, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("Usage: fts3_exprtest(tokenizer, expr, col1, ...")
		}
		tokenizer, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("fts3_exprtest: the tokenizer name must be text")
		}
		expr, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("fts3_exprtest: the expression must be text")
		}
		columns := make([]string, 0, len(args)-2)
		for _, arg := range args[2:] {
			name, ok := arg.(string)
			if !ok {
				return nil, fmt.Errorf("fts3_exprtest: the column names must be text")
			}
			columns = append(columns, name)
		}
		result, err := ExprTest(tokenizer, expr, columns, rebalance)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}

// fts3MaxExprDepth is the maximum depth of a balanced expression tree
// (SQLITE_FTS3_MAX_EXPR_DEPTH).
const fts3MaxExprDepth = 100

// fts3isspace mirrors the C helper of the same name: it never reads a byte
// outside the ASCII range.
func fts3isspace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// findBarredChar returns the index of the first quote (or, in parenthesis
// mode, the first bracket) in the buffer, or -1.
func findBarredChar(z string, enableParen bool) int {
	for i := 0; i < len(z); i++ {
		if z[i] == '"' || (enableParen && (z[i] == '(' || z[i] == ')')) {
			return i
		}
	}
	return -1
}

// fts3ExprParseUnbalanced parses a query expression without rebalancing it
// (fts3ExprParseUnbalanced).
func fts3ExprParseUnbalanced(ctx *fts3ParseCtx, expr string) (*fts3ExprNode, error) {
	res, _, err := fts3ExprParse(ctx, 0)
	if err != nil {
		return nil, err
	}
	if ctx.nest != 0 {
		// A mismatched parenthesis. SQLite's fts3ExprTestCommon reports
		// every parse failure as "Error parsing expression".
		return nil, &fts3ExprError{msg: "Error parsing expression"}
	}
	return res, nil
}

// fts3ExprParse parses a query expression, stopping at the end of the input
// or at an unmatched ')' (fts3ExprParse).
func fts3ExprParse(ctx *fts3ParseCtx, base int) (*fts3ExprNode, int, error) {
	var root, prev *fts3ExprNode
	var notBranch *fts3ExprNode
	isRequirePhrase := true
	pos := base

	for {
		node, consumed, done, err := fts3GetNextNode(ctx, pos)
		if err != nil {
			return nil, 0, err
		}
		if done {
			// An unmatched ')' ends the expression (the C returns
			// SQLITE_DONE from getNextNode in this case).
			pos += consumed
			break
		}
		if node != nil {
			if !ctx.enableParen && node.eType == fts3QueryPhrase && ctx.isNot {
				// The legacy syntax: a unary '-' becomes an implicit NOT
				// operator applied to the whole branch.
				not := &fts3ExprNode{eType: fts3QueryNot, right: node}
				node.parent = not
				if notBranch != nil {
					not.left = notBranch
					notBranch.parent = not
				}
				notBranch = not
				node = prev
			} else {
				eType := node.eType
				isPhrase := eType == fts3QueryPhrase || node.left != nil

				// A binary operator cannot be the first thing in the
				// expression, and a phrase cannot follow one directly.
				if !isPhrase && isRequirePhrase {
					return nil, 0, &fts3ExprError{msg: "Error parsing expression"}
				}
				if isPhrase && !isRequirePhrase {
					// Insert an implicit AND operator.
					and := &fts3ExprNode{eType: fts3QueryAnd}
					fts3InsertBinaryOperator(ctx, &root, prev, and)
					prev = and
				}
				// NEAR only accepts phrases as operands.
				if prev != nil &&
					((eType == fts3QueryNear && !isPhrase && prev.eType != fts3QueryPhrase) ||
						(eType != fts3QueryPhrase && isPhrase && prev.eType == fts3QueryNear)) {
					return nil, 0, &fts3ExprError{msg: "Error parsing expression"}
				}
				if isPhrase {
					if root != nil {
						prev.right = node
						node.parent = prev
					} else {
						root = node
					}
				} else {
					fts3InsertBinaryOperator(ctx, &root, prev, node)
				}
				isRequirePhrase = !isPhrase
			}
			prev = node
		}
		if consumed <= 0 {
			break
		}
		pos += consumed
	}

	if root != nil && isRequirePhrase {
		return nil, 0, &fts3ExprError{msg: "Error parsing expression"}
	}
	if !ctx.enableParen && notBranch != nil {
		if root == nil {
			return nil, 0, &fts3ExprError{msg: "Error parsing expression"}
		}
		iter := notBranch
		for iter.left != nil {
			iter = iter.left
		}
		iter.left = root
		root.parent = iter
		root = notBranch
	}
	return root, pos - base, nil
}

// fts3GetNextNode reads the next node of the expression (getNextNode). The
// position is an absolute offset into the expression.
func fts3GetNextNode(ctx *fts3ParseCtx, base int) (*fts3ExprNode, int, bool, error) {
	ctx.isNot = false
	expr := ctx.expr

	// Skip the whitespace before a keyword, a bracket or a quote.
	skip := base
	for skip < len(expr) && fts3isspace(expr[skip]) {
		skip++
	}
	if skip >= len(expr) {
		return nil, 0, false, nil
	}
	z := expr[skip:]

	// A keyword?
	keywords := []struct {
		text      string
		parenOnly bool
		eType     int
	}{
		{"OR", false, fts3QueryOr},
		{"AND", true, fts3QueryAnd},
		{"NOT", true, fts3QueryNot},
		{"NEAR", false, fts3QueryNear},
	}
	for _, kw := range keywords {
		if kw.parenOnly && !ctx.enableParen {
			continue
		}
		if !strings.HasPrefix(z, kw.text) {
			continue
		}
		nKey := len(kw.text)
		nNear := 10 // SQLITE_FTS3_DEFAULT_NEAR_PARAM
		if kw.eType == fts3QueryNear && len(z) > nKey+1 && z[nKey] == '/' && z[nKey+1] >= '0' && z[nKey+1] <= '9' {
			digits := nKey + 1
			for digits < len(z) && z[digits] >= '0' && z[digits] <= '9' {
				digits++
			}
			if v, err := strconv.Atoi(z[nKey+1 : digits]); err == nil {
				nNear = v
				nKey = digits
			}
		}
		// The keyword must be followed by whitespace, a quote, a bracket
		// or the end of the input.
		if nKey < len(z) {
			cNext := z[nKey]
			if !(fts3isspace(cNext) || cNext == '"' || cNext == '(' || cNext == ')') {
				continue
			}
		}
		return &fts3ExprNode{eType: kw.eType, nNear: nNear}, skip + nKey - base, false, nil
	}

	// A quoted phrase.
	if z[0] == '"' {
		end := strings.IndexByte(z[1:], '"')
		if end < 0 {
			return nil, 0, false, &fts3ExprError{msg: "Error parsing expression"}
		}
		node, err := fts3GetNextString(ctx, skip+1, skip+1+end)
		if err != nil {
			return nil, 0, false, err
		}
		return node, skip + end + 2 - base, false, nil
	}

	if ctx.enableParen {
		if z[0] == '(' {
			ctx.nest++
			if ctx.nest > 1000 {
				return nil, 0, false, &fts3ExprError{msg: "Error parsing expression"}
			}
			node, consumed, err := fts3ExprParse(ctx, skip+1)
			if err != nil {
				return nil, 0, false, err
			}
			return node, skip + 1 + consumed - base, false, nil
		}
		if z[0] == ')' {
			ctx.nest--
			return nil, skip + 1 - base, true, nil
		}
	}

	// A regular token, possibly with an explicit column specifier.
	iCol := ctx.defaultCol
	pos := skip
	for ii, col := range ctx.columns {
		if len(z) > len(col) && z[len(col)] == ':' && strings.EqualFold(z[:len(col)], col) {
			iCol = ii
			pos = skip + len(col) + 1
			break
		}
	}
	node, consumed, err := fts3GetNextToken(ctx, iCol, pos)
	if err != nil {
		return nil, 0, false, err
	}
	return node, consumed - base, false, nil
}

// fts3GetNextToken reads one token at the given position and builds the
// phrase node for it (getNextToken). The position is an absolute offset into
// the expression; the returned offset is the end of the token.
func fts3GetNextToken(ctx *fts3ParseCtx, iCol, pos int) (*fts3ExprNode, int, error) {
	expr := ctx.expr
	// The next token at or after pos.
	for ctx.tokIdx < len(ctx.allTokens) && ctx.allTokens[ctx.tokIdx].Start < pos {
		ctx.tokIdx++
	}
	if ctx.tokIdx >= len(ctx.allTokens) {
		// No token: the input ends here (or holds only delimiters).
		if barred := findBarredChar(expr[pos:], ctx.enableParen); barred >= 0 {
			return nil, pos + barred, nil
		}
		return nil, len(expr), nil
	}
	tok := ctx.allTokens[ctx.tokIdx]
	// SQLite retries tokenization on input truncated at the first quote or
	// parenthesis. When that delimiter precedes the next token, consume only
	// the prefix and leave the token available for the next parser segment.
	if barred := findBarredChar(expr[pos:tok.Start], ctx.enableParen); barred >= 0 {
		return nil, pos + barred, nil
	}
	ctx.tokIdx++
	// The tokenizer must not have swallowed a quote or a bracket.
	end := tok.End
	if barred := findBarredChar(expr[tok.Start:end], ctx.enableParen); barred >= 0 {
		end = tok.Start + barred
	}
	phrase := &fts3Phrase{column: iCol}
	phrase.tokens = append(phrase.tokens, fts3PhraseToken{text: expr[tok.Start:end]})
	if end < len(expr) && expr[end] == '*' {
		phrase.tokens[0].isPrefix = true
		end++
	}
	start := tok.Start
	for {
		if !ctx.enableParen && start > 0 && expr[start-1] == '-' {
			ctx.isNot = true
			start--
			continue
		}
		if ctx.bFts4 && start > 0 && expr[start-1] == '^' {
			phrase.tokens[0].bFirst = true
			start--
			continue
		}
		break
	}
	return &fts3ExprNode{eType: fts3QueryPhrase, phrase: phrase}, end, nil
}

// fts3GetNextString builds a phrase node from a quoted string (getNextString).
// The range is the content of the quotes, as absolute offsets.
func fts3GetNextString(ctx *fts3ParseCtx, start, end int) (*fts3ExprNode, error) {
	phrase := &fts3Phrase{column: ctx.defaultCol}
	for ctx.tokIdx < len(ctx.allTokens) {
		tok := ctx.allTokens[ctx.tokIdx]
		if tok.Start >= end {
			break
		}
		ctx.tokIdx++
		if tok.Start < start {
			continue
		}
		tokEnd := tok.End
		if tokEnd > end {
			tokEnd = end
		}
		phrase.tokens = append(phrase.tokens, fts3PhraseToken{
			text:     ctx.expr[tok.Start:tokEnd],
			isPrefix: tokEnd < end && ctx.expr[tokEnd] == '*',
			bFirst:   tok.Start > 0 && ctx.expr[tok.Start-1] == '^',
		})
	}
	return &fts3ExprNode{eType: fts3QueryPhrase, phrase: phrase}, nil
}

// fts3OpPrecedence returns the precedence of a binary operator: lower values
// group more tightly (opPrecedence).
func fts3OpPrecedence(ctx *fts3ParseCtx, node *fts3ExprNode) int {
	if ctx.enableParen {
		return node.eType
	}
	switch node.eType {
	case fts3QueryNear:
		return 1
	case fts3QueryOr:
		return 2
	}
	return 3
}

// fts3InsertBinaryOperator inserts a binary operator into the tree, honouring
// the operator precedence (insertBinaryOperator).
func fts3InsertBinaryOperator(ctx *fts3ParseCtx, head **fts3ExprNode, prev, new *fts3ExprNode) {
	split := prev
	for split.parent != nil && fts3OpPrecedence(ctx, split.parent) <= fts3OpPrecedence(ctx, new) {
		split = split.parent
	}
	if split.parent != nil {
		split.parent.right = new
		new.parent = split.parent
	} else {
		*head = new
	}
	new.left = split
	split.parent = new
}

// fts3ExprCheckDepth reports an error when the tree is deeper than the limit
// (fts3ExprCheckDepth).
func fts3ExprCheckDepth(node *fts3ExprNode, maxDepth int) error {
	if node == nil {
		return nil
	}
	if maxDepth < 0 {
		return &fts3ExprError{msg: "FTS expression tree is too large"}
	}
	if err := fts3ExprCheckDepth(node.left, maxDepth-1); err != nil {
		return err
	}
	return fts3ExprCheckDepth(node.right, maxDepth-1)
}

// fts3ExprBalance transforms the tree into an equivalent but more balanced
// form, in place (fts3ExprBalance).
func fts3ExprBalance(root *fts3ExprNode, maxDepth int) (*fts3ExprNode, error) {
	if maxDepth == 0 {
		return nil, &fts3ExprError{msg: "FTS expression tree is too large"}
	}
	eType := root.eType
	if eType == fts3QueryAnd || eType == fts3QueryOr {
		leaves := make([]*fts3ExprNode, maxDepth)
		// Set p to the left-most leaf of the tree of eType nodes.
		p := root
		for p.eType == eType {
			p = p.left
		}
		for {
			parent := p.parent
			p.parent = nil
			if parent != nil {
				parent.left = nil
			} else {
				root = nil
			}
			balanced, err := fts3ExprBalance(p, maxDepth-1)
			if err != nil {
				return nil, err
			}
			p = balanced
			// Place the balanced subtree in the leaf list, pairing the
			// leaves of the same level as they come.
			level := 0
			for p != nil && level < maxDepth {
				if leaves[level] == nil {
					leaves[level] = p
					p = nil
				} else {
					free := &fts3ExprNode{eType: eType}
					free.left = leaves[level]
					free.right = p
					free.left.parent = free
					free.right.parent = free
					p = free
					leaves[level] = nil
				}
				level++
			}
			if p != nil {
				return nil, &fts3ExprError{msg: "FTS expression tree is too large"}
			}
			if parent == nil {
				break
			}
			// The next leaf of the tree of eType nodes.
			p = parent.right
			for p.eType == eType {
				p = p.left
			}
			// Remove the parent from the original tree.
			parent.right.parent = parent.parent
			if parent.parent != nil {
				parent.parent.left = parent.right
			} else {
				root = parent.right
			}
			parent.parent = nil
		}
		// Build the new tree from the leaf list.
		var result *fts3ExprNode
		for _, leaf := range leaves {
			if leaf == nil {
				continue
			}
			if result == nil {
				result = leaf
				result.parent = nil
				continue
			}
			free := &fts3ExprNode{eType: eType}
			free.right = result
			free.left = leaf
			free.left.parent = free
			free.right.parent = free
			result = free
			result.parent = nil
		}
		root = result
	} else if eType == fts3QueryNot {
		left, right := root.left, root.right
		root.left, root.right = nil, nil
		left.parent, right.parent = nil, nil
		var err error
		left, err = fts3ExprBalance(left, maxDepth-1)
		if err == nil {
			right, err = fts3ExprBalance(right, maxDepth-1)
		}
		if err != nil {
			return nil, err
		}
		root.left = left
		left.parent = root
		root.right = right
		right.parent = root
	}
	return root, nil
}

// fts3ExprBinary renders a binary operator node: "<OP> {<left>} {<right>}".
func fts3ExprBinary(op string, node *fts3ExprNode) string {
	return op + " {" + fts3ExprToString(node.left) + "} {" + fts3ExprToString(node.right) + "}"
}

// fts3ExprToString renders a parsed expression the way SQLite's exprToString
// does: a phrase as "PHRASE <column> 0 <token>...", a binary operator as
// "<OP> {<left>} {<right>}".
func fts3ExprToString(node *fts3ExprNode) string {
	if node == nil {
		return ""
	}
	switch node.eType {
	case fts3QueryPhrase:
		var b strings.Builder
		b.WriteString("PHRASE ")
		b.WriteString(strconv.Itoa(node.phrase.column))
		b.WriteString(" 0")
		for _, tok := range node.phrase.tokens {
			b.WriteString(" ")
			b.WriteString(tok.text)
			if tok.isPrefix {
				b.WriteString("+")
			}
		}
		return b.String()
	case fts3QueryNear:
		return fts3ExprBinary("NEAR/"+strconv.Itoa(node.nNear), node)
	case fts3QueryNot:
		return fts3ExprBinary("NOT", node)
	case fts3QueryAnd:
		return fts3ExprBinary("AND", node)
	case fts3QueryOr:
		return fts3ExprBinary("OR", node)
	}
	return ""
}
