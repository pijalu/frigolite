package execexpr

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"math"
	"strconv"
	"strings"
)

func normalizeINItemShape(opIsRow bool, ival interface{}, opArity int) (bool, []interface{}, error) {
	ivRow, ivIsRow := ival.([]interface{})
	if opIsRow && !ivIsRow {
		if opArity != 1 {
			return false, nil, fmt.Errorf("IN(...) element has 1 term - expected %d", opArity)
		}
		ivIsRow = true
		ivRow = []interface{}{ival}
	}
	if !opIsRow && ivIsRow {
		return false, nil, fmt.Errorf("row value misused")
	}
	if opIsRow && ivIsRow && len(ivRow) != opArity {
		return false, nil, fmt.Errorf("IN(...) element has %d terms - expected %d", len(ivRow), opArity)
	}
	return ivIsRow, ivRow, nil
}

// inListRowEqual compares a row-value operand element-wise against an IN-list
// row item (element-wise = with SQLite row-value equality semantics).
func (ev *Evaluator) inListRowEqual(opRow, ivRow []interface{}) bool {
	for i := range opRow {
		l, lColl := extractValue(opRow[i])
		r, _ := extractValue(ivRow[i])
		if lColl != "" {
			if ev.ctx.CompareValuesCollate(util.UnwrapColumnValue(l), util.UnwrapColumnValue(r), lColl) != 0 {
				return false
			}
		} else if util.CompareValues(util.UnwrapColumnValue(l), util.UnwrapColumnValue(r)) != 0 {
			return false
		}
	}
	return true
}

// inListScalarEqual compares a scalar operand against a scalar IN-list item,
// applying the operand's collation, column affinity, or plain value equality.
func (ev *Evaluator) inListScalarEqual(operand, ival interface{}) bool {
	opRaw, opColl := extractValue(operand)
	ivRaw, _ := extractValue(ival)
	lhsAff := util.ColumnAffinity(opRaw)
	opRaw = util.UnwrapColumnValue(opRaw)
	ivRaw = util.UnwrapColumnValue(ivRaw)
	if opColl != "" {
		return ev.ctx.CompareValuesCollate(opRaw, ivRaw, opColl) == 0
	}
	if lhsAff != 0 {
		return compareWithAffinity(opRaw, ivRaw, lhsAff) == 0
	}
	return util.CompareValues(opRaw, ivRaw) == 0
}

// addInt64 adds two int64 values, promoting the result to REAL when the sum
// overflows int64 (SQLite promotes to REAL instead of wrapping).
func addInt64(ia, ib int64) (interface{}, error) {
	sum := ia + ib
	if (ib > 0 && sum < ia) || (ib < 0 && sum > ia) {
		return float64(ia) + float64(ib), nil
	}
	return sum, nil
}

// addFloatValues adds two float-converted operands, keeping an integer result
// when both operands are integer-valued (SQLite: 5.0 + 3.0 is 8). The int64
// conversion of the float operands can overflow at the int64 boundaries;
// detect and promote to REAL (same rule as the pure int64 path).
func addFloatValues(af, bf float64, a, b interface{}) (interface{}, error) {
	if NumericIsInt(a) && NumericIsInt(b) {
		ia := int64(af)
		ib := int64(bf)
		sum := ia + ib
		if (ib > 0 && sum < ia) || (ib < 0 && sum > ia) {
			return NanToNil(af + bf), nil
		}
		return sum, nil
	}
	return NanToNil(af + bf), nil
}

// subInt64 subtracts two int64 values, promoting the result to REAL when the
// difference overflows int64 (SQLite: -9223372036854775808 - 1 is
// 9.22337203685478e+18, not the wrapped MaxInt64).
func subInt64(ia, ib int64) (interface{}, error) {
	diff := ia - ib
	if (ib > 0 && diff > ia) || (ib < 0 && diff < ia) {
		return float64(ia) - float64(ib), nil
	}
	return diff, nil
}

// subFloatValues subtracts two float-converted operands, keeping an integer
// result when both operands are integer-valued. Converting float64 back to
// int64 can overflow at the int64 boundaries; detect overflow like the
// pure-int64 path and promote to REAL.
func subFloatValues(af, bf float64, a, b interface{}) (interface{}, error) {
	if NumericIsInt(a) && NumericIsInt(b) {
		ia := int64(af)
		ib := int64(bf)
		diff := ia - ib
		if (ib > 0 && diff > ia) || (ib < 0 && diff < ia) {
			return NanToNil(af - bf), nil
		}
		return diff, nil
	}
	return NanToNil(af - bf), nil
}

