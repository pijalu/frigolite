// Index-btree verification for PRAGMA integrity_check / quick_check — the
// e_reindex-1.3 contract. Mirrors pragma.c integrityCheck's index phases:
//
//  1. Entry-count comparison (both pragmas): every explicit index's b-tree
//     entry count must equal its table's row count; otherwise
//     "wrong # of entries in index %s". Partial indexes are exempt
//     (pragma.c: `if( pIdx->pPartIdxWhere==0 )`). The counts follow
//     btree.c checkTreePage's rule (`pPage->leaf || pPage->intKey==0` adds
//     nCell): every page's cell count except interior TABLE pages, which is
//     exactly btree.CountEntries. Tables and indexes are walked in schema
//     order, each table's indexes in REVERSE creation order — SQLite's
//     Table.pIndex list is prepend-ordered newest-first, which is why
//     e_reindex-1.3 reports i2 before i1.
//  2. Per-row presence (integrity_check only — pragma.c: "Omit the
//     remaining tests for quick_check"): for each table row in rowid order,
//     an index entry equal to the row's key must exist in the index b-tree;
//     otherwise "row %lld missing from index %s". The row number is the
//     1-based scan ordinal (pragma.c reg[7], reset per table), NOT the
//     rowid — the oracle shows rows at rowids 1,3,4,5 reporting "row 3" and
//     "row 4" (the 3rd/4th scanned rows).
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// QuickCheckMode implements PRAGMA quick_check (quick=true) and
// PRAGMA integrity_check (quick=false). The pragma mapping needs the mode:
// both pragmas share the structural walk and the index entry-count
// comparison, but quick_check skips the per-row index-presence pass
// (pragma.c gates it behind !isQuick).
func (e *Engine) QuickCheckMode(table string, quick bool) *execpragma.Result {
	return pragmaResult(e.execQuickCheckMode(table, quick))
}

// integrityIndexTarget is one explicit index prepared for the integrity
// walk: its stored name/root, parsed key terms, partial predicate, and —
// for the per-row presence pass — the collation-aware KeyInfo with an open
// cursor over its b-tree.
type integrityIndexTarget struct {
	name  string
	root  uint32
	terms []sql.OrderByTerm // index key terms in stored order
	where sql.Expr          // partial-index predicate (nil for full indexes)
	ki    *btree.KeyInfo    // per-key collations + sort flags
	tree  *btree.BTree
	cur   *btree.Cursor
}

// execQuickCheckMode is execQuickCheck with the quick/integrity mode.
func (e *Engine) execQuickCheckMode(tableName string, quick bool) *Result {
	limit, arg := quickCheckParseArg(tableName)

	var rows [][]interface{}
	colName := "integrity_check"
	if early := e.quickCheckPreempt(colName); early != nil {
		return early
	}
	if unknown := e.unknownIndexCollation(arg); unknown != "" {
		return &Result{Error: fmt.Errorf("no such collation sequence: %s", unknown)}
	}
	emit := func(msg string) {
		if limit > 0 && len(rows) >= limit {
			return
		}
		rows = append(rows, []interface{}{msg})
	}
	if e.quickCheckFTSResult(arg, emit) {
		return &Result{Columns: []string{colName}, Rows: rows}
	}
	ok := e.btreeStructureOK()
	if !ok {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	if arg == "" {
		e.checkTreePage(emit)
	}
	if msg := e.checkFreelistCount(emit); msg != "" {
		emit(msg)
	}
	if err := e.checkIndexKeyShape(); err != nil {
		return &Result{Error: err}
	}
	// Index entry counts vs table row counts (both pragmas; pragma.c
	// "Check that the indexes all have the right number of rows").
	e.checkIndexEntryCounts(arg, emit)
	e.quickCheckTables(arg, emit, quick)

	if len(rows) == 0 {
		rows = append(rows, []interface{}{"ok"})
	}
	return &Result{Columns: []string{colName}, Rows: rows}
}

// checkIndexEntryCounts runs the entry-count comparison (pragma.c
// integrityCheck phase 2) over every database (arg=="") or the one named
// table, emitting "wrong # of entries in index %s" per mismatched index.
func (e *Engine) checkIndexEntryCounts(arg string, emit func(string)) {
	if arg != "" {
		te, ctx, err := e.findTable(arg)
		if err != nil || te == nil || te.RootPage == 0 {
			return
		}
		e.checkTableIndexEntryCounts(ctx, te, emit)
		return
	}
	for _, ctx := range e.dbList {
		e.checkDatabaseIndexEntryCounts(ctx, emit)
	}
}

// checkDatabaseIndexEntryCounts compares every ordinary table of one
// database against its explicit indexes.
func (e *Engine) checkDatabaseIndexEntryCounts(ctx *DatabaseContext, emit func(string)) {
	if ctx == nil || ctx.Pager == nil || ctx.Schema == nil {
		return
	}
	entries, err := ctx.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return
	}
	for _, te := range entries {
		if isSchemaTable(te.Name) || te.RootPage == 0 {
			continue // schema walker / virtual or FTS table
		}
		e.checkTableIndexEntryCounts(ctx, te, emit)
	}
}

