// SPDX-License-Identifier: GPL-3.0-or-later
package main

// helpersTemplateFpnum appends the float-tolerant result comparator to the
// generated helpers_test.go (Part1 + Part2 + Fpnum). It ports the TCL test
// suite's own do_test fallback comparator, src/test1.c fpnum_compare, so a
// result that differs from the expected text only in trailing zero digits or
// exponent padding compares equal — exactly as it does when SQLite's TCL
// suite runs the same assertion (misc3-2.5: %.15e renders 15 digits after
// the point, while the historical expectation carries 13; tester.tcl's
// string-compare failure falls back to fpnum_compare before failing a test).
const helpersTemplateFpnum = `// tclFpnumCompare ports src/test1.c fpnum_compare, the do_test fallback
// comparator of SQLite's TCL test suite (tester.tcl: string compare first,
// then fpnum_compare). Whitespace-separated tokens are compared pairwise:
// non-numeric tokens must match exactly; floating-point tokens must agree on
// the digits before the decimal point, on up to 15 digits after it (taking
// rounding into account), and on the exponent (e+NN matches e+N). Returns
// true when the two strings describe the same value.
func tclFpnumCompare(a, b interface{}) bool {
	aStr, aok := a.(string)
	bStr, bok := b.(string)
	if !aok || !bok {
		return false
	}
	return tclFpnumCompareStr(aStr, bStr)
}

// tclFpnumCompareStr is the string-form fpnum comparison (see tclFpnumCompare).
func tclFpnumCompareStr(aStr, bStr string) bool {
	zA := []byte(aStr)
	zB := []byte(bStr)
	i, j := 0, 0
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	isSpace := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
	}
	at := func(z []byte, k int) byte {
		if k < len(z) {
			return z[k]
		}
		return 0
	}
	for {
		for isSpace(at(zA, i)) {
			i++
		}
		for isSpace(at(zB, j)) {
			j++
		}

		if at(zA, i) != at(zB, j) {
			break // first character must match
		}
		if at(zA, i) == '-' && isDigit(at(zA, i+1)) {
			i++ // skip initial '-'
			j++
		}
		if !isDigit(at(zA, i)) {
			// Not a number: the token must match exactly.
			for at(zA, i) != 0 && !isSpace(at(zA, i)) && at(zA, i) == at(zB, j) {
				i++
				j++
			}
			if at(zA, i) != at(zB, j) {
				break
			}
			if isSpace(at(zA, i)) {
				continue
			}
			break
		}

		// A number on both sides. Match the digits before the decimal point
		// (which must all agree), then up to 15 fraction digits.
		nDigit := 0
		for at(zA, i) == at(zB, j) && isDigit(at(zA, i)) {
			i++
			j++
			nDigit++
		}
		if at(zA, i) != at(zB, j) {
			break
		}
		if at(zA, i) == 0 {
			break
		}
		if at(zA, i) == '.' && at(zB, j) == '.' {
			i++
			j++
			for at(zA, i) == at(zB, j) && isDigit(at(zA, i)) {
				i++
				j++
				nDigit++
			}
			if at(zA, i) == 0 {
				for at(zB, j) == '0' || (isDigit(at(zB, j)) && nDigit >= 15) {
					j++
					nDigit++
				}
				break
			}
			if at(zB, j) == 0 {
				for at(zA, i) == '0' || (isDigit(at(zA, i)) && nDigit >= 15) {
					i++
					nDigit++
				}
				break
			}
			if isSpace(at(zA, i)) && isSpace(at(zB, j)) {
				continue
			}
			if isDigit(at(zA, i)) && isDigit(at(zB, j)) {
				// A and B are both digits, but different digits: accept a
				// rounding boundary (one side ends in ...5 rounding up).
				if at(zA, i) == at(zB, j)+1 && !isDigit(at(zA, i+1)) && isDigit(at(zB, j+1)) {
					j++
					for at(zB, j) == '9' {
						j++
						nDigit++
					}
					if nDigit < 14 && (!isDigit(at(zB, j)) || at(zB, j) < '5') {
						break
					}
					for isDigit(at(zB, j)) {
						j++
					}
					i++
				} else if at(zB, j) == at(zA, i)+1 && !isDigit(at(zB, j+1)) && isDigit(at(zA, i+1)) {
					i++
					for at(zA, i) == '9' {
						i++
						nDigit++
					}
					if nDigit < 14 && (!isDigit(at(zA, i)) || at(zA, i) < '5') {
						break
					}
					for isDigit(at(zA, i)) {
						i++
					}
					j++
				} else {
					break
				}
			} else if !isDigit(at(zA, i)) && isDigit(at(zB, j)) {
				for at(zB, j) == '0' {
					j++
					nDigit++
				}
				if nDigit < 15 {
					break
				}
				for isDigit(at(zB, j)) {
					j++
				}
			} else if !isDigit(at(zB, j)) && isDigit(at(zA, i)) {
				for at(zA, i) == '0' {
					i++
					nDigit++
				}
				if nDigit < 15 {
					break
				}
				for isDigit(at(zA, i)) {
					i++
				}
			} else {
				break
			}
		}
		if at(zA, i) == 'e' && at(zB, j) == 'e' {
			i++
			j++
			if (at(zA, i) == '+' || at(zA, i) == '-') && at(zB, j) == at(zA, i) {
				i++
				j++
			}
			if at(zA, i) != at(zB, j) {
				if at(zA, i) == '0' && at(zA, i+1) == at(zB, j) {
					i++
				}
				if at(zB, j) == '0' && at(zB, j+1) == at(zA, i) {
					j++
				}
			}
			for at(zA, i) == at(zB, j) && isDigit(at(zA, i)) {
				i++
				j++
			}
			if at(zA, i) != at(zB, j) {
				break
			}
			if at(zA, i) == 0 {
				break
			}
			continue
		}
	}
	for isSpace(at(zA, i)) {
		i++
	}
	for isSpace(at(zB, j)) {
		j++
	}
	return at(zA, i) == 0 && at(zB, j) == 0
}
`
