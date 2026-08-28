// Package function — arbitrary-precision decimal arithmetic.
//
// Port of SQLite ext/misc/decimal.c (2020-06-22). A Decimal holds a sign, a
// most-significant-first digit array, and the number of digits to the right
// of the decimal point (scale): value == (-1)^sign * digits * 10^-nFrac.
package function

import (
	"fmt"
	"math"
	"strings"
)

// maxDecimalDigits mirrors SQLITE_DECIMAL_MAX_DIGIT.
const maxDecimalDigits = 10000000

// decimalVal is an arbitrary-precision decimal number.
type decimalVal struct {
	sign   bool   // true = negative
	digits []byte // most significant first
	nFrac  int    // digits to the right of the decimal point
}

// decimalNewFromText parses zIn into a Decimal (decimal.c decimalNewFromText).
// Returns nil on parse failure (OOM / too many digits). Non-numeric input
// yields a Decimal with zero digits (isNull-like; decimal_result renders "0").
func decimalNewFromText(zIn string) *decimalVal {
	p := &decimalVal{}
	iExp := decimalParseText(zIn, p)
	if p.nFrac > 0 {
		p.nFrac = len(p.digits) - (p.nFrac - 1)
	}
	decimalApplyExponent(p, iExp)
	if p.sign {
		allZero := true
		for _, d := range p.digits {
			if d != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			p.sign = false
		}
	}
	if len(p.digits) > maxDecimalDigits {
		return nil
	}
	return p
}

// decimalParseText scans the text into p's digits and returns the parsed
// exponent (0 if none). Sign, leading zeros, digits, decimal point, and the
// optional e/E exponent are handled (decimal.c decimalNewFromText scan loop).
func decimalParseText(zIn string, p *decimalVal) int {
	iExp := 0
	i := 0
	n := len(zIn)
	for i < n && isASCIISpace(zIn[i]) {
		i++
	}
	if i < n && zIn[i] == '-' {
		p.sign = true
		i++
	} else if i < n && zIn[i] == '+' {
		i++
	}
	for i < n && zIn[i] == '0' {
		i++
	}
	for i < n {
		c := zIn[i]
		switch {
		case c >= '0' && c <= '9':
			p.digits = append(p.digits, c-'0')
		case c == '.':
			p.nFrac = len(p.digits) + 1
		case c == 'e' || c == 'E':
			return decimalParseExponent(zIn, i+1, n, iExp)
		}
		i++
	}
	return 0
}

// decimalParseExponent parses the optional e/E exponent starting at zIn[j],
// returning the signed exponent value (decimal.c decimalNewFromText).
func decimalParseExponent(zIn string, j, n, iExp int) int {
	neg := false
	if j >= n {
		return 0
	}
	if zIn[j] == '-' {
		neg = true
		j++
	} else if zIn[j] == '+' {
		j++
	}
	for j < n && iExp < 1000000 {
		if zIn[j] >= '0' && zIn[j] <= '9' {
			iExp = iExp*10 + int(zIn[j]-'0')
		}
		j++
	}
	if neg {
		iExp = -iExp
	}
	return iExp
}

// decimalApplyExponent adjusts the digit array and scale for a parsed
// exponent (decimal.c decimalNewFromText's iExp>0 / iExp<0 blocks).
func decimalApplyExponent(p *decimalVal, iExp int) {
	if iExp > 0 {
		if p.nFrac > 0 {
			if iExp <= p.nFrac {
				p.nFrac -= iExp
				iExp = 0
			} else {
				iExp -= p.nFrac
				p.nFrac = 0
			}
		}
		if iExp > 0 {
			p.digits = append(p.digits, make([]byte, iExp)...)
		}
		return
	}
	if iExp < 0 {
		iExp = -iExp
		nExtra := len(p.digits) - p.nFrac - 1
		if nExtra > 0 {
			if nExtra >= iExp {
				p.nFrac += iExp
				iExp = 0
			} else {
				iExp -= nExtra
				p.nFrac = len(p.digits) - 1
			}
		}
		if iExp > 0 {
			p.digits = append(make([]byte, iExp), p.digits...)
			p.nFrac += iExp
		}
	}
}

// decimalNewFromInt builds a Decimal from an integer value.
func decimalNewFromInt(v int64) *decimalVal {
	sign := v < 0
	if sign {
		v = -v
	}
	s := fmt.Sprintf("%d", v)
	p := &decimalVal{sign: sign}
	for i := 0; i < len(s); i++ {
		p.digits = append(p.digits, s[i]-'0')
	}
	return p
}

