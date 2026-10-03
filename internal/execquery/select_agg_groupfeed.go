package execquery

import (
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// Grouped-aggregate feed (src/vdbe.c OP_AggStep/OP_AggFinal over a GROUP BY
// sorter): a single-table SELECT whose output columns are bare aggregates from
// the COUNT/SUM/AVG/TOTAL/MIN/MAX family (single bare column argument or
// COUNT(*)) or a GROUP BY term projection, with no HAVING/ORDER BY/DISTINCT/
// window/compound context, accumulates each group's aggregates DURING the
// scan. The generic path materializes a positional row clone for every input
// row, partitions them under a string key, and re-walks each group's rows per
// output aggregate; the feed keeps only per-GROUP state (one registry
// aggregator set per group, the first row's key values), so per-input-row work
// is key classification + map lookup + Step. Group output order (sortGroupKeys
// by the first row's key values), the storage-class key tags
// (collationGroupKey), the collated-key merge (resolveGroupKeyMiss), and every
// aggregate's Step/Final semantics are the generic path's own code.

// groupFeedCall is one output aggregate's compiled form: where its per-row
// argument comes from and which accumulator template instantiates per group.
type groupFeedCall struct {
	slot      int
	countStar bool
	// aggFn instantiates the registry aggregator (COUNT/SUM/AVG/TOTAL);
	// mmProto is the MIN/MAX reduction template (aggFn nil). Exactly one is
	// non-nil (countStar calls carry aggFn and read no slot).
	aggFn   func() function.Aggregator
	mmProto *groupMinMax
}

// groupMinMax is the streaming single-argument MIN/MAX reduction, mirroring
// evalMinMaxAggregate: NULLs skipped, first extreme on ties wins, collation
// from the argument column's declared COLLATE (the generic path donates it
// from the first value's wrapper; feed mode steps raw values, so the
// compile-time resolution is the same collation), with the runtime donation
// kept as a second chance for wrapper-carrying slots.
type groupMinMax struct {
	isMax bool
	best  interface{}
	coll  string
}

// step folds one row's argument value into the reduction (the
// evalMinMaxAggregate unwrap chain).
func (m *groupMinMax) step(e *SelectEngine, raw interface{}) error {
	if raw == nil {
		return nil
	}
	val := util.UnwrapColumnValue(raw)
	if cv, ok := raw.(*CollatedValue); ok {
		val = util.UnwrapColumnValue(cv.Value)
		if m.coll == "" && cv.Collation != "" {
			m.coll = cv.Collation
		}
	}
	if val == nil {
		return nil
	}
	if m.best == nil {
		m.best = val
		return nil
	}
	cmp := e.ctx.CompareValuesCollate(val, m.best, m.coll)
	if (m.isMax && cmp > 0) || (!m.isMax && cmp < 0) {
		m.best = val
	}
	return nil
}

// groupAggInst is one group's instance of one output aggregate.
type groupAggInst struct {
	agg function.Aggregator // registry aggregates (mm nil)
	mm  *groupMinMax        // MIN/MAX reduction (agg nil)
}

// groupAccum is one group's streaming state: the serialized spelling it was
// first filed under ("" for typed buckets), the first row's key values (the
// output projection and the collated-merge comparator's inputs), and the
// per-call aggregator instances.
type groupAccum struct {
	key    string
	keyVal []interface{}
	colls  []string
	insts  []groupAggInst
}

// groupFeedOut maps one output column to its source: a GROUP BY term
// projection (the group's first key value, buildGroupByAggRow's groupVals) or
// an aggregate call's Final.
type groupFeedOut struct {
	term int
	call int
}

// groupFeedPartition is the statement-scoped GROUP BY accumulator behind
// simpleAggFeed.group. Typed buckets file single-term integral keys by their
// shared numeric spelling (map[int64]); every other key takes the generic
// serialized-string map with the collated merge scan. A group whose spelling
// is only reachable from the OTHER map cannot exist: integral-spelled floats
// are filed typed (typedIntGroupKey), and non-integral/NaN/out-of-range floats
// never spell an integer.
type groupFeedPartition struct {
	e        *SelectEngine
	calls    []groupFeedCall
	outs     []groupFeedOut
	keySlots []int
	keyColls []string
	// typed marks the single-term, collation-free key whose per-row values
	// commonly classify as int64 (or a spelling-equal float64).
	typed bool

	typedIdx map[int64]int32
	strIdx   map[string]int32
	groups   []*groupAccum

	keyParts []string
	keyVals  []interface{}
	scratch  [1]interface{}
}

// compileGroupedAggFeed extracts the grouped feed for a real-table SELECT, or
// nil when the statement keeps the generic GROUP BY passes. Guard style
// mirrors compileSimpleAggFeed: every false branch names the semantic hazard
// that keeps the generic path.
func (e *SelectEngine) compileGroupedAggFeed(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *simpleAggFeed {
	if s == nil || tableEntry == nil || len(s.Columns) == 0 || len(s.GroupBy) == 0 {
		return nil
	}
	if !e.groupFeedStatementEligible(s, tableEntry) {
		return nil
	}
	groupBy, gbErr := resolveGroupByOrdinals(s, colDefs)
	if gbErr != nil {
		return nil // the generic pass surfaces the identical error
	}
	p := &groupFeedPartition{e: e, typedIdx: make(map[int64]int32), strIdx: make(map[string]int32)}
	if !p.compileKeyTerms(groupBy, colDefs) {
		return nil
	}
	if !p.compileOutputs(s, groupBy, colDefs) {
		return nil
	}
	return &simpleAggFeed{group: p}
}

// groupFeedStatementEligible reports the statement-level guards: one real
// rowid table (the simple feed's FROM shape minus its GROUP BY rejection), no
// HAVING/ORDER BY/DISTINCT output the feed cannot evaluate (ORDER BY and
// DISTINCT consume per-group representative row maps the feed does not keep),
// and the shared evaluation-context guards.
func (e *SelectEngine) groupFeedStatementEligible(s *sql.SelectStmt, tableEntry *schema.Entry) bool {
	if s.Union != nil || s.From.Subquery != nil || s.From.Name == "" || len(s.Joins) > 0 {
		return false
	}
	if s.Having != nil || s.Distinct || len(s.OrderBy) > 0 {
		return false
	}
	if IsSchemaTable(tableEntry.Name) {
		return false
	}
	if e.ctx.TableIsWithoutRowidEntry(tableEntry) {
		return false
	}
	// A covering-index GROUP BY reorders the scanned rows before grouping
	// (reorderAggRowsByCoveringIndex) — the feed's accumulation order and
	// first-seen key retention must match, so those statements stay generic.
	if len(e.coveringIndexForAggregate(s)) > 0 {
		return false
	}
	return e.aggFeedEvaluationEligible(s)
}

// compileKeyTerms compiles the GROUP BY terms: every term must be a plain
// unqualified column reference resolving to a stored slot (or the rowid
// pseudo-column) — exactly the computeGroupByKeyValues fast path's shape, so
// the key value and its collation tag are the generic pass's.
func (p *groupFeedPartition) compileKeyTerms(groupBy []sql.Expr, colDefs []sql.ColumnDef) bool {
	p.keySlots = make([]int, len(groupBy))
	p.keyColls = make([]string, len(groupBy))
	for i, expr := range groupBy {
		ref, fast := groupByFastRef(expr)
		if !fast {
			return false
		}
		slot, ok := aggFeedArgSlot(colDefs, ref.Name)
		if !ok {
			return false
		}
		p.keySlots[i] = slot
		p.keyColls[i] = columnDeclaredCollation(&colDefs[slot])
	}
	p.typed = len(groupBy) == 1 && p.keyColls[0] == "" && p.keySlots[0] != feedRowidSlot
	return true
}

// columnDeclaredCollation resolves a column's key/minmax collation the way
// wrapAffinityCollated builds the runtime marker: non-BINARY declared COLLATE,
// uppercased.
func columnDeclaredCollation(cd *sql.ColumnDef) string {
	if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
		return strings.ToUpper(coll)
	}
	return ""
}

// compileOutputs compiles the output columns: each is either a GROUP BY term
// projection (matchGroupByExpr, buildGroupByAggRow's reuse-the-key-value rule)
// or a bare aggregate call from the feed family extended with single-argument
// MIN/MAX. Anything else (bare non-term columns, expressions) evaluates over
// the group's rows and keeps the generic pass.
func (p *groupFeedPartition) compileOutputs(s *sql.SelectStmt, groupBy []sql.Expr, colDefs []sql.ColumnDef) bool {
	p.outs = make([]groupFeedOut, 0, len(s.Columns))
	for _, col := range s.Columns {
		if p.e.exprHasWindowFunc(col.Expr) {
			return false
		}
		if gi := matchGroupByExpr(groupBy, col.Expr); gi >= 0 {
			p.outs = append(p.outs, groupFeedOut{term: gi, call: -1})
			continue
		}
		fn, ok := col.Expr.(*sql.FuncCall)
		if !ok {
			return false
		}
		call, ok := p.compileCall(fn, s, colDefs)
		if !ok {
			return false
		}
		p.outs = append(p.outs, groupFeedOut{term: -1, call: len(p.calls)})
		p.calls = append(p.calls, call)
	}
	return true
}

// compileCall compiles one aggregate output column. MIN/MAX take the
// collation-aware reduction template; COUNT/SUM/AVG/TOTAL reuse the simple
// feed's argument compilation (bare column reference or COUNT(*)) and the
// registry aggregator.
func (p *groupFeedPartition) compileCall(fn *sql.FuncCall, s *sql.SelectStmt, colDefs []sql.ColumnDef) (groupFeedCall, bool) {
	reg, found := p.e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		return groupFeedCall{}, false
	}
	if fn.Distinct || fn.Filter != nil || len(fn.OrderBy) > 0 {
		return groupFeedCall{}, false
	}
	if nested := p.e.findAggNestedAggregates(fn); nested != "" {
		return groupFeedCall{}, false
	}
	switch strings.ToUpper(fn.Name) {
	case "MIN", "MAX":
		return p.compileMinMaxCall(fn, s, colDefs)
	case "COUNT", "SUM", "AVG", "TOTAL":
		return p.compileRegistryCall(fn, reg, s, colDefs)
	}
	return groupFeedCall{}, false
}

