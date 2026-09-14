package fts5

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/pijalu/frigolite/internal/fts"
)

// This file ports the fts5_expr() and fts5_expr_tcl() test-support scalar
// functions (fts5_expr.c fts5ExprFunction, compiled under SQLITE_TEST /
// SQLITE_FTS5_DEBUG; the engine registers them unconditionally — documented
// deviation). fts5_expr(zExpr, col...) parses zExpr against a synthetic
// single-table configuration whose columns are the trailing arguments
// (azConfig = ["", "main", "tbl", col...]) and returns the parsed tree in
// fts5ExprPrint's textual form; fts5_expr_tcl renders fts5ExprPrintTcl's
// TCL form with the caller's nearset command name (second argument, default
// "nearset").

// ExprFuncExpr is the fts5_expr() registration entry point.
func ExprFuncExpr(args []interface{}) (interface{}, error) { return ExprFunc(args, false) }

// ExprFuncTcl is the fts5_expr_tcl() registration entry point.
func ExprFuncTcl(args []interface{}) (interface{}, error) { return ExprFunc(args, true) }

// valueInt coerces a function-argument value to an integer
// (sqlite3_value_int: REAL truncates toward zero, text parses numerically).
func valueInt(v interface{}) int {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	case []byte:
		n, _ := strconv.Atoi(strings.TrimSpace(string(x)))
		return n
	}
	return 0
}

// IsAlnumFunc implements fts5_isalnum(iCode) (fts5_expr.c fts5ExprIsAlnum):
// 1 when the codepoint's Unicode category is L*, N* or Co — the same set the
// unicode61 tokenizer treats as token characters. Registered with the other
// SQLITE_TEST-only fts5 expression helpers.
func IsAlnumFunc(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments to function fts5_isalnum")
	}
	r := rune(uint32(valueInt(args[0])))
	isAlnum := false
	for _, tbl := range []*unicode.RangeTable{unicode.L, unicode.N, unicode.Co} {
		if unicode.Is(tbl, r) {
			isAlnum = true
			break
		}
	}
	if isAlnum {
		return 1, nil
	}
	return 0, nil
}

// FoldFunc implements fts5_fold(iCode [, bRemoveDiacritics]) (fts5_expr.c
// fts5ExprFold): the unicode61 case/diacritic fold of one codepoint.
func FoldFunc(args []interface{}) (interface{}, error) {
	if len(args) != 1 && len(args) != 2 {
		return nil, fmt.Errorf("wrong number of arguments to function fts5_fold")
	}
	iCode := valueInt(args[0])
	bRemoveDiacritics := 0
	if len(args) == 2 {
		bRemoveDiacritics = valueInt(args[1])
	}
	return fts.Unicode61Fold(iCode, bRemoveDiacritics), nil
}

// ExprFunc implements fts5_expr() (bTcl=false) and fts5_expr_tcl()
// (bTcl=true). The trailing arguments go through the full config parse
// (C's azConfig -> sqlite3Fts5ConfigParse), so tokenize= directives apply and
// a malformed argument fails with C's "parse error in \"%s\"" — an empty
// argument fails too (fts5ConfigSkipBareword returns NULL for it,
// fts5_config.c:627). An empty parse (a zero-token phrase root) renders as "".
func ExprFunc(args []interface{}, bTcl bool) (interface{}, error) {
	name := "fts5_expr"
	if bTcl {
		name = "fts5_expr_tcl"
	}
	if len(args) < 1 {
		return nil, fmt.Errorf("wrong number of arguments to function %s", name)
	}
	iArg := 1
	nearsetCmd := "nearset"
	if bTcl && len(args) > 1 {
		nearsetCmd = valueText(args[1])
		iArg = 2
	}
	cfgArgs := make([]string, 0, len(args)-iArg)
	for ; iArg < len(args); iArg++ {
		s := valueText(args[iArg])
		if s == "" {
			return nil, fmt.Errorf("parse error in \"\"")
		}
		cfgArgs = append(cfgArgs, s)
	}
	t, err := syntheticExprTable(cfgArgs)
	if err != nil {
		return nil, err
	}
	node, _, err := parseQueryAll(t, valueText(args[0]))
	if err != nil {
		return nil, err
	}
	if _, eof := node.(eofNode); eof {
		// pRoot->xNext==0: a zero-instance root renders as the empty string.
		return "", nil
	}
	if bTcl {
		return printExprTcl(t, nearsetCmd, node), nil
	}
	return printExpr(t, node), nil
}

