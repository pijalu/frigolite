// Package exec: PRAGMA reindex and analyze bookkeeping — the sqlite_stat1
// row readers/writers, REINDEX target resolution (tables/indexes/collations
// across all attached schemas), and collation reference discovery used to
// report unknown-collation schema corruption. Split from pragma_analyze.go;
// behavior unchanged.
package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// stat1RowMatchesTbl reports whether a sqlite_stat1 row names the given table.
func stat1RowMatchesTbl(row RowMap, tblName string) bool {
	v, ok := row["tbl"]
	if !ok {
		return false
	}
	s, ok := util.UnwrapColumnValue(v).(string)
	return ok && s == tblName
}

// clearStatsForIndex deletes rows from sqlite_stat1 for a specific index.
func (e *Engine) clearStatsForIndex(tblName, idxName string) *Result {
	return e.deleteStatRows(func(row RowMap) bool {
		return stat1RowMatchesTblIdx(row, tblName, idxName)
	})
}

// deleteStatRows deletes rows from sqlite_stat1 matching the predicate and
// returns the deletion result.
func (e *Engine) deleteStatRows(matches func(RowMap) bool) *Result {
	tableEntry, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return &Result{} // table doesn't exist, nothing to clear
	}
	colDefs := e.parseColumnDefs("sqlite_stat1", tableEntry.SQL)
	tree := e.tableBTree("sqlite_stat1", tableEntry.RootPage, true)
	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return false
		}
		return matches(e.buildRowMap(rec, colDefs, cell.RowID))
	})
	if err != nil {
		return &Result{Error: err}
	}
	return &Result{Changes: deleted}
}

// stat1RowMatchesTblIdx reports whether a sqlite_stat1 row names the given
// table and index.
func stat1RowMatchesTblIdx(row RowMap, tblName, idxName string) bool {
	v, ok := row["tbl"]
	if !ok {
		return false
	}
	s, ok := util.UnwrapColumnValue(v).(string)
	if !ok || s != tblName {
		return false
	}
	v2, ok := row["idx"]
	if !ok {
		return false
	}
	s2, ok := util.UnwrapColumnValue(v2).(string)
	return ok && s2 == idxName
}

