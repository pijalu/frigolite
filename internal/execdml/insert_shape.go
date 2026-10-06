// Package execdml implements DML execution.
package execdml

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// insertTableShape is one target table's insert-path shape: the per-row
// checks SQLite compiles into the VDBE program at prepare time (tabFlags,
// constraint masks, index lists). Frigolite re-derives them from the CREATE
// TABLE text on every row — the WITHOUT ROWID / STRICT / AUTOINCREMENT /
// trigger / FTS / uniqueness-source probes — so a bulk load re-walks the
// same declarations once per row. The shape memo (insertShapeFor) resolves
// all of them once per (table, column definitions, schema fingerprint).
//
// Every field is a pure function of the table's schema row, so the
// fingerprint guard makes the shape invalidate exactly when the historical
// per-row derivations would have seen the change.
type insertTableShape struct {
	withoutRowid   bool // CREATE ... WITHOUT ROWID (tabFlags TF_WithoutRowid)
	strict         bool // CREATE ... STRICT (tabFlags TF_Strict)
	autoinc        bool // INTEGER PRIMARY KEY AUTOINCREMENT declared
	hasTriggers    bool // any trigger registered on the table
	noFTS          bool // neither fts5- nor fts3/4-backed (the plain b-tree shape)
	hasConstraints bool // hasInsertConstraints: NOT NULL/CHECK/PK/UNIQUE anywhere

	// reducedRowidUnique: the table's ONLY uniqueness source is the INTEGER
	// PRIMARY KEY rowid alias (no column-level UNIQUE, no table-level
	// PK/UNIQUE group, no CREATE UNIQUE INDEX). The conflict question then
	// reduces to "does this rowid exist" — one append-biased seek — and the
	// generic unique machinery (value walk, col-list build, composite-group
	// scan, unique-index scan) cannot fire.
	reducedRowidUnique bool
	ipkIdx             int // INTEGER PRIMARY KEY rowid-alias column (-1 none)

	indexDefs []indexDef // allTableIndexes result (read-only by convention)
}

// insertShapeFor returns the memoized insert-path shape for the table,
// keyed on the schema fingerprint + (entry, colDefs) identity — the ciCache
// guard pattern. The shape struct is shared and read-only; callers must not
// mutate it.
func (e *DMLExecutor) insertShapeFor(tableEntry *schema.Entry, colDefs []sql.ColumnDef) *insertTableShape {
	if tableEntry == nil || len(colDefs) == 0 {
		return nil
	}
	fp := e.schemaFingerprint()
	if e.shapeEntry == tableEntry && e.shapeDefs == &colDefs[0] && e.shapeLen == len(colDefs) && e.shapeFingerprint == fp {
		return e.shapeCache
	}
	sh := &insertTableShape{
		withoutRowid:   tableIsWithoutRowid(tableEntry.SQL),
		strict:         isStrictTable(tableEntry.SQL),
		autoinc:        e.ctx.TableHasAutoIncrement(tableEntry.Name),
		hasTriggers:    e.hasTriggersForTable(tableEntry.Name),
		hasConstraints: e.hasInsertConstraints(tableEntry, colDefs),
		ipkIdx:         e.ipkAliasIndex(colDefs),
		indexDefs:      e.allTableIndexes(tableEntry.Name),
	}
	sh.noFTS = !e.isFTSBackedInsert(tableEntry.Name)
	// Uniqueness sources beyond the rowid alias: column-level UNIQUE (the
	// IPK's own PRIMARY KEY is the alias itself), table-level PK/UNIQUE
	// groups, and CREATE UNIQUE INDEX.
	plainUnique := false
	for i := range colDefs {
		cd := &colDefs[i]
		if cd.Unique || (cd.PrimaryKey && i != sh.ipkIdx) {
			plainUnique = true
			break
		}
	}
	sh.reducedRowidUnique = sh.ipkIdx >= 0 && !sh.withoutRowid && !plainUnique &&
		len(e.compositeUniqueGroups(tableEntry.Name, tableEntry.SQL, colDefs)) == 0 &&
		len(e.uniqueIndexColumns(tableEntry.Name)) == 0
	e.shapeFingerprint, e.shapeEntry, e.shapeDefs, e.shapeLen, e.shapeCache = fp, tableEntry, &colDefs[0], len(colDefs), sh
	return sh
}

// isFTSBackedInsert reports whether the named table is backed by either FTS
// engine (fts5 or fts3/4): such tables route rows through insertFTSRow
// instead of the b-tree writer.
func (e *DMLExecutor) isFTSBackedInsert(name string) bool {
	if _, ok := e.ctx.FTS5Tables()[name]; ok {
		return true
	}
	_, ok := e.ctx.FTSTables()[name]
	return ok
}

// reducedRowidUniqueConflict answers the rowid-alias-only uniqueness question
// with the append-biased seek alone (btree.c BTREE_APPEND's moveto bias via
// aboveCachedMaxRowID). A non-int64 IPK value or an unseeded largest-rowid
// cache falls back to the generic machinery (which owns the cache seeding).
// excludeRowID/haveExclude carry the UPDATE-form exclusion (upsert4 DO
// UPDATE probes insert the candidate row first): a found rowid equal to the
// excluded row is the row itself, not a conflict.
func (e *DMLExecutor) reducedRowidUniqueConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, ipkIdx int, excludeRowID int64, haveExclude bool) error {
	v, ok := values[ipkIdx].(int64)
	if !ok {
		colIndex := e.columnIndexFor(colDefs)
		return e.bareUniqueConflictError(tableEntry, colDefs, colIndex, values, excludeRowID, haveExclude)
	}
	if !e.hasCachedMaxRowID(tableEntry.Name, tableEntry.RootPage) {
		// Unseeded cache: the generic path owns the scanMaxRowID seeding and
		// every other uniqueness corner; run it for this row only.
		colIndex := e.columnIndexFor(colDefs)
		return e.bareUniqueConflictError(tableEntry, colDefs, colIndex, values, excludeRowID, haveExclude)
	}
	if e.aboveCachedMaxRowID(tableEntry.Name, tableEntry.RootPage, v) {
		return nil
	}
	tree, owned := e.uniqueScanTree(tableEntry.Name, tableEntry.RootPage)
	if owned {
		defer tree.Close()
	}
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	defer cursor.Close()
	found, err := cursor.SeekToRowID(v)
	if err != nil || !found {
		return nil
	}
	// The seek is by rowid, so a found row's rowid IS v: the UPDATE-form
	// exclusion (upsert4 DO UPDATE) treats the excluded row as itself.
	if haveExclude && v == excludeRowID {
		return nil
	}
	return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, colDefs[ipkIdx].Name)
}
