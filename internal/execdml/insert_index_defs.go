// This file resolves the unique-index definitions DML conflict scans run
// against: UNIQUE indexes, PK/UNIQUE table constraints and their autoindex
// columns, per database context, plus the partial-index row matching used
// during conflict scans.
package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// allMatch returns true if ALL columns in the group match between the existing
// record and the new values. NULL values never match (NULL != NULL). Each
// column's declared collation is applied to its comparison.

func (e *DMLExecutor) uniqueIndexColumns(tableName string) []uniqueIndexDef {
	e.ctx.InitUniqueIdxCache()
	if defs, ok := e.ctx.CachedUniqueIdx(tableName); ok {
		return defs
	}
	var result []uniqueIndexDef
	for _, ctx := range e.ctx.Databases() {
		result = append(result, e.uniqueIndexDefsIn(ctx, tableName)...)
	}
	// SQLite checks UNIQUE indexes newest-first (its table index list is
	// prepended on creation), so the error text names the most recently
	// created conflicting index. Reverse to match the exact error message.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	e.ctx.SetCachedUniqueIdx(tableName, result)
	return result
}

// uniqueIndexDefsIn returns the UNIQUE indexes for a table defined in one
// database context.
func (e *DMLExecutor) uniqueIndexDefsIn(ctx *DatabaseContext, tableName string) []uniqueIndexDef {
	var result []uniqueIndexDef
	entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return result
	}
	// The table entry provides CREATE TABLE SQL for deriving autoindex
	// columns (sqlite_autoindex_* entries store no SQL; their columns come
	// from the table's PRIMARY KEY / UNIQUE constraints).
	tableEntry, _ := ctx.Schema.FindTable(tableName)
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, tableName) {
			continue
		}
		var cols []string
		var keyColl []string
		if uniqueIndexColsRe.MatchString(ent.SQL) {
			colText := indexColumnListText(ent.SQL)
			if colText == "" {
				continue
			}
			cols = parseIndexKeyCols(colText)
			keyColl = parseIndexKeyCollations(colText)
		} else if tableEntry != nil && strings.HasPrefix(strings.ToUpper(ent.Name), "SQLITE_AUTOINDEX_") {
			// An autoindex created for a table-level PRIMARY KEY or UNIQUE
			// constraint: its columns are the constraint's columns (SQLite
			// assigns sqlite_autoindex_<table>_<N> in creation order: PK
			// first when both exist).
			cols = autoindexConstraintColumns(tableEntry, e)
		}
		if len(cols) == 0 {
			continue
		}
		def := uniqueIndexDef{Name: ent.Name, Cols: cols, KeyColl: keyColl}
		if wm := indexWhereRe.FindStringSubmatch(ent.SQL); wm != nil {
			def.Where = strings.TrimSpace(wm[1])
		}
		result = append(result, def)
	}
	return result
}

// autoindexConstraintColumns returns the column list of a table's PRIMARY KEY
// or UNIQUE constraint for its autoindex (the first table-level constraint
// found; SQLite creates one autoindex per constraint in declaration order).
func autoindexConstraintColumns(tableEntry *schema.Entry, e *DMLExecutor) []string {
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintPrimaryKey && tc.Type != sql.ConstraintUnique {
			continue
		}
		var cols []string
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		if len(cols) > 0 {
			return cols
		}
	}
	return nil
}

// parseIndexKeyCols parses a CREATE INDEX key column-list into stripped key
// expressions (plain names or expression text), removing COLLATE/ASC/DESC
// suffixes where they are not part of an expression.

// allTableIndexes returns every index defined on the given table (unique and
// non-unique alike), with their key expressions, partial predicates, and root
// pages. This drives index maintenance on INSERT.
func (e *DMLExecutor) allTableIndexes(tableName string) []indexDef {
	var result []indexDef
	for _, ctx := range e.ctx.Databases() {
		result = append(result, e.indexDefsIn(ctx, tableName)...)
	}
	return result
}

