// REINDEX support: physical index rebuilds. SQLite's REINDEX empties each
// target index b-tree and re-inserts every table row's key (build.c
// sqlite3Reindex generates the clear+insert program per index), so an index
// whose keys' collation sequence changed is rebuilt in the NEW order. The
// rebuild reuses the DML index-maintenance path — writeIndexCell with the
// collation-aware comparator — so rebuilt trees order exactly like rows
// inserted by DML.

package execdml

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// autoindexKeyColumns derives an autoindex's key columns from the table's
// constraints, by the autoindex's 1-based ordinal in its name
// (sqlite_autoindex_<table>_<N>): N=1 is the PRIMARY KEY (when it is not an
// INTEGER rowid alias, which has no autoindex), then the column-level UNIQUE
// declarations in column order, then the table-level UNIQUE constraints in
// declaration order — SQLite's autoindex creation order.
func autoindexKeyColumns(tableEntry *schema.Entry, e *DMLExecutor, colDefs []sql.ColumnDef, indexName string) []string {
	ordinal := 0
	if idx := strings.LastIndexByte(strings.ToUpper(indexName), '_'); idx >= 0 {
		if n, err := strconv.Atoi(indexName[idx+1:]); err == nil {
			ordinal = n
		}
	}
	if ordinal <= 0 {
		return nil
	}
	// Candidate constraint column lists in creation order. The engine's
	// schema materializes an autoindex entry for every PK/UNIQUE constraint
	// (including an INTEGER rowid-alias PK's entry), so the candidate list
	// mirrors the entries the schema actually created.
	var list [][]string
	// 1. PRIMARY KEY: column-level flags (covers the promoted table-level
	// PK spelling) or the table-level PRIMARY KEY constraint columns.
	var pkCols []string
	for i := range colDefs {
		if colDefs[i].PrimaryKey {
			pkCols = append(pkCols, colDefs[i].Name)
		}
	}
	if len(pkCols) == 0 {
		for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
			if tc.Type != sql.ConstraintPrimaryKey {
				continue
			}
			for _, ic := range tc.Columns {
				pkCols = append(pkCols, ic.Name)
			}
			break
		}
	}
	if len(pkCols) > 0 {
		list = append(list, pkCols)
	}
	// 2. column-level UNIQUE declarations in column order.
	for i := range colDefs {
		if colDefs[i].Unique {
			list = append(list, []string{colDefs[i].Name})
		}
	}
	// 3. table-level UNIQUE constraints in declaration order.
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintUnique {
			continue
		}
		var cols []string
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		if len(cols) > 0 {
			list = append(list, cols)
		}
	}
	if ordinal > len(list) {
		return nil
	}
	return list[ordinal-1]
}

// RebuildIndex clears one index's b-tree and re-inserts every table row's
// key with the index's collation ordering. Returns the number of entries
// written.
func (e *DMLExecutor) RebuildIndex(ctx *DatabaseContext, tableEntry *schema.Entry, indexEntry *schema.Entry) (int, error) {
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	def := e.indexDefForEntry(ctx, tableEntry, indexEntry, colDefs)
	if def == nil {
		return 0, fmt.Errorf("unable to identify the object to be reindexed")
	}

	// Empty the index b-tree in place (the schema rootpage stays valid,
	// sqlite3BtreeClearTable semantics).
	tree := btree.NewBTree(ctx.Pager, indexEntry.RootPage, false)
	if err := tree.Clear(); err != nil {
		return 0, err
	}

	colIndex := buildColumnIndex(colDefs)
	count := 0
	var scanErr error
	e.scanTableForMatch(tableEntry, func(rec *storage.Record, cell *storage.Cell) bool {
		rowID := cell.RowID
		row := buildRowMapFromValues(rec.Values, colDefs, rowID)
		values := make([]interface{}, len(rec.Values))
		copy(values, rec.Values)
		inIndex, werr := e.indexRowIncluded(*def, row)
		if werr != nil {
			scanErr = werr
			return true
		}
		if !inIndex {
			return false
		}
		indexValues, kerr := e.indexKeyValuesForRow(*def, colDefs, colIndex, values, row)
		if kerr != nil {
			scanErr = kerr
			return true
		}
		if err := e.writeIndexCell(*def, colDefs, append(indexValues, rowID)); err != nil {
			scanErr = err
			return true
		}
		count++
		return false
	})
	if scanErr != nil {
		return count, scanErr
	}
	// A mid-rebuild split moves the index root; the write path persists it
	// (updateIndexRootPage), so nothing further is needed here.
	return count, nil
}

// indexDefForEntry builds the maintenance-time indexDef for one schema
// entry, deriving key columns for autoindex entries from the table's
// PRIMARY KEY/UNIQUE constraint.
func (e *DMLExecutor) indexDefForEntry(ctx *DatabaseContext, tableEntry *schema.Entry, indexEntry *schema.Entry, colDefs []sql.ColumnDef) *indexDef {
	indexSQL := indexEntry.SQL
	if strings.TrimSpace(indexSQL) == "" {
		cols := autoindexKeyColumns(tableEntry, e, colDefs, indexEntry.Name)
		if len(cols) == 0 {
			return nil
		}
		indexSQL = fmt.Sprintf("CREATE INDEX x ON %s(%s)", tableEntry.Name, strings.Join(cols, ", "))
	}
	colText := indexColumnListText(indexSQL)
	if colText == "" {
		return nil
	}
	cols := parseIndexKeyCols(colText)
	if len(cols) == 0 {
		return nil
	}
	def := &indexDef{Name: indexEntry.Name, Cols: cols, RootPage: indexEntry.RootPage, Ctx: ctx, SQL: indexSQL}
	if wm := indexWhereRe.FindStringSubmatch(indexSQL); wm != nil {
		def.Where = strings.TrimSpace(wm[1])
	}
	return def
}
