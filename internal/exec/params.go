// Parameter indexes are assigned exactly like SQLite's resolve.c
// (sqlite3ExprAssignVarNumber): bare "?" takes the next sequential index,
// "?NNN" takes index NNN (raising the total count when NNN exceeds it, and
// leaving any skipped indexes unnamed), and every occurrence of a named
// parameter (:name / @name / $name, case-insensitive, full token text
// including its punctuation) shares one index assigned at first appearance.
package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// MaxVariableNumber mirrors SQLITE_MAX_VARIABLE_NUMBER's post-3.32 default.
// The transpiler emits the same constant for the TCL harness, so prepare-time
// rejection of ?NNN above this limit matches the generated tests.
const MaxVariableNumber = 32766

// paramAssigner accumulates parameter slots while scanning a statement.
type paramAssigner struct {
	names []string       // slot i holds parameter i+1's token ("" unnamed)
	named map[string]int // case-folded token text -> assigned index
	next  int            // highest index assigned so far
}

func (p *paramAssigner) grow(n int) {
	for len(p.names) < n {
		p.names = append(p.names, "")
	}
}

// assignBare gives the next sequential index to an anonymous "?".
func (p *paramAssigner) assignBare() {
	p.next++
	p.grow(p.next)
}

// assignNumbered binds a numbered variable token ("?4" or ":123", num being
// its digit run) to slot N, raising the total when N exceeds it; duplicates
// reuse the existing slot. Out-of-range N is a prepare error with resolve.c's
// message text.
func (p *paramAssigner) assignNumbered(token, num string) error {
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > MaxVariableNumber {
		return fmt.Errorf("variable number must be between ?1 and ?%d", MaxVariableNumber)
	}
	p.grow(n)
	if n > p.next {
		p.next = n
	}
	if p.names[n-1] == "" {
		p.names[n-1] = token // keeps its original token text as the name
	}
	return nil
}

// assignNamed dedups named parameters on case-folded full token text (:abc and
// @abc are distinct variables, matching SQLite's token-text key).
func (p *paramAssigner) assignNamed(token string) {
	key := strings.ToLower(token)
	if _, ok := p.named[key]; ok {
		return
	}
	if p.named == nil {
		p.named = make(map[string]int)
	}
	p.next++
	p.grow(p.next)
	p.named[key] = p.next
	p.names[p.next-1] = token
}

// isDigitByte reports whether c is an ASCII digit.
func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// CollectParameterNames scans SQL text with the real SQL tokenizer (so string
// literals, comments, identifiers and TCL-style array variables never yield
// false parameters) and assigns parameter indexes per resolve.c. It returns
// one entry per parameter slot (index i holds the name of parameter i+1, ""
// when unnamed) plus the assignment error for ?0 / ?NNN above
// MaxVariableNumber.
//
// Tokenizer errors are deliberately ignored here: malformed SQL is reported by
// the parser with its own diagnostic; this pass only runs on successfully
// parsed statements.
func CollectParameterNames(sqlText string) ([]string, error) {
	t := sql.NewTokenizer(sqlText)
	p := &paramAssigner{}
	for {
		tok := t.Next()
		switch tok.Type {
		case sql.TokenEOF, sql.TokenError:
			// Malformed input: the parser owns that diagnostic.
			return p.names, nil
		case sql.TokenParam:
		default:
			continue
		}
		var err error
		// resolve.c: ':NNN' with an all-digit name is a NUMBERED variable,
		// exactly like '?NNN' (sqlite3ExprAssignVarNumber).
		numbered := tok.Value[0] == '?' && len(tok.Value) > 1 ||
			tok.Value[0] == ':' && len(tok.Value) > 1 && isDigitByte(tok.Value[1])
		switch {
		case numbered:
			err = p.assignNumbered(tok.Value, strings.TrimLeft(tok.Value[1:], ":?"))
		case tok.Value[0] == '?':
			p.assignBare()
		default:
			p.assignNamed(tok.Value)
		}
		if err != nil {
			return nil, err
		}
	}
}