// indexDefsIn returns the indexes for a table defined in one database
// context.
func (e *DMLExecutor) indexDefsIn(ctx *DatabaseContext, tableName string) []indexDef {
	var result []indexDef
	entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return result
	}
	for _, ent := range entries {
		if !strings.EqualFold(ent.TblName, tableName) {
			continue
		}
		colText := indexColumnListText(ent.SQL)
		if colText == "" {
			continue
		}
		cols := parseIndexKeyCols(colText)
		if len(cols) == 0 {
			continue
		}
		def := indexDef{Name: ent.Name, Cols: cols, RootPage: ent.RootPage, Ctx: ctx, SQL: ent.SQL}
		if wm := indexWhereRe.FindStringSubmatch(ent.SQL); wm != nil {
			def.Where = strings.TrimSpace(wm[1])
		}
		result = append(result, def)
	}
	return result
}

// indexDef describes any (unique or non-unique) index for index maintenance.

// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.
func (e *DMLExecutor) findRowByIndexCols(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, def uniqueIndexDef) (int64, []interface{}, bool) {
	colIndex := buildColumnIndex(colDefs)
	// The new row must itself satisfy the partial-index predicate.
	row := buildRowMapFromValues(values, colDefs, 0)
	if inIndex, _ := e.evalIndexWhere(def.Where, row); !inIndex {
		return 0, nil, false
	}
	idxCols := def.Cols
	key := make([]interface{}, len(idxCols))
	for i, cn := range idxCols {
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, values, row)
		if !ok {
			return 0, nil, false
		}
		key[i] = kv
	}
	cell, rec, _ := e.scanTableForMatch(tableEntry, func(rc *storage.Record, cl *storage.Cell) bool {
		return e.rowMatchesIndexKey(rc, cl, colDefs, colIndex, idxCols, key, def)
	})
	if cell == nil {
		return 0, nil, false
	}
	return cell.RowID, rec.Values, true
}

// rowMatchesIndexKey reports whether an existing row belongs to the partial
// index and matches the new row's key on every index column.
func (e *DMLExecutor) rowMatchesIndexKey(rc *storage.Record, cl *storage.Cell, colDefs []sql.ColumnDef, colIndex map[string]int, idxCols []string, key []interface{}, def uniqueIndexDef) bool {
	// Only rows satisfying the partial predicate are in the index.
	erow := buildRowMapFromValues(rc.Values, colDefs, cl.RowID)
	if inIndex, _ := e.evalIndexWhere(def.Where, erow); !inIndex {
		return false
	}
	for i, cn := range idxCols {
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, rc.Values, erow)
		if !ok {
			return false
		}
		// Apply the column's declared-type affinity to both sides so an
		// int literal key matches the TEXT-stored value (b TEXT, key 2).
		cd := colDefAt(colDefs, cn)
		typ := ""
		if cd != nil {
			typ = cd.Type
		}
		// Index-key collation (build.c sqlite3CreateIndex): the key's
		// explicit COLLATE wins; a plain column key falls back to the
		// column's declared collation (BINARY when neither). Without this,
		// `CREATE UNIQUE INDEX i ON t(a COLLATE NOCASE)` misses 'ABC' vs
		// 'abc' (collate4-3.11).
		coll := ""
		if i < len(def.KeyColl) && def.KeyColl[i] != "" {
			coll = def.KeyColl[i]
		} else if cd != nil {
			coll = cd.Collate
		}
		// Expression keys may carry a CollatedValue wrapper (the index
		// key's explicit COLLATE, e.g. substr(b,2,4) COLLATE nocase). Peel
		// both wrapper layers before comparing: the collation is already
		// resolved into `coll` above, and comparing the wrapper structs
		// themselves would classify distinct keys as equal — a false UNIQUE
		// conflict on the second row of any expression index (indexexpr1-4.x).
		kv = execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(kv))
		key[i] = execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(key[i]))
		if e.ctx.CompareValuesCollate(util.ApplyColumnAffinity(kv, typ), util.ApplyColumnAffinity(key[i], typ), coll) != 0 {
			return false
		}
	}
	return true
}

// isIPKRowidAliasCol reports whether a column is an INTEGER PRIMARY KEY
// rowid-alias candidate: PRIMARY KEY, declared type exactly INTEGER
// (case-insensitive), and NOT PRIMARY KEY DESC. SQLite treats INTEGER
// PRIMARY KEY DESC as an ordinary (non-rowid) column (build.c
// sqlite3AddPrimaryKey checks pCol->sortOrder), so DESC columns get a
// separate autoindex and their own rowid.

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
