// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// tclExprVal is a value produced by the harness Tcl-expression evaluator.
// kind is 'i' (int), 'f' (float), or 's' (string).
type tclExprVal struct {
	kind byte
	i    int64
	f    float64
	s    string
}

func intVal(v int64) tclExprVal     { return tclExprVal{kind: 'i', i: v} }
func floatVal(v float64) tclExprVal { return tclExprVal{kind: 'f', f: v} }
func strVal(v string) tclExprVal    { return tclExprVal{kind: 's', s: v} }

// format renders the value the way Tcl's expr command does.
func (v tclExprVal) format() string {
	switch v.kind {
	case 'i':
		return strconv.FormatInt(v.i, 10)
	case 'f':
		s := strconv.FormatFloat(v.f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEnN") {
			s += ".0"
		}
		return s
	default:
		return v.s
	}
}

// numeric returns the value's numeric form, reporting whether it can be
// interpreted as a number (Tcl's "looks like a number" rule).
func (v tclExprVal) numeric() (float64, bool) {
	switch v.kind {
	case 'i':
		return float64(v.i), true
	case 'f':
		return v.f, true
	default:
		s := strings.TrimSpace(v.s)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
}

// integer returns the value as an int64 when it is integral.
func (v tclExprVal) integer() (int64, bool) {
	switch v.kind {
	case 'i':
		return v.i, true
	case 'f':
		if v.f == math.Trunc(v.f) && v.f >= math.MinInt64 && v.f <= math.MaxInt64 {
			return int64(v.f), true
		}
		return 0, false
	default:
		s := strings.TrimSpace(v.s)
		ival, err := strconv.ParseInt(s, 0, 64)
		if err == nil {
			return ival, true
		}
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr == nil && f == math.Trunc(f) && f >= math.MinInt64 && f <= math.MaxInt64 {
			return int64(f), true
		}
		return 0, false
	}
}

// truthy applies Tcl's boolean interpretation.
func (v tclExprVal) truthy() (bool, error) {
	if num, ok := v.numeric(); ok {
		return num != 0, nil
	}
	switch strings.ToLower(strings.TrimSpace(v.s)) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("expected boolean value but got %q", v.format())
}

// evalHarnessTclExpr evaluates the Tcl expression subset used by converted
// do_test bodies whose final command is `expr` over runtime-captured values
// (e.g. date.test:545-548). Supported: $var / $::var references resolved from
// vars, numbers, double-quoted and braced strings, boolean literals,
// parentheses, unary ! + -, arithmetic + - * / %, comparison == != < > <= >=
// plus the string forms eq/ne, logical && ||, and the ?: ternary. Unsupported
// syntax returns an error so the assertion fails loudly instead of silently
// passing.
func evalHarnessTclExpr(expr string, vars map[string]string) (string, error) {
	p := &tclExprParser{input: expr, vars: vars}
	v, err := p.parseTernary()
	if err != nil {
		return "", err
	}
	p.skipSpace()
	if p.pos != len(p.input) {
		return "", fmt.Errorf("unexpected trailing text %q", p.input[p.pos:])
	}
	return v.format(), nil
}

type tclExprParser struct {
	input string
	pos   int
	vars  map[string]string
}

func (p *tclExprParser) skipSpace() {
	for p.pos < len(p.input) && unicode.IsSpace(rune(p.input[p.pos])) {
		p.pos++
	}
}

// matchKeyword matches a bare keyword at the current position, ensuring a word
// boundary follows it.
func (p *tclExprParser) matchKeyword(word string) bool {
	p.skipSpace()
	end := p.pos + len(word)
	if end > len(p.input) || p.input[p.pos:end] != word {
		return false
	}
	if end < len(p.input) {
		c := rune(p.input[end])
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' {
			return false
		}
	}
	p.pos = end
	return true
}

func (p *tclExprParser) matchSymbol(sym string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.input[p.pos:], sym) {
		p.pos += len(sym)
		return true
	}
	return false
}

func (p *tclExprParser) parseTernary() (tclExprVal, error) {
	cond, err := p.parseOr()
	if err != nil {
		return tclExprVal{}, err
	}
	if !p.matchSymbol("?") {
		return cond, nil
	}
	whenTrue, err := p.parseTernary()
	if err != nil {
		return tclExprVal{}, err
	}
	if !p.matchSymbol(":") {
		return tclExprVal{}, fmt.Errorf("missing : in ternary expression")
	}
	whenFalse, err := p.parseTernary()
	if err != nil {
		return tclExprVal{}, err
	}
	truth, err := cond.truthy()
	if err != nil {
		return tclExprVal{}, err
	}
	if truth {
		return whenTrue, nil
	}
	return whenFalse, nil
}