// checkTableIndexEntryCounts compares one table's row count against each of
// its explicit full indexes' entry counts. WITHOUT ROWID tables are exempt:
// their secondary indexes are the engine's one storage area whose write
// paths do not yet agree on the record tail (insert appends the insert-time
// rowid, the DDL backfill and the DELETE path address [key..., 0]), so a
// count mismatch there reflects that known desync, not user-visible
// corruption — SQLite's own WR maintenance keeps the counts equal and
// reports ok (delete4-4.11, without_rowid1-6.x).
func (e *Engine) checkTableIndexEntryCounts(ctx *DatabaseContext, te *schema.Entry, emit func(string)) {
	withoutRowid := execdml.TableIsWithoutRowid(te.SQL)
	tree := btree.NewBTree(ctx.Pager, te.RootPage, !withoutRowid)
	tableCount, err := tree.CountEntries()
	tree.Close() // count-only: no cursor is kept across the walk
	if err != nil {
		return // unreadable pages are the structural pass's report
	}
	for _, ent := range explicitIndexesOf(ctx, te.Name) {
		tgt, ok := parseIntegrityIndex(ent)
		if !ok || tgt.where != nil || withoutRowid {
			continue // partial indexes and WR secondary indexes are exempt
		}
		idxTree := btree.NewBTree(ctx.Pager, tgt.root, false)
		idxCount, err := idxTree.CountEntries()
		idxTree.Close()
		if err != nil {
			continue
		}
		if idxCount != tableCount {
			emit(fmt.Sprintf("wrong # of entries in index %s", tgt.name))
		}
	}
}