// compileMinMaxCall compiles a single-argument MIN/MAX over a bare column
// reference (or the rowid pseudo-column): the reduction template carries the
// argument column's declared collation, the same marker the generic path's
// wrapped value donates.
func (p *groupFeedPartition) compileMinMaxCall(fn *sql.FuncCall, s *sql.SelectStmt, colDefs []sql.ColumnDef) (groupFeedCall, bool) {
	if len(fn.Args) != 1 {
		return groupFeedCall{}, false
	}
	reg, _ := p.e.ctx.Functions().Find(fn.Name)
	c, ok := aggFeedColumnArg(fn.Args[0], reg, s, colDefs)
	if !ok {
		return groupFeedCall{}, false
	}
	mm := &groupMinMax{isMax: strings.EqualFold(fn.Name, "MAX")}
	if c.slot != feedRowidSlot {
		mm.coll = columnDeclaredCollation(&colDefs[c.slot])
	}
	return groupFeedCall{slot: c.slot, mmProto: mm}, true
}

// compileRegistryCall compiles COUNT/SUM/AVG/TOTAL: COUNT with zero or one
// bare argument (COUNT(*)), the others with exactly one.
func (p *groupFeedPartition) compileRegistryCall(fn *sql.FuncCall, reg *function.Func, s *sql.SelectStmt, colDefs []sql.ColumnDef) (groupFeedCall, bool) {
	switch strings.ToUpper(fn.Name) {
	case "COUNT":
		c, ok := compileAggFeedCount(fn, reg, s, colDefs)
		if !ok {
			return groupFeedCall{}, false
		}
		return groupFeedCall{slot: c.slot, countStar: c.countStar, aggFn: reg.AggregateFn}, true
	case "SUM", "AVG", "TOTAL":
		if len(fn.Args) != 1 {
			return groupFeedCall{}, false
		}
		c, ok := aggFeedColumnArg(fn.Args[0], reg, s, colDefs)
		if !ok {
			return groupFeedCall{}, false
		}
		return groupFeedCall{slot: c.slot, aggFn: reg.AggregateFn}, true
	}
	return groupFeedCall{}, false
}