// rowLookupUnqualified resolves an unqualified column name against the
// current row (including rowid/_rowid_/oid) and the outer rows visible for
// correlated references, innermost first.
func (ev *Evaluator) rowLookupUnqualified(name string, row Row) (interface{}, bool) {
	if row != nil {
		if val, ok := row.Get(name); ok {
			return val, true
		}
		// Unqualified rowid/_rowid_/oid resolve to the scanned row's rowid.
		if isRowIDName(name) {
			if val, ok := row.Get("rowid"); ok {
				return val, true
			}
		}
	}
	for _, outer := range ev.ctx.OuterRowsForResolution() {
		if outer == nil {
			continue
		}
		if val, ok := outer.Get(name); ok {
			return val, true
		}
	}
	return nil, false
}

// strictReturningUnqualified resolves an unqualified column reference against
// the RETURNING row in strict mode (rowid aliases included).
func (ev *Evaluator) strictReturningUnqualified(name string, row Row) (interface{}, bool) {
	if isRowIDName(name) && row != nil {
		if val, ok := row.Get("rowid"); ok {
			return val, true
		}
	}
	return nil, false
}

// evalAliasRef resolves an unqualified column reference to a SELECT-list
// output-column alias and evaluates it, guarding against infinite recursion
// when an alias expression refers back to its own name.
func (ev *Evaluator) evalAliasRef(name string, row Row) (interface{}, bool, error) {
	expr, ok := ev.ctx.ResolveAliasRef(name)
	if !ok {
		return nil, false, nil
	}
	lower := strings.ToLower(name)
	if ev.aliasResolving == nil {
		ev.aliasResolving = make(map[string]bool)
	}
	if ev.aliasResolving[lower] {
		return nil, false, nil
	}
	ev.aliasResolving[lower] = true
	val, err := ev.evalExpr(expr, row)
	delete(ev.aliasResolving, lower)
	return val, true, err
}

// inListScanItems iterates an IN list, comparing each item (subquery or
// scalar) against the operand and collecting whether a match was found and
// whether a NULL comparison was seen.
func (ev *Evaluator) inListScanItems(v *sql.InList, row Row, opIsRow bool, opRow []interface{}, opArity int, operand interface{}) (bool, bool, error) {
	found := false
	sawNull := false
	for _, item := range v.List {
		f, n, err := ev.inListItemMatch(v, item, row, opIsRow, opRow, opArity, operand)
		if err != nil {
			return false, false, err
		}
		if n {
			sawNull = true
		}
		if f {
			found = true
			if !opIsRow {
				break
			}
		}
	}
	return found, sawNull, nil
}

// inListItemMatch compares one IN-list item (subquery or scalar) against the
// operand. Returns (matched, sawNull, err).
func (ev *Evaluator) inListItemMatch(v *sql.InList, item sql.Expr, row Row, opIsRow bool, opRow []interface{}, opArity int, operand interface{}) (bool, bool, error) {
	if subq, ok := item.(*sql.Subquery); ok {
		return ev.evalInListSubqueryItem(v, subq, row, opIsRow, opRow, opArity, operand)
	}
	return ev.evalInListScalarItem(v, item, row, opIsRow, opRow, opArity, operand)
}

// inListEmptyResult returns the IN/NOT IN result for an empty list.
func inListEmptyResult(negated bool) interface{} {
	if negated {
		return int64(1)
	}
	return int64(0)
}

// inListNullOperand handles a NULL IN operand: a single-item subquery list
// that returns zero rows behaves like an empty list; anything else leaves the
// result unknown (NULL).
func (ev *Evaluator) inListNullOperand(v *sql.InList, row Row) (interface{}, error) {
	if len(v.List) == 1 {
		if subq, ok := v.List[0].(*sql.Subquery); ok {
			res, err := ev.evalSubqueryRows(subq, row)
			if err != nil {
				return nil, nil
			}
			if len(res) == 0 {
				return inListEmptyResult(v.Negated), nil
			}
		}
	}
	return nil, nil
}

