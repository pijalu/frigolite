package vtab

import (
	"unicode/utf8"
)

// phoneticHashSkipPair reports whether the character pair at zIn[i:] is
// silenced by phoneticHash's digraph rules (wr, dj, dg, tch).
func phoneticHashSkipPair(zIn string, i int) bool {
	if i+1 >= len(zIn) {
		return false
	}
	c := zIn[i]
	switch {
	case c == 'w' && zIn[i+1] == 'r':
		return true
	case c == 'd' && (zIn[i+1] == 'j' || zIn[i+1] == 'g'):
		return true
	case i+2 < len(zIn) && c == 't' && zIn[i+1] == 'c' && zIn[i+2] == 'h':
		return true
	}
	return false
}

// phoneticHashTrimGN drops a word-initial K in KN or G in GN.
func phoneticHashTrimGN(zIn string) string {
	if len(zIn) > 2 {
		switch zIn[0] {
		case 'g', 'k':
			if zIn[1] == 'n' {
				return zIn[1:]
			}
		}
	}
	return zIn
}

// phoneticHashChar maps one input byte to its class, applying the
// space/other skip rules. ok is false when the character contributes
// nothing and the loop should move on.
func phoneticHashChar(c byte, cPrev byte, aClass []byte) (class byte, ok bool) {
	c = aClass[c&0x7f]
	if c == cclassSpace {
		return 0, false
	}
	if c == cclassOther && cPrev != cclassDigit {
		return 0, false
	}
	return c, true
}

// Character-class constants (spellfix.c lines 57-69).
const (
	cclassSilent = 0
	cclassVowel  = 1
	cclassB      = 2
	cclassC      = 3
	cclassD      = 4
	cclassH      = 5
	cclassL      = 6
	cclassR      = 7
	cclassM      = 8
	cclassY      = 9
	cclassDigit  = 10
	cclassSpace  = 11
	cclassOther  = 12
)

// spellfixMxHash is SPELLFIX_MX_HASH.
const spellfixMxHash = 32

// phoneticHash ports spellfix.c phoneticHash(): generate a phonetic hash from
// ASCII input (k1 text). The hash compresses consonant classes so words that
// sound alike share a prefix; the shadow vocabulary is looked up by the
// hash's iScope-character prefix.
func phoneticHash(zIn string) string {
	var out []byte
	cPrev := byte(0x77)
	cPrevX := byte(0x77)
	aClass := spellfixInitClass[:]

	// Omit K in KN or G in GN at the beginning of a word.
	zIn = phoneticHashTrimGN(zIn)

	for i := 0; i < len(zIn); i++ {
		if phoneticHashSkipPair(zIn, i) {
			continue
		}
		c, ok := phoneticHashChar(zIn[i], cPrev, aClass)
		if !ok {
			continue
		}
		aClass = spellfixMidClass[:]
		var skip bool
		out, skip = phoneticHashAdjacentRL(c, cPrevX, out)
		if skip {
			continue // No vowels beside L or R
		}
		cPrev = c
		if c == cclassSilent {
			continue
		}
		cPrevX = c
		ch := spellfixClassName[c]
		if len(out) == 0 || ch != out[len(out)-1] {
			out = append(out, ch)
		}
	}
	return string(out)
}

// phoneticHashAdjacentRL applies the no-vowels-beside-L-or-R rules: a
// vowel beside L/R skips the character; an L/R after a vowel pops the
// vowel already in the hash.
func phoneticHashAdjacentRL(c, cPrevX byte, out []byte) ([]byte, bool) {
	if c == cclassVowel && (cPrevX == cclassR || cPrevX == cclassL) {
		return out, true
	}
	if (c == cclassR || c == cclassL) && cPrevX == cclassVowel && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, false
}

// spellfixFindTranslit ports spellfixFindTranslit: binary search of the
// translit table (sorted by cFrom). ok is false when c has no mapping.
func spellfixFindTranslit(c rune) (entry spellfixTranslitEntry, ok bool) {
	lo, hi := 0, len(spellfixTranslit)-1
	for hi >= lo {
		mid := (lo + hi) / 2
		switch {
		case spellfixTranslit[mid].cFrom == c:
			return spellfixTranslit[mid], true
		case spellfixTranslit[mid].cFrom > c:
			hi = mid - 1
		default:
			lo = mid + 1
		}
	}
	return spellfixTranslitEntry{}, false
}

