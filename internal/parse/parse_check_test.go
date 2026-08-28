package parse

import (
	"fmt"
	"os"
	"testing"

	sql "github.com/pijalu/frigolite/internal/sql"
)

// TestParseCheck is a manual debug helper: parses the statement given in
// FRIGOLITE_PARSE_STMT and prints the resulting AST. Not part of the suite.
func TestParseCheck(t *testing.T) {
	stmt := os.Getenv("FRIGOLITE_PARSE_STMT")
	if stmt == "" {
		t.Skip("FRIGOLITE_PARSE_STMT not set")
	}
	p := NewParser(GetParseTables())
	p.SetTrace(os.Getenv("FRIGOLITE_TRACE") == "1")
	p.OnReduce(func(ruleNo int, parser *Parser, lookahead int, lookaheadToken interface{}) {
		t2 := parser.tables
		size := -t2.RuleInfoNRhs[ruleNo]
		result := handleRule(ruleNo, parser, lookahead, lookaheadToken)
		lhsSlot := parser.pos
		if size > 0 {
			lhsSlot = parser.pos - size + 1
		}
		parser.stack[lhsSlot].Minor = result
	})
	tok := sql.NewTokenizer(stmt)
	var res ParseResult = ParseAccept
	for {
		tk := tok.Next()
		if tk.Type == 0 {
			res = p.Parse(0, nil)
			break
		}
		res = p.Parse(tokenCode(int(tk.Type), tk.Value), tk)
		if res != ParseAccept {
			break
		}
	}
	if res != ParseAccept {
		t.Fatalf("PARSE FAIL: %v", res)
	}
	fmt.Printf("PARSE OK: %#v\n", p.stack[p.pos].Minor)
}
