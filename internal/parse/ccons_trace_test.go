package parse

import (
	"fmt"
	"testing"

	sql "github.com/pijalu/frigolite/internal/sql"
)

func TestCconsTrace(t *testing.T) {
	cases := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY);",
		"CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT);",
		"CREATE TABLE t1(a INTEGER PRIMARY KEY DESC);",
		"CREATE TABLE t1(a INTEGER NOT NULL);",
		"CREATE TABLE t1(a INTEGER DEFAULT 5);",
		"CREATE TABLE t1(a INTEGER DEFAULT 'x');",
		"CREATE TABLE t1(a INTEGER DEFAULT -5);",
		"CREATE TABLE t1(a INTEGER CHECK(a>0));",
		"CREATE TABLE t1(a INTEGER REFERENCES t2(id));",
		"CREATE TABLE t1(a TEXT COLLATE NOCASE);",
		"CREATE TABLE t1(a INTEGER UNIQUE);",
		"CREATE TABLE t1(a INTEGER CONSTRAINT foo NOT NULL);",
	}
	for _, stmt := range cases {
		p := NewParser(GetParseTables())
		fired := map[int]int{}
		p.OnReduce(func(ruleNo int, parser *Parser, lookahead int, lookaheadToken interface{}) {
			t2 := parser.tables
			size := -t2.RuleInfoNRhs[ruleNo]
			result := handleRule(ruleNo, parser, lookahead, lookaheadToken)
			if result == nil && size > 0 {
				result = getRHS(parser, ruleNo, 1)
			}
			lhsSlot := parser.pos
			if size > 0 {
				lhsSlot = parser.pos - size + 1
			}
			parser.stack[lhsSlot].Minor = result
			fired[ruleNo] = size
		})
		tok := sql.NewTokenizer(stmt)
		for {
			tk := tok.Next()
			if tk.Type == 0 {
				p.Parse(0, nil)
				break
			}
			if p.Parse(tokenCode(int(tk.Type), tk.Value), tk) != ParseAccept {
				break
			}
		}
		rules := []int{}
		for r := range fired {
			if r >= 21 && r <= 74 {
				rules = append(rules, r)
			}
		}
		fmt.Printf("%-58s rules=%v\n", stmt, rules)
	}
}
