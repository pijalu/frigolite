package parse

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sql "github.com/pijalu/frigolite/internal/sql"
)

// TestRuleInventory extracts SQL statements from all testgen suites, parses
// each with the LALR parser, and reports:
//   - which grammar rules fired
//   - which fired rules had no explicit handler (fell through to passthrough)
//   - which rules never fired (unreachable from this corpus)
//
// Output is written to /tmp/rule_inventory.txt for the committed inventory doc.
// FRIGOLITE_INVENTORY=1 enables the file write.
func TestRuleInventory(t *testing.T) {
	tables := GetParseTables()
	fired := map[int]int{}
	passthrough := map[int]map[string]bool{} // rule -> sample statement

	pkgs, err := filepath.Glob("../../testgen/*")
	if err != nil {
		t.Fatal(err)
	}
	var sqlStmts []string
	seen := map[string]bool{}
	for _, dir := range pkgs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		for _, f := range files {
			for _, s := range extractSQLStatements(f) {
				if !seen[s] {
					seen[s] = true
					sqlStmts = append(sqlStmts, s)
				}
			}
		}
	}

	for _, stmt := range sqlStmts {
		p := NewParser(tables)
		p.OnReduce(func(ruleNo int, parser *Parser, lookahead int, lookaheadToken interface{}) {
			t2 := parser.tables
			size := -t2.RuleInfoNRhs[ruleNo]
			result := handleRule(ruleNo, parser, lookahead, lookaheadToken)
			if result == nil && size > 0 {
				if passthrough[ruleNo] == nil {
					passthrough[ruleNo] = map[string]bool{}
				}
				passthrough[ruleNo][strings.TrimSpace(stmt)] = true
			}
			fired[ruleNo]++
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
			code := tokenCode(int(tk.Type), tk.Value)
			if code < 0 {
				res = ParseError
				break
			}
			res = p.Parse(code, tk)
			if res != ParseAccept {
				break
			}
		}
	}

	// Report
	var b strings.Builder
	b.WriteString(fmt.Sprintf("total rules: %d, fired: %d, unfired: %d\n", tables.YYNRule, len(fired), tables.YYNRule-len(fired)))
	b.WriteString(fmt.Sprintf("statements parsed: %d\n\n", len(sqlStmts)))

	// Passthrough rules (no explicit handler, fired, non-empty RHS)
	var ptRules []int
	for r := range passthrough {
		ptRules = append(ptRules, r)
	}
	sort.Ints(ptRules)
	b.WriteString(fmt.Sprintf("=== PASSTHROUGH RULES (fired, no handler, non-empty RHS): %d ===\n", len(ptRules)))
	for _, r := range ptRules {
		size := -tables.RuleInfoNRhs[r]
		var samples []string
		for s := range passthrough[r] {
			if len(samples) < 3 {
				samples = append(samples, s)
			}
		}
		b.WriteString(fmt.Sprintf("rule %3d (nrhs=%3d) lhsSym=%d samples: %v\n", r, size, tables.RuleInfoLhs[r], samples))
	}

	// Unfired rules
	var unfired []int
	for r := 0; r < tables.YYNRule; r++ {
		if fired[r] == 0 {
			unfired = append(unfired, r)
		}
	}
	b.WriteString(fmt.Sprintf("\n=== UNFIRED RULES: %d ===\n", len(unfired)))
	for _, r := range unfired {
		size := -tables.RuleInfoNRhs[r]
		b.WriteString(fmt.Sprintf("rule %3d (nrhs=%3d) lhsSym=%d\n", r, size, tables.RuleInfoLhs[r]))
	}

	if os.Getenv("FRIGOLITE_INVENTORY") == "1" {
		os.WriteFile("/tmp/rule_inventory.txt", []byte(b.String()), 0644)
	}
	t.Logf("\n%s", b.String())
}