// decimalIsNull reports whether the decimal is nil (the result of NULL input).
func (p *decimalVal) decimalIsNull() bool {
	return p == nil
}

// decimalResult formats the Decimal back into text (decimal.c decimal_result).
func decimalResult(p *decimalVal) string {
	if p == nil {
		return ""
	}
	if len(p.digits) == 0 || (len(p.digits) == 1 && p.digits[0] == 0) {
		p.sign = false
	}
	var b strings.Builder
	if p.sign {
		b.WriteByte('-')
	}
	n := len(p.digits) - p.nFrac
	if n <= 0 {
		b.WriteByte('0')
	}
	j := 0
	for n > 1 && p.digits[j] == 0 {
		j++
		n--
	}
	for n > 0 {
		b.WriteByte(p.digits[j] + '0')
		j++
		n--
	}
	if p.nFrac > 0 {
		b.WriteByte('.')
		for j < len(p.digits) {
			b.WriteByte(p.digits[j] + '0')
			j++
		}
	}
	return b.String()
}

// decimalRound rounds p to N significant digits (decimal.c decimal_round).
// N must be positive.
func decimalRound(p *decimalVal, N int) {
	if N < 1 || p == nil || len(p.digits) <= N {
		return
	}
	nZero := 0
	for nZero < len(p.digits) && p.digits[nZero] == 0 {
		nZero++
	}
	N += nZero
	if len(p.digits) <= N {
		return
	}
	if p.digits[N] >= 5 {
		// If all leading digits are 9, expand by adding a new 0 at the front.
		allNine := true
		for i := 0; i < N; i++ {
			if p.digits[i] != 9 {
				allNine = false
				break
			}
		}
		if allNine {
			p.digits = append([]byte{0}, p.digits...)
		}
		p.digits[N-1]++
		for i := N - 1; i > 0 && p.digits[i] > 9; i-- {
			p.digits[i] = 0
			p.digits[i-1]++
		}
	}
	for i := N; i < len(p.digits); i++ {
		p.digits[i] = 0
	}
}

// decimalResultSci formats p in exponential notation similar to '%+#e'
// (decimal.c decimal_result_sci). N limits the significant digits (0 = all).
func decimalResultSci(p *decimalVal, N int) string {
	if p == nil {
		return ""
	}
	if N < 1 {
		N = 0
	}
	nDigit := len(p.digits)
	for nDigit > N && nDigit > 0 && p.digits[nDigit-1] == 0 {
		nDigit--
	}
	nZero := 0
	for nZero < nDigit && p.digits[nZero] == 0 {
		nZero++
	}
	nFrac := p.nFrac + (nDigit - len(p.digits))
	nDigit -= nZero
	if nDigit == 0 {
		nDigit = 1
		nFrac = 0
	}
	var b strings.Builder
	if p.sign && nDigit > 0 {
		b.WriteByte('-')
	} else {
		b.WriteByte('+')
	}
	if nZero < len(p.digits) {
		b.WriteByte(p.digits[nZero] + '0')
	} else {
		b.WriteByte('0')
	}
	b.WriteByte('.')
	if nDigit == 1 {
		b.WriteByte('0')
	} else {
		for i := 1; i < nDigit; i++ {
			if nZero+i < len(p.digits) {
				b.WriteByte(p.digits[nZero+i] + '0')
			} else {
				b.WriteByte('0')
			}
		}
	}
	exp := nDigit - nFrac - 1
	b.WriteString(fmt.Sprintf("e%+03d", exp))
	return b.String()
}

// decimalCmp compares two decimals (decimal.c decimal_cmp). Both must be
// non-null. Returns negative/zero/positive.
func decimalCmp(pA, pB *decimalVal) int {
	for pA.nFrac > 0 && len(pA.digits) > 0 && pA.digits[len(pA.digits)-1] == 0 {
		pA.digits = pA.digits[:len(pA.digits)-1]
		pA.nFrac--
	}
	for pB.nFrac > 0 && len(pB.digits) > 0 && pB.digits[len(pB.digits)-1] == 0 {
		pB.digits = pB.digits[:len(pB.digits)-1]
		pB.nFrac--
	}
	if pA.sign != pB.sign {
		if pA.sign {
			return -1
		}
		return 1
	}
	if pA.sign {
		pA, pB = pB, pA
	}
	nASig := len(pA.digits) - pA.nFrac
	nBSig := len(pB.digits) - pB.nFrac
	if nASig != nBSig {
		return nASig - nBSig
	}
	n := len(pA.digits)
	if n > len(pB.digits) {
		n = len(pB.digits)
	}
	rc := 0
	for i := 0; i < n; i++ {
		if pA.digits[i] != pB.digits[i] {
			rc = int(pA.digits[i]) - int(pB.digits[i])
			break
		}
	}
	if rc == 0 {
		rc = len(pA.digits) - len(pB.digits)
	}
	return rc
}