// transliterate ports spellfix.c transliterate(): convert UTF-8 input into
// pure ASCII, mapping non-ASCII characters through the translit table
// (unmapped characters become '?').
func transliterate(zIn string) string {
	var out []byte
	for len(zIn) > 0 {
		c, sz := utf8.DecodeRuneInString(zIn)
		if c == utf8.RuneError && sz <= 1 {
			// The C decoder is lenient; keep a '?' placeholder for the byte.
			out = append(out, '?')
			zIn = zIn[1:]
			continue
		}
		zIn = zIn[sz:]
		if c <= 127 {
			out = append(out, byte(c))
			continue
		}
		if e, ok := spellfixFindTranslit(c); ok {
			for _, b := range e.cTo {
				if b == 0 {
					break
				}
				out = append(out, b)
			}
			continue
		}
		out = append(out, '?')
	}
	return string(out)
}

// utf8Charlen ports spellfix.c utf8Charlen: number of characters in the
// first nIn bytes of the input.
func utf8Charlen(zIn string, nIn int) int {
	n := 0
	for i := 0; i < nIn && i < len(zIn); {
		_, sz := utf8.DecodeRuneInString(zIn[i:])
		if sz == 0 {
			sz = 1
		}
		i += sz
		n++
	}
	return n
}

// translen_to_charlen ports spellfix.c translen_to_charlen: how many input
// characters produce nTrans transliteration output bytes (matchlen parity
// for prefix searches).
func translen_to_charlen(zIn string, nIn, nTrans int) int {
	i, nOut := 0, 0
	nChar := 0
	for i < nIn && nOut < nTrans {
		c, sz := utf8.DecodeRuneInString(zIn[i:])
		if sz == 0 {
			sz = 1
		}
		i += sz
		nOut++
		if c >= 128 {
			nOut += translitExtraLen(c)
		}
		nChar++
	}
	return nChar
}

// translitExtraLen counts the additional output bytes cTo contributes past
// its first byte (translen_to_charlen's inner j loop).
func translitExtraLen(c rune) int {
	e, ok := spellfixFindTranslit(c)
	if !ok {
		return 0
	}
	n := 0
	for j := 1; j < 4; j++ {
		if e.cTo[j] == 0 {
			break
		}
		n++
	}
	return n
}

// characterClass ports spellfix.c characterClass: the class of c given the
// previous character (word-initial characters use initClass).
func characterClass(cPrev, c byte) byte {
	if cPrev == 0 {
		return spellfixInitClass[c&0x7f]
	}
	return spellfixMidClass[c&0x7f]
}

// insertOrDeleteCost ports spellfix.c insertOrDeleteCost: cost to insert or
// delete character c immediately following cPrev (cPrev==0 => word start).
func insertOrDeleteCost(cPrev, c, cNext byte) int {
	classC := characterClass(cPrev, c)
	if classC == cclassSilent {
		// Insert or delete "silent" characters such as H or W.
		return 1
	}
	if cPrev == c {
		// Repeated characters, or miss a repeat.
		return 10
	}
	if classC == cclassVowel && (cPrev == 'r' || cNext == 'r') {
		return 20 // Insert a vowel before or after 'r'
	}
	classCprev := characterClass(cPrev, cPrev)
	if classC == classCprev {
		if classC == cclassVowel {
			return 15 // Remove or add a new vowel to a vowel cluster
		}
		return 50 // Remove or add a consonant not in the same class
	}
	// Any other character insertion or deletion.
	return 100
}

// substituteCost ports spellfix.c substituteCost: cost to change cFrom into
// cTo assuming the previous character is cPrev (0 => word start).
func substituteCost(cPrev, cFrom, cTo byte) int {
	if cFrom == cTo {
		return 0 // Exact match
	}
	// Differ only in case.
	if cFrom == cTo^0x20 && ((cTo >= 'A' && cTo <= 'Z') || (cTo >= 'a' && cTo <= 'z')) {
		return 0
	}
	classFrom := characterClass(cPrev, cFrom)
	classTo := characterClass(cPrev, cTo)
	if classFrom == classTo {
		return 40 // Same character class
	}
	if classFrom >= cclassB && classFrom <= cclassY && classTo >= cclassB && classTo <= cclassY {
		return 75 // Consonant to consonant, different class
	}
	// Any other substitution.
	return 100
}