// unionDecodeSlots adds the feed's read slots (key terms + aggregate
// arguments) to the scan's phase-1 lazy-decode set: feed mode decodes only
// that set, and the statement's references are exactly these slots (plus the
// WHERE's, already collected).
func (p *groupFeedPartition) unionDecodeSlots(idx map[int]bool) {
	for _, slot := range p.keySlots {
		if slot != feedRowidSlot {
			idx[slot] = true
		}
	}
	for ci := range p.calls {
		if !p.calls[ci].countStar && p.calls[ci].slot != feedRowidSlot {
			idx[p.calls[ci].slot] = true
		}
	}
}

// step accumulates one decoded row. The argument unwrapping mirrors
// simpleAggFeed.step (util.UnwrapColumnValue then unwrapCollatedValue), so
// every accumulator receives the raw scalar the generic path's Step sees.
func (p *groupFeedPartition) step(values []interface{}, rowID int64) error {
	if p.typed {
		if idx, ok := p.typedBucket(values); ok {
			return p.stepGroup(idx, values, rowID)
		}
	}
	key, vals, colls := p.serializedKey(values, rowID)
	idx, ok := p.strIdx[key]
	if !ok {
		idx = p.newStringGroup(key, vals, colls)
	}
	return p.stepGroup(idx, values, rowID)
}

