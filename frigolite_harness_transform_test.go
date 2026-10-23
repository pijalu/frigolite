// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// The Tcl postprocessing a fixture records for a step's result.
//
// The converter cannot evaluate a postprocessing that depends on a query's
// rows — they only exist at run time — so it records what the Tcl code does
// to the result ("resultTransform") and the harness applies it to the rows
// it rendered.
//
// where7.test's count_steps_sort helper is the motivating case:
//
//	proc count_steps_sort {sql} {
//	  set r [lsort -integer [db eval $sql]]
//	  return "$r scan [db status step] sort [db status sort]"
//	}
//
// The expectation of the test is the *sorted* row list, so the rendered rows
// must be sorted before they are compared.

// applyResultTransform applies the recorded Tcl postprocessing to a result
// and returns its rendered form.
func applyResultTransform(res *Result, transform, nullToken string) (string, error) {
	switch {
	case transform == "":
		return flattenResultNull(res, nullToken), nil
	case transform == "tolower":
		return strings.ToLower(flattenResultNull(res, nullToken)), nil
	case strings.HasPrefix(transform, "lreplace:"):
		got := flattenResultNull(res, nullToken)
		parts := strings.SplitN(transform, ":", 4)
		if len(parts) != 4 {
			return "", fmt.Errorf("invalid lreplace transform %q", transform)
		}
		start, e1 := strconv.Atoi(parts[1])
		end, e2 := strconv.Atoi(parts[2])
		fields := strings.Fields(got)
		if e1 != nil || e2 != nil || start != end || start < 0 || start >= len(fields) {
			return "", fmt.Errorf("invalid lreplace transform range %q for result %q", transform, got)
		}
		fields[start] = parts[3]
		return strings.Join(fields, " "), nil
	case strings.HasPrefix(transform, "lsort:"):
		if err := sortResultRows(res, transform); err != nil {
			return "", err
		}
		return flattenResultNull(res, nullToken), nil
	}
	return "", fmt.Errorf("unsupported result transform %q", transform)
}

// sortResultRows orders the result rows the way Tcl's `lsort` does.
//
// The rows of a query are the elements of the list `db eval` returns, so the
// comparison uses the row's list string (or, with `-index`, one element of
// it). The modes mirror Tcl's: -ascii (the default), -integer, -real and
// -dictionary.
func sortResultRows(res *Result, transform string) error {
	parts := strings.Split(transform, ":")
	if len(parts) < 2 {
		return fmt.Errorf("invalid lsort transform %q", transform)
	}
	mode := parts[1]
	decreasing := false
	unique := false
	index := -1
	for idx := 2; idx < len(parts); idx++ {
		switch parts[idx] {
		case "decreasing":
			decreasing = true
		case "unique":
			unique = true
		case "index":
			if idx+1 >= len(parts) {
				return fmt.Errorf("invalid lsort transform %q", transform)
			}
			n, err := strconv.Atoi(parts[idx+1])
			if err != nil {
				return fmt.Errorf("invalid lsort transform %q", transform)
			}
			index = n
			idx++
		default:
			return fmt.Errorf("invalid lsort transform %q", transform)
		}
	}

	rows := res.Rows
	keys := make([]string, len(rows))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, v := range row {
			if v == nil {
				cells[j] = "NULL"
				continue
			}
			cells[j] = formatSQLiteValue(v)
		}
		if index >= 0 {
			if index < len(cells) {
				keys[i] = cells[index]
			} else {
				keys[i] = ""
			}
			continue
		}
		keys[i] = tclListString(cells)
	}

	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ka, kb := keys[order[a]], keys[order[b]]
		less := tclSortLess(mode, ka, kb)
		if decreasing {
			return !less && ka != kb
		}
		return less
	})
	sortedRows := make([][]interface{}, len(rows))
	for i, idx := range order {
		sortedRows[i] = rows[idx]
	}
	if unique {
		sortedRows = uniqueResultRows(sortedRows, keys, order)
	}
	res.Rows = sortedRows
	return nil
}

// uniqueResultRows removes the duplicates of a sorted row list (Tcl's
// `lsort -unique` keeps the first of each run of equal elements).
func uniqueResultRows(rows [][]interface{}, keys []string, order []int) [][]interface{} {
	out := make([][]interface{}, 0, len(rows))
	var lastKey string
	for i, idx := range order {
		if i > 0 && keys[idx] == lastKey {
			continue
		}
		lastKey = keys[idx]
		out = append(out, rows[idx])
	}
	return out
}

// tclSortLess compares two values the way Tcl's `lsort` does in the given
// mode. A value that does not parse as a number sorts before one that does
// in the numeric modes (Tcl raises an error; the harness reports the
// mismatch instead).
func tclSortLess(mode, a, b string) bool {
	switch mode {
	case "integer":
		ia, oka := harnessTclInteger(a)
		ib, okb := harnessTclInteger(b)
		switch {
		case oka && okb:
			return ia < ib
		case oka:
			return true
		case okb:
			return false
		}
		return a < b
	case "real":
		fa, oka := harnessTclReal(a)
		fb, okb := harnessTclReal(b)
		switch {
		case oka && okb:
			return fa < fb
		case oka:
			return true
		case okb:
			return false
		}
		return a < b
	case "dictionary":
		return strings.ToLower(a) < strings.ToLower(b)
	default: // -ascii
		return a < b
	}
}

