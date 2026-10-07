package execquery

import (
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// Typed aggregate feed (the covered-WHERE scan lane): under a covered seek
// plan the rowid-range loop's ONLY per-row consumers are the feed's own
// aggregate arguments, so the decode pipeline (values buffer, boxing,
// StructRow assembly) and the boxed feed step collapse into one
// straight-from-payload walk: each call's column serial type is read off the
// record and an int64/float64 feeds the registry accumulator's unboxed
// surface (function.TypedSumStep / function.TypedCountStep) — the same
// accumulation state the boxed Step reaches for that input. Non-numeric
// inputs (text, blob) fall back to the boxed Step for that one value; the
// accumulators mix typed and boxed steps state-identically. Every semantic
// the compiled feed guards preserve (NULL skipping, ADD COLUMN defaults,
// INTEGER PRIMARY KEY rowid-alias substitution, dropped-column disk layout,
// SUM integer/REAL promotion and "integer overflow") is mirrored here; any
// shape the lane cannot express keeps the decoded feed path.

// typedAggCall is one compiled call's direct-from-payload accumulator.
type typedAggCall struct {
	agg       function.Aggregator // the call's registry aggregator (boxed fallback)
	sum       function.TypedSumStep
	counter   function.TypedCountStep
	slot      int  // colDefs slot (>=0) or feedRowidSlot
	diskSlot  int  // the slot's on-disk record position (-1: rowid / no stored column)
	countStar bool // COUNT(*): every row counts
	ipkAlias  bool // the slot is the INTEGER PRIMARY KEY rowid-alias column
	hasDef    bool // the slot's column has an ADD COLUMN DEFAULT (applies past the record width)
	defVal    interface{}
}

// typedAggLane is the compiled direct-feed plan for a statement's simple
// aggregate feed.
type typedAggLane struct {
	calls []typedAggCall
}

// compileTypedAggLane builds the direct-feed lane for feed's calls over
// colDefs, or nil when any call cannot feed unboxed. The disk layout mirrors
// the decode pipeline's: on-disk record positions are colDefs order minus
// dropped columns (shiftDroppedColumns's contract).
func (e *SelectEngine) compileTypedAggLane(feed *simpleAggFeed, colDefs []sql.ColumnDef) *typedAggLane {
	if feed == nil || feed.group != nil || len(feed.calls) == 0 {
		return nil
	}
	lane := &typedAggLane{calls: make([]typedAggCall, len(feed.calls))}
	diskOf := diskSlotRanks(colDefs)
	for ci := range feed.calls {
		if !e.compileTypedAggCall(&lane.calls[ci], &feed.calls[ci], colDefs, diskOf) {
			return nil
		}
	}
	return lane
}

// diskSlotRanks maps each colDefs index to its on-disk record position (rank
// among non-dropped columns); dropped columns get -1.
func diskSlotRanks(colDefs []sql.ColumnDef) []int {
	diskOf := make([]int, len(colDefs))
	disk := 0
	for i := range colDefs {
		if colDefs[i].Dropped {
			diskOf[i] = -1
			continue
		}
		diskOf[i] = disk
		disk++
	}
	return diskOf
}

// compileTypedAggCall configures one call's direct-feed state from its
// compiled feed call. ok=false reports the call cannot feed unboxed (the
// whole lane stays off). See typedAggCall's field docs for the slot/disk/
// default semantics.
func (e *SelectEngine) compileTypedAggCall(tc *typedAggCall, c *aggFeedCall, colDefs []sql.ColumnDef, diskOf []int) bool {
	tc.agg = c.agg
	tc.slot = c.slot
	tc.countStar = c.countStar
	switch {
	case c.countStar:
		counter, ok := c.agg.(function.TypedCountStep)
		if !ok {
			return false // COUNT(*) with a non-counter aggregator: keep boxed
		}
		tc.counter = counter
		tc.diskSlot = -1
		return true
	case c.slot == feedRowidSlot:
		tc.diskSlot = -1
	default:
		tc.diskSlot = diskOf[c.slot]
		tc.ipkAlias = isIPKRowidAliasCol(colDefs[c.slot])
	}
	// Typed surfaces: the SUM family's unboxed accumulator and the counter's
	// unboxed row feed. Either may be absent (a future aggregate in the
	// compile family) — that call stays boxed.
	tc.sum, _ = c.agg.(function.TypedSumStep)
	tc.counter, _ = c.agg.(function.TypedCountStep)
	if tc.sum == nil && tc.counter == nil {
		return false
	}
	if c.slot < 0 {
		return true
	}
	cd := &colDefs[c.slot]
	if cd.Generated != nil {
		return false // generated columns keep the decoded path
	}
	if cd.Default == nil {
		return true
	}
	dv, err := e.defaultEvalFor(cd)
	if err != nil {
		return false // default eval error: the decoded path surfaces it per row
	}
	tc.hasDef, tc.defVal = true, dv
	return true
}

// defaultEvalFor evaluates one ADD COLUMN default the way
// applyColumnDefaults does (empty row, then the column's declared affinity).
func (e *SelectEngine) defaultEvalFor(cd *sql.ColumnDef) (interface{}, error) {
	dv, err := e.ctx.EvalExpr(cd.Default, nil)
	if err != nil {
		return nil, err
	}
	return util.ApplyColumnAffinity(dv, cd.Type), nil
}

// stepDirect feeds one row's record (payload + pre-parsed serial types) into
// the lane's accumulators without decoding any interface value. err
// propagates the aggregators' errors ("integer overflow" surfaces at Final;
// a Step error here is the decoded path's step error too). offs is the
// header parse's per-slot span table (parseRecordSerialTypesOffsetsInto) —
// nil falls back to the generic header walk per call.
func (l *typedAggLane) stepDirect(payload []byte, dataStart int, serialTypes []uint64, offs []int32, rowID int64) error {
	for ci := range l.calls {
		if err := l.calls[ci].stepCall(payload, dataStart, serialTypes, offs, rowID); err != nil {
			return err
		}
	}
	return nil
}

// stepCall feeds one call's input for the row at payload/dataStart. The
// classification mirrors the decoded feed exactly:
//   - no stored column (rowid slot / COUNT(*)) or a record too short for the
//     slot: the rowid feeds numerically; a column past the record width takes
//     its ADD COLUMN DEFAULT (applyColumnDefaults), else it is NULL —
//     SUM-family steps skip, the counter skips a NULL column argument;
//   - a stored NULL skips (defaults apply only past the record width), except
//     the INTEGER PRIMARY KEY alias whose stored NULL substitutes the rowid;
//   - integer/float serial types feed the unboxed accumulators;
//   - text/blob serial types decode and step through the boxed path (the
//     SUM family's numeric-text classification lives there).
func (c *typedAggCall) stepCall(payload []byte, dataStart int, serialTypes []uint64, offs []int32, rowID int64) error {
	if c.countStar {
		c.counter.CountRow()
		return nil
	}
	if c.diskSlot < 0 {
		// The rowid pseudo-column: a plain int64 input to the feed.
		return c.feedInt(rowID)
	}
	// Walk the record to the slot's value. A record shorter than the slot
	// (rows written before ALTER TABLE ADD COLUMN) takes the default.
	if c.diskSlot >= len(serialTypes) {
		return c.feedDefault()
	}
	var st uint64
	var data []byte
	var ok bool
	if offs != nil {
		st, data, ok = c.resolveSlotOffs(payload, dataStart, offs, serialTypes)
	} else {
		st, data, ok = c.resolveSlot(payload, dataStart, serialTypes)
	}
	if !ok {
		return nil // corrupt record: the decoded path leaves the slot NULL
	}
	switch {
	case st == storage.SerialNull:
		if c.ipkAlias {
			return c.feedInt(rowID)
		}
		return nil // stored NULL: skipped by both the SUM family and the counter
	case st >= storage.SerialInt8 && st <= storage.SerialInt64:
		v, _ := storage.DecodeSerialInt64(st, data)
		return c.feedInt(v)
	case st == storage.SerialFloat:
		return c.feedFloat(storage.DecodeSerialFloat64(data))
	default:
		return c.feedBoxed(storage.DecodeRecordValue(st, data))
	}
}

// resolveSlot walks the record header to the call's on-disk slot and returns
// its serial type and value bytes. ok=false reports a corrupt or truncated
// record (the decoded decode path leaves the slot NULL in exactly those
// cases).
func (c *typedAggCall) resolveSlot(payload []byte, dataStart int, serialTypes []uint64) (st uint64, data []byte, ok bool) {
	pos := dataStart
	for i := 0; i < c.diskSlot; i++ {
		n, err := storage.SerialTypeLength(serialTypes[i])
		if err != nil || pos+int(n) > len(payload) {
			return 0, nil, false
		}
		pos += int(n)
	}
	n, err := storage.SerialTypeLength(serialTypes[c.diskSlot])
	if err != nil || pos+int(n) > len(payload) {
		return 0, nil, false
	}
	return serialTypes[c.diskSlot], payload[pos : pos+int(n)], true
}

// resolveSlotOffs is resolveSlot over the header parse's span table: the
// slot's value bytes are payload[dataStart+offs[i] : dataStart+offs[i+1]] —
// O(1), no preceding-types walk. Not-ok parity with the walk: a poisoned
// span (corrupt serial type, itself or upstream), or an end past the payload
// (truncated body) reports false — the slot reads NULL in both engines.
func (c *typedAggCall) resolveSlotOffs(payload []byte, dataStart int, offs []int32, serialTypes []uint64) (st uint64, data []byte, ok bool) {
	if c.diskSlot+1 >= len(offs) {
		return 0, nil, false
	}
	start, end := offs[c.diskSlot], offs[c.diskSlot+1]
	if start < 0 || end < 0 {
		return 0, nil, false // corrupt serial type on this slot or upstream
	}
	vs, ve := dataStart+int(start), dataStart+int(end)
	if vs > ve || ve > len(payload) {
		return 0, nil, false // truncated body: the slot reads NULL
	}
	return serialTypes[c.diskSlot], payload[vs:ve], true
}

// feedDefault feeds the column's ADD COLUMN DEFAULT (past the record width).
func (c *typedAggCall) feedDefault() error {
	if !c.hasDef || c.defVal == nil {
		return nil
	}
	switch v := c.defVal.(type) {
	case int64:
		return c.feedInt(v)
	case float64:
		return c.feedFloat(v)
	default:
		return c.feedBoxed(v)
	}
}

// feedInt feeds one integer input through the typed accumulator when the
// call has one, else the boxed Step (state-identical for an int64).
func (c *typedAggCall) feedInt(v int64) error {
	if c.sum != nil {
		return c.sum.StepInt64(v)
	}
	return c.feedBoxed(v)
}

// feedFloat feeds one real input through the typed accumulator.
func (c *typedAggCall) feedFloat(v float64) error {
	if c.sum != nil {
		return c.sum.StepFloat64(v)
	}
	return c.feedBoxed(v)
}

// feedBoxed routes one value through the boxed Step (the decoded feed's
// argument unwrapping mirrors evalAggCallArgs: unwrap the column value, then
// the collation wrapper).
func (c *typedAggCall) feedBoxed(v interface{}) error {
	if c.counter != nil {
		c.counter.CountRow()
		return nil
	}
	if c.agg == nil {
		return nil
	}
	return c.agg.Step([]interface{}{unwrapCollatedValue(util.UnwrapColumnValue(v))})
}
