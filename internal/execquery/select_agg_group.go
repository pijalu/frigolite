// Package exec implements query execution.
package execquery

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"

	"bytes"
	"strconv"
)

// GROUP BY key partitioning (split from select_agg.go for file-size
// hygiene): partitioning rows by their GROUP BY key and merging keys that
// compare equal under a term's collation.

// partitionByGroupKey partitions rowMaps by their GROUP BY key, preserving
// first-seen order in keyOrder. It returns the per-key row slices, the per-key
// evaluated key values, and the ordered list of keys.
func (e *SelectEngine) partitionByGroupKey(groupBy []sql.Expr, rowMaps []RowMap) (map[string][]RowMap, map[string][]interface{}, []string) {
	groups := make(map[string][]RowMap)
	keyVals := make(map[string][]interface{})
	var keyOrder []string
	for _, row := range rowMaps {
		key, vals, colls := e.computeGroupByKeyValues(groupBy, row)
		if _, exists := groups[key]; !exists {
			// Values equal under a term's collation share a group even when
			// their serialized keys differ (collate5-4.2: '1' and '1.0'
			// under a COLLATE NUMERIC column — the sorter compares with the
			// per-term collation, so the textual keys need not match).
			// Without any collated term the scan cannot merge: the key
			// serializer is %v-faithful for every value type, so a textual
			// miss already proves the values differ. Skipping the linear
			// scan keeps uncollated GROUP BY at one map lookup per row
			// (a 1000-group key otherwise cost O(groups) compares per new
			// key and ~12% of the group-phase profile).
			collated := false
			for _, c := range colls {
				if c != "" {
					collated = true
					break
				}
			}
			if collated {
				if merged := e.equivalentGroupKey(keyOrder, keyVals, vals, colls); merged != "" {
					key = merged
				}
			}
		}
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
			// vals is the key computation's scratch buffer (reused for the
			// next row): clone it for the group's retention.
			keyVals[key] = append([]interface{}{}, vals...)
		}
		groups[key] = append(groups[key], row)
	}
	return groups, keyVals, keyOrder
}

// equivalentGroupKey returns an existing group key whose key values compare
// equal to vals under the per-term collations, or "". Terms without a
// collation must match textually (their serialized keys are exact).
func (e *SelectEngine) equivalentGroupKey(keyOrder []string, keyVals map[string][]interface{}, vals []interface{}, colls []string) string {
	for _, k := range keyOrder {
		if e.groupKeyValuesEqual(keyVals[k], vals, colls) {
			return k
		}
	}
	return ""
}

// groupKeyValuesEqual reports whether vals compare equal to existing under
// the per-term collations. A collated term compares via the collation; an
// uncollated term must match textually (its serialized key is exact).
// The uncollated comparison is typed with a %v-spelling fallback: the
// historical fmt.Sprintf("%v") pair per value per row dominated the
// group-phase CPU profile and allocated on every comparison.
func (e *SelectEngine) groupKeyValuesEqual(existing, vals []interface{}, colls []string) bool {
	if len(existing) != len(vals) {
		return false
	}
	for i := range vals {
		coll := ""
		if i < len(colls) {
			coll = colls[i]
		}
		uv := util.UnwrapColumnValue(vals[i])
		ev := util.UnwrapColumnValue(existing[i])
		if coll == "" {
			if !groupKeyScalarEqual(uv, ev) {
				return false
			}
			continue
		}
		if c := e.ctx.CompareValuesCollate(uv, ev, coll); c != 0 {
			return false
		}
	}
	return true
}

// groupKeyScalarEqual reports whether two unwrapped scalars compare equal
// under fmt's %v spelling — the semantics the serialized group keys are
// built on (int64(5) and float64(5.0) share the spelling "5" and must group
// together; 5.5 does not). Same-type pairs compare directly; mixed pairs
// fall back to the spelling strings.
func groupKeyScalarEqual(a, b interface{}) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case float64:
			return strconv.FormatInt(av, 10) == strconv.FormatFloat(bv, 'g', -1, 64)
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int64:
			return strconv.FormatFloat(av, 'g', -1, 64) == strconv.FormatInt(bv, 10)
		}
	case string:
		bs, ok := b.(string)
		return ok && av == bs
	case bool:
		bb, ok := b.(bool)
		return ok && av == bb
	case []byte:
		bb, ok := b.([]byte)
		return ok && bytes.Equal(av, bb)
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