// inListFoundResult computes the IN/NOT IN result from whether a match was
// found and whether a NULL comparison was seen.
func inListFoundResult(negated, found, sawNull bool) interface{} {
	if found {
		if negated {
			return int64(0)
		}
		return int64(1)
	}
	// Not found: a NULL list item makes the result unknown (NULL);
	// otherwise the result is FALSE for IN and TRUE for NOT IN.
	if sawNull {
		return nil
	}
	if negated {
		return int64(1)
	}
	return int64(0)
}

// inListSubqueryRow compares one subquery result row against the IN operand
// (row-value or scalar). Returns (matched, sawNull, err).
func (ev *Evaluator) inListSubqueryRow(v *sql.InList, subq *sql.Subquery, subRow []interface{}, opIsRow bool, opRow []interface{}, opArity int, operand interface{}) (bool, bool, error) {
	if opIsRow {
		if len(subRow) != opArity {
			return false, false, fmt.Errorf("sub-select returns %d columns - expected %d", len(subRow), opArity)
		}
		f, n := ev.subqueryRowMatch(subq, opRow, subRow)
		return f, n, nil
	}
	return ev.subqueryScalarMatch(v, subq, subRow, operand)
}

// subqueryRowMatch compares a row-value operand against one subquery result
// row element-wise with IN affinity rules. Returns (matched, sawNull): a row
// is matched only when every element definitively equals the operand element.
// A NULL element while all earlier elements are equal makes the comparison
// unknown (sawNull), not a match — SQLite row-value semantics: (1,NULL) IN
// (SELECT 1,NULL) is NULL, and a later differing element decides FALSE
// regardless of an earlier NULL ((1,NULL,5) vs (1,2,3) is 0).
func (ev *Evaluator) subqueryRowMatch(subq *sql.Subquery, opRow, subRow []interface{}) (bool, bool) {
	equal := true
	sawRowNull := false
	for i := range opRow {
		l, lColl := extractValue(opRow[i])
		if util.UnwrapColumnValue(l) == nil || util.UnwrapColumnValue(subRow[i]) == nil {
			sawRowNull = true
			continue
		}
		// SQLite collation resolution for IN (subquery): an explicit COLLATE
		// or a column collation on the LHS element wins; otherwise a non-column
		// LHS falls back to the subquery column's collation (expr.c
		// sqlite3ExprCollSeq via the IN comparison). E.g. (NULL,'two','three')
		// IN (SELECT a,b,c FROM t) with c COLLATE nocase compares 'three' vs
		// the stored 'THREE' case-insensitively (nulls2 2.x.1).
		coll := lColl
		if coll == "" && !isColumnValue(opRow[i]) {
			coll = ev.subqueryOutputCollation(subq.Select, i)
		}
		if coll != "" {
			if ev.ctx.CompareValuesCollate(util.UnwrapColumnValue(l), util.UnwrapColumnValue(subRow[i]), coll) != 0 {
				equal = false
				break
			}
		} else {
			lhsAff := util.ColumnAffinity(opRow[i])
			subqAff := ev.subqueryOutputAffinity(subq.Select, i)
			if compareWithAffinity(util.UnwrapColumnValue(l), util.UnwrapColumnValue(subRow[i]), mergeINAffinity(subqAff, lhsAff)) != 0 {
				equal = false
				break
			}
		}
	}
	return equal && !sawRowNull, equal && sawRowNull
}

// subqueryScalarMatch compares a scalar operand against one subquery result
// row (first column) with IN affinity rules. Returns (matched, sawNull, err).
func (ev *Evaluator) subqueryScalarMatch(v *sql.InList, subq *sql.Subquery, subRow []interface{}, operand interface{}) (bool, bool, error) {
	if len(subRow) > 1 {
		return false, false, fmt.Errorf("sub-select returns %d columns - expected 1", len(subRow))
	}
	if len(subRow) == 0 {
		return false, false, nil
	}
	if util.UnwrapColumnValue(subRow[0]) == nil {
		return false, true, nil
	}
	opRaw, opColl := extractValue(operand)
	subRaw, _ := extractValue(subRow[0])
	if opColl != "" {
		if ev.ctx.CompareValuesCollate(util.UnwrapColumnValue(opRaw), util.UnwrapColumnValue(subRaw), opColl) == 0 {
			return true, false, nil
		}
		return false, false, nil
	}
	// A non-column LHS falls back to the subquery column's collation
	// ('three' IN (SELECT c FROM t) with c COLLATE nocase is case-insensitive).
	if !isColumnValue(operand) {
		if coll := ev.subqueryOutputCollation(subq.Select, 0); coll != "" {
			if ev.ctx.CompareValuesCollate(util.UnwrapColumnValue(opRaw), util.UnwrapColumnValue(subRaw), coll) == 0 {
				return true, false, nil
			}
			return false, false, nil
		}
	}
	subqAff := ev.subqueryOutputAffinity(subq.Select, 0)
	lhsAff := util.ColumnAffinity(operand)
	if compareWithAffinity(operand, subRow[0], mergeINAffinity(subqAff, lhsAff)) == 0 {
		return true, false, nil
	}
	return false, false, nil
}