// spellfixFinalInsCostDiv is FINAL_INS_COST_DIV.
const spellfixFinalInsCostDiv = 4

// editdist1 ports spellfix.c editdist1: Wagner edit distance over ASCII
// byte strings with the fixed default cost model. A '*' as the LAST
// character of zA turns the query into a prefix search and, when pnMatch is
// non-nil, reports the matched prefix length of zB. Negative results are
// error codes: -1 null input, -2 non-ASCII input.
func editdist1(zA, zB string, pnMatch *int) int {
	nMatch := 0
	// dc carries the last byte of the skipped common prefix (0 when none);
	// it seeds the "previous character" context of the matrix.
	dc := byte(0)
	for len(zA) > 0 && len(zB) > 0 && zA[0] == zB[0] {
		dc = zA[0]
		zA, zB = zA[1:], zB[1:]
		nMatch++
	}
	if pnMatch != nil {
		*pnMatch = nMatch
	}
	if len(zA) == 0 && len(zB) == 0 {
		return 0
	}
	if !editdist1IsASCII(zA) || !editdist1IsASCII(zB) {
		return -2
	}
	nA, nB := len(zA), len(zB)

	// Special processing if either string is empty.
	if nA == 0 {
		return editdist1EmptySide(zB, dc, true)
	}
	if nB == 0 {
		return editdist1EmptySide(zA, dc, false)
	}

	// A is a prefix of B.
	if zA == "*" {
		return 0
	}
	return editdist1Matrix(zA, zB, dc, nMatch, pnMatch)
}

// editdist1IsASCII reports whether s is pure ASCII (editdist1's error -2).
func editdist1IsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i]&0x80 != 0 {
			return false
		}
	}
	return true
}

// editdist1EmptySide prices inserting (final==true, divide by
// FINAL_INS_COST_DIV) or deleting every character of z after prefix dc.
func editdist1EmptySide(z string, dc byte, final bool) int {
	res, cPrev := 0, dc
	for x := 0; x < len(z); x++ {
		c := z[x]
		var cNext byte
		if x+1 < len(z) {
			cNext = z[x+1]
		}
		cost := insertOrDeleteCost(cPrev, c, cNext)
		if final {
			cost /= spellfixFinalInsCostDiv
		}
		res += cost
		cPrev = c
	}
	return res
}

// editdist1Matrix runs the Wagner matrix over the full (non-empty) inputs
// and reduces the result (star-prefix minimum or bottom-right cell).
func editdist1Matrix(zA, zB string, dc byte, nMatch int, pnMatch *int) int {
	nB := len(zB)
	// Wagner matrix: m[xB] is the current row's cost; cx[xB] tracks the
	// source character for the doubled-letter heuristics.
	m := make([]int, nB+1)
	cx := make([]byte, nB+1)
	m[0] = 0
	cx[0] = dc
	cBprev := dc
	for xB := 1; xB <= nB; xB++ {
		cB := zB[xB-1]
		var cBnext byte
		if xB < nB {
			cBnext = zB[xB]
		}
		cx[xB] = cB
		m[xB] = m[xB-1] + insertOrDeleteCost(cBprev, cB, cBnext)
		cBprev = cB
	}
	brokeAtStar := editdist1Rows(zA, zB, dc, m, cx)

	if !brokeAtStar {
		return m[nB]
	}
	res := m[1]
	for xB := 1; xB <= nB; xB++ {
		if m[xB] < res {
			res = m[xB]
			if pnMatch != nil {
				*pnMatch = xB + nMatch
			}
		}
	}
	return res
}

// editdist1Rows advances the Wagner matrix row by row over zA; it returns
// true when the scan stopped early at a trailing '*' in zA.
func editdist1Rows(zA, zB string, dc byte, m []int, cx []byte) bool {
	nA := len(zA)
	cAprev := dc
	for xA := 1; xA <= nA; xA++ {
		lastA := xA == nA
		cA := zA[xA-1]
		var cAnext byte
		if xA < nA {
			cAnext = zA[xA]
		}
		if cA == '*' && lastA {
			return true
		}
		d := m[0]
		m[0] = d + insertOrDeleteCost(cAprev, cA, cAnext)
		editdist1InnerRow(zB, m, cx, &d, cA, lastA)
		cAprev = cA
	}
	return false
}

