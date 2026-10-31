package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// spanMaxOffset bounds the RawSQL/token-space offset search (leading
// whitespace plus multi-statement statement starts; far larger offsets mean
// the statement cannot be the span's source).
const spanMaxOffset = 8192

// colnameMaxDepth bounds recursive operand resolution (a CTE body whose star
// expands over itself would otherwise recurse without bound).
const colnameMaxDepth = 32

// This file implements the PRAGMA short_column_names / full_column_names
// result-column naming modes (select.c sqlite3GenerateColumnNames):
//
//	short=ON, full=OFF (default): a direct column reference names the bare
//	                            column; other expressions use the span text.
//	full=ON (any short):        a direct column reference names
//	                            RESOLVED-TABLE.COLUMN — pTab->zName, the REAL
//	                            table name (never the written alias, which
//	                            only the star expansion uses).
//	short=OFF, full=OFF:        the name is the expression's original span
//	                            text (parse.y sqlite3ExprListSetSpan).
//
// Star expansions name their columns at expansion time (select.c
// sqlite3SelectExpand): "zTabName.column" when longNames (full=ON &&
// short=OFF), otherwise the bare column name. A FROM-clause subquery's
// synthesized table is named after its alias, or "(subquery-N)" when
// unaliased (select.c sqlite3ExpandSubquery + printf.c "%!S").

// longNames reports the star-expansion long-name mode: full_column_names ON
// with short_column_names OFF (select.c: longNames = FullColNames &&
// !ShortColNames).
func (e *SelectEngine) longNames() bool {
	return e.ctx.FullColumnNames() && !e.ctx.ShortColumnNames()
}

// colnameOperand is one FROM/JOIN operand as the result-column renderer sees
// it: the name as written (SQLite's zTabName), the resolved table name
// (pTab->zName for full_column_names), and the operand's output column names.
type colnameOperand struct {
	// written is the operand's name as written in the FROM clause: the alias
	// when one is given, else the table name. Star expansions prefix with it.
	written string
	// resolved is the full_column_names qualifier: the declared table/view/CTE
	// name for a named source, the alias (or "(subquery-N)") for a FROM-clause
	// subquery (sqlite3ExpandSubquery).
	resolved string
	// names lists the operand's visible output column names in order.
	names []string
}

// colnameOperands enumerates the SELECT's FROM/JOIN operands in appearance
// order for result-column naming, rooted at the top-level statement (for
// span-text recovery).
func (e *SelectEngine) colnameOperands(sel *sql.SelectStmt) []colnameOperand {
	return e.colnameOperandsAt(sel, sel, 0)
}

// subqueryDisplayName renders a FROM-clause subquery's synthesized table
// name: the alias when one is given, else "(subquery-N)". SQLite numbers the
// name from every parsed SELECT node statement-wide (selId); the corpus
// treats the number as arbitrary — select1-6.9.7 normalizes it with
// `regsub -all {subquery-\d+}` — and the converted fixture pins the literal
// 0, so the counter starts at 0.
func subqueryDisplayName(alias string, seq int) string {
	if alias != "" {
		return alias
	}
	return "(subquery-" + strconv.Itoa(seq) + ")"
}

// colnameOperandsAt is colnameOperands with the span-root statement and the
// recursion depth made explicit for nested (subquery/CTE body) enumerations.
func (e *SelectEngine) colnameOperandsAt(sel *sql.SelectStmt, root *sql.SelectStmt, depth int) []colnameOperand {
	if sel == nil || depth > colnameMaxDepth {
		return nil
	}
	var ops []colnameOperand
	subqSeq := 0 // "(subquery-N)" counter: 0-based, see subqueryDisplayName
	add := func(ref sql.TableRef) {
		if op, ok := e.colnameOperandFor(ref, sel, root, &subqSeq, depth); ok {
			ops = append(ops, op)
		}
	}
	if sel.From.Name != "" || sel.From.Subquery != nil {
		add(sel.From)
	}
	for _, j := range sel.Joins {
		if j.Table.Name != "" || j.Table.Subquery != nil {
			add(j.Table)
		}
	}
	return ops
}