// syntheticExprTable builds the bare table the expression parses against
// (no index and no storage: only the column list, the tokenizer and the
// detail mode take part in parsing).
func syntheticExprTable(cfgArgs []string) (*Table, error) {
	cfg, err := ParseConfig("tbl", cfgArgs)
	if err != nil {
		return nil, err
	}
	tok, err := tableTokenizer(cfg)
	if err != nil {
		return nil, err
	}
	return &Table{cfg: cfg, tok: tok}, nil
}

// printExpr ports fts5ExprPrint.
func printExpr(t *Table, n queryNode) string {
	switch x := n.(type) {
	case eofNode:
		return `""`
	case stringNode:
		var sb strings.Builder
		printColset(&sb, t, x.phrases[0].colset)
		if len(x.phrases) > 1 {
			sb.WriteString("NEAR(")
		}
		for i, ph := range x.phrases {
			if i != 0 {
				sb.WriteString(" ")
			}
			for j, tm := range ph.terms {
				if j != 0 {
					sb.WriteString(" + ")
				}
				sb.WriteString(`"`)
				sb.WriteString(strings.ReplaceAll(tm.term, `"`, `""`))
				sb.WriteString(`"`)
				if tm.prefix {
					sb.WriteString(" *")
				}
			}
		}
		if len(x.phrases) > 1 {
			fmt.Fprintf(&sb, ", %d)", x.window)
		}
		return sb.String()
	default:
		op, kids := combinatorParts(n)
		var sb strings.Builder
		for i, c := range kids {
			sub := printExpr(t, c)
			bare := false
			switch c.(type) {
			case stringNode, eofNode:
				bare = true
			}
			if i != 0 {
				sb.WriteString(op)
			}
			if bare {
				sb.WriteString(sub)
			} else {
				sb.WriteString("(")
				sb.WriteString(sub)
				sb.WriteString(")")
			}
		}
		return sb.String()
	}
}

// printColset renders a node's column filter ("col : " or "{a b} : ").
func printColset(sb *strings.Builder, t *Table, colset []int) {
	if colset == nil {
		return
	}
	names := make([]string, len(colset))
	for i, c := range colset {
		name := ""
		if c >= 0 && c < len(t.cfg.Columns) {
			name = t.cfg.Columns[c]
		}
		names[i] = name
	}
	if len(names) > 1 {
		sb.WriteString("{")
	}
	sb.WriteString(strings.Join(names, " "))
	if len(names) > 1 {
		sb.WriteString("}")
	}
	sb.WriteString(" : ")
}

// printExprTcl ports fts5ExprPrintTcl.
func printExprTcl(t *Table, nearsetCmd string, n queryNode) string {
	switch x := n.(type) {
	case eofNode:
		return "{}"
	case stringNode:
		var sb strings.Builder
		sb.WriteString(nearsetCmd)
		sb.WriteString(" ")
		if cs := x.phrases[0].colset; cs != nil {
			if len(cs) == 1 {
				fmt.Fprintf(&sb, "-col %d ", cs[0])
			} else {
				fmt.Fprintf(&sb, "-col {%d", cs[0])
				for _, c := range cs[1:] {
					fmt.Fprintf(&sb, " %d", c)
				}
				sb.WriteString("} ")
			}
		}
		if len(x.phrases) > 1 {
			fmt.Fprintf(&sb, "-near %d ", x.window)
		}
		sb.WriteString("--")
		for _, ph := range x.phrases {
			sb.WriteString(" {")
			for j, tm := range ph.terms {
				if j != 0 {
					sb.WriteString(" ")
				}
				sb.WriteString(tm.term)
				if tm.prefix {
					sb.WriteString("*")
				}
			}
			sb.WriteString("}")
		}
		return sb.String()
	default:
		op, kids := combinatorParts(n)
		var sb strings.Builder
		sb.WriteString(strings.TrimSpace(op))
		for _, c := range kids {
			sb.WriteString(" [")
			sb.WriteString(printExprTcl(t, nearsetCmd, c))
			sb.WriteString("]")
		}
		return sb.String()
	}
}

// combinatorParts returns a node's operator word and children for the
// AND/OR/NOT nodes; other node kinds have no combinator rendering.
func combinatorParts(n queryNode) (string, []queryNode) {
	switch x := n.(type) {
	case andNode:
		return " AND ", x.children
	case orNode:
		return " OR ", x.children
	case notNode:
		return " NOT ", []queryNode{x.l, x.r}
	}
	return "", nil
}
