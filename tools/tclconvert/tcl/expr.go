// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// EvalExpr evaluates a TCL expression string.
// Supports: arithmetic (+,-,*,/,%), comparison (<,>,<=,>=,==,!=),
// logical (&&,||,!), string comparison (eq, ne), parentheses,
// functions (abs, double, int, etc.), and variable references.
// Variable references ($var) must already be substituted before calling.
func EvalExpr(expr string, interp *Interp, localVars map[string]string) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", nil
	}
	// Substitute TCL variables ($var) and commands ([cmd]) before evaluating
	if interp != nil {
		expr = interp.substitute(expr, localVars)
	}
	p := &exprParser{input: expr, pos: 0}
	result, err := p.parseExpr()
	if err != nil {
		return "", err
	}
	return formatExprResult(result), nil
}

type exprParser struct {
	input string
	pos   int
}

//lint:ignore U1000 reserved TCL expr-parser API
func (p *exprParser) peek() byte {
	p.skipSpaces()
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

// skipSpaces advances past whitespace.
func (p *exprParser) skipSpaces() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t' || p.input[p.pos] == '\n' || p.input[p.pos] == '\r') {
		p.pos++
	}
}

// match tries to match a literal string at the current position.
// Returns true and advances if matched.
func (p *exprParser) match(s string) bool {
	p.skipSpaces()
	if p.pos+len(s) <= len(p.input) && p.input[p.pos:p.pos+len(s)] == s {
		p.pos += len(s)
		return true
	}
	return false
}