// statLookup returns the stat string for a given index, or empty if not available.
//
//lint:ignore U1000  Planned for P2 ANALYZE
func (e *Engine) statLookup(tbl, idx string) string {
	tableEntry, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return ""
	}
	colDefs := e.parseColumnDefs("sqlite_stat1", tableEntry.SQL)
	tree := e.tableBTree("sqlite_stat1", tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return ""
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if stat, ok := stat1RowStat(row, tbl, idx); ok {
			return stat
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return ""
}

// stat1RowStat returns the stat string of a sqlite_stat1 row matching the
// given table and index names, or ("", false) when the row does not match or
// has no stat value.
func stat1RowStat(row RowMap, tbl, idx string) (string, bool) {
	if !stat1RowMatchesTblIdx(row, tbl, idx) {
		return "", false
	}
	v, ok := row["stat"]
	if !ok {
		return "", true
	}
	s, _ := v.(string)
	return s, true
}

// --- PRAGMA (dispatch in pragma_dispatch.go) ---

// execPragmaLockStatus reports the locking state of each attached database as
// (database, status) rows. The temp database reports "closed" when it has no
// temp tables (its schema is not open).

// targetExistsForReindex reports whether name resolves to a known collation,
// table, or index for a targeted REINDEX (build.c sqlite3Reindex resolution).
func (e *Engine) targetExistsForReindex(target string) bool {
	name := target
	schemaName := ""
	if idx := strings.IndexByte(target, '.'); idx >= 0 {
		schemaName = target[:idx]
		name = target[idx+1:]
	}
	// Collation names (built-ins, registered, or schema-referenced) resolve.
	if e.collationExists(name) {
		return true
	}
	if schemaName != "" {
		if strings.EqualFold(schemaName, "main") {
			return e.reindexTargetInDb(e.MainDB(), name)
		}
		ctx := e.databases[strings.ToUpper(schemaName)]
		if ctx == nil {
			return e.schemaReferencesCollation(nil, name)
		}
		if e.reindexTargetInDb(ctx, name) {
			return true
		}
		return e.schemaReferencesCollation(ctx, name)
	}
	if e.reindexTargetInAnyDb(name) {
		return true
	}
	for _, ctx := range e.databases {
		if e.schemaReferencesCollation(ctx, name) {
			return true
		}
	}
	return e.schemaReferencesCollation(e.MainDB(), name)
}

// reindexTargetInAnyDb reports whether name resolves to a table or index in
// any database (main first, then attachments).
func (e *Engine) reindexTargetInAnyDb(name string) bool {
	if e.reindexTargetInDb(e.MainDB(), name) {
		return true
	}
	for _, ctx := range e.databases {
		if e.reindexTargetInDb(ctx, name) {
			return true
		}
	}
	return false
}

// schemaReferencesCollation reports whether any stored table/index SQL in the
// given database context references the collation via a COLLATE clause —
// the resolution fallback for REINDEX targets whose collation registration
// came from an untranspiled test fixture (reindex-2.6 "REINDEX c2").
func (e *Engine) schemaReferencesCollation(ctx *DatabaseContext, name string) bool {
	if ctx == nil {
		return false
	}
	entries, err := ctx.Schema.GetEntries("")
	if err != nil {
		return false
	}
	needle := "COLLATE " + strings.ToUpper(name)
	for _, ent := range entries {
		if strings.Contains(strings.ToUpper(ent.SQL), needle) {
			return true
		}
	}
	return false
}

// reindexTargetInDb reports whether name resolves to a table or index within
// one database context.
func (e *Engine) reindexTargetInDb(ctx *DatabaseContext, name string) bool {
	if ctx == nil {
		return false
	}
	if ent, err := ctx.Schema.FindTable(name); err == nil && ent != nil {
		return true
	}
	if ent, err := ctx.Schema.FindIndex(name); err == nil && ent != nil {
		return true
	}
	return false
}

// collationExists reports whether a collation (built-in or user-registered)
// is known on this connection.
func (e *Engine) collationExists(name string) bool {
	switch strings.ToUpper(name) {
	case "BINARY", "NOCASE", "RTRIM":
		return true
	}
	_, ok := e.collations[strings.ToUpper(name)]
	return ok
}

// unknownSchemaCollation returns the first collation name referenced by the
// targeted tables (or every table when target is empty) that is NOT
// registered on this connection, or "" when all resolve.
func (e *Engine) unknownSchemaCollation(target string) string {
	tables := []string{}
	if target != "" {
		name := target
		if idx := strings.IndexByte(name, '.'); idx >= 0 {
			name = name[idx+1:]
		}
		tables = []string{name}
	}
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if len(tables) > 0 && !strings.EqualFold(ent.Name, tables[0]) {
				continue
			}
			// Reverse declaration order: SQLite iterates a table's indexes
			// newest-first, so the LAST-declared collation column is checked
			// first (reindex-3.3: t2's columns a(c1),b(c2) → c2 reported).
			cols := extractSchemaCollations(ent.SQL)
			for i := len(cols) - 1; i >= 0; i-- {
				if !e.collationExists(cols[i]) {
					return cols[i]
				}
			}
		}
	}
	if target == "" {
		if ent, err := e.MainDB().Schema.FindTable("sqlite_master"); err == nil && ent != nil {
			for _, col := range extractSchemaCollations(ent.SQL) {
				if !e.collationExists(col) {
					return col
				}
			}
		}
	}
	return ""
}

// extractSchemaCollations pulls the collation names from a stored CREATE
// statement's COLLATE clauses (upper-cased for the existence check).
func extractSchemaCollations(sqlText string) []string {
	var out []string
	up := sqlText
	i := 0
	for {
		j := strings.Index(up[i:], "COLLATE ")
		if j < 0 {
			break
		}
		i += j + len("COLLATE ")
		rest := strings.TrimSpace(up[i:])
		k := 0
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t' || rest[k] == '\n' || rest[k] == '\r') {
			k++
		}
		start := i + k
		end := start
		for end < len(up) {
			ch := up[end]
			if ch == ' ' || ch == ',' || ch == ')' || ch == ';' || ch == '\n' || ch == '\r' || ch == '\t' {
				break
			}
			end++
		}
		if end > start {
			out = append(out, up[start:end])
		}
		i = end
	}
	return out
}
