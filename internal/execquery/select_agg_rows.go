package execquery

// Positional row plumbing for the aggregate passes (select.c's aggregate
// machinery reads every input row exactly once per aggregate step, so the
// rows ride through the Row interface as index-addressed StructRow clones;
// name-keyed RowMaps materialize only where a consumer demands them — the
// window pass, the per-group representative map, and the lazily-read
// nested-aggregate / correlated-outer sets).

// rowMapRows views a []RowMap slice as []Row without copying the rows: a
// RowMap IS a Row, so the conversion is one interface word per slot (one
// slice allocation, no per-row work). Get dispatches through the same map
// lookup the []RowMap consumer would have performed.
func rowMapRows(maps []RowMap) []Row {
	rows := make([]Row, len(maps))
	for i, m := range maps {
		rows[i] = m
	}
	return rows
}

// rowsToRowMaps materializes a positional row slice as name-keyed RowMaps.
// StructRow rows convert through StructRowToMap (the scan's retention
// discipline: values shared, blobs deep-copied); RowMap rows pass through
// by identity. Anything else is an internal contract violation — the two
// constructors above are the only producers of aggregate row slices.
func rowsToRowMaps(rows []Row) []RowMap {
	maps := make([]RowMap, len(rows))
	for i, r := range rows {
		maps[i] = rowToRowMap(r)
	}
	return maps
}

// rowToRowMap materializes one positional row as a RowMap (see
// rowsToRowMaps).
func rowToRowMap(r Row) RowMap {
	switch v := r.(type) {
	case nil:
		return nil
	case *StructRow:
		return StructRowToMap(v)
	case RowMap:
		return v
	default:
		panic("execquery: unexpected Row implementation in the aggregate row set")
	}
}

// reverseRows reverses a positional row slice in place (the
// reverse_unordered_selects mirror of reverseRowMaps).
func reverseRows(rows []Row) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}