func (p *tclExprParser) parseOr() (tclExprVal, error) {
	left, err := p.parseAnd()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.input[p.pos:], "||") {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseAnd()
		if err != nil {
			return tclExprVal{}, err
		}
		lt, err := left.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		rt, err := right.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		left = intVal(boolToInt(lt || rt))
	}
}

func (p *tclExprParser) parseAnd() (tclExprVal, error) {
	left, err := p.parseComparison()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.input[p.pos:], "&&") {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseComparison()
		if err != nil {
			return tclExprVal{}, err
		}
		lt, err := left.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		rt, err := right.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		left = intVal(boolToInt(lt && rt))
	}
}

// comparisonOps lists the comparison operators longest-first so prefixes like
// `<` do not shadow `<=`.
var comparisonOps = []string{"<=", ">=", "==", "!=", "<", ">", "eq", "ne"}

func (p *tclExprParser) parseComparison() (tclExprVal, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		op := ""
		for _, candidate := range comparisonOps {
			if len(candidate) == 2 && candidate[0] >= 'a' && candidate[0] <= 'z' {
				if p.matchKeyword(candidate) {
					op = candidate
					break
				}
				continue
			}
			if p.matchSymbol(candidate) {
				op = candidate
				break
			}
		}
		if op == "" {
			return left, nil
		}
		right, err := p.parseAdditive()
		if err != nil {
			return tclExprVal{}, err
		}
		truth, err := compareTclValues(left, right, op)
		if err != nil {
			return tclExprVal{}, err
		}
		left = intVal(boolToInt(truth))
	}
}

func (p *tclExprParser) parseAdditive() (tclExprVal, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		var op byte
		p.skipSpace()
		if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
			op = p.input[p.pos]
			p.pos++
		} else {
			return left, nil
		}
		right, err := p.parseMultiplicative()
		if err != nil {
			return tclExprVal{}, err
		}
		left, err = arithmeticTclValues(left, right, op)
		if err != nil {
			return tclExprVal{}, err
		}
	}
}

func (p *tclExprParser) parseMultiplicative() (tclExprVal, error) {
	left, err := p.parseUnary()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		var op byte
		p.skipSpace()
		if p.pos < len(p.input) && (p.input[p.pos] == '*' || p.input[p.pos] == '/' || p.input[p.pos] == '%') {
			op = p.input[p.pos]
			p.pos++
		} else {
			return left, nil
		}
		right, err := p.parseUnary()
		if err != nil {
			return tclExprVal{}, err
		}
		left, err = arithmeticTclValues(left, right, op)
		if err != nil {
			return tclExprVal{}, err
		}
	}
}

func (p *tclExprParser) parseUnary() (tclExprVal, error) {
	if p.matchSymbol("!") {
		v, err := p.parseUnary()
		if err != nil {
			return tclExprVal{}, err
		}
		truth, err := v.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		return intVal(boolToInt(!truth)), nil
	}
	if p.matchSymbol("-") {
		v, err := p.parseUnary()
		if err != nil {
			return tclExprVal{}, err
		}
		iv, ok := v.integer()
		if ok {
			return intVal(-iv), nil
		}
		fv, ok := v.numeric()
		if !ok {
			return tclExprVal{}, fmt.Errorf("can't negate %q", v.format())
		}
		return floatVal(-fv), nil
	}
	if p.matchSymbol("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *tclExprParser) parsePrimary() (tclExprVal, error) {
	p.skipSpace()
	if p.pos >= len(p.input) {
		return tclExprVal{}, fmt.Errorf("unexpected end of expression")
	}
	switch c := p.input[p.pos]; {
	case c == '(':
		p.pos++
		v, err := p.parseTernary()
		if err != nil {
			return tclExprVal{}, err
		}
		if !p.matchSymbol(")") {
			return tclExprVal{}, fmt.Errorf("missing closing parenthesis")
		}
		return v, nil
	case c == '"':
		return p.parseQuotedString()
	case c == '{':
		return p.parseBracedString()
	case c == '$':
		return p.parseVariable()
	case c == '[':
		return tclExprVal{}, fmt.Errorf("command substitution is not supported in runtime expressions")
	case unicode.IsDigit(rune(c)) || c == '.':
		return p.parseNumber()
	case unicode.IsLetter(rune(c)) || c == '_':
		return p.parseIdentifier()
	default:
		return tclExprVal{}, fmt.Errorf("unexpected character %q", string(c))
	}
}

func (p *tclExprParser) parseQuotedString() (tclExprVal, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if c == '\\' && p.pos+1 < len(p.input) {
			p.pos++
			b.WriteByte(tclBackslash(p.input[p.pos]))
			p.pos++
			continue
		}
		if c == '"' {
			p.pos++
			return strVal(b.String()), nil
		}
		b.WriteByte(c)
		p.pos++
	}
	return tclExprVal{}, fmt.Errorf("unterminated quoted string")
}

