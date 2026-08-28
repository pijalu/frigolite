package vtab

import (
	"strings"
)

// amatchDP computes weighted edit distances from one fixed target string
// using the cost matrix of an edit_distances table (amatch.c amatchVCheck):
//
//	iLang=0 rows only;
//	cFrom='' cTo=X  -> insert X into the target-derived word (cost rule);
//	cFrom=X cTo=''  -> delete X;
//	cFrom/cTo '?'   -> wildcard matching any single character pair;
//	operations with no matching rule default to amatchDefaultCost.
type amatchDP struct {
	rules    []AmatchCostRule
	target   []rune
	insert   map[rune]int64 // insert rune into word: cFrom='' cTo=rune
	deleteM  map[rune]int64 // delete rune from word: cFrom=rune cTo=''
	subst    map[string]int64
	wildIns  int64
	wildDel  int64
	wildSub  int64
	hasWildI bool
	hasWildD bool
	hasWildS bool
}

// newAmatchDP compiles the cost rules against a fixed MATCH target.
func newAmatchDP(rules []AmatchCostRule, target string) *amatchDP {
	dp := &amatchDP{
		rules:    rules,
		target:   []rune(target),
		insert:   map[rune]int64{},
		deleteM:  map[rune]int64{},
		subst:    map[string]int64{},
		wildIns:  amatchDefaultCost,
		wildDel:  amatchDefaultCost,
		wildSub:  amatchDefaultCost,
		hasWildI: false,
		hasWildD: false,
		hasWildS: false,
	}
	for _, r := range rules {
		if r.Lang != 0 {
			continue
		}
		switch {
		case r.From == "" && r.To == "?":
			dp.wildIns, dp.hasWildI = r.Cost, true
		case r.From == "?" && r.To == "":
			dp.wildDel, dp.hasWildD = r.Cost, true
		case r.From == "?" && r.To == "?":
			dp.wildSub, dp.hasWildS = r.Cost, true
		case r.From == "":
			dp.insert[[]rune(r.To)[0]] = r.Cost
		case r.To == "":
			dp.deleteM[[]rune(r.From)[0]] = r.Cost
		default:
			fr := []rune(r.From)
			to := []rune(r.To)
			if len(fr) == 1 && len(to) == 1 {
				dp.subst[string([]rune{fr[0], to[0]})] = r.Cost
			} else if len(fr) == 1 && to[0] == '?' {
				dp.subst[string([]rune{fr[0], '?'})] = r.Cost
			} else if fr[0] == '?' && len(to) == 1 {
				dp.subst["?"+string(to[0])] = r.Cost
			}
		}
	}
	return dp
}

// insertCost returns the cost of inserting rune r.
func (d *amatchDP) insertCost(r rune) int64 {
	if c, ok := d.insert[r]; ok {
		return c
	}
	return d.wildIns
}

// deleteCost returns the cost of deleting rune r.
func (d *amatchDP) deleteCost(r rune) int64 {
	if c, ok := d.deleteM[r]; ok {
		return c
	}
	return d.wildDel
}

// subCost returns the cost of transforming a into b. An exact rule wins;
// otherwise wildcard rules apply; otherwise the default.
func (d *amatchDP) subCost(a, b rune) int64 {
	if a == b {
		return 0
	}
	if c, ok := d.subst[string(a)+string(b)]; ok {
		return c
	}
	if d.hasWildS {
		return d.wildSub
	}
	if c, ok := d.subst[string(a)+"?"]; ok {
		return c
	}
	if c, ok := d.subst["?"+string(b)]; ok {
		return c
	}
	return amatchDefaultCost
}

// distance computes the weighted edit distance from w to the target.
func (d *amatchDP) distance(w string) int64 {
	src := []rune(strings.ToLower(w))
	tgt := d.target
	n, m := len(src), len(tgt)
	prev := make([]int64, m+1)
	curr := make([]int64, m+1)
	for j := 1; j <= m; j++ {
		prev[j] = prev[j-1] + d.insertCost(tgt[j-1])
	}
	for i := 1; i <= n; i++ {
		curr[0] = prev[0] + d.deleteCost(src[i-1])
		for j := 1; j <= m; j++ {
			del := prev[j] + d.deleteCost(src[i-1])
			ins := curr[j-1] + d.insertCost(tgt[j-1])
			sub := prev[j-1] + d.subCost(src[i-1], tgt[j-1])
			best := del
			if ins < best {
				best = ins
			}
			if sub < best {
				best = sub
			}
			curr[j] = best
		}
		prev, curr = curr, prev
	}
	return prev[m]
}