// editdist1InnerRow computes one matrix row's xB loop in place. d carries
// the diagonal cell (old m[0] at entry, m[xB] between columns).
func editdist1InnerRow(zB string, m []int, cx []byte, d *int, cA byte, lastA bool) {
	nB := len(zB)
	for xB := 1; xB <= nB; xB++ {
		cB := zB[xB-1]
		var cBnext byte
		if xB < nB {
			cBnext = zB[xB]
		}
		// Cost to insert cB.
		insCost := insertOrDeleteCost(cx[xB-1], cB, cBnext)
		if lastA {
			insCost /= spellfixFinalInsCostDiv
		}
		// Cost to delete cA.
		delCost := insertOrDeleteCost(cx[xB], cA, cBnext)
		// Cost to substitute cA -> cB.
		subCost := substituteCost(cx[xB-1], cA, cB)

		// Best cost: ncx tracks the source character feeding the next
		// column's doubled-letter heuristics (insert -> cB, delete ->
		// cA, substitute keeps cB — C's ncx dance).
		totalCost := insCost + m[xB-1]
		ncx := cB
		if delCost+m[xB] < totalCost {
			totalCost = delCost + m[xB]
			ncx = cA
		}
		if subCost+*d < totalCost {
			totalCost = subCost + *d
		}
		*d = m[xB]
		m[xB] = totalCost
		cx[xB] = ncx
	}
}

// spellfix1Score ports spellfix1Score: distance plus 32 minus log2(rank);
// rank 0..1 contributes the maximum bonus (iLog2 0 => +32 when rank<=1? C:
// for(iLog2=0; iRank>0; iLog2++, iRank>>=1){} => rank 0/1 -> 0 -> +32).
func spellfix1Score(iDistance, iRank int) int {
	iLog2 := 0
	for iRank > 0 {
		iLog2++
		iRank >>= 1
	}
	return iDistance + 32 - iLog2
}

// spellfix script-code bit flags (scriptCodeSqlFunc's mask bits).
const (
	scriptLatin    = 0x0001
	scriptCyrillic = 0x0002
	scriptGreek    = 0x0004
	scriptHebrew   = 0x0008
	scriptArabic   = 0x0010
)

// spellfixScriptBit folds one rune into the script mask; digits set
// seenDigit instead of a bit.
func spellfixScriptBit(c rune, seenDigit *bool) int {
	switch {
	case int(c) < 0x02af:
		return spellfixScriptLatinBit(c, seenDigit)
	case int(c) >= 0x0400 && int(c) <= 0x04ff:
		return scriptCyrillic
	case int(c) >= 0x0386 && int(c) <= 0x03ce:
		return scriptGreek
	case int(c) >= 0x0590 && int(c) <= 0x05ff:
		return scriptHebrew
	case int(c) >= 0x0600 && int(c) <= 0x06ff:
		return scriptArabic
	}
	return 0
}

// spellfixScriptLatinBit classifies the Latin range: non-digits below
// 0x80 are Latin, digits only mark seenDigit.
func spellfixScriptLatinBit(c rune, seenDigit *bool) int {
	if int(c) >= 0x80 || spellfixMidClass[int(c)&0x7f] < cclassDigit {
		return scriptLatin
	}
	if c >= '0' && c <= '9' {
		*seenDigit = true
	}
	return 0
}

// spellfix1ScriptCode ports scriptCodeSqlFunc: the dominant ISO 15924
// numeric script code of the input (215 Latin, 220 Cyrillic, 200 Greek,
// 125 Hebrew, 160 Arabic; 998 mixed, 999 none).
func spellfix1ScriptCode(zIn string) int {
	scriptMask := 0
	seenDigit := false
	for len(zIn) > 0 {
		c, sz := utf8.DecodeRuneInString(zIn)
		if sz == 0 {
			sz = 1
		}
		zIn = zIn[sz:]
		scriptMask |= spellfixScriptBit(c, &seenDigit)
	}
	if scriptMask == 0 && seenDigit {
		scriptMask = scriptLatin
	}
	switch scriptMask {
	case 0:
		return 999
	case scriptLatin:
		return 215
	case scriptCyrillic:
		return 220
	case scriptGreek:
		return 200
	case scriptHebrew:
		return 125
	case scriptArabic:
		return 160
	default:
		return 998
	}
}