func (p *tclExprParser) parseBracedString() (tclExprVal, error) {
	p.pos++ // opening brace
	depth := 1
	var b strings.Builder
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		switch c {
		case '\\':
			if p.pos+1 < len(p.input) {
				b.WriteByte(c)
				b.WriteByte(p.input[p.pos+1])
				p.pos += 2
				continue
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				p.pos++
				return strVal(b.String()), nil
			}
		}
		b.WriteByte(c)
		p.pos++
	}
	return tclExprVal{}, fmt.Errorf("unterminated braced string")
}

func (p *tclExprParser) parseVariable() (tclExprVal, error) {
	p.pos++ // $
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' || c == ':' {
			p.pos++
			continue
		}
		if c == '(' {
			depth := 1
			end := p.pos + 1
			for end < len(p.input) && depth > 0 {
				if p.input[end] == '(' {
					depth++
				} else if p.input[end] == ')' {
					depth--
				}
				end++
			}
			p.pos = end
			continue
		}
		break
	}
	name := p.input[start:p.pos]
	if name == "" {
		return tclExprVal{}, fmt.Errorf("empty variable reference")
	}
	value, ok := p.vars[strings.TrimPrefix(name, "::")]
	if !ok {
		value, ok = p.vars[name]
	}
	if !ok {
		return tclExprVal{}, fmt.Errorf("runtime variable %q is not set", name)
	}
	return strVal(value), nil
}

func (p *tclExprParser) parseNumber() (tclExprVal, error) {
	start := p.pos
	sawDot := false
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if unicode.IsDigit(rune(c)) {
			p.pos++
			continue
		}
		if c == '.' && !sawDot {
			sawDot = true
			p.pos++
			continue
		}
		if (c == 'e' || c == 'E') && p.pos+1 < len(p.input) {
			p.pos++
			if p.input[p.pos] == '+' || p.input[p.pos] == '-' {
				p.pos++
			}
			continue
		}
		break
	}
	text := p.input[start:p.pos]
	if !sawDot {
		if iv, err := strconv.ParseInt(text, 0, 64); err == nil {
			return intVal(iv), nil
		}
	}
	fv, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return tclExprVal{}, fmt.Errorf("bad number %q", text)
	}
	return floatVal(fv), nil
}

func (p *tclExprParser) parseIdentifier() (tclExprVal, error) {
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' {
			p.pos++
			continue
		}
		break
	}
	word := p.input[start:p.pos]
	switch strings.ToLower(word) {
	case "true", "yes", "on":
		return intVal(1), nil
	case "false", "no", "off":
		return intVal(0), nil
	}
	if value, ok := p.vars[word]; ok {
		return strVal(value), nil
	}
	return tclExprVal{}, fmt.Errorf("unknown identifier %q", word)
}

// tclBackslash maps a backslash escape to its byte.
func tclBackslash(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	default:
		return c
	}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func TestCompareTclValuesMixedNumericAndStringFallsBackToString(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		op          string
		want        bool
	}{
		{"1", "", "==", false},
		{"1", "", "!=", true},
		{"10", "2x", "<", true},
	} {
		got, err := compareTclValues(strVal(tc.left), strVal(tc.right), tc.op)
		if err != nil || got != tc.want {
			t.Errorf("compare(%q %s %q) = %v, %v; want %v", tc.left, tc.op, tc.right, got, err, tc.want)
		}
	}
}

