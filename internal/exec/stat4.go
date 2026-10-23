package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

const stat4MaxSamples = 24

type stat4Sample struct {
	row      int
	col      int
	periodic bool
	eq       []int
	lt       []int
	dlt      []int
	record   []byte
	hash     uint32
}

type stat4Accumulator struct {
	nCol       int
	period     int
	iPrn       uint32
	current    stat4Sample
	best       []stat4Sample
	samples    []stat4Sample
	nMaxEqZero int
}

func selectStat4Samples(entries [][]interface{}, records [][]byte, indexSQL string, defs []sql.ColumnDef, e *Engine) []stat4Sample {
	if len(entries) == 0 {
		return nil
	}
	acc := newStat4Accumulator(len(entries[0]), len(entries))
	for row := range entries {
		acc.push(entries, records, row, indexSQL, defs, e)
	}
	acc.pushPrevious(0)
	return acc.samples
}

func newStat4Accumulator(nCol, nRow int) *stat4Accumulator {
	acc := &stat4Accumulator{
		nCol:   nCol,
		period: nRow/(stat4MaxSamples/3+1) + 1,
		iPrn:   uint32(0x689e962d)*uint32(nCol) ^ uint32(0xd0944565)*uint32(nRow),
		current: stat4Sample{
			eq:  make([]int, nCol),
			lt:  make([]int, nCol),
			dlt: make([]int, nCol),
		},
		best: make([]stat4Sample, nCol-1),
	}
	for col := range acc.best {
		acc.best[col] = stat4Sample{
			row: -1, col: col,
			eq: make([]int, nCol), lt: make([]int, nCol), dlt: make([]int, nCol),
		}
	}
	return acc
}

func (a *stat4Accumulator) push(entries [][]interface{}, records [][]byte, row int, indexSQL string, defs []sql.ColumnDef, e *Engine) {
	iChng := 0
	if row == 0 {
		for col := range a.current.eq {
			a.current.eq[col] = 1
		}
	} else {
		iChng = a.changedColumn(entries[row-1], entries[row], indexSQL, defs, e)
		a.pushPrevious(iChng)
		a.updateCounts(iChng)
	}
	a.current.row = row
	a.current.record = append(a.current.record[:0], records[row]...)
	a.current.hash = a.nextHash()
	a.insertPeriodic()
	a.updateBest(row, iChng)
}

func (a *stat4Accumulator) changedColumn(previous, current []interface{}, indexSQL string, defs []sql.ColumnDef, e *Engine) int {
	for col := 0; col < a.nCol-1; col++ {
		if compareIndexPrefix(previous, current, col+1, indexSQL, defs, e) != 0 {
			return col
		}
	}
	return a.nCol - 1
}

func (a *stat4Accumulator) pushPrevious(iChng int) {
	for col := a.nCol - 2; col >= iChng; col-- {
		best := &a.best[col]
		best.eq[col] = a.current.eq[col]
		candidate := cloneStat4Sample(*best)
		if len(a.samples) < stat4MaxSamples || stat4SampleBetterThanMin(a.samples, candidate) {
			a.insert(candidate, col)
		}
	}
	if iChng < a.nMaxEqZero {
		a.fillZeroEq(iChng)
		a.nMaxEqZero = iChng
	}
}

func (a *stat4Accumulator) fillZeroEq(first int) {
	for i := range a.samples {
		for col := first; col < a.nCol; col++ {
			if a.samples[i].eq[col] == 0 {
				a.samples[i].eq[col] = a.current.eq[col]
			}
		}
	}
}

func (a *stat4Accumulator) updateCounts(iChng int) {
	for col := 0; col < iChng; col++ {
		a.current.eq[col]++
	}
	for col := iChng; col < a.nCol; col++ {
		a.current.dlt[col]++
		a.current.lt[col] += a.current.eq[col]
		a.current.eq[col] = 1
	}
}

func (a *stat4Accumulator) nextHash() uint32 {
	a.iPrn = a.iPrn*1103515245 + 12345
	return a.iPrn
}

func (a *stat4Accumulator) insertPeriodic() {
	lastLt := a.current.lt[a.nCol-1]
	if lastLt/a.period != (lastLt+1)/a.period {
		candidate := cloneStat4Sample(a.current)
		candidate.periodic = true
		candidate.col = 0
		a.insert(candidate, a.nCol-1)
	}
}