// harnessTclInteger parses a Tcl integer ("0x10", "010", " 42 "); ok is false
// when the text is not an integer.
func harnessTclInteger(s string) (int64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	neg := false
	switch t[0] {
	case '+':
		t = t[1:]
	case '-':
		neg = true
		t = t[1:]
	}
	base := 10
	switch {
	case strings.HasPrefix(t, "0x"), strings.HasPrefix(t, "0X"):
		base, t = 16, t[2:]
	case strings.HasPrefix(t, "0b"), strings.HasPrefix(t, "0B"):
		base, t = 2, t[2:]
	case strings.HasPrefix(t, "0o"), strings.HasPrefix(t, "0O"):
		base, t = 8, t[2:]
	}
	if t == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(t, base, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// harnessTclReal parses a Tcl real number; ok is false when the text is not
// a number.
func harnessTclReal(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// TestApplyResultTransformSortsRows pins the Tcl `lsort` postprocessing the
// converter records for a query whose rows are sorted by the test helper
// (where7.test's count_steps_sort).
//
// The rows of a query are the elements of the list `db eval` returns, so the
// sort reorders the rows, not the cells of a row.
func TestApplyResultTransformSortsRows(t *testing.T) {
	res := &Result{
		Columns: []string{"a"},
		Rows:    [][]interface{}{{int64(100)}, {int64(6)}, {int64(18)}},
	}
	got, err := applyResultTransform(res, "lsort:integer", "")
	if err != nil {
		t.Fatalf("applyResultTransform() error: %v", err)
	}
	if got != "6 18 100" {
		t.Fatalf("applyResultTransform() = %q, want %q", got, "6 18 100")
	}
}

// TestApplyResultTransformSortsRowsDecreasing pins the -decreasing flag.
func TestApplyResultTransformSortsRowsDecreasing(t *testing.T) {
	res := &Result{
		Columns: []string{"a"},
		Rows:    [][]interface{}{{int64(6)}, {int64(100)}, {int64(18)}},
	}
	got, err := applyResultTransform(res, "lsort:integer:decreasing", "")
	if err != nil {
		t.Fatalf("applyResultTransform() error: %v", err)
	}
	if got != "100 18 6" {
		t.Fatalf("applyResultTransform() = %q, want %q", got, "100 18 6")
	}
}

// TestApplyResultTransformSortsRowsByIndex pins the -index option: the
// comparison uses one element of each row.
func TestApplyResultTransformSortsRowsByIndex(t *testing.T) {
	res := &Result{
		Columns: []string{"a", "b"},
		Rows:    [][]interface{}{{int64(1), int64(9)}, {int64(1), int64(4)}},
	}
	got, err := applyResultTransform(res, "lsort:ascii:index:1", "")
	if err != nil {
		t.Fatalf("applyResultTransform() error: %v", err)
	}
	if got != "1 4 1 9" {
		t.Fatalf("applyResultTransform() = %q, want %q", got, "1 4 1 9")
	}
}

// TestApplyResultTransformRejectsUnknown pins that an unknown transform is
// an error, not a silent pass.
func TestApplyResultTransformRejectsUnknown(t *testing.T) {
	res := &Result{Columns: []string{"a"}, Rows: [][]interface{}{{int64(1)}}}
	if _, err := applyResultTransform(res, "nonsense", ""); err == nil {
		t.Fatal("applyResultTransform() accepted an unknown transform")
	}
}

// harnessSortMarker reports whether the statement sorted: the testfixture's
// cksort helper (collate4.test) appends "sort" or "nosort" to the result,
// depending on the test build's sort counter.
func harnessSortMarker(db *DB) string {
	if db.engine.StmtStatusSortPeek() > 0 {
		return "sort"
	}
	return "nosort"
}

// applyTclTransform applies the Tcl procedure the fixture records for a
// step's result.
//
// A procedure that receives a runtime query value post-processes the rows
// the query returns: fts3expr3.test's strip_phrase_data turns the rendered
// parse tree into the tree without the phrase data ("PHRASE 3 0 ab" becomes
// "P ab"). The procedure body travels with the step, so the harness
// evaluates it with the converter's Tcl interpreter.
func applyTclTransform(step TestStep, value string) (string, error) {
	if step.TransformProc == "" {
		return value, nil
	}
	interp := tcl.NewInterp()
	interp.LoadProcedures([]tcl.Proc{{
		Name: step.TransformProc,
		Args: step.TransformArgs,
		Body: step.TransformBody,
	}})
	return interp.InvokeProcedure(step.TransformProc, value)
}
