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
	"github.com/pijalu/frigolite/internal/sql"
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

// --- REINDEX execution (build.c sqlite3Reindex) ---

func (e *Engine) execReindex(s *sql.ReindexStmt) *Result {
	// A targeted or whole-schema REINDEX re-builds indexes whose keys use
	// their tables' declared collations: an unknown collation fails with
	// "no such collation sequence: NAME" (build.c sqlite3Reindex →
	// sqlite3CheckCollationSeq; reindex-3.3: a second connection without
	// the c1/c2 UDF collations registered).
	if unknown := e.unknownSchemaCollation(s.Target); unknown != "" {
		return &Result{Error: fmt.Errorf("no such collation sequence: %s", unknown)}
	}
	// REINDEX with a target that names no known collation, table, or index
	// fails (build.c sqlite3Reindex: "unable to identify the object to be
	// reindexed"; reindex.test 4.x "REINDEX bogus").
	if target := strings.TrimSpace(s.Target); target != "" {
		if !e.targetExistsForReindex(target) {
			return &Result{Error: fmt.Errorf("unable to identify the object to be reindexed")}
		}
	}
	seen := make(map[string]string) // index name -> table
	if err := e.checkReindexIndexTables(seen); err != nil {
		return &Result{Error: err}
	}
	if res := e.reindexCollationTargetGuard(s.Target); res != nil {
		return res
	}
	// Physical rebuild: clear each target index b-tree and re-insert every
	// table row's key (build.c sqlite3Reindex's clear+insert program), so
	// indexes rebuilt under a CHANGED collation sequence take the new order.
	targets, err := e.reindexTargets(s.Target)
	if err != nil {
		return &Result{Error: err}
	}
	for _, t := range targets {
		if _, err := e.dml.RebuildIndex(t.ctx, t.table, t.index); err != nil {
			return &Result{Error: fmt.Errorf("(at rebuild %s) %w", t.index.Name, err)}
		}
	}
	return &Result{}
}

// reindexCollationTargetGuard fails a collation-named REINDEX target that the
// schema references but this connection cannot resolve, before any rebuild
// (build.c sqlite3Reindex → sqlite3CheckCollationSeq: reindex-3.1, a second
// connection without the c1 collation registered).
func (e *Engine) reindexCollationTargetGuard(target string) *Result {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	obj := reindexTargetObject(target)
	if !e.reindexTargetInAnyDb(obj) && !e.collationExists(obj) && e.LookupCollation(obj) == nil && e.schemaReferencesCollationInAnyDb(obj) {
		return &Result{Error: fmt.Errorf("no such collation sequence: %s", obj)}
	}
	return nil
}

// reindexIndexTarget is one rebuild unit: an index entry, its table, and the
// database context holding both.
type reindexIndexTarget struct {
	ctx   *DatabaseContext
	table *schema.Entry
	index *schema.Entry
}

// reindexTargets resolves a REINDEX target to the index entries to rebuild:
// every index in every schema for an empty target, the named index, the
// table's indexes, or — when the target names a collation — every index
// whose keys use that collation (build.c sqlite3Reindex resolution).
func (e *Engine) reindexTargets(target string) ([]reindexIndexTarget, error) {
	target = strings.TrimSpace(target)
	var out []reindexIndexTarget
	matched := false
	for _, ctx := range e.databases {
		m, targets := e.reindexTargetsInDb(ctx, target)
		out = append(out, targets...)
		matched = matched || m
	}
	if target != "" && !matched {
		return e.reindexTargetNoIndexFallback(target)
	}
	return out, nil
}

// reindexTargetsInDb collects one database context's rebuildable index
// targets. matched reports whether any entry matched the target.
func (e *Engine) reindexTargetsInDb(ctx *DatabaseContext, target string) (matched bool, out []reindexIndexTarget) {
	indexEntries, err := ctx.Schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return false, nil
	}
	for _, idxEnt := range indexEntries {
		if target != "" && !e.reindexEntryMatchesTarget(ctx, idxEnt, target) {
			continue
		}
		tblEnt, err := ctx.Schema.FindTable(idxEnt.TblName)
		if err != nil || tblEnt == nil {
			continue
		}
		out = append(out, reindexIndexTarget{ctx: ctx, table: tblEnt, index: idxEnt})
		matched = true
	}
	return matched, out
}

// reindexEntryMatchesTarget applies build.c sqlite3Reindex's target match for
// one index entry: a schema-qualified target matches only within its schema;
// a target naming the index or its table selects it; any other target is a
// collation name the index's keys must use.
func (e *Engine) reindexEntryMatchesTarget(ctx *DatabaseContext, idxEnt *schema.Entry, target string) bool {
	obj := reindexTargetObject(target)
	schemaQualified := strings.ContainsRune(target, '.')
	namedIndex := strings.EqualFold(idxEnt.Name, obj)
	namedTable := strings.EqualFold(idxEnt.TblName, obj)
	schemaOK := !schemaQualified || e.targetSchemaMatches(ctx, target)
	switch {
	case namedIndex && schemaOK:
		// named index
	case namedTable && schemaOK:
		// named table: all its indexes
	default:
		return e.indexUsesCollation(ctx, idxEnt, obj)
	}
	return true
}

// targetSchemaMatches reports whether a schema-qualified REINDEX target
// ("main.t1") names the given database context.
func (e *Engine) targetSchemaMatches(ctx *DatabaseContext, target string) bool {
	idx := strings.IndexByte(target, '.')
	if idx < 0 {
		return true
	}
	return strings.EqualFold(target[:idx], e.schemaNameOf(ctx))
}

// schemaNameOf returns the registered name of a database context.
func (e *Engine) schemaNameOf(ctx *DatabaseContext) string {
	for name, c := range e.databases {
		if c == ctx {
			return name
		}
	}
	return ""
}

// indexUsesCollation reports whether an index's key collations include the
// named collation.
func (e *Engine) indexUsesCollation(ctx *DatabaseContext, idxEnt *schema.Entry, collation string) bool {
	if !e.collationExists(collation) && !e.schemaReferencesCollation(ctx, collation) {
		return false
	}
	colDefs := e.indexTableColumnDefs(ctx, idxEnt.TblName)
	tblEnt, _ := ctx.Schema.FindTable(idxEnt.TblName)
	for _, name := range e.dml.IndexEntryKeyCollations(ctx, tblEnt, idxEnt, colDefs) {
		if strings.EqualFold(name, collation) {
			return true
		}
	}
	return false
}

// schemaReferencesCollationInAnyDb reports whether any stored schema SQL
// references the collation.
func (e *Engine) schemaReferencesCollationInAnyDb(name string) bool {
	for _, ctx := range e.databases {
		if e.schemaReferencesCollation(ctx, name) {
			return true
		}
	}
	return false
}

// checkReindexIndexTables verifies that duplicate index names across attached
// databases resolve to the same table (SQLite's integrity rule behind
// REINDEX's name-keyed index lookup); a mismatch is a malformed image.
func (e *Engine) checkReindexIndexTables(seen map[string]string) error {
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if prev, ok := seen[strings.ToUpper(ent.Name)]; ok {
				if !strings.EqualFold(prev, ent.TblName) {
					return fmt.Errorf("database disk image is malformed")
				}
			}
			seen[strings.ToUpper(ent.Name)] = ent.TblName
		}
	}
	return nil
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