// decimalExpand grows p to have at least nDigit digits and nFrac fractional
// digits (decimal.c decimal_expand). Returns false on OOM.
func decimalExpand(p *decimalVal, nDigit, nFrac int) bool {
	if p == nil {
		return false
	}
	nAddFrac := nFrac - p.nFrac
	if nAddFrac < 0 {
		nAddFrac = 0
	}
	nAddSig := (nDigit - len(p.digits)) - nAddFrac
	if nAddFrac == 0 && nAddSig == 0 {
		return true
	}
	if nDigit+1 > maxDecimalDigits {
		return false
	}
	if nAddSig > 0 {
		p.digits = append(make([]byte, nAddSig), p.digits...)
	}
	if nAddFrac > 0 {
		p.digits = append(p.digits, make([]byte, nAddFrac)...)
		p.nFrac += nAddFrac
	}
	return true
}

// decimalAdd adds pB into pA: A := A + B (decimal.c decimal_add).
func decimalAdd(pA, pB *decimalVal) {
	if pA == nil || pB == nil {
		return
	}
	nSig := len(pA.digits) - pA.nFrac
	if nSig > 0 && pA.digits[0] == 0 {
		nSig--
	}
	if nSig < len(pB.digits)-pB.nFrac {
		nSig = len(pB.digits) - pB.nFrac
	}
	nFrac := pA.nFrac
	if nFrac < pB.nFrac {
		nFrac = pB.nFrac
	}
	nDigit := nSig + nFrac + 1
	if !decimalExpand(pA, nDigit, nFrac) {
		return
	}
	if !decimalExpand(pB, nDigit, nFrac) {
		return
	}
	if pA.sign == pB.sign {
		decimalAddSameSign(pA, pB, nDigit)
	} else {
		decimalAddOppositeSign(pA, pB, nDigit)
	}
}

// decimalAddSameSign performs the digit-wise addition for equal signs.
func decimalAddSameSign(pA, pB *decimalVal, nDigit int) {
	carry := 0
	for i := nDigit - 1; i >= 0; i-- {
		x := int(pA.digits[i]) + int(pB.digits[i]) + carry
		if x >= 10 {
			carry = 1
			pA.digits[i] = byte(x - 10)
		} else {
			carry = 0
			pA.digits[i] = byte(x)
		}
	}
}

// decimalAddOppositeSign performs the digit-wise subtraction for opposite
// signs, taking the absolute value of the larger operand (decimal.c decimal_add
// borrow loop).
func decimalAddOppositeSign(pA, pB *decimalVal, nDigit int) {
	rc := 0
	for i := 0; i < nDigit; i++ {
		if pA.digits[i] != pB.digits[i] {
			rc = int(pA.digits[i]) - int(pB.digits[i])
			break
		}
	}
	aA := pA.digits
	aB := pB.digits
	if rc < 0 {
		aA = pB.digits
		aB = pA.digits
		pA.sign = !pA.sign
	}
	borrow := 0
	for i := nDigit - 1; i >= 0; i-- {
		x := int(aA[i]) - int(aB[i]) - borrow
		if x < 0 {
			pA.digits[i] = byte(x + 10)
			borrow = 1
		} else {
			pA.digits[i] = byte(x)
			borrow = 0
		}
	}
}

