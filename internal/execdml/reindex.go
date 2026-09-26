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
	ordinal := autoindexOrdinal(indexName)
	if ordinal <= 0 {
		return nil
	}
	list := autoindexCandidateLists(tableEntry, e, colDefs)
	if ordinal > len(list) {
		return nil
	}
	return list[ordinal-1]
}

// autoindexOrdinal parses the 1-based constraint ordinal from an autoindex
// name (sqlite_autoindex_<table>_<N>); 0 when the name carries no ordinal.
func autoindexOrdinal(indexName string) int {
	ordinal := 0
	if idx := strings.LastIndexByte(strings.ToUpper(indexName), '_'); idx >= 0 {
		if n, err := strconv.Atoi(indexName[idx+1:]); err == nil {
			ordinal = n
		}
	}
	return ordinal
}

// autoindexCandidateLists builds the candidate constraint column lists in
// creation order under the DDL's slot rules (see autoindexSlotLists).
func autoindexCandidateLists(tableEntry *schema.Entry, e *DMLExecutor, colDefs []sql.ColumnDef) [][]string {
	cands := collectAutoindexConstraintCandidates(tableEntry, e, colDefs)
	return autoindexSlotLists(cands, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)), colDefs)
}

// autoindexConstraint is one PK/UNIQUE constraint in table-creation order.
type autoindexConstraint struct {
	cols []string
	isPK bool
	pkDe bool
}

// collectAutoindexConstraintCandidates gathers a table's UNIQUE and PRIMARY
// KEY constraints in creation order: column-level first (in column order,
// UNIQUE before PRIMARY KEY within a column — collectUniqueDefs's order),
// then table-level constraints in declaration order.
func collectAutoindexConstraintCandidates(tableEntry *schema.Entry, e *DMLExecutor, colDefs []sql.ColumnDef) []autoindexConstraint {
	var cands []autoindexConstraint
	for i := range colDefs {
		if colDefs[i].Unique {
			cands = append(cands, autoindexConstraint{cols: []string{colDefs[i].Name}})
		}
		if colDefs[i].PrimaryKey {
			cands = append(cands, autoindexConstraint{cols: []string{colDefs[i].Name}, isPK: true, pkDe: colDefs[i].PKDesc})
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type != sql.ConstraintUnique && tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		var cols []string
		de := false
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
			de = de || ic.Desc
		}
		if len(cols) > 0 {
			cands = append(cands, autoindexConstraint{cols: cols, isPK: tc.Type == sql.ConstraintPrimaryKey, pkDe: de})
		}
	}
	return cands
}

// autoindexSlotLists applies the DDL's slot assignment to the ordered
// constraint candidates:
//   - a rowid table's INTEGER PRIMARY KEY rowid alias gets NO index and
//     consumes NO slot — skipping it here keeps the sqlite_autoindex_<table>_<N>
//     ordinals aligned with the entries the schema materialized (prepending
//     the PK shifted every ordinal and keyed the _1 tree on the rowid-alias
//     column instead of the first UNIQUE constraint's column);
//   - on WITHOUT ROWID the PRIMARY KEY is the clustered key: no entry of its
//     own, and an equivalent UNIQUE slot created earlier is absorbed (the
//     slot stays consumed, later ordinals unchanged);
//   - a duplicate of an already-indexed column set creates no entry and no
//     slot.
func autoindexSlotLists(cands []autoindexConstraint, isWR bool, colDefs []sql.ColumnDef) [][]string {
	seen := map[string]bool{}
	absorbed := map[string]bool{}
	var order []string
	keyCols := map[string][]string{}
	for _, u := range cands {
		if autoindexIsRowidAliasPK(u, isWR, colDefs) {
			continue
		}
		key := autoindexConstraintKey(u.cols)
		if u.isPK && isWR {
			// The clustered PRIMARY KEY: no entry of its own; absorb an
			// equivalent index created earlier in the constraint list.
			seen[key] = true
			absorbed[key] = true
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		order = append(order, key)
		keyCols[key] = u.cols
	}
	list := make([][]string, 0, len(order))
	for _, k := range order {
		if absorbed[k] {
			continue
		}
		list = append(list, keyCols[k])
	}
	return list
}

// autoindexIsRowidAliasPK reports whether one constraint is a single-column
// PRIMARY KEY that aliases the rowid on a rowid table (such a PK owns no
// implicit index and consumes no autoindex slot).
func autoindexIsRowidAliasPK(u autoindexConstraint, isWR bool, colDefs []sql.ColumnDef) bool {
	if isWR || !u.isPK || len(u.cols) != 1 {
		return false
	}
	cd := colDefAt(colDefs, u.cols[0])
	if cd == nil {
		return false
	}
	alias := *cd
	alias.PrimaryKey = true
	alias.PKDesc = u.pkDe
	return IsIPKRowidAliasCol(alias)
}

// autoindexConstraintKey builds the duplicate-detection key for a
// constraint's column list: a comma join of the lowercased column names.
func autoindexConstraintKey(cols []string) string {
	lowered := make([]string, len(cols))
	for i, c := range cols {
		lowered[i] = strings.ToLower(c)
	}
	return strings.Join(lowered, ",")
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
