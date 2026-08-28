package vtab

import (
	"fmt"
	"sort"
	"strings"
)

// Edit-distance-3 machinery (spellfix.c lines ~560-1230): configurable cost
// rules loaded from a user table, compiled per-FROM-string, evaluated with a
// Wagner matrix over UTF-8 characters.

// spellfixCostRule is one EditDist3Cost entry: transform the cFrom bytes
// into the cTo bytes for iCost.
type spellfixCostRule struct {
	cFrom, cTo string
	iCost      int
}

// spellfixEditDist3Lang holds the costs for one language ID
// (struct EditDist3Lang; defaults 100/100/150).
type spellfixEditDist3Lang struct {
	iLang              int
	iInsCost, iDelCost int
	iSubCost           int
	costs              []spellfixCostRule
}

// spellfixEditDist3Config is the complete configuration (EditDist3Config).
type spellfixEditDist3Config struct {
	langs []*spellfixEditDist3Lang
}

// spellfixDefaultLang is editDist3Lang: the default cost set.
var spellfixDefaultLang = spellfixEditDist3Lang{iLang: 0, iInsCost: 100, iDelCost: 100, iSubCost: 150}

// spellfixCostLess mirrors editDist3CostCompare: order by increasing FROM
// (byte compare on the common prefix, then shorter FROM first). TO bytes
// break full ties through the stable sort's original order.
func spellfixCostLess(a, b spellfixCostRule) bool {
	n := len(a.cFrom)
	if len(b.cFrom) < n {
		n = len(b.cFrom)
	}
	if c := strings.Compare(a.cFrom[:n], b.cFrom[:n]); c != 0 {
		return c < 0
	}
	return len(a.cFrom) < len(b.cFrom)
}

// spellfixEditDist3ConfigLoad ports editDist3ConfigLoad: read
// (iLang, cFrom, cTo, iCost) rules from the named table. '?' rows adjust the
// default insert/delete/substitute costs.
func spellfixEditDist3ConfigLoad(db Database, table string) (*spellfixEditDist3Config, error) {
	cfg := &spellfixEditDist3Config{}
	sqlStr := fmt.Sprintf("SELECT iLang, cFrom, cTo, iCost FROM %s WHERE iLang>=0 ORDER BY iLang",
		spellfixIdent(table))
	rows, err := db.ExecSQL(sqlStr)
	if err != nil {
		return nil, err
	}
	var cur *spellfixEditDist3Lang
	prev := -9999
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		iLang := spellfixValueInt(r[0])
		zFrom, _ := r[1].(string)
		zTo, _ := r[2].(string)
		iCost := spellfixValueInt(r[3])
		if !spellfixCostRowValid(zFrom, zTo, iCost) {
			continue
		}
		if cur == nil || iLang != prev {
			cur = &spellfixEditDist3Lang{iLang: iLang, iInsCost: 100, iDelCost: 100, iSubCost: 150}
			cfg.langs = append(cfg.langs, cur)
			prev = iLang
		}
		if !spellfixApplyCostDefault(cur, zFrom, zTo, iCost) {
			cur.costs = append(cur.costs, spellfixCostRule{cFrom: zFrom, cTo: zTo, iCost: iCost})
		}
	}
	for _, l := range cfg.langs {
		sort.SliceStable(l.costs, func(i, j int) bool { return spellfixCostLess(l.costs[i], l.costs[j]) })
	}
	return cfg, nil
}

// spellfixCostRowValid mirrors editDist3ConfigLoad's row filter: strings
// are capped at 100 bytes and costs of 10000+ are "infinite".
func spellfixCostRowValid(zFrom, zTo string, iCost int) bool {
	if len(zFrom) > 100 || len(zTo) > 100 {
		return false
	}
	return iCost >= 0 && iCost < 10000
}

// spellfixApplyCostDefault applies a '?'-row to the default ins/del/sub
// costs; it reports whether the row was a default row.
func spellfixApplyCostDefault(cur *spellfixEditDist3Lang, zFrom, zTo string, iCost int) bool {
	switch {
	case len(zFrom) == 1 && zFrom[0] == '?' && zTo == "":
		cur.iDelCost = iCost
	case zFrom == "" && len(zTo) == 1 && zTo[0] == '?':
		cur.iInsCost = iCost
	case len(zFrom) == 1 && len(zTo) == 1 && zFrom[0] == '?' && zTo[0] == '?':
		cur.iSubCost = iCost
	default:
		return false
	}
	return true
}

// spellfixEditDist3FindLang ports editDist3FindLang.
func spellfixEditDist3FindLang(cfg *spellfixEditDist3Config, iLang int) *spellfixEditDist3Lang {
	if cfg != nil {
		for _, l := range cfg.langs {
			if l.iLang == iLang {
				return l
			}
		}
	}
	return &spellfixDefaultLang
}