// compareTclValues implements Tcl comparison semantics for the evaluator.
// eq/ne always compare string representations; ==/!=/</>/<=/>= compare
// numerically when both operands look like numbers and as strings otherwise.
func compareTclValues(left, right tclExprVal, op string) (bool, error) {
	switch op {
	case "eq":
		return left.format() == right.format(), nil
	case "ne":
		return left.format() != right.format(), nil
	}
	lnum, lok := left.numeric()
	rnum, rok := right.numeric()
	if lok && rok {
		switch op {
		case "==":
			return lnum == rnum, nil
		case "!=":
			return lnum != rnum, nil
		case "<":
			return lnum < rnum, nil
		case ">":
			return lnum > rnum, nil
		case "<=":
			return lnum <= rnum, nil
		case ">=":
			return lnum >= rnum, nil
		}
	}
	if !lok || !rok {
		lstr, rstr := left.format(), right.format()
		switch op {
		case "==":
			return lstr == rstr, nil
		case "!=":
			return lstr != rstr, nil
		case "<":
			return lstr < rstr, nil
		case ">":
			return lstr > rstr, nil
		case "<=":
			return lstr <= rstr, nil
		case ">=":
			return lstr >= rstr, nil
		}
	}
	return false, fmt.Errorf("can't compare %q with %q using %s", left.format(), right.format(), op)
}

// arithmeticTclValues applies + - * / % with Tcl integer semantics when both
// operands are integral.
func arithmeticTclValues(left, right tclExprVal, op byte) (tclExprVal, error) {
	li, lok := left.integer()
	ri, rok := right.integer()
	if lok && rok {
		switch op {
		case '+':
			return intVal(li + ri), nil
		case '-':
			return intVal(li - ri), nil
		case '*':
			return intVal(li * ri), nil
		case '/':
			if ri == 0 {
				return tclExprVal{}, fmt.Errorf("divide by zero")
			}
			return intVal(floorDiv(li, ri)), nil
		case '%':
			if ri == 0 {
				return tclExprVal{}, fmt.Errorf("divide by zero")
			}
			return intVal(floorMod(li, ri)), nil
		}
	}
	lf, lok2 := left.numeric()
	rf, rok2 := right.numeric()
	if !lok2 || !rok2 {
		return tclExprVal{}, fmt.Errorf("can't use %q and %q with operator %s", left.format(), right.format(), string(op))
	}
	switch op {
	case '+':
		return floatVal(lf + rf), nil
	case '-':
		return floatVal(lf - rf), nil
	case '*':
		return floatVal(lf * rf), nil
	case '/':
		if rf == 0 {
			return tclExprVal{}, fmt.Errorf("divide by zero")
		}
		return floatVal(lf / rf), nil
	case '%':
		if rf == 0 {
			return tclExprVal{}, fmt.Errorf("divide by zero")
		}
		return floatVal(math.Mod(lf, rf)), nil
	}
	return tclExprVal{}, fmt.Errorf("unknown operator %q", string(op))
}

// floorDiv implements Tcl's integer division (results round toward negative
// infinity).
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// floorMod implements Tcl's modulo (result carries the divisor's sign).
func floorMod(a, b int64) int64 {
	m := a % b
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

// TestHarnessTclExprEvaluator pins the expression subset the harness must
// evaluate for converted runtime_expr assertions.
func TestHarnessTclExprEvaluator(t *testing.T) {
	vars := map[string]string{
		"date":  "2008-06-12 00:00:00",
		"date2": "2008-06-11 23:59:59",
		"x":     "5",
		"s":     "abc",
	}
	cases := []struct {
		expr string
		want string
	}{
		{`$date eq "2008-06-12 00:00:00" || $date eq "2008-06-11 23:59:59"`, "1"},
		{`$date2 eq "2008-06-12 00:00:00" || $date2 eq "2008-06-11 23:59:59"`, "1"},
		{`$s eq "2008-06-12 00:00:00" || $s eq "2008-06-11 23:59:59"`, "0"},
		{`$x < 10`, "1"},
		{`$x < 10 && $x > 0`, "1"},
		{`1 + 2`, "3"},
		{`7 / 2`, "3"},
		{`-7 / 2`, "-4"},
		{`7 % 3`, "1"},
		{`-7 % 3`, "2"},
		{`!($x == 5)`, "0"},
		{`$s eq "abc" ? 10 : 20`, "10"},
		{`1.0`, "1.0"},
		{`2.5 * 2`, "5.0"},
		{`"5" == 5`, "1"},
		{`{braced} eq "braced"`, "1"},
	}
	for _, c := range cases {
		got, err := evalHarnessTclExpr(c.expr, vars)
		if err != nil {
			t.Errorf("evalHarnessTclExpr(%q) error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("evalHarnessTclExpr(%q) = %q, want %q", c.expr, got, c.want)
		}
	}

	errorCases := []string{
		`$missing eq "x"`,
		`unknownIdent eq "x"`,
		`$x <`,
	}
	for _, expr := range errorCases {
		if got, err := evalHarnessTclExpr(expr, vars); err == nil {
			t.Errorf("evalHarnessTclExpr(%q) = %q, want error", expr, got)
		}
	}
}
