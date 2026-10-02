// Package exec implements query execution.
package execquery

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// GROUP BY key partitioning (split from select_agg.go for file-size
// hygiene): partitioning rows by their GROUP BY key and merging keys that
// compare equal under a term's collation.

// groupByKeywordName reports whether a bare identifier is one of the keyword
// names evalColumnRef resolves as literals BEFORE the row lookup (TRUE, FALSE,
// CURRENT_TIME, CURRENT_DATE, CURRENT_TIMESTAMP). Such terms must take the
// generic evaluation path even when a column of the same name exists.
func groupByKeywordName(name string) bool {
	switch len(name) {
	case 4: // TRUE
		return strings.EqualFold(name, "TRUE")
	case 5: // FALSE
		return strings.EqualFold(name, "FALSE")
	case 12: // CURRENT_TIME, CURRENT_DATE
		return strings.EqualFold(name, "CURRENT_TIME") || strings.EqualFold(name, "CURRENT_DATE")
	case 17: // CURRENT_TIMESTAMP
		return strings.EqualFold(name, "CURRENT_TIMESTAMP")
	}
	return false
}

// groupByFastRef reports whether expr is a plain unqualified column reference
// whose value EvalExpr returns unchanged from the row: evalExpr peels
// ParenExpr wrappers and evalColumnRef resolves an unqualified reference to
// row.Get's value as its first resolution step, so a hit means the fast path
// and the generic path would produce the identical (value, collation) pair
// after the unwrap. Returns nil when the term needs generic evaluation
// (qualified refs, "*", keyword names, and every non-reference expression).
func groupByFastRef(expr sql.Expr) (*sql.ColumnRef, bool) {
	for {
		switch v := expr.(type) {
		case *sql.ParenExpr:
			expr = v.Expr
		case *sql.ColumnRef:
			if v.Table != "" || v.Name == "*" || groupByKeywordName(v.Name) {
				return nil, false
			}
			return v, true
		default:
			return nil, false
		}
	}
}

// partitionByGroupKey partitions rows by their GROUP BY key, preserving
// first-seen order in keyOrder. It returns the per-key row slices, the per-key
// evaluated key values, and the ordered list of keys.
func (e *SelectEngine) partitionByGroupKey(groupBy []sql.Expr, rows []Row) (map[string][]Row, map[string][]interface{}, []string) {
	groups := make(map[string][]Row)
	keyVals := make(map[string][]interface{})
	var keyOrder []string
	for _, row := range rows {
		key, vals, colls := e.computeGroupByKeyValues(groupBy, row)
		group, exists := groups[key]
		if !exists {
			key, group = e.resolveGroupKeyMiss(groups, &keyOrder, keyVals, key, vals, colls)
		}
		groups[key] = append(group, row)
	}
	return groups, keyVals, keyOrder
}

// resolveGroupKeyMiss handles a row whose serialized GROUP BY key has no
// group yet. Values equal under a term's collation share a group even when
// their serialized keys differ (collate5-4.2: '1' and '1.0' under a COLLATE
// NUMERIC column — the sorter compares with the per-term collation, so the
// textual keys need not match). Without any collated term the scan cannot
// merge: the key serializer is %v-faithful for every value type, so a
// textual miss already proves the values differ. Skipping the linear scan
// keeps uncollated GROUP BY at one map lookup per row (a 1000-group key
// otherwise cost O(groups) compares per new key and ~12% of the group-phase
// profile). A group that stays new is registered in keyOrder/keyVals and
// returns a nil row slice.
func (e *SelectEngine) resolveGroupKeyMiss(groups map[string][]Row, keyOrder *[]string, keyVals map[string][]interface{}, key string, vals []interface{}, colls []string) (string, []Row) {
	collated := false
	for _, c := range colls {
		if c != "" {
			collated = true
			break
		}
	}
	if collated {
		if merged := e.equivalentGroupKey(*keyOrder, keyVals, vals, colls); merged != "" {
			return merged, groups[merged]
		}
	}
	*keyOrder = append(*keyOrder, key)
	// vals is the key computation's scratch buffer (reused for the next
	// row): clone it for the group's retention.
	keyVals[key] = append([]interface{}{}, vals...)
	return key, nil
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
		return int64GroupKeyEqual(av, b)
	case float64:
		return float64GroupKeyEqual(av, b)
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

// int64GroupKeyEqual compares an int64 key value against b: int64s directly,
// float64s numerically (SQLite compares INTEGER and REAL as numbers — 1e15
// equals 1000000000000000); other types fall back to the spelling
// comparison, matching the serialized keys.
func int64GroupKeyEqual(av int64, b interface{}) bool {
	switch bv := b.(type) {
	case int64:
		return av == bv
	case float64:
		if i, ok := integralFloatKey(bv); ok {
			return av == i
		}
	}
	return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", b)
}

// float64GroupKeyEqual compares a float64 key value against b: float64s
// directly, int64s numerically (same integralFloatKey rule as the serialized
// keys); other types fall back to the spelling comparison.
func float64GroupKeyEqual(av float64, b interface{}) bool {
	switch bv := b.(type) {
	case float64:
		return av == bv
	case int64:
		if i, ok := integralFloatKey(av); ok {
			return bv == i
		}
	}
	return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", b)
}