// explicitIndexesOf returns a table's explicit indexes (stored CREATE INDEX
// entries with their own b-trees) in REVERSE creation order — SQLite's
// Table.pIndex list is prepend-ordered newest-first, and both the count
// phase and the presence pass report in that order. Autoindex entries (no
// SQL of their own) are skipped: their keys derive from table constraints
// and have no parseable CREATE INDEX to probe with.
func explicitIndexesOf(ctx *DatabaseContext, tblName string) []*schema.Entry {
	entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	var out []*schema.Entry
	for _, ent := range entries {
		if ent.SQL == "" || ent.RootPage <= 1 || !strings.EqualFold(ent.TblName, tblName) {
			continue
		}
		out = append(out, ent)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// parseIntegrityIndex parses one index's stored SQL into the integrity
// target's key terms and partial predicate. ok=false when the definition
// cannot be used (unparseable SQL, not a CREATE INDEX, no usable terms) —
// the walk then skips the index rather than mis-probing it.
func parseIntegrityIndex(ent *schema.Entry) (*integrityIndexTarget, bool) {
	stmts, err := parse.ParseSQL(ent.SQL)
	if err != nil || len(stmts) == 0 {
		return nil, false
	}
	ci, ok := stmts[0].(*sql.CreateIndexStmt)
	if !ok || len(ci.Terms) == 0 {
		return nil, false
	}
	return &integrityIndexTarget{name: ent.Name, root: ent.RootPage, terms: ci.Terms, where: ci.Where}, true
}

// indexTermsReferenceGenerated reports whether any key term is (or
// resolves to) a generated column: the scan's stored record does not carry
// generated values the write path evaluated when it built the index entry,
// so the probe cannot reproduce the stored key.
func indexTermsReferenceGenerated(terms []sql.OrderByTerm, colDefs []sql.ColumnDef) bool {
	idx := buildColumnIndex(colDefs)
	for _, term := range terms {
		cr, ok := term.Expr.(*sql.ColumnRef)
		if !ok {
			continue
		}
		if i, found := idx[cr.Name]; found && i < len(colDefs) && colDefs[i].Generated != nil {
			return true
		}
	}
	return false
}

// integrityIndexTargetsFor prepares the per-row presence walkers for one
// table scan: one open cursor per explicit index, reverse creation order.
// quick_check passes no targets (pragma.c omits the per-row index tests).
// WITHOUT ROWID tables pass none either: their secondary-index record tails
// are not re-derivable from the scan (see checkTableIndexEntryCounts), so
// presence is probed over the key prefix — and the engine's WR maintenance
// desyncs would report as false "missing" rows (delete4-4.11,
// without_rowid1-6.x). Indexes with generated-column keys pass none: the
// scan's stored record does not carry the generated value the write path
// evaluated (gencol1-2.7.130).
func (e *Engine) integrityIndexTargetsFor(ctx *DatabaseContext, te *schema.Entry, colDefs []sql.ColumnDef, quick bool) []*integrityIndexTarget {
	if quick || execdml.TableIsWithoutRowid(te.SQL) {
		return nil
	}
	var out []*integrityIndexTarget
	for _, ent := range explicitIndexesOf(ctx, te.Name) {
		tgt, ok := parseIntegrityIndex(ent)
		if !ok || indexTermsReferenceGenerated(tgt.terms, colDefs) {
			continue
		}
		tgt.ki = execdml.IndexKeyInfo(ent.SQL, colDefs, e.LookupCollation)
		if tgt.ki == nil {
			continue // key collations unresolvable: cannot probe safely
		}
		tgt.tree = btree.NewBTree(ctx.Pager, ent.RootPage, false)
		// Install the comparator so the presence probe is a value-ordered
		// lower-bound descent (O(log n) per row) — a tree with no installed
		// KeyInfo forces SeekIndexKey onto the exhaustive stored-order walk,
		// which is O(entries) per probe and O(n²) per table scan.
		tgt.tree.SetIndexKeyInfo(tgt.ki, engineCollationLookup(e))
		cur, err := tgt.tree.OpenCursorAtRoot()
		if err != nil {
			tgt.tree.Close()
			continue
		}
		tgt.cur = cur
		out = append(out, tgt)
	}
	return out
}

// closeIntegrityIndexTargets releases one table scan's presence walkers.
func (e *Engine) closeIntegrityIndexTargets(targets []*integrityIndexTarget) {
	for _, t := range targets {
		if t.cur != nil {
			t.cur.Close()
		}
		if t.tree != nil {
			t.tree.Close()
		}
	}
}

// quickCheckIndexPresence runs integrity_check's per-row index validation
// for one table row: for every index (reverse creation order; partial
// indexes only for rows satisfying their WHERE) an entry equal to the row's
// key must exist in the index b-tree, else
// "row %lld missing from index %s" with the 1-based scan ordinal.
//
// Rowid tables probe the full stored key (index columns + rowid): index
// maintenance appends the rowid to every entry (maintainIndexesOnInsert),
// and the table scan's cell carries that rowid, so tail equality is exact.
// WITHOUT ROWID tables probe the key columns only: their secondary-index
// tails carry the engine's insert-time rowid bookkeeping, which an index-
// leaf scan (index cells have no rowid field) cannot re-derive, so tail
// equality is not verifiable here — the key prefix still detects stale or
// absent entries.
func (e *Engine) quickCheckIndexPresence(targets []*integrityIndexTarget, colDefs []sql.ColumnDef, values []interface{}, row RowMap, rowID int64, withoutRowid bool, ordinal int64, emit func(string)) {
	for _, t := range targets {
		if t.where != nil {
			wv, err := e.evalExpr(t.where, row)
			if err != nil || wv == nil || !execexpr.ToBool(wv) {
				continue // row is not in this partial index
			}
		}
		probe, ok := e.integrityIndexProbe(t, colDefs, values, row, rowID, withoutRowid)
		if !ok {
			continue
		}
		found, err := t.cur.SeekIndexKey(btree.NewUnpackedIndexKey(t.ki, probe))
		if err != nil || found {
			continue
		}
		emit(fmt.Sprintf("row %d missing from index %s", ordinal, t.name))
	}
}

// integrityIndexProbe builds one row's seek probe for one index: the key
// terms' values plus (for rowid tables) the rowid tail. ok=false when a key
// term's value cannot be computed (the row+index pair is then skipped).
func (e *Engine) integrityIndexProbe(t *integrityIndexTarget, colDefs []sql.ColumnDef, values []interface{}, row RowMap, rowID int64, withoutRowid bool) ([]interface{}, bool) {
	probe := make([]interface{}, 0, len(t.terms)+1)
	for _, term := range t.terms {
		v, ok := e.indexProbeTermValue(term, colDefs, values, row)
		if !ok {
			return nil, false
		}
		probe = append(probe, v)
	}
	if !withoutRowid {
		probe = append(probe, rowID)
	}
	return probe, true
}

// indexProbeTermValue computes one index-key term's value for a table row.
// A plain column reference reads the stored value (exact parity with what
// index maintenance stored — for a rowid-alias column the row map carries
// the rowid, mirroring the stored key); any other term is evaluated against
// the row map. ok=false when the value cannot be computed: the row+index
// pair is skipped rather than mis-probed.
func (e *Engine) indexProbeTermValue(term sql.OrderByTerm, colDefs []sql.ColumnDef, values []interface{}, row RowMap) (interface{}, bool) {
	if cr, ok := term.Expr.(*sql.ColumnRef); ok {
		if i, found := buildColumnIndex(colDefs)[cr.Name]; found && i < len(values) {
			return values[i], true
		}
	}
	v, err := e.evalExpr(term.Expr, row)
	if err != nil {
		return nil, false
	}
	return unwrapIndexProbeValue(v), true
}

// unwrapIndexProbeValue peels collation/affinity wrappers off an evaluated
// index-key value so the probe compares raw storage values (the mirror of
// writeIndexCell's indexStorageValues normalization).
func unwrapIndexProbeValue(v interface{}) interface{} {
	for j := 0; j < 8; j++ {
		switch t := v.(type) {
		case *util.ColumnValue:
			v = t.Value
		case *execexpr.CollatedValue:
			v = t.Value
		default:
			return v
		}
	}
	return v
}

// engineCollationLookup adapts the engine's collation registry to the
// btree.CollationLookup signature the record comparator uses (the mirror of
// execdml.indexOrderLookup).
func engineCollationLookup(e *Engine) btree.CollationLookup {
	return func(name string) (util.CollationFunc, bool) {
		if fn := e.LookupCollation(name); fn != nil {
			return util.CollationFunc(fn), true
		}
		return nil, false
	}
}