// typedBucket files an integral-class single-term key by its shared numeric
// spelling. ok=false sends the row to the serialized-string map (the bucket
// is engaged only for values whose spelling an int64 key can carry — see
// typedIntGroupKey).
func (p *groupFeedPartition) typedBucket(values []interface{}) (int32, bool) {
	v := unwrapCollatedValue(util.UnwrapColumnValue(values[p.keySlots[0]]))
	switch t := v.(type) {
	case int64:
		idx, ok := p.typedIdx[t]
		if !ok {
			idx = p.newTypedGroup(t, v)
		}
		return idx, true
	case float64:
		if k, ok := typedIntGroupKey(t); ok {
			idx, ok := p.typedIdx[k]
			if !ok {
				idx = p.newTypedGroup(k, v)
			}
			return idx, true
		}
	}
	return 0, false
}

// typedIntGroupKey reports the int64 bucket a float64 key value shares:
// SQLite compares INTEGER and REAL numerically, and collationGroupKey spells
// integral in-range floats with the INTEGER digits, so every integral exact
// float buckets with its int64 (5.0 with 5; 1e15 with 1000000000000000).
// ±0.0 normalize to int 0's bucket (collationGroupKey folds -0.0 to "0").
// NaN, non-integrals and out-of-range floats (9e99, 2^63) keep the string
// map — the same group the generic pass files them under.
func typedIntGroupKey(f float64) (int64, bool) {
	if f == 0 {
		return 0, true
	}
	return integralFloatKey(f)
}

// serializedKey computes the row's GROUP BY key the computeGroupByKeyValues
// fast path does: per term, the unwrapped slot value serialized by
// collationGroupKey under the term's (compile-time resolved) collation. The
// partition's scratch buffers are rewritten per row; newGroup clones what it
// retains.
func (p *groupFeedPartition) serializedKey(values []interface{}, rowID int64) (string, []interface{}, []string) {
	n := len(p.keySlots)
	if cap(p.keyParts) < n {
		p.keyParts = make([]string, n)
		p.keyVals = make([]interface{}, n)
	}
	parts, vals := p.keyParts[:n], p.keyVals[:n]
	for i, slot := range p.keySlots {
		var v interface{}
		if slot == feedRowidSlot {
			v = rowID
		} else {
			v = values[slot]
		}
		uv := unwrapCollatedValue(util.UnwrapColumnValue(v))
		if uv == nil {
			parts[i] = "\x00"
			vals[i] = nil
			continue
		}
		parts[i] = collationGroupKey(uv, p.keyColls[i])
		vals[i] = uv
	}
	if n == 1 {
		return parts[0], vals, p.keyColls
	}
	return strings.Join(parts, "\x00"), vals, p.keyColls
}

// newStringGroup registers a string-keyed group, first re-scanning for a
// collation-equal existing group (resolveGroupKeyMiss's merge: values equal
// under a term's collation share a group even when their serialized keys
// differ). Without any collated term the serialized key is exact and the scan
// is skipped. The merged spelling is not indexed — later rows with it re-scan,
// exactly like the generic pass.
func (p *groupFeedPartition) newStringGroup(key string, vals []interface{}, colls []string) int32 {
	if p.hasCollatedTerm() {
		if merged := p.mergeCollatedGroup(vals, colls); merged >= 0 {
			return merged
		}
	}
	g := &groupAccum{key: key, keyVal: append([]interface{}{}, vals...), colls: colls}
	p.initGroupAggs(g)
	idx := int32(len(p.groups))
	p.groups = append(p.groups, g)
	p.strIdx[key] = idx
	return idx
}

// hasCollatedTerm reports whether any GROUP BY term carries a collation (the
// merge precondition resolveGroupKeyMiss checks).
func (p *groupFeedPartition) hasCollatedTerm() bool {
	for _, c := range p.keyColls {
		if c != "" {
			return true
		}
	}
	return false
}