// colnameOperandFor builds the naming operand for one FROM table reference.
func (e *SelectEngine) colnameOperandFor(ref sql.TableRef, sel *sql.SelectStmt, root *sql.SelectStmt, subqSeq *int, depth int) (colnameOperand, bool) {
	// FROM-clause subquery: the synthesized table is named after the alias,
	// or "(subquery-N)" when unaliased (sqlite3ExpandSubquery).
	if ref.Subquery != nil {
		name := subqueryDisplayName(ref.As, *subqSeq)
		*subqSeq++
		return colnameOperand{
			written:  name,
			resolved: name,
			names:    e.subqueryOutputColumnNames(ref.Subquery, root, depth+1),
		}, true
	}
	if ref.Name == "" {
		return colnameOperand{}, false
	}
	// CTE reference: pTab->zName is the CTE name; declared column names
	// (WITH c(a,b)) override the body's own result names.
	if cte, ok := e.findCTE(sel, ref.Name); ok {
		name := ref.As
		if name == "" {
			name = cte.Name
		}
		names := cte.Columns
		if len(names) == 0 {
			names = e.subqueryOutputColumnNames(cte.Select, root, depth+1)
		}
		return colnameOperand{written: name, resolved: cte.Name, names: names}, true
	}
	// Base table or view: pTab->zName is the declared (schema) spelling, never
	// the alias. tableColumnNames resolves views through their body's result
	// names (viewFallbackColumnNames) and eponymous modules through their
	// declared schema.
	tableName := ref.Name
	if dot := strings.LastIndex(tableName, "."); dot >= 0 {
		tableName = tableName[dot+1:]
	}
	if entry, _, err := e.ctx.FindTable(tableName); err == nil && entry != nil {
		tableName = entry.Name
	}
	names, _ := e.tableColumnNames(tableName)
	names = dropHiddenOperandNames(e, tableName, names)
	written := ref.As
	if written == "" {
		written = tableName
	}
	return colnameOperand{written: written, resolved: tableName, names: names}, true
}

// dropHiddenOperandNames removes an operand's hidden columns (FTS hidden
// table-name/docid aliases never expand in t.*).
func dropHiddenOperandNames(e *SelectEngine, tableName string, names []string) []string {
	if len(names) == 0 {
		return names
	}
	hidden := e.hiddenColumnNames(tableName)
	if len(hidden) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !hidden[n] {
			out = append(out, n)
		}
	}
	return out
}

// matchOperandWritten finds the operand whose FROM-clause name (alias or
// table name, as written) matches q. A schema prefix on q ("main.t") is
// ignored the way sqlite3TwoPartName resolves references.
func matchOperandWritten(ops []colnameOperand, q string) (colnameOperand, bool) {
	if dot := strings.LastIndex(q, "."); dot >= 0 {
		q = q[dot+1:]
	}
	for _, op := range ops {
		if strings.EqualFold(op.written, q) {
			return op, true
		}
	}
	return colnameOperand{}, false
}

// operandWrittenForColumn returns the written name of the first operand (in
// FROM order) whose output columns contain name — the star expansion's
// zTabName attribution for a merged result column.
func operandWrittenForColumn(ops []colnameOperand, name string) string {
	for _, op := range ops {
		for _, n := range op.names {
			if strings.EqualFold(n, name) {
				return op.written
			}
		}
	}
	return ""
}

// operandResolvedForColumn returns the resolved table name of the first
// operand (in FROM order) whose output columns contain name — the
// full_column_names qualifier for an unqualified reference.
func operandResolvedForColumn(ops []colnameOperand, name string) string {
	for _, op := range ops {
		for _, n := range op.names {
			if strings.EqualFold(n, name) {
				return op.resolved
			}
		}
	}
	return ""
}

