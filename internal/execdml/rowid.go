package execdml

import (
	"math"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
)

func (e *DMLExecutor) findNextRowID(tableName string, rootPage uint32) int64 {
	pg := e.dmlPager(tableName)
	// AUTOINCREMENT tables use a persistent sequence: the largest rowid ever
	// used is remembered (like SQLite's sqlite_sequence), so after DELETE the
	// next rowid still continues from the old maximum.
	if e.ctx.TableHasAutoIncrement(tableName) {
		if next, ok := e.autoIncNextRowID(pg, rootPage, tableName); ok {
			return next
		}
	}
	return e.plainNextRowID(pg, rootPage, tableName)
}

// dmlPager resolves the pager for the table being modified. During an INSERT
// the current DML context identifies the modified table's owning database
// (aux.t8 vs main.t8 can share rootPage but live on different pagers);
// resolving from that context keeps the rowid cache collision-free across
// schemas.
func (e *DMLExecutor) dmlPager(tableName string) *pager.Pager {
	if ctx := e.currentDMLCtx; ctx != nil && ctx.Pager != nil {
		return ctx.Pager
	}
	return e.ctx.TablePager(tableName)
}

// dmlTableBTree builds the b-tree of the table being modified. Like dmlPager,
// it binds the tree to the current DML context's pager: a bare name resolves
// temp/main/attached (main-first), so "a1.t0" with a same-named main.t0 would
// otherwise read and write main's tree instead of the DML target's.
func (e *DMLExecutor) dmlTableBTree(tableName string, rootPage uint32) *btree.BTree {
	if ctx := e.currentDMLCtx; ctx != nil && ctx.Pager != nil {
		return e.ctx.TableBTreePg(ctx.Pager, tableName, rootPage, true)
	}
	return e.ctx.TableBTreeForName(tableName, rootPage, true)
}

// autoIncNextRowID returns (next, true) when the AUTOINCREMENT sequence
// determines the next rowid; (0, false) falls through to the plain path.
func (e *DMLExecutor) autoIncNextRowID(pg *pager.Pager, rootPage uint32, tableName string) (int64, bool) {
	// Cached statement-start sequence (sqlite3AutoincrementBegin).
	if seq, ok := e.ctx.AutoIncSeqFor(pg, rootPage); ok {
		return seqOverflowNext(seq), true
	}
	// No cached sequence: read the real sqlite_sequence table. The row may
	// not exist yet (first insert on a new AUTOINCREMENT table).
	seq, found, err := e.ctx.SQLiteSequenceSeqFor(pg, tableName)
	if err != nil || !found {
		return 0, false
	}
	// SQLite uses the larger of sqlite_sequence.seq and the table's current
	// maximum rowid, after numeric affinity coercion.
	tree := btree.NewBTree(pg, e.ctx.RootPagePg(pg, tableName, rootPage), true)
	if maxID := e.scanMaxRowID(tree); maxID > seq {
		seq = maxID
	}
	e.ctx.SetAutoIncSeqFor(pg, rootPage, seq)
	return seqOverflowNext(seq), true
}

// seqOverflowNext maps an AUTOINCREMENT sequence to its next rowid: 0 signals
// exhaustion ("database or disk is full") once seq reaches the 32-bit limit.
func seqOverflowNext(seq int64) int64 {
	if seq >= math.MaxInt32 {
		return 0
	}
	return seq + 1
}

// plainNextRowID computes the next rowid for a non-AUTOINCREMENT path (or an
// AUTOINCREMENT table with no sequence yet): cached largest-rowid fast path,
// otherwise a full scan.
func (e *DMLExecutor) plainNextRowID(pg *pager.Pager, rootPage uint32, tableName string) int64 {
	isAutoInc := e.ctx.TableHasAutoIncrement(tableName)
	// Use the cached largest rowid when available (SQLite keeps the largest
	// rowid seen so far and recomputes it only after a DELETE or when the
	// cache is empty). This avoids a full-table scan per insert, which is
	// O(n²) for bulk auto-rowid inserts (e.g. selectG inserts 100k rows).
	if cached, ok := e.ctx.NextRowIDFor(pg, rootPage); ok {
		if cached == math.MaxInt64 {
			return e.overflowRowID(pg, tableName, rootPage, isAutoInc)
		}
		return cached + 1
	}
	tree := btree.NewBTree(pg, e.ctx.RootPagePg(pg, tableName, rootPage), true)
	maxID := e.scanMaxRowID(tree)
	e.ctx.SetNextRowIDFor(pg, rootPage, maxID)
	// AUTOINCREMENT never reuses rowid 1 after the sequence starts; the
	// sequence itself is recorded by bumpRowIDCache on the successful insert.
	if isAutoInc && maxID < 1 {
		return 1
	}
	if maxID == math.MaxInt64 {
		return e.overflowRowID(pg, tableName, rootPage, isAutoInc)
	}
	return maxID + 1
}

// overflowRowID handles a table already holding the maximum rowid:
// AUTOINCREMENT reports exhaustion (0 → "database or disk is full"); plain
// tables fall back to a random positive rowid (sqlite3Randomness + scan for
// a free one) rather than overflowing int64 to MinInt64.
func (e *DMLExecutor) overflowRowID(pg *pager.Pager, tableName string, rootPage uint32, isAutoInc bool) int64 {
	if isAutoInc {
		return 0
	}
	tree := btree.NewBTree(pg, e.ctx.RootPagePg(pg, tableName, rootPage), true)
	return e.ctx.RandomFreeRowID(tree)
}

// scanMaxRowID returns the largest rowid stored in a btree table (0 when
// empty).
func (e *DMLExecutor) scanMaxRowID(tree *btree.BTree) int64 {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0
	}
	var maxID int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		if cell.RowID > maxID {
			maxID = cell.RowID
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return maxID
}

// stripHiddenToken removes a standalone "hidden" word from a column type
// string and reports whether one was found. This mirrors SQLite's
// sqlite3VtabCallConnect post-processing of virtual-table declarations: after
// declare_vtab, each column type is scanned for the token "hidden" (delimited
// by spaces or string boundaries) which flags the column as HIDDEN and is
// stripped from the declared type (vtabA.test: "b HIDDEN VARCHAR" declares a
// column named b of type VARCHAR that is hidden).
func stripHiddenToken(typ string) (string, bool) {
	lower := strings.ToLower(typ)
	i := 0
	for i < len(lower) {
		j := strings.Index(lower[i:], "hidden")
		if j < 0 {
			return typ, false
		}
		j += i
		beforeOK := j == 0 || lower[j-1] == ' '
		after := j + 6
		afterOK := after >= len(lower) || lower[after] == ' '
		if beforeOK && afterOK {
			return removeHiddenToken(typ, j, after), true
		}
		i = j + 6
	}
	return typ, false
}

// removeHiddenToken removes the standalone "hidden" word at [j, j+6) from a
// column type, preserving the rest (and one surrounding space).
func removeHiddenToken(typ string, j, after int) string {
	// Remove "hidden" and one surrounding space, preserving the rest.
	var b strings.Builder
	b.WriteString(typ[:j])
	if j > 0 && typ[j-1] == ' ' {
		b.Reset()
		b.WriteString(typ[:j-1])
	}
	if after < len(typ) && typ[after] == ' ' {
		after++
	}
	b.WriteString(typ[after:])
	return strings.TrimSpace(b.String())
}
