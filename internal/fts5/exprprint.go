package fts5

import (
	"fmt"
	"strings"
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

// ExprFunc implements fts5_expr() (bTcl=false) and fts5_expr_tcl()
// (bTcl=true). An empty parse (a zero-token phrase root) renders as "".
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
	cols := make([]string, 0, len(args)-iArg)
	for ; iArg < len(args); iArg++ {
		cols = append(cols, valueText(args[iArg]))
	}
	t, err := syntheticExprTable(cols)
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
// (no index and no storage: only the column list, the default tokenizer and
// detail=full take part in parsing).
func syntheticExprTable(cols []string) (*Table, error) {
	tok, err := NewTokenizer([]string{"unicode61"})
	if err != nil {
		return nil, err
	}
	return &Table{
		cfg: &Config{Name: "tbl", Columns: cols, Detail: DetailFull},
		tok: tok,
	}, nil
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