// decimalMul multiplies pB into pA: A := A * B (decimal.c decimalMul).
// All significant digits after the decimal point are retained; trailing
// zeros after the decimal point are omitted as long as the number of digits
// after the decimal point is no less than either input's.
func decimalMul(pA, pB *decimalVal) {
	if pA == nil || pB == nil {
		return
	}
	sumDigit := len(pA.digits) + len(pB.digits) + 2
	if sumDigit > maxDecimalDigits {
		return
	}
	acc := make([]byte, len(pA.digits)+len(pB.digits)+2)
	minFrac := pA.nFrac
	if pB.nFrac < minFrac {
		minFrac = pB.nFrac
	}
	for i := len(pA.digits) - 1; i >= 0; i-- {
		f := int(pA.digits[i])
		carry := 0
		k := i + len(pB.digits) + 3
		for j := len(pB.digits) - 1; j >= 0; j-- {
			k--
			x := int(acc[k]) + f*int(pB.digits[j]) + carry
			acc[k] = byte(x % 10)
			carry = x / 10
		}
		k--
		x := int(acc[k]) + carry
		acc[k] = byte(x % 10)
		acc[k-1] += byte(x / 10)
	}
	pA.digits = acc
	pA.nFrac += pB.nFrac
	pA.sign = pA.sign != pB.sign
	for pA.nFrac > minFrac && len(pA.digits) > 0 && pA.digits[len(pA.digits)-1] == 0 {
		pA.nFrac--
		pA.digits = pA.digits[:len(pA.digits)-1]
	}
}

// decimalPow2 returns a Decimal for 2^N (decimal.c decimalPow2).
func decimalPow2(N int) *decimalVal {
	if N < -20000 || N > 20000 {
		return nil
	}
	pA := decimalNewFromText("1.0")
	if N == 0 {
		return pA
	}
	var pX *decimalVal
	if N > 0 {
		pX = decimalNewFromText("2.0")
	} else {
		N = -N
		pX = decimalNewFromText("0.5")
	}
	for {
		if N&1 != 0 {
			decimalMul(pA, pX)
		}
		N >>= 1
		if N == 0 {
			break
		}
		decimalMul(pX, pX)
	}
	return pA
}

// decimalFromDouble expands a binary64 value into its exact decimal
// representation (decimal.c decimalFromDouble). Returns nil for NaN/Inf.
func decimalFromDouble(r float64) *decimalVal {
	isNeg := r < 0.0
	if isNeg {
		r = -r
	}
	a := int64(math.Float64bits(r))
	var m int64
	e := 0
	if a == 0 || a == math.MinInt64 { // 0x8000000000000000 (-0.0)
		e = 0
		m = 0
	} else {
		e = int(a >> 52)
		m = a & ((int64(1) << 52) - 1)
		if e == 0 {
			m <<= 1
		} else {
			m |= int64(1) << 52
		}
		for e < 1075 && m > 0 && m&1 == 0 {
			m >>= 1
			e++
		}
		if isNeg {
			m = -m
		}
		e = e - 1075
		if e > 971 {
			return nil // NaN or Infinity
		}
	}
	pA := decimalNewFromText(fmt.Sprintf("%d", m))
	pX := decimalPow2(e)
	if pX != nil {
		decimalMul(pA, pX)
	}
	return pA
}

// decimalNewFromValue converts an sqlite3_value-equivalent into a Decimal
// (decimal.c decimal_new). bTextOnly forces FLOAT/BLOB to be read as text.
// NULL input yields nil (result NULL); non-8-byte blobs are read as text.
func decimalNewFromValue(v interface{}, bTextOnly bool) *decimalVal {
	v = unwrap(v)
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return decimalNewFromText(x)
	case int64:
		return decimalNewFromInt(x)
	case float64:
		if bTextOnly {
			return decimalNewFromText(toString(x))
		}
		return decimalFromDouble(x)
	case []byte:
		if bTextOnly || len(x) != 8 {
			return decimalNewFromText(string(x))
		}
		var bits uint64
		for _, b := range x {
			bits = bits<<8 | uint64(b)
		}
		return decimalFromDouble(math.Float64frombits(bits))
	default:
		return decimalNewFromText(toString(v))
	}
}

// ValueText renders a SQL value the way sqlite3_column_text does: NULL as "",
// INTEGER/REAL via SQLite's numeric text rendering, TEXT as-is, BLOB as raw
// bytes. Used by ext/misc/eval.c's eval() port (EvalExecSQL).
func ValueText(v interface{}) string {
	if v == nil {
		return ""
	}
	return toString(v)
}

// DecimalCollation compares two strings as decimal numbers (decimal.c
// decimalCollFunc). Used by the engine's built-in "decimal" collation.
func DecimalCollation(a, b string) int {
	pA := decimalNewFromText(a)
	pB := decimalNewFromText(b)
	if pA == nil || pB == nil {
		return 0
	}
	return decimalCmp(pA, pB)
}