// spellfixUtf8Len ports utf8Len: the length of the UTF-8 character whose
// first byte is c, capped at N.
func spellfixUtf8Len(c byte, N int) int {
	l := 1
	if c > 0x7f {
		switch {
		case (c & 0xe0) == 0xc0:
			l = 2
		case (c & 0xf0) == 0xe0:
			l = 3
		default:
			l = 4
		}
	}
	if l > N {
		l = N
	}
	return l
}

// matchTo ports matchTo: does the TO side of the rule match z[0..n)?
// p.a[p.nFrom] is the TO side's first byte.
func matchTo(p spellfixCostRule, z string, n int) bool {
	if p.cTo == "" {
		return false
	}
	if p.cTo[0] != z[0] {
		return false
	}
	if len(p.cTo) > n {
		return false
	}
	return z[:len(p.cTo)] == p.cTo
}

// matchFrom ports matchFrom: does the FROM side of the rule match z?
func matchFrom(p spellfixCostRule, z string) bool {
	if p.cFrom == "" {
		return true
	}
	if len(z) < len(p.cFrom) {
		return false
	}
	return z[:len(p.cFrom)] == p.cFrom
}

// matchFromTo ports matchFromTo: are the next FROM and TO characters the
// same? i1 is the FROM byte offset.
func matchFromTo(from *spellfixFromString, i1 int, z2 string, n2 int) bool {
	b1 := from.chars[i1].nByte
	if b1 > n2 {
		return false
	}
	if from.z[i1] != z2[0] {
		return false
	}
	return from.z[i1:i1+b1] == z2[:b1]
}

// spellfixFromChar is EditDist3From: per-FROM-character compiled rules.
type spellfixFromChar struct {
	nByte   int
	apDel   []spellfixCostRule
	apSubst []spellfixCostRule
	z       string // the character's bytes within the FROM text
}

// spellfixFromString is EditDist3FromString: a precompiled FROM string.
// Columns of the Wagner matrix are BYTE positions (C's f.n is a byte
// length); each byte position records the width of the character starting
// there (continuation bytes carry C's utf8Len garbage width, never used).
type spellfixFromString struct {
	z        string // full text with the trailing '*' stripped
	n        int    // byte length of z
	isPrefix bool
	chars    []spellfixFromChar // indexed by byte position
}

// spellfixFromStringNew ports editDist3FromStringNew.
func spellfixFromStringNew(lang *spellfixEditDist3Lang, z string) *spellfixFromString {
	if lang == nil {
		return nil
	}
	s := &spellfixFromString{z: z, n: len(z)}
	if n := len(z); n > 0 && z[n-1] == '*' {
		s.isPrefix = true
		s.z = z[:n-1]
		s.n = len(s.z)
	}
	s.chars = make([]spellfixFromChar, s.n)
	for i := 0; i < s.n; {
		b := spellfixUtf8Len(s.z[i], s.n-i)
		fc := spellfixFromChar{nByte: b, z: s.z[i : i+b]}
		fc = spellfixCollectFromRules(lang, s, i, fc)
		s.chars[i] = fc
		i += b
	}
	return s
}

// spellfixCollectFromRules gathers the delete/substitute rules matching the
// character starting at byte i of the FROM text (editDist3FromStringNew's
// inner cost loop; multi-character rules like "ss" are legal).
func spellfixCollectFromRules(lang *spellfixEditDist3Lang, s *spellfixFromString, i int, fc spellfixFromChar) spellfixFromChar {
	for _, p := range lang.costs {
		if p.cFrom == "" {
			continue
		}
		// C: if( i+p->nFrom>n ) continue; matchFrom compares the full
		// FROM text.
		if len(p.cFrom) > s.n-i {
			continue
		}
		if !matchFrom(p, s.z[i:]) {
			continue
		}
		if p.cTo == "" {
			fc.apDel = append(fc.apDel, p)
		} else {
			fc.apSubst = append(fc.apSubst, p)
		}
	}
	return fc
}

// spellfixToChar is EditDist3To: per-TO-character compiled rules.
type spellfixToChar struct {
	nByte int
	apIns []spellfixCostRule
}

// spellfixEditDist3Core ports editDist3Core: minimum edit distance from the
// compiled FROM string to z2 under lang's rules. The Wagner matrix is laid
// out over BYTE positions of both strings (szRow = nFrom bytes + 1); steps
// advance by each character's byte width. When pnMatch is non-nil it
// receives the number of TO characters matched (prefix searches).
func spellfixEditDist3Core(from *spellfixFromString, z2 string, lang *spellfixEditDist3Lang, pnMatch *int) int {
	if from == nil || lang == nil {
		return -1
	}
	n2 := len(z2)
	a2 := spellfixCompileTo(lang, z2)

	// Wagner matrix: m[i2*szRow + i1], initialized to a large value.
	szRow := from.n + 1
	const big = 1 << 30
	m := make([]int, szRow*(n2+1))
	for i := range m {
		m[i] = big
	}
	m[0] = 0

	// First fill in the top-row of the matrix with FROM deletion costs.
	spellfixFillDeleteRow(m, from, lang)

	// Fill in all subsequent rows, top-to-bottom, left-to-right.
	spellfixFillBody(m, from, a2, z2, lang)

	res := m[szRow*(n2+1)-1]
	nMatched := n2
	if from.isPrefix {
		res, nMatched = spellfixPrefixMin(m, szRow, n2)
	}
	if pnMatch != nil {
		*pnMatch = spellfixCountMatched(z2, nMatched)
	}
	return res
}

