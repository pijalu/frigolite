package exec

import (
	"math"
	"math/rand"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
)

// outerRowsForResolution returns the correlated-scope rows visible to the
// current evaluation, innermost first: the current outerRow followed by the
// stack of enclosing outer rows (for multi-level correlated subqueries).
func (e *Engine) outerRowsForResolution() []Row {
	return e.selectEngine.OuterRowsForResolution()
}

// pushOuterRow sets row as the current outer scope for correlated subquery
// resolution, preserving the previous scope on a stack.
func (e *Engine) pushOuterRow(row Row) {
	e.selectEngine.PushOuterRow(row)
}

// popOuterRow restores the outer scope saved by pushOuterRow.
func (e *Engine) popOuterRow() {
	e.selectEngine.PopOuterRow()
}

// compareValuesWithCollate compares two values using the collation from either side.
func (e *Engine) compareValuesWithCollate(left, right interface{}) int {
	lv, lc := extractValue(left)
	rv, rc := extractValue(right)
	// SQLite collation resolution for a binary comparison (datatype3.html):
	// 1. An explicit COLLATE clause on either operand (the COLLATE operator)
	//    wins over any column collation: `a = 'ABC' COLLATE BINARY` compares
	//    BINARY even when column a is declared COLLATE NOCASE, and
	//    `a COLLATE BINARY = 'ABC'` likewise. If both sides are explicit,
	//    the left one wins (matching sqlite3ExprCollSeq).
	// 2. Otherwise, if the LEFT operand is a column, its column collation is
	//    used — defaulting to BINARY when the column has no COLLATE (a plain
	//    ColumnValue wrapper). A column on the left masks a collation on
	//    the right: `t2.y > t1.b` (b COLLATE NOCASE) compares BINARY
	//    because t2.y is a column without collation.
	// 3. Only when the left operand is NOT a column (literal/expression)
	//    does the right operand's column collation apply, e.g. `'abc' > b`.
	if le, ok := left.(*collatedValue); ok && le.Explicit {
		return e.compareValuesCollate(lv, rv, le.Collation)
	}
	if re, ok := right.(*collatedValue); ok && re.Explicit {
		return e.compareValuesCollate(lv, rv, re.Collation)
	}
	leftIsColumn := isColumnValue(left)
	if leftIsColumn {
		return e.compareValuesCollate(lv, rv, lc)
	}
	collation := lc
	if collation == "" {
		collation = rc
	}
	return e.compareValuesCollate(lv, rv, collation)
}

// rowIDExistsInTree reports whether a row with the given rowid exists in the
// btree (used by randomFreeRowID to avoid collisions).
func (e *Engine) rowIDExistsInTree(tree *btree.BTree, rowID int64) bool {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		if cell.RowID == rowID {
			return true
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return false
}

// randomFreeRowID picks a random positive rowid that is not already in the
// table (SQLite's behavior when the auto-assigned rowid would overflow).
func (e *Engine) randomFreeRowID(tree *btree.BTree) int64 {
	for attempts := 0; attempts < 100; attempts++ {
		candidate := rand.Int63n(math.MaxInt64-1) + 1
		if !e.rowIDExistsInTree(tree, candidate) {
			return candidate
		}
	}
	return 1
}

// tableHasAutoIncrement reports whether the table declares an AUTOINCREMENT
// column (an INTEGER PRIMARY KEY AUTOINCREMENT column in a rowid table).
func (e *Engine) tableHasAutoIncrement(tableName string) bool {
	// The colCache is keyed by tableName + "\x00" + createSQL (see
	// parseColumnDefs); scan all entries for this table name. The table may
	// be cached under multiple SQL keys after ALTER TABLE.
	for k, colDefs := range e.caches.colCache {
		if k == tableName || strings.HasPrefix(k, tableName+"\x00") {
			for _, cd := range colDefs {
				if cd.AutoInc {
					return true
				}
			}
		}
	}
	return false
}

// bumpRowIDCache records a row with the given rowid as present in the table.
// The cache always holds the largest rowid seen so far; explicit-rowid inserts
// must bump it so later auto-rowid inserts do not collide. Keyed by (pager,
// root page) so tables in different databases with the same root page do not
// share rowid state.
func (e *Engine) bumpRowIDCache(pg *pager.Pager, rootPage uint32, rowID int64) {
	key := e.rowidCacheKey(pg, rootPage)
	if cur, ok := e.caches.nextRowIDCache[key]; !ok || rowID > cur {
		e.caches.nextRowIDCache[key] = rowID
	}
	if cur, ok := e.caches.autoIncSeq[key]; !ok || rowID > cur {
		e.caches.autoIncSeq[key] = rowID
	}
}

// invalidateRowIDCache drops the cached largest rowid for a table. Called after
// any DELETE (or rowid-changing UPDATE) because the largest rowid may have been
// removed; the next findNextRowID recomputes it by scanning. The AUTOINCREMENT
// sequence is deliberately kept: DELETE does not rewind sqlite_sequence.
func (e *Engine) invalidateRowIDCache(pg *pager.Pager, rootPage uint32) {
	delete(e.caches.nextRowIDCache, e.rowidCacheKey(pg, rootPage))
}

// CompareValuesWithCollate compares two values using the collation from
// either side. Exported for the expression evaluator's comparison ops.
func (e *Engine) CompareValuesWithCollate(left, right interface{}) int {
	return e.compareValuesWithCollate(left, right)
}

// OuterRowsForResolution returns the correlated-scope rows visible to the
// current evaluation, innermost first. Exported for the expression
// evaluator's qualified column resolution.
func (e *Engine) OuterRowsForResolution() []Row {
	return e.outerRowsForResolution()
}

// PushOuterRow sets row as the current outer scope for correlated subquery
// resolution. Exported for the expression evaluator's subquery evaluation.
func (e *Engine) PushOuterRow(row Row) {
	e.pushOuterRow(row)
}

// PopOuterRow restores the outer scope saved by PushOuterRow. Exported for
// the expression evaluator's subquery evaluation.
func (e *Engine) PopOuterRow() {
	e.popOuterRow()
}