// mergeCollatedGroup returns the first group whose key values compare equal
// under the per-term collations (equivalentGroupKey over the first-seen group
// order), or -1.
func (p *groupFeedPartition) mergeCollatedGroup(vals []interface{}, colls []string) int32 {
	for i, g := range p.groups {
		if p.e.groupKeyValuesEqual(g.keyVal, vals, colls) {
			return int32(i)
		}
	}
	return -1
}

// newTypedGroup registers an integral-spelled group under its int64 bucket
// (its string spelling is unreachable from the string map — see the
// groupFeedPartition map invariant).
func (p *groupFeedPartition) newTypedGroup(bucket int64, v interface{}) int32 {
	g := &groupAccum{keyVal: []interface{}{v}}
	p.initGroupAggs(g)
	idx := int32(len(p.groups))
	p.groups = append(p.groups, g)
	p.typedIdx[bucket] = idx
	return idx
}

// initGroupAggs instantiates one accumulator per output call (the registry
// aggregators are single-use: a fresh set per group, exactly the generic
// pass's per-group evalAggFuncCall).
func (p *groupFeedPartition) initGroupAggs(g *groupAccum) {
	g.insts = make([]groupAggInst, len(p.calls))
	for ci := range p.calls {
		if proto := p.calls[ci].mmProto; proto != nil {
			g.insts[ci].mm = &groupMinMax{isMax: proto.isMax, coll: proto.coll}
		} else {
			g.insts[ci].agg = p.calls[ci].aggFn()
		}
	}
}

// stepGroup folds the row into group idx's accumulators.
func (p *groupFeedPartition) stepGroup(idx int32, values []interface{}, rowID int64) error {
	g := p.groups[idx]
	for ci := range p.calls {
		c := &p.calls[ci]
		if c.mmProto != nil {
			raw := groupFeedArg(values, c.slot, rowID)
			if err := g.insts[ci].mm.step(p.e, raw); err != nil {
				return err
			}
			continue
		}
		if c.countStar {
			if err := g.insts[ci].agg.Step(nil); err != nil {
				return err
			}
			continue
		}
		p.scratch[0] = unwrapCollatedValue(util.UnwrapColumnValue(groupFeedArg(values, c.slot, rowID)))
		if err := g.insts[ci].agg.Step(p.scratch[:1]); err != nil {
			return err
		}
	}
	return nil
}

// groupFeedArg reads a call's raw argument: the loop's rowid for the
// pseudo-column slot, the decoded slot value otherwise (COUNT(*) reads
// nothing).
func groupFeedArg(values []interface{}, slot int, rowID int64) interface{} {
	if slot == feedRowidSlot {
		return rowID
	}
	return values[slot]
}

// finishGroupedAggFeed builds the statement result after the scan: groups in
// sortGroupKeys order (by the first row's key values), each output row built
// per GROUP — term projections emit the retained key value, aggregates emit
// their Final. A zero-group input emits the empty result set the generic pass
// produces.
func (e *SelectEngine) finishGroupedAggFeed(s *sql.SelectStmt, feed *simpleAggFeed, colDefs []sql.ColumnDef) *Result {
	p := feed.group
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	order := make([]int32, len(p.groups))
	for i := range order {
		order[i] = int32(i)
	}
	if len(order) >= 2 {
		sort.SliceStable(order, func(a, b int) bool {
			return groupFeedKeyLess(p.groups[order[a]].keyVal, p.groups[order[b]].keyVal)
		})
	}
	outRows := make([][]interface{}, 0, len(order))
	for _, gi := range order {
		g := p.groups[gi]
		outRow := make([]interface{}, len(p.outs))
		for j, o := range p.outs {
			if o.term >= 0 {
				outRow[j] = g.keyVal[o.term]
				continue
			}
			inst := g.insts[o.call]
			if inst.mm != nil {
				outRow[j] = inst.mm.best
				continue
			}
			v, err := inst.agg.Final()
			if err != nil {
				e.aggPendingErr = err
				return &Result{Error: err}
			}
			outRow[j] = v
		}
		outRows = append(outRows, outRow)
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, nil)
}

// groupFeedKeyLess is sortGroupKeys' comparator over the groups' retained key
// values (util.CompareValues per term, shorter key list first).
func groupFeedKeyLess(a, b []interface{}) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for k := 0; k < n; k++ {
		if c := util.CompareValues(a[k], b[k]); c != 0 {
			return c < 0
		}
	}
	return len(a) < len(b)
}