// subqueryOutputColumnNames computes a subquery's result-column names under
// the current naming modes — the derived table's column list (select.c
// sqlite3ColumnsFromExprList over the subquery's result set). Compound
// members take their names from the leftmost member. root carries the
// top-level statement whose RawSQL backs span-mode extraction.
func (e *SelectEngine) subqueryOutputColumnNames(sub *sql.SelectStmt, root *sql.SelectStmt, depth int) []string {
	if sub == nil || depth > colnameMaxDepth {
		return nil
	}
	for cur := sub; cur != nil; cur = cur.Union {
		if cur.Union == nil {
			sub = cur // the leftmost member names a compound result set
			break
		}
	}
	ops := e.colnameOperandsAt(sub, root, depth)
	long := e.longNames()
	var names []string
	for _, col := range sub.Columns {
		if col.As != "" {
			names = append(names, col.As)
			continue
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			names = append(names, e.subqueryStarNames(ref, ops, long)...)
			continue
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			names = append(names, e.subqueryRefName(ref, ops, root))
			continue
		}
		names = append(names, exprResultName(col.Expr))
	}
	// A derived table's column names are unique (sqlite3ColumnsFromExprList
	// ":N" suffixing) — matching the materialization boundary so the star
	// naming aligns with the value path.
	return dedupDerivedColumnNames(names)
}

// subqueryStarNames expands a star inside a subquery's result list for
// subqueryOutputColumnNames.
func (e *SelectEngine) subqueryStarNames(ref *sql.ColumnRef, ops []colnameOperand, long bool) []string {
	if ref.Table != "" {
		if op, ok := matchOperandWritten(ops, ref.Table); ok {
			return qualifyStarNames(bareOperandNames(op), op.written, long)
		}
		return nil
	}
	var names []string
	for _, op := range ops {
		names = append(names, qualifyStarNames(bareOperandNames(op), op.written, long)...)
	}
	return names
}

// subqueryRefName names one direct column reference inside a subquery's
// result list (the same mode matrix as resolveColumnRefName, without the
// colDefs-level spelling resolution a nested scan would need).
func (e *SelectEngine) subqueryRefName(ref *sql.ColumnRef, ops []colnameOperand, root *sql.SelectStmt) string {
	if !e.ctx.FullColumnNames() && !e.ctx.ShortColumnNames() {
		if name := spanResultName(ref, root); name != "" {
			return name
		}
		if ref.Table != "" {
			return ref.Table + "." + ref.Name
		}
		return ref.Name
	}
	if e.ctx.FullColumnNames() {
		qualifier := ""
		if ref.Table != "" {
			if op, ok := matchOperandWritten(ops, ref.Table); ok {
				qualifier = op.resolved
			}
		} else if q := operandResolvedForColumn(ops, ref.Name); q != "" {
			qualifier = q
		}
		if qualifier != "" {
			return qualifier + "." + ref.Name
		}
	}
	return ref.Name
}

// bareOperandNames returns an operand's output column names without any
// qualifier.
func bareOperandNames(op colnameOperand) []string {
	out := make([]string, len(op.names))
	for i, n := range op.names {
		if dot := strings.LastIndex(n, "."); dot >= 0 {
			n = n[dot+1:]
		}
		out[i] = n
	}
	return out
}

// dedupDerivedColumnNames makes a derived table's column names unique the
// way select.c sqlite3ColumnsFromExprList does: a name colliding
// (case-insensitively — the name hash is sqlite3StrIHash) with an earlier
// column is suffixed ":N" (N counted per column from 1), after stripping any
// existing ":N" suffix from the base. Derived tables (FROM-clause subqueries,
// CTE bodies, views) carry unique column names; top-level result sets do not
// dedup.
func dedupDerivedColumnNames(names []string) []string {
	used := make(map[string]bool, len(names))
	out := make([]string, len(names))
	for i, name := range names {
		if name == "" {
			continue
		}
		if !used[strings.ToLower(name)] {
			out[i] = name
			used[strings.ToLower(name)] = true
		}
	}
	for i, name := range out {
		if name != "" || names[i] == "" {
			continue
		}
		// Collision: strip an existing ":N" suffix from the base, then count
		// per column from 1.
		base := names[i]
		if colon := strings.LastIndexByte(base, ':'); colon > 0 {
			suffix := base[colon+1:]
			if suffix != "" && strings.Trim(suffix, "0123456789") == "" {
				base = base[:colon]
			}
		}
		for n := 1; ; n++ {
			candidate := base + ":" + strconv.Itoa(n)
			if !used[strings.ToLower(candidate)] {
				out[i] = candidate
				used[strings.ToLower(candidate)] = true
				break
			}
		}
	}
	return out
}