func (a *stat4Accumulator) updateBest(row, iChng int) {
	for col := 0; col < a.nCol-1; col++ {
		if iChng > col && a.best[col].row >= 0 && !stat4SampleBetterPost(a.current, a.best[col], col) {
			continue
		}
		a.best[col] = a.copyCurrent(row, col)
	}
}

func (a *stat4Accumulator) copyCurrent(row, col int) stat4Sample {
	c := cloneStat4Sample(a.current)
	c.row, c.col, c.periodic = row, col, false
	return c
}

func cloneStat4Sample(sample stat4Sample) stat4Sample {
	sample.eq = append([]int(nil), sample.eq...)
	sample.lt = append([]int(nil), sample.lt...)
	sample.dlt = append([]int(nil), sample.dlt...)
	sample.record = append([]byte(nil), sample.record...)
	return sample
}

func stat4SampleBetterThanMin(samples []stat4Sample, candidate stat4Sample) bool {
	min := -1
	for i, sample := range samples {
		if sample.periodic {
			continue
		}
		if min < 0 || stat4SampleBetter(samples[min], sample) {
			min = i
		}
	}
	return min < 0 || stat4SampleBetter(candidate, samples[min])
}

func (a *stat4Accumulator) insert(candidate stat4Sample, nEqZero int) {
	if nEqZero > a.nMaxEqZero {
		a.nMaxEqZero = nEqZero
	}
	if a.upgrade(candidate) {
		return
	}
	zeroLeadingEq(&candidate, nEqZero)
	if len(a.samples) >= stat4MaxSamples && !a.evictFor() {
		return
	}
	a.samples = append(a.samples, candidate)
}

func (a *stat4Accumulator) upgrade(candidate stat4Sample) bool {
	if candidate.periodic {
		return false
	}
	upgrade := -1
	for i := len(a.samples) - 1; i >= 0; i-- {
		old := a.samples[i]
		if old.eq[candidate.col] != 0 {
			continue
		}
		if old.periodic {
			return true
		}
		if upgrade < 0 || stat4SampleBetter(old, a.samples[upgrade]) {
			upgrade = i
		}
	}
	if upgrade < 0 {
		return false
	}
	a.samples[upgrade].col = candidate.col
	a.samples[upgrade].eq[candidate.col] = candidate.eq[candidate.col]
	return true
}

func zeroLeadingEq(candidate *stat4Sample, count int) {
	for col := 0; col < count && col < len(candidate.eq); col++ {
		candidate.eq[col] = 0
	}
}

func (a *stat4Accumulator) evictFor() bool {
	min := -1
	for i := range a.samples {
		if a.samples[i].periodic {
			continue
		}
		if min < 0 || stat4SampleBetter(a.samples[min], a.samples[i]) {
			min = i
		}
	}
	if min < 0 {
		return false
	}
	a.samples = append(a.samples[:min], a.samples[min+1:]...)
	return true
}

func stat4SampleBetter(a, b stat4Sample) bool {
	if a.eq[a.col] != b.eq[b.col] {
		return a.eq[a.col] > b.eq[b.col]
	}
	if a.col != b.col {
		return a.col < b.col
	}
	for col := a.col + 1; col < len(a.eq); col++ {
		if a.eq[col] != b.eq[col] {
			return a.eq[col] > b.eq[col]
		}
	}
	return a.hash > b.hash
}

func stat4SampleBetterPost(candidate, old stat4Sample, col int) bool {
	for i := col + 1; i < len(candidate.eq); i++ {
		if candidate.eq[i] != old.eq[i] {
			return candidate.eq[i] > old.eq[i]
		}
	}
	return candidate.hash > old.hash
}

func compareIndexPrefix(a, b []interface{}, n int, indexSQL string, defs []sql.ColumnDef, e *Engine) int {
	cols := parseIndexColumns(indexSQL)
	colIndex := buildColumnIndex(defs)
	colIdx := make([]int, 0, len(cols))
	for _, col := range cols {
		if x, ok := colIndex[col]; ok {
			colIdx = append(colIdx, x)
		}
	}
	colls := indexStatCollations(colIdx, defs, parseIndexColumnCollations(indexSQL))
	for i := 0; i < n && i < len(a) && i < len(b); i++ {
		coll := "BINARY"
		if i < len(colls) && i < len(cols) {
			coll = colls[i]
		}
		if cmp := util.CompareValuesCollateFn(a[i], b[i], coll, func(name string) (util.CollationFunc, bool) {
			f := e.lookupCollation(name)
			return util.CollationFunc(f), f != nil
		}); cmp != 0 {
			return cmp
		}
	}
	return 0
}
