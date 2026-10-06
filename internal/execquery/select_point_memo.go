package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns the point-SELECT output scaffolding memos: per-statement
// walks whose results are pure functions of (schema, AST shape) and whose
// dominant shapes are bare column-reference projections. The memo keys are
// the SAME identity pattern seekColIndexFor uses (context.go): the schema
// fingerprint guards DDL, and the slices the walk consumes are shared with
// the statement template by the copy-on-write clone (a column list without
// literal slots is returned untouched), so their addresses are stable across
// the statements of one template. A memo hit returns a fresh copy of the
// memoized slice — the caller's slice semantics (fresh per statement,
// caller-owned) are unchanged; only the recomputation disappears.

// colNamesMemoKey identifies one memoized result-column name list: the
// SELECT-columns slice (by its first element's address and length) plus the
// colDefs slice the names resolve against. Two statements share an entry
// only when BOTH slices are the template-shared instances, so a changed
// projection or a different table cannot collide.
type colNamesMemoKey struct {
	cols    *sql.SelectColumn
	colsLen int
	defs    *sql.ColumnDef
	defsLen int
}

// colNamesMemoCap bounds the memo between DDLs. Entries are keyed by AST
// slice identity, so a workload streaming distinct statement shapes would
// grow it without a cap; reaching the cap drops the whole epoch (the next
// statement rebuilds from one entry), keeping the memory bounded at the
// cost of a cold cache — the same flush-on-change policy the fingerprint
// already applies.
const colNamesMemoCap = 64

// collOutMemoKey identifies one memoized output-collation list: the
// SELECT-columns slice (template-shared) plus the schema fingerprint. The
// collations derive from the FROM table's column defs, fixed per (template,
// schema) pair.
type collOutMemoKey struct {
	cols    *sql.SelectColumn
	colsLen int
}

// bareRefPlanKey identifies one memoized bare-reference slot plan: the
// SELECT-columns slice and the colDefs slice the references resolve against,
// both by first-element address and length (the colNamesMemoKey pattern).
type bareRefPlanKey = colNamesMemoKey

// columnNamesMemoizable reports whether every SELECT column is an unaliased,
// unqualified, non-star column reference — the shape whose result names are
// independent of the enclosing statement (short_column_names resolution reads
// only the ref name and colDefs; full_column_names keeps its historical
// uncached path because the name embeds the FROM operand).
func columnNamesMemoizable(columns []sql.SelectColumn) bool {
	for i := range columns {
		col := &columns[i]
		if col.As != "" {
			return false
		}
		ref, ok := col.Expr.(*sql.ColumnRef)
		if !ok || ref.Name == "*" || ref.Table != "" {
			return false
		}
	}
	return len(columns) > 0
}

// columnNamesMemoGet returns the memoized name list for (columns, colDefs).
func (e *SelectEngine) columnNamesMemoGet(columns []sql.SelectColumn, colDefs []sql.ColumnDef) ([]string, bool) {
	fp := e.schemaFingerprint()
	if len(columns) == 0 || len(colDefs) == 0 {
		return nil, false
	}
	key := colNamesMemoKey{cols: &columns[0], colsLen: len(columns), defs: &colDefs[0], defsLen: len(colDefs)}
	if e.colNamesMemoFP != fp || e.colNamesMemo == nil {
		return nil, false
	}
	names, ok := e.colNamesMemo[key]
	if !ok {
		return nil, false
	}
	out := make([]string, len(names))
	copy(out, names)
	return out, true
}

// columnNamesMemoPut records the name list computed for (columns, colDefs).
func (e *SelectEngine) columnNamesMemoPut(columns []sql.SelectColumn, colDefs []sql.ColumnDef, names []string) {
	if len(columns) == 0 || len(colDefs) == 0 || len(names) == 0 {
		return
	}
	fp := e.schemaFingerprint()
	if e.colNamesMemoFP != fp || e.colNamesMemo == nil {
		e.colNamesMemoFP = fp
		e.colNamesMemo = make(map[colNamesMemoKey][]string)
	}
	if len(e.colNamesMemo) >= colNamesMemoCap {
		e.colNamesMemo = make(map[colNamesMemoKey][]string)
	}
	e.colNamesMemo[colNamesMemoKey{cols: &columns[0], colsLen: len(columns), defs: &colDefs[0], defsLen: len(colDefs)}] = names
}