// qualifyStarNames prefixes bare star-expansion names with the operand's
// written name when longNames is active (select.c sqlite3SelectExpand:
// zEName = zTabName.zName under longNames, else the bare column name).
func qualifyStarNames(names []string, written string, long bool) []string {
	if !long {
		return names
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = written + "." + n
	}
	return out
}

// spanResultName recovers a direct column reference's original text — the
// span name SQLite uses when short and full column names are both off
// (sqlite3GenerateColumnNames's zSpan fallback). The AST keeps the name
// token's byte span (ColumnRef.NameTok) positioned in the full statement
// input, while SelectStmt.RawSQL is the trimmed statement text; the offset
// between the two coordinate spaces is recovered by validation. Returns ""
// when no verified span can be extracted (the caller falls back to the
// reconstructed name).
func spanResultName(ref *sql.ColumnRef, root *sql.SelectStmt) string {
	if ref == nil || root == nil || root.RawSQL == "" {
		return ""
	}
	raw := root.RawSQL
	tokLen := ref.NameTok.End - ref.NameTok.Start
	if tokLen <= 0 || tokLen > len(raw) {
		return ""
	}
	// The name token's byte span anchors the offset between the token
	// coordinate space and RawSQL: try each offset, extend over the qualifier
	// chain, and keep the first candidate that denotes the reference and sits
	// behind a delimiter.
	for off := 0; off <= ref.NameTok.Start && off+tokLen <= len(raw); off++ {
		if off > spanMaxOffset {
			break
		}
		start := ref.NameTok.Start - off
		end := ref.NameTok.End - off
		s := scanQualifiedRefStart(raw, start)
		if s > 0 && !isSpanDelimiter(raw[s-1]) {
			continue
		}
		if spanMatchesRef(raw[s:end], ref) {
			return raw[s:end]
		}
	}
	return ""
}

// spanMatchesRef reports whether the candidate span text denotes ref: after
// stripping whitespace, quotes, and dots it must equal the reference's
// qualifier-and-name concatenation.
func spanMatchesRef(cand string, ref *sql.ColumnRef) bool {
	norm := normalizeSpanText(cand)
	want := normalizeSpanText(ref.Table) + normalizeSpanText(ref.Name)
	return norm != "" && strings.EqualFold(norm, want)
}

// normalizeSpanText strips whitespace, quoting, and dots from a span
// candidate (the comparison itself is case-insensitive).
func normalizeSpanText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\r', '"', '`', '[', ']', '\'', '.':
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isSpanDelimiter reports whether c may precede a result expression's span.
func isSpanDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', '(', '+', '-', '*', '/', '%', '=', '<', '>', '|', '&', '~':
		return true
	}
	return false
}

// scanQualifiedRefStart walks backward from the name token's start over any
// ". qualifier" chain, returning the span's first byte offset (the start of
// the reference's leftmost identifier).
func scanQualifiedRefStart(raw string, nameStart int) int {
	i := nameStart
	for i > 0 {
		j := i
		for j > 0 && isSpanSpace(raw[j-1]) {
			j--
		}
		if j == 0 || raw[j-1] != '.' {
			break
		}
		j-- // the dot
		for j > 0 && isSpanSpace(raw[j-1]) {
			j--
		}
		next := scanSpanIdentStart(raw, j)
		if next == j {
			break
		}
		i = next
	}
	return i
}