// castToInteger converts a value to INTEGER like SQLite's CAST(x AS
// INTEGER): int64/float64 convert directly, text parses only the leading
// integer prefix (an exponent or decimal part is ignored, e.g.
// CAST('123e+5' AS INTEGER) is 123), and out-of-range values clamp.
func castToInteger(val interface{}) (interface{}, error) {
	switch x := val.(type) {
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case string:
		return castStringToInteger(x), nil
	default:
		return int64(0), nil
	}
}

// castStringToInteger parses a string's numeric prefix as int64, clamping
// out-of-range values to the int64 limits (SQLite CAST semantics).
func castStringToInteger(s string) interface{} {
	t := strings.TrimSpace(s)
	end := 0
	if end < len(t) && (t[end] == '+' || t[end] == '-') {
		end++
	}
	for end < len(t) && t[end] >= '0' && t[end] <= '9' {
		end++
	}
	if end > 0 {
		if i, err := strconv.ParseInt(t[:end], 10, 64); err == nil {
			return i
		}
		// Out of int64 range: SQLite clamps to the max/min integer.
		if t[0] == '-' {
			return int64(math.MinInt64)
		}
		return int64(math.MaxInt64)
	}
	return int64(0)
}

// castToReal converts a value to REAL like SQLite's CAST(x AS REAL):
// int64/float64 convert directly, text parses as a number accepting a leading
// numeric prefix and ignoring trailing garbage (sqlite3AtoF), and non-numeric
// text becomes 0.
func castToReal(val interface{}) (interface{}, error) {
	switch x := val.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case string:
		if f, ok := parseNumericPrefix(x); ok {
			return f, nil
		}
		return float64(0), nil
	default:
		return float64(0), nil
	}
}

// castToText converts a value to TEXT like SQLite's CAST(x AS TEXT): a blob
// decodes as UTF-8 text (byte copy), a REAL renders like SQLite
// (CAST(123.0 AS TEXT) is '123.0'), and other values use their text form.
func castToText(val interface{}) (interface{}, error) {
	if b, ok := val.([]byte); ok {
		return string(b), nil
	}
	if f, ok := val.(float64); ok {
		return util.FormatSQLiteReal(f), nil
	}
	return fmt.Sprintf("%v", val), nil
}

// castToBlob converts a value to BLOB like SQLite's CAST(x AS BLOB): a TEXT
// value becomes its byte content, other types become their canonical text
// form's bytes (CAST(123 AS BLOB) is X'313233'), and a blob passes through.
func castToBlob(val interface{}) (interface{}, error) {
	switch x := val.(type) {
	case []byte:
		return x, nil
	case string:
		return []byte(x), nil
	case int64:
		return []byte(fmt.Sprintf("%d", x)), nil
	case float64:
		return []byte(util.FormatSQLiteReal(x)), nil
	default:
		return []byte(fmt.Sprintf("%v", val)), nil
	}
}

// castToNumeric converts a value to NUMERIC like SQLite's CAST(x AS
// NUMERIC): text coerces to a number (non-numeric text becomes 0), a float64
// input stays float64 (CAST(4.0 AS NUMERIC) is 4.0), and whole-number text
// parses to INTEGER (CAST('123e+5' AS NUMERIC) is 12300000).
func castToNumeric(val interface{}) (interface{}, error) {
	switch x := val.(type) {
	case int64:
		return x, nil
	case float64:
		return x, nil
	case string:
		t := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(t, 10, 64); err == nil {
			return i, nil
		}
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			if f == float64(int64(f)) {
				return int64(f), nil
			}
			return f, nil
		}
		return int64(0), nil
	default:
		return int64(0), nil
	}
}
