// Package exec: PRAGMA reindex and analyze bookkeeping — the sqlite_stat1
// row readers/writers, REINDEX target resolution (tables/indexes/collations
// across all attached schemas), and collation reference discovery used to
// report unknown-collation schema corruption. Split from pragma_analyze.go;
// behavior unchanged.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// reindexTargetNoIndexFallback resolves a targeted REINDEX that matched no
// index entry (build.c sqlite3Reindex resolution order: collation, then
// TABLE via sqlite3FindTable — virtual tables included — then index; only
// when none resolve does it report "unable to identify the object to be
// reindexed"). A named table with zero schema index entries — e.g. an rtree
// virtual table (rtree PK/UNIQUE constraints are module-delegated and
// materialize no sqlite_autoindex) or a plain indexless table — is a
// successful no-op: reindexTable skips IsVirtual tables and iterates the
// possibly-empty pIndex list.
func (e *Engine) reindexTargetNoIndexFallback(target string) ([]reindexIndexTarget, error) {
	obj := reindexTargetObject(target)
	schemaQualified := strings.ContainsRune(target, '.')
	for _, ctx := range e.databases {
		if schemaQualified && !e.targetSchemaMatches(ctx, target) {
			continue
		}
		if ent, err := ctx.Schema.FindTable(obj); err == nil && ent != nil {
			return nil, nil
		}
	}
	// A collation target that no index uses is still a successful no-op
	// REINDEX (build.c matches the collation, finds nothing).
	if e.collationExists(obj) || e.schemaReferencesCollationInAnyDb(obj) {
		return nil, nil
	}
	return nil, fmt.Errorf("unable to identify the object to be reindexed")
}

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
	var tables []string
	if target != "" {
		tables = []string{reindexTargetObject(target)}
	}
	if name := e.unknownTableCollation(tables); name != "" {
		return name
	}
	if target == "" {
		return e.unknownMasterCollation()
	}
	return ""
}

// reindexTargetObject strips the schema qualifier from a REINDEX target
// ("schema.table" → "table").
func reindexTargetObject(target string) string {
	if idx := strings.IndexByte(target, '.'); idx >= 0 {
		return target[idx+1:]
	}
	return target
}

// unknownTableCollation walks every database's table entries (optionally
// restricted to a single table) and reports the first unregistered key
// collation; "" when all resolve.
func (e *Engine) unknownTableCollation(tables []string) string {
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if len(tables) > 0 && !strings.EqualFold(ent.Name, tables[0]) {
				continue
			}
			if name := e.firstUnknownCollation(extractSchemaCollations(ent.SQL)); name != "" {
				return name
			}
		}
	}
	return ""
}

// unknownMasterCollation checks the sqlite_master declaration's collations
// (only consulted for the untargeted whole-schema REINDEX); "" when all
// resolve.
func (e *Engine) unknownMasterCollation() string {
	ent, err := e.MainDB().Schema.FindTable("sqlite_master")
	if err != nil || ent == nil {
		return ""
	}
	return e.firstUnknownCollation(extractSchemaCollations(ent.SQL))
}

// firstUnknownCollation returns the first collation name NOT registered on
// this connection, or "". Reverse declaration order: SQLite iterates a
// table's indexes newest-first, so the LAST-declared collation column is
// checked first (reindex-3.3: t2's columns a(c1),b(c2) → c2 reported).
func (e *Engine) firstUnknownCollation(cols []string) string {
	for i := len(cols) - 1; i >= 0; i-- {
		if !e.collationExists(cols[i]) {
			return cols[i]
		}
	}
	return ""
}

// extractSchemaCollations pulls the collation names from a stored CREATE
// statement's COLLATE clauses (upper-cased for the existence check).
func extractSchemaCollations(sqlText string) []string {
	var out []string
	i := 0
	for {
		tok, next, ok := nextSchemaCollation(sqlText, i)
		if !ok {
			break
		}
		if tok != "" {
			out = append(out, tok)
		}
		i = next
	}
	return out
}

// nextSchemaCollation scans up for the next "COLLATE " clause and returns
// its collation token plus the scan position just past it (ok=false when no
// further clause exists).
func nextSchemaCollation(up string, i int) (token string, next int, ok bool) {
	j := strings.Index(up[i:], "COLLATE ")
	if j < 0 {
		return "", 0, false
	}
	i += j + len("COLLATE ")
	rest := strings.TrimSpace(up[i:])
	k := len(rest) - len(strings.TrimLeft(rest, " \t\n\r"))
	start := i + k
	end := len(up)
	if idx := strings.IndexAny(up[start:], " ,);\n\r\t"); idx >= 0 {
		end = start + idx
	}
	if end > start {
		return up[start:end], end, true
	}
	return "", end, true
}