// schemaFingerprint returns the current schema fingerprint (0 when no schema
// manager is wired) — the DDL-invalidation guard all the AST-shape memos
// share.
func (e *SelectEngine) schemaFingerprint() uint64 {
	if sm := e.ctx.Schema(); sm != nil {
		return sm.SchemaFingerprint()
	}
	return 0
}

// collationsMemoPut records one output-collation list under the current
// fingerprint (flushed on DDL and at the same entry cap as the name memo).
func (e *SelectEngine) collationsMemoPut(key collOutMemoKey, colls []string) {
	fp := e.schemaFingerprint()
	if e.collOutFP != fp || e.collOutMemo == nil {
		e.collOutFP = fp
		e.collOutMemo = make(map[collOutMemoKey][]string)
	}
	if len(e.collOutMemo) >= colNamesMemoCap {
		e.collOutMemo = make(map[collOutMemoKey][]string)
	}
	e.collOutMemo[key] = colls
}

// applyColumnWidthLimit applies buildColumnNames's SQLITE_LIMIT_COLUMN side
// effect for a memoized name list (the limit check runs per statement even
// when the names themselves are shared).
func (e *SelectEngine) applyColumnWidthLimit(names []string) {
	if limit := e.ctx.ColumnLimit(); limit != 0 && len(names) > limit {
		e.resultTooWide = true
	}
}

// outputCollationsBare computes selectOutputCollations for the no-UNION,
// single-table, all-bare-references shape without the per-column collation
// map: a bare reference's output collation is its declared non-BINARY table
// collation (uppercase), read off the parsed colDefs. Returns nil when the
// FROM name does not resolve to a real table (views, pragma functions) —
// the caller falls back to the general walk, which yields the same "" list.
func (e *SelectEngine) outputCollationsBare(s *sql.SelectStmt) []string {
	if len(s.Columns) > 0 {
		key := collOutMemoKey{cols: &s.Columns[0], colsLen: len(s.Columns)}
		if e.collOutFP == e.schemaFingerprint() && e.collOutMemo != nil {
			if names, ok := e.collOutMemo[key]; ok {
				out := make([]string, len(names))
				copy(out, names)
				return out
			}
		}
		colls := e.outputCollationsBareCompute(s)
		if colls != nil {
			e.collationsMemoPut(key, colls)
		}
		return colls
	}
	return e.outputCollationsBareCompute(s)
}

// outputCollationsBareCompute resolves the declared collations through the
// FROM table's parsed colDefs (uncached form).
func (e *SelectEngine) outputCollationsBareCompute(s *sql.SelectStmt) []string {
	entry, _, err := e.ctx.FindTable(s.From.Name)
	if err != nil || entry == nil {
		return nil
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	colls := make([]string, len(s.Columns))
	for ci := range s.Columns {
		ref := s.Columns[ci].Expr.(*sql.ColumnRef)
		if cd, ok := findColDefByName(colDefs, ref.Name); ok && cd.Collate != "" && !strings.EqualFold(cd.Collate, "BINARY") {
			colls[ci] = strings.ToUpper(cd.Collate)
		}
	}
	return colls
}

// outputCollationsFast reports whether selectOutputCollations can take the
// bare-reference fast path: a single member (no UNION chain) over one real
// table (no joins, a named FROM term) whose columns are all unaliased,
// unqualified, non-star references. For that shape memberColumnCollation
// reduces to the declared table collation of each referenced column —
// ExprCollation of a bare reference is ("", false), an unresolved name
// yields "" exactly like the map-lookup miss it replaces.
func outputCollationsFast(s *sql.SelectStmt) bool {
	if s == nil || s.Union != nil || s.From.Name == "" || len(s.Joins) > 0 {
		return false
	}
	if len(s.Columns) == 0 {
		return false
	}
	return columnNamesMemoizable(s.Columns)
}