// matchOne matches a single character.
func (p *exprParser) matchOne(c byte) bool {
	p.skipSpaces()
	if p.pos < len(p.input) && p.input[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

// parseExpr is the entry point (handles ternary).
func (p *exprParser) parseExpr() (interface{}, error) {
	cond, err := p.parseLogicalOr()
	if err != nil {
		return nil, err
	}
	p.skipSpaces()
	if p.matchOne('?') {
		trueVal, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if !p.matchOne(':') {
			return nil, fmt.Errorf("expected ':' in ternary")
		}
		falseVal, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if toBool(cond) {
			return trueVal, nil
		}
		return falseVal, nil
	}
	return cond, nil
}

func (p *exprParser) parseLogicalOr() (interface{}, error) {
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		if p.match("||") {
			right, err := p.parseLogicalAnd()
			if err != nil {
				return nil, err
			}
			left = boolVal(toBool(left) || toBool(right))
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseLogicalAnd() (interface{}, error) {
	left, err := p.parseBitOr()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		if p.match("&&") {
			right, err := p.parseBitOr()
			if err != nil {
				return nil, err
			}
			left = boolVal(toBool(left) && toBool(right))
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseBitOr() (interface{}, error) {
	left, err := p.parseBitXor()
	if err != nil {
		return nil, err
	}
	for p.matchOne('|') {
		right, err := p.parseBitXor()
		if err != nil {
			return nil, err
		}
		left = float64(toInt(left) | toInt(right))
	}
	return left, nil
}

func (p *exprParser) parseBitXor() (interface{}, error) {
	left, err := p.parseBitAnd()
	if err != nil {
		return nil, err
	}
	for p.matchOne('^') {
		right, err := p.parseBitAnd()
		if err != nil {
			return nil, err
		}
		left = float64(toInt(left) ^ toInt(right))
	}
	return left, nil
}

func (p *exprParser) parseBitAnd() (interface{}, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for p.matchOne('&') {
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		left = float64(toInt(left) & toInt(right))
	}
	return left, nil
}

func (p *exprParser) parseEquality() (interface{}, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		switch {
		case p.match("=="):
			left, err = p.binaryOperand(left, p.parseComparison, func(l, r interface{}) interface{} {
				return boolVal(compareValues(l, r) == 0)
			})
		case p.match("!="):
			left, err = p.binaryOperand(left, p.parseComparison, func(l, r interface{}) interface{} {
				return boolVal(compareValues(l, r) != 0)
			})
		case p.matchWord("eq"):
			left, err = p.binaryOperand(left, p.parseComparison, func(l, r interface{}) interface{} {
				return boolVal(toString(l) == toString(r))
			})
		case p.matchWord("ne"):
			left, err = p.binaryOperand(left, p.parseComparison, func(l, r interface{}) interface{} {
				return boolVal(toString(l) != toString(r))
			})
		default:
			return left, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (p *exprParser) parseComparison() (interface{}, error) {
	left, err := p.parseShift()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		switch {
		case p.match("<="):
			left, err = p.binaryOperand(left, p.parseShift, func(l, r interface{}) interface{} {
				return boolVal(compareValues(l, r) <= 0)
			})
		case p.match(">="):
			left, err = p.binaryOperand(left, p.parseShift, func(l, r interface{}) interface{} {
				return boolVal(compareValues(l, r) >= 0)
			})
		case p.match("<"):
			left, err = p.binaryOperand(left, p.parseShift, func(l, r interface{}) interface{} {
				return boolVal(compareValues(l, r) < 0)
			})
		case p.match(">"):
			left, err = p.binaryOperand(left, p.parseShift, func(l, r interface{}) interface{} {
				return boolVal(compareValues(l, r) > 0)
			})
		default:
			return left, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (p *exprParser) parseShift() (interface{}, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		if p.match("<<") {
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = float64(toInt(left) << toInt(right))
		} else if p.match(">>") {
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = float64(toInt(left) >> toInt(right))
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseAdditive() (interface{}, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		if p.matchOne('+') {
			right, err := p.parseMultiplicative()
			if err != nil {
				return nil, err
			}
			left = toFloat(left) + toFloat(right)
		} else if p.matchOne('-') {
			right, err := p.parseMultiplicative()
			if err != nil {
				return nil, err
			}
			left = toFloat(left) - toFloat(right)
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseMultiplicative() (interface{}, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		switch {
		case p.matchOne('*'):
			left, err = p.binaryOperand(left, p.parseUnary, func(l, r interface{}) interface{} {
				return toFloat(l) * toFloat(r)
			})
		case p.matchOne('/'):
			left, err = p.binaryOperand(left, p.parseUnary, func(l, r interface{}) interface{} {
				rv := toFloat(r)
				if rv == 0 {
					return float64(0)
				}
				return toFloat(l) / rv
			})
		case p.matchOne('%'):
			left, err = p.binaryOperand(left, p.parseUnary, func(l, r interface{}) interface{} {
				rv := toInt(r)
				if rv == 0 {
					return float64(0)
				}
				return float64(toInt(l) % rv)
			})
		default:
			return left, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// binaryOperand parses the right operand of a binary operation using next and
// combines it with left via combine. It is the shared body of the binary
// operator parse loops.
func (p *exprParser) binaryOperand(left interface{}, next func() (interface{}, error), combine func(l, r interface{}) interface{}) (interface{}, error) {
	right, err := next()
	if err != nil {
		return nil, err
	}
	return combine(left, right), nil
}

func (p *exprParser) parseUnary() (interface{}, error) {
	p.skipSpaces()
	if p.matchOne('-') {
		val, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return -toFloat(val), nil
	}
	if p.matchOne('+') {
		return p.parseUnary()
	}
	if p.matchOne('!') {
		val, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return boolVal(!toBool(val)), nil
	}
	if p.matchOne('~') {
		val, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return float64(^toInt(val)), nil
	}
	return p.parsePrimary()
}

func (p *exprParser) parsePrimary() (interface{}, error) {
	p.skipSpaces()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("unexpected end of expression")
	}

	ch := p.input[p.pos]

	// Parenthesized expression
	if ch == '(' {
		return p.parseParen()
	}

	// Number
	if unicode.IsDigit(rune(ch)) || (ch == '.' && p.pos+1 < len(p.input) && unicode.IsDigit(rune(p.input[p.pos+1]))) {
		return p.parseNumber()
	}

	// String literal in quotes
	if ch == '"' {
		return p.parseStringLit()
	}

	// Identifier — could be function call or bare word (string)
	if unicode.IsLetter(rune(ch)) || ch == '_' {
		return p.parseIdent()
	}

	return nil, fmt.Errorf("unexpected character '%c' at position %d", ch, p.pos)
}

// parseParen parses a parenthesized expression at the current '(' position.
func (p *exprParser) parseParen() (interface{}, error) {
	p.pos++ // consume (
	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipSpaces()
	if !p.matchOne(')') {
		return nil, fmt.Errorf("expected ')' at position %d", p.pos)
	}
	return val, nil
}

// parseStringLit parses a " ... " string literal.
func (p *exprParser) parseStringLit() (interface{}, error) {
	p.pos++
	start := p.pos
	for p.pos < len(p.input) && p.input[p.pos] != '"' {
		p.pos++
	}
	s := p.input[start:p.pos]
	if p.pos < len(p.input) {
		p.pos++ // skip closing "
	}
	return s, nil
}

// parseIdent parses an identifier: either a function call or a bare word.
func (p *exprParser) parseIdent() (interface{}, error) {
	start := p.pos
	for p.pos < len(p.input) && (unicode.IsLetter(rune(p.input[p.pos])) || unicode.IsDigit(rune(p.input[p.pos])) || p.input[p.pos] == '_') {
		p.pos++
	}
	name := p.input[start:p.pos]
	p.skipSpaces()

	// Function call
	if p.pos < len(p.input) && p.input[p.pos] == '(' {
		args, err := p.parseFuncArgs()
		if err != nil {
			return nil, err
		}
		return evalFunc(name, args)
	}

	// Bare word — treat as string
	return name, nil
}

// parseFuncArgs parses a parenthesized function argument list, consuming the
// closing ')'.
func (p *exprParser) parseFuncArgs() ([]interface{}, error) {
	p.pos++ // consume (
	var args []interface{}
	p.skipSpaces()
	if p.pos < len(p.input) && p.input[p.pos] != ')' {
		for {
			arg, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			p.skipSpaces()
			if !p.matchOne(',') {
				break
			}
		}
	}
	p.matchOne(')') // consume )
	return args, nil
}

func (p *exprParser) parseNumber() (interface{}, error) {
	start := p.pos
	isFloat := false
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if c >= '0' && c <= '9' {
			p.pos++
		} else if c == '.' {
			isFloat = true
			p.pos++
		} else if c == 'e' || c == 'E' {
			isFloat = true
			p.pos++
			if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
				p.pos++
			}
		} else if c == 'x' || c == 'X' {
			return p.parseHexNumber(start)
		} else {
			break
		}
	}
	return parseDecimalNumber(p.input[start:p.pos], isFloat), nil
}

// parseHexNumber parses a 0x... hexadecimal literal, returning a float value.
func (p *exprParser) parseHexNumber(start int) (interface{}, error) {
	p.pos++
	for p.pos < len(p.input) {
		hc := p.input[p.pos]
		if (hc >= '0' && hc <= '9') || (hc >= 'a' && hc <= 'f') || (hc >= 'A' && hc <= 'F') {
			p.pos++
		} else {
			break
		}
	}
	n, err := strconv.ParseInt(p.input[start+2:p.pos], 16, 64)
	if err == nil {
		return float64(n), nil
	}
	return float64(0), nil
}

// parseDecimalNumber parses a decimal number string as float (or int when
// integral), returning the raw string when parsing fails.
func parseDecimalNumber(numStr string, isFloat bool) interface{} {
	if isFloat {
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return numStr
		}
		return f
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		f, err2 := strconv.ParseFloat(numStr, 64)
		if err2 != nil {
			return numStr
		}
		return f
	}
	return float64(n)
}

// matchWord matches a whole word (followed by whitespace or operator).
func (p *exprParser) matchWord(w string) bool {
	p.skipSpaces()
	if p.pos+len(w) > len(p.input) {
		return false
	}
	if p.input[p.pos:p.pos+len(w)] != w {
		return false
	}
	// Check that the next char is not alphanumeric (word boundary)
	end := p.pos + len(w)
	if end < len(p.input) && (unicode.IsLetter(rune(p.input[end])) || unicode.IsDigit(rune(p.input[end])) || p.input[end] == '_') {
		return false
	}
	p.pos += len(w)
	return true
}

// --- Type conversion helpers ---

func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func toInt(v interface{}) int64 {
	return int64(toFloat(v))
}

func toBool(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case int64:
		return x != 0
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return x != ""
		}
		return f != 0
	}
	return false
}

func toString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "1"
		}
		return "0"
	}
	return fmt.Sprintf("%v", v)
}

func boolVal(b bool) interface{} {
	if b {
		return float64(1)
	}
	return float64(0)
}

func compareValues(a, b interface{}) int {
	// Try numeric comparison first
	af, aErr := strconv.ParseFloat(toString(a), 64)
	bf, bErr := strconv.ParseFloat(toString(b), 64)
	if aErr == nil && bErr == nil {
		if af < bf {
			return -1
		}
		if af > bf {
			return 1
		}
		return 0
	}
	// String comparison
	sa, sb := toString(a), toString(b)
	return strings.Compare(sa, sb)
}

// exprFunc executes a TCL expr function with its parsed arguments.
type exprFunc func(args []interface{}) (interface{}, error)

// exprFuncs maps TCL expr function names to implementations.
var exprFuncs = map[string]exprFunc{
	"abs":    unaryExprFunc(func(v float64) float64 { return math.Abs(v) }),
	"double": unaryExprFunc(func(v float64) float64 { return v }),
	"int":    unaryExprFunc(func(v float64) float64 { return float64(toInt(v)) }),
	"round":  unaryExprFunc(func(v float64) float64 { return math.Round(v) }),
	"floor":  unaryExprFunc(func(v float64) float64 { return math.Floor(v) }),
	"ceil":   unaryExprFunc(func(v float64) float64 { return math.Ceil(v) }),
	"sqrt":   unaryExprFunc(func(v float64) float64 { return math.Sqrt(v) }),
	"log":    unaryExprFunc(func(v float64) float64 { return math.Log(v) }),
	"exp":    unaryExprFunc(func(v float64) float64 { return math.Exp(v) }),
	"wide":   unaryExprFunc(func(v float64) float64 { return float64(toInt(v)) }),
	"pow":    binaryExprFunc(func(a, b float64) float64 { return math.Pow(a, b) }),
	"min":    binaryExprFunc(func(a, b float64) float64 { return math.Min(a, b) }),
	"max":    binaryExprFunc(func(a, b float64) float64 { return math.Max(a, b) }),
	"hypot":  binaryExprFunc(func(a, b float64) float64 { return math.Hypot(a, b) }),
	"rand": func(args []interface{}) (interface{}, error) {
		return math.Float64frombits(uint64(12345)), nil // deterministic pseudo-random
	},
}

// unaryExprFunc wraps a float-to-float function as a 1-argument expr func.
func unaryExprFunc(f func(float64) float64) exprFunc {
	return func(args []interface{}) (interface{}, error) {
		if len(args) < 1 {
			return nil, nil
		}
		return f(toFloat(args[0])), nil
	}
}

// binaryExprFunc wraps a two-float function as a 2-argument expr func.
func binaryExprFunc(f func(a, b float64) float64) exprFunc {
	return func(args []interface{}) (interface{}, error) {
		if len(args) < 2 {
			return nil, nil
		}
		return f(toFloat(args[0]), toFloat(args[1])), nil
	}
}

func evalFunc(name string, args []interface{}) (interface{}, error) {
	if fn, ok := exprFuncs[name]; ok {
		return fn(args)
	}
	return nil, fmt.Errorf("unknown function: %s", name)
}

// formatExprResult converts a Go value to a TCL string representation.
func formatExprResult(v interface{}) string {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case string:
		return x
	default:
		return fmt.Sprintf("%v", v)
	}
}