// ---------------------------------------------------------------------------
// SQL function implementations (decimal.c aFunc table).
// ---------------------------------------------------------------------------

// fnDECIMAL implements decimal(X) and decimal(X,N): normalize X into decimal
// text; with N>0, round to N significant digits first (decimalFunc).
func fnDECIMAL(args []interface{}) (interface{}, error) {
	p := decimalNewFromValue(args[0], false)
	if p == nil || p.decimalIsNull() {
		return nil, nil
	}
	N := 0
	if len(args) >= 2 {
		N = int(toInt64(unwrap(args[1])))
		if N > 0 {
			decimalRound(p, N)
		}
	}
	return decimalResult(p), nil
}

// fnDECIMALEXP implements decimal_exp(X[,N]): exponential notation.
func fnDECIMALEXP(args []interface{}) (interface{}, error) {
	p := decimalNewFromValue(args[0], false)
	if p == nil || p.decimalIsNull() {
		return nil, nil
	}
	N := 0
	if len(args) >= 2 {
		N = int(toInt64(unwrap(args[1])))
		if N > 0 {
			decimalRound(p, N)
		}
	}
	return decimalResultSci(p, N), nil
}

// fnDECIMALCMP implements decimal_cmp(X,Y): -1/0/1.
func fnDECIMALCMP(args []interface{}) (interface{}, error) {
	pA := decimalNewFromValue(args[0], true)
	pB := decimalNewFromValue(args[1], true)
	if pA == nil || pB == nil || pA.decimalIsNull() || pB.decimalIsNull() {
		return nil, nil
	}
	rc := decimalCmp(pA, pB)
	if rc < 0 {
		return int64(-1), nil
	}
	if rc > 0 {
		return int64(1), nil
	}
	return int64(0), nil
}

// fnDECIMALADD implements decimal_add(X,Y).
func fnDECIMALADD(args []interface{}) (interface{}, error) {
	pA := decimalNewFromValue(args[0], true)
	pB := decimalNewFromValue(args[1], true)
	if pA == nil || pB == nil || pA.decimalIsNull() || pB.decimalIsNull() {
		return nil, nil
	}
	decimalAdd(pA, pB)
	return decimalResult(pA), nil
}

// fnDECIMALSUB implements decimal_sub(X,Y).
func fnDECIMALSUB(args []interface{}) (interface{}, error) {
	pA := decimalNewFromValue(args[0], true)
	pB := decimalNewFromValue(args[1], true)
	if pA == nil || pB == nil || pA.decimalIsNull() || pB.decimalIsNull() {
		return nil, nil
	}
	pB.sign = !pB.sign
	decimalAdd(pA, pB)
	return decimalResult(pA), nil
}

// fnDECIMALMUL implements decimal_mul(X,Y).
func fnDECIMALMUL(args []interface{}) (interface{}, error) {
	pA := decimalNewFromValue(args[0], true)
	pB := decimalNewFromValue(args[1], true)
	if pA == nil || pB == nil || pA.decimalIsNull() || pB.decimalIsNull() {
		return nil, nil
	}
	decimalMul(pA, pB)
	return decimalResult(pA), nil
}

// fnDECIMALPOW2 implements decimal_pow2(N): the N-th power of 2, in
// exponential notation (decimalPow2Func).
func fnDECIMALPOW2(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	n, ok := v.(int64)
	if !ok {
		return nil, nil
	}
	pA := decimalPow2(int(n))
	if pA == nil {
		return nil, nil
	}
	return decimalResultSci(pA, 0), nil
}

// decimalSumAgg is the decimal_sum(X) aggregate (decimalSumStep/Value/
// Finalize). NULL inputs are skipped.
type decimalSumAgg struct {
	acc *decimalVal
}

func newDecimalSumAgg() Aggregator { return &decimalSumAgg{} }

func (a *decimalSumAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	pArg := decimalNewFromValue(args[0], true)
	if pArg == nil || pArg.decimalIsNull() {
		return nil
	}
	if a.acc == nil {
		a.acc = decimalNewFromText("0")
	}
	decimalAdd(a.acc, pArg)
	return nil
}

func (a *decimalSumAgg) Final() (interface{}, error) {
	if a.acc == nil {
		return nil, nil
	}
	return decimalResult(a.acc), nil
}