// spellfixCompileTo builds the per-TO-character insertion rules
// (editDist3Core's first pass over z2).
func spellfixCompileTo(lang *spellfixEditDist3Lang, z2 string) []spellfixToChar {
	n2 := len(z2)
	a2 := make([]spellfixToChar, n2)
	for i2 := 0; i2 < n2; {
		b2 := spellfixUtf8Len(z2[i2], n2-i2)
		a2[i2].nByte = b2
		for _, p := range lang.costs {
			if p.cFrom != "" {
				break
			}
			if len(p.cTo) > n2-i2 {
				continue
			}
			if p.cTo[0] > z2[i2] {
				break
			}
			if !matchTo(p, z2[i2:], n2-i2) {
				continue
			}
			a2[i2].apIns = append(a2[i2].apIns, p)
		}
		i2 += b2
	}
	return a2
}

// spellfixFillDeleteRow seeds the top matrix row with FROM deletion costs
// (default iDelCost plus the compiled per-character delete rules).
func spellfixFillDeleteRow(m []int, from *spellfixFromString, lang *spellfixEditDist3Lang) {
	for i1 := 0; i1 < from.n; {
		b1 := from.chars[i1].nByte
		spellfixUpdateCost(m, i1+b1, i1, lang.iDelCost)
		for _, p := range from.chars[i1].apDel {
			spellfixUpdateCost(m, i1+len(p.cFrom), i1, p.iCost)
		}
		i1 += b1
	}
}

// spellfixFillBody fills every row below the top, left-to-right
// (editDist3Core's main double loop).
func spellfixFillBody(m []int, from *spellfixFromString, a2 []spellfixToChar, z2 string, lang *spellfixEditDist3Lang) {
	n2 := len(z2)
	szRow := from.n + 1
	for i2 := 0; i2 < n2; {
		b2 := a2[i2].nByte
		rx := szRow * (i2 + b2)
		rxp := szRow * i2
		spellfixUpdateCost(m, rx, rxp, lang.iInsCost)
		for _, p := range a2[i2].apIns {
			spellfixUpdateCost(m, szRow*(i2+len(p.cTo)), rxp, p.iCost)
		}
		for i1 := 0; i1 < from.n; {
			spellfixFillCell(m, from, lang, rx, rxp, i1, z2[i2:], n2-i2)
			i1 += from.chars[i1].nByte
		}
		i2 += b2
	}
}

// spellfixFillCell computes one (i1,i2) Wagner cell and its rule edges
// (editDist3Core's inner cell body).
func spellfixFillCell(m []int, from *spellfixFromString, lang *spellfixEditDist3Lang, rx, rxp, i1 int, z2Tail string, nTail int) {
	szRow := from.n + 1
	b1 := from.chars[i1].nByte
	cxp := rx + i1
	cx := cxp + b1
	cxd := rxp + i1
	cxu := cxd + b1
	spellfixUpdateCost(m, cx, cxp, lang.iDelCost)
	for _, p := range from.chars[i1].apDel {
		spellfixUpdateCost(m, cxp+len(p.cFrom), cxp, p.iCost)
	}
	spellfixUpdateCost(m, cx, cxu, lang.iInsCost)
	if matchFromTo(from, i1, z2Tail, nTail) {
		spellfixUpdateCost(m, cx, cxd, 0)
	}
	spellfixUpdateCost(m, cx, cxd, lang.iSubCost)
	for _, p := range from.chars[i1].apSubst {
		if matchTo(p, z2Tail, nTail) {
			spellfixUpdateCost(m, cxd+len(p.cFrom)+szRow*len(p.cTo), cxd, p.iCost)
		}
	}
}

// spellfixPrefixMin reduces a prefix-search result: the cheapest row end
// and how many TO bytes it consumed.
func spellfixPrefixMin(m []int, szRow, n2 int) (res, nMatched int) {
	res = m[szRow*(n2+1)-1]
	nMatched = n2
	for i2 := 1; i2 <= n2; i2++ {
		b := m[szRow*i2-1]
		if b <= res {
			res = b
			nMatched = i2 - 1
		}
	}
	return res, nMatched
}

// spellfixCountMatched converts a byte match length into character count.
func spellfixCountMatched(z2 string, nMatched int) int {
	nExtra := 0
	for k := 0; k < nMatched; k++ {
		if z2[k]&0xc0 == 0x80 {
			nExtra++
		}
	}
	return nMatched - nExtra
}

// spellfixUpdateCost ports updateCost: m[i] = min(m[i], m[j]+iCost).
func spellfixUpdateCost(m []int, i, j, iCost int) {
	if iCost < 0 || iCost >= 10000 {
		return
	}
	if b := m[j] + iCost; b < m[i] {
		m[i] = b
	}
}