// scanSpanIdentStart walks backward over one identifier (quoted or bare),
// returning its start offset. j points just past the identifier's last byte.
func scanSpanIdentStart(raw string, j int) int {
	if j == 0 {
		return j
	}
	switch raw[j-1] {
	case '"', '`', ']':
		open := raw[j-1]
		if open == ']' {
			open = '['
		}
		for k := j - 1; k >= 0; k-- {
			if raw[k] == open {
				return k
			}
		}
		return j
	case '\'':
		for k := j - 1; k >= 0; k-- {
			if raw[k] == '\'' {
				return k
			}
		}
		return j
	}
	k := j
	for k > 0 {
		c := raw[k-1]
		if c == '_' || c == '$' || c >= 0x80 ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			k--
			continue
		}
		break
	}
	return k
}

// isSpanSpace reports whether c is span whitespace.
func isSpanSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// selectHasStarColumn reports whether any SELECT column is a star (qualified
// or bare) — the shape whose naming needs the FROM-operand context.
func selectHasStarColumn(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			return true
		}
	}
	return false
}

// starColumnNamesFor names a star expansion's columns. Per select.c
// sqlite3SelectExpand, each expanded column's name is fixed at expansion
// time: "zTabName.column" when longNames (full=ON && short=OFF, zTabName =
// the FROM item's alias, or its table name — for a subquery its alias or
// "(subquery-N)"), otherwise the bare column name.
func (e *SelectEngine) starColumnNamesFor(ref *sql.ColumnRef, colDefs []sql.ColumnDef, sel *sql.SelectStmt, ops []colnameOperand) []string {
	if ref.Table != "" {
		if !selectOperandVisible(sel, ref.Table) {
			e.starNoSuchTable = ref.Table
			return nil
		}
		long := e.longNames()
		// Operand-backed naming: the FROM item matched by its written name.
		// A subquery operand resolves through its own result-column names —
		// the row map's leftover inner-scan keys must not leak into t.*
		// names (select1-11.15).
		if op, ok := matchOperandWritten(ops, ref.Table); ok {
			return qualifyStarNames(bareOperandNames(op), op.written, long)
		}
		// Legacy resolution (schema-qualified stars, sqlite_master,
		// table-valued operands the naming pass cannot enumerate): the
		// defs/schema names, rendered bare — or prefixed with the matched
		// operand's written name under longNames.
		names := bareOperandNames(colnameOperand{names: e.buildQualifiedStarNames(ref, colDefs, sel)})
		if long {
			return qualifyStarNames(names, writtenOperandName(sel, ref.Table), true)
		}
		return names
	}
	// Plain *: outside longNames the defs-based bare names stand (aligned
	// with the value expansion's colDefs walk). Under longNames each def is
	// prefixed with its owning operand's written name — the def's own
	// "table.col" conflict prefix when present, else first-operand
	// attribution in FROM order.
	if e.longNames() {
		out := make([]string, 0, len(colDefs))
		for _, cd := range colDefs {
			if cd.Dropped || IsHiddenColumnDef(cd) {
				continue
			}
			name := cd.Name
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				// Conflict-renamed def ("b.f1"): its prefix is the owning
				// operand's written name already.
				out = append(out, name)
				continue
			}
			if q := operandWrittenForColumn(ops, name); q != "" {
				out = append(out, q+"."+name)
				continue
			}
			out = append(out, name)
		}
		return out
	}
	return expandStarColNames(colDefs)
}

// writtenOperandName returns the matched FROM item's name as written (alias
// when aliased, else the table name) for legacy long-name star prefixes.
func writtenOperandName(sel *sql.SelectStmt, tableRef string) string {
	q := tableRef
	if dot := strings.LastIndex(q, "."); dot >= 0 {
		q = q[dot+1:]
	}
	if sel != nil {
		if operandVisible(sel.From.Name, sel.From.As, q) {
			return aliasOrTable(sel.From.Name, sel.From.As)
		}
		for _, j := range sel.Joins {
			if operandVisible(j.Table.Name, j.Table.As, q) {
				return aliasOrTable(j.Table.Name, j.Table.As)
			}
		}
	}
	return q
}
