// Package exec implements query execution.
package execquery

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
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
			if merged := e.equivalentGroupKey(keyOrder, keyVals, vals, colls); merged != "" {
				key = merged
			}
		}
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
			keyVals[key] = vals
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
		existing := keyVals[k]
		if len(existing) != len(vals) {
			continue
		}
		equal := true
		for i := range vals {
			coll := ""
			if i < len(colls) {
				coll = colls[i]
			}
			uv := util.UnwrapColumnValue(vals[i])
			ev := util.UnwrapColumnValue(existing[i])
			if coll == "" {
				if fmt.Sprintf("%v", uv) != fmt.Sprintf("%v", ev) {
					equal = false
					break
				}
				continue
			}
			if c := e.ctx.CompareValuesCollate(uv, ev, coll); c != 0 {
				equal = false
				break
			}
		}
		if equal {
			return k
		}
	}
	return ""
}
