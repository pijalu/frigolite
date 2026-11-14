package execdml

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// dmlCorruptError is the write-path corruption verdict (where-25.x):
// a UNIQUE index entry whose table row is missing reports SQLITE_CORRUPT —
// "database disk image is malformed" for a rowid table, "corrupt database"
// for a WITHOUT ROWID table (oracle 3.54, where-25.1/25.2 vs 25.5; the two
// texts come from C's distinct raise paths).
func dmlCorruptError(withoutRowid bool) error {
	if withoutRowid {
		return fmt.Errorf("corrupt database")
	}
	return fmt.Errorf("database disk image is malformed")
}

// indexConflictEntry probes a UNIQUE index's own b-tree for the key
// (sqlite3BtreeIndexMoveto under the index's KeyInfo — the same comparator
// the insert side ordered the tree with) and, on an equal entry, returns the
// entry's row locator (the record's trailing value, the rowid every index
// write appends — writeIndexCell's format for both table shapes). C resolves
// the conflict AT the index cursor (OP_Found); the table fetch that follows is
// the recheck whose missing row raises SQLITE_CORRUPT (vdbe FinishSeek /
// OP_NotExists p2==0).
//
// A key with any nil component never conflicts (SQL UNIQUE: NULL compares
// unequal to everything, so OP_Found cannot match one).
//
// ok=false with errFallback=false is a definitive "no equal entry" (the
// previous fast-negative verdict); errFallback=true means the probe was
// unavailable and the caller keeps its scan-based verdict.
func (e *DMLExecutor) indexConflictEntry(tableName, indexName string, colDefs []sql.ColumnDef, key []interface{}) (entryRowID int64, ok bool, errFallback bool) {
	if len(key) == 0 {
		return 0, false, true
	}
	for _, v := range key {
		if v == nil {
			return 0, false, false
		}
	}
	var def *indexDef
	for _, cand := range e.allTableIndexes(tableName) {
		if cand.Name == indexName {
			c := cand
			def = &c
			break
		}
	}
	if def == nil || def.RootPage == 0 {
		return 0, false, true
	}
	ki := e.ikiKeyInfo(*def, colDefs, e.collationLookup())
	if ki == nil {
		return 0, false, true
	}
	tree := btree.NewBTree(def.Ctx.Pager, def.RootPage, false)
	defer tree.Close()
	e.ikiOrder(tree, *def, colDefs, e.collationLookup())
	cursor, err := tree.OpenCursorAtRoot()
	if err != nil {
		return 0, false, true
	}
	found, err := cursor.SeekIndexKey(btree.NewUnpackedIndexKey(ki, key))
	if err != nil {
		return 0, false, true
	}
	if !found {
		return 0, false, false
	}
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return 0, false, true
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil || len(rec.Values) == 0 {
		return 0, false, true
	}
	rid, isInt := rec.Values[len(rec.Values)-1].(int64)
	if !isInt {
		return 0, false, true
	}
	return rid, true, false
}
