package exec

import (
	"encoding/binary"
	"fmt"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/value"
	"strings"
)

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
	// A collation-named target that the schema references but this
	// connection cannot resolve fails before any rebuild (build.c
	// sqlite3Reindex → sqlite3CheckCollationSeq: reindex-3.1, a second
	// connection without the c1 collation registered).
	if target := strings.TrimSpace(s.Target); target != "" {
		obj := reindexTargetObject(target)
		if !e.reindexTargetInAnyDb(obj) && !e.collationExists(obj) && e.LookupCollation(obj) == nil && e.schemaReferencesCollationInAnyDb(obj) {
			return &Result{Error: fmt.Errorf("no such collation sequence: %s", obj)}
		}
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
		indexEntries, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, idxEnt := range indexEntries {
			if target != "" {
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
					if !e.indexUsesCollation(ctx, idxEnt, obj) {
						continue
					}
				}
			}
			tblEnt, err := ctx.Schema.FindTable(idxEnt.TblName)
			if err != nil || tblEnt == nil {
				continue
			}
			out = append(out, reindexIndexTarget{ctx: ctx, table: tblEnt, index: idxEnt})
			matched = true
		}
	}
	if target != "" && !matched {
		// A collation target that no index uses is still a successful
		// no-op REINDEX (build.c matches the collation, finds nothing).
		if e.collationExists(reindexTargetObject(target)) || e.schemaReferencesCollationInAnyDb(reindexTargetObject(target)) {
			return nil, nil
		}
		return nil, fmt.Errorf("unable to identify the object to be reindexed")
	}
	return out, nil
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

// schemaPrefixOf returns the schema prefix portion of a possibly-qualified
// object name ("" when unqualified).
func schemaPrefixOf(name string) string {
	if idx := strings.IndexByte(name, '.'); idx >= 0 {
		return name[:idx]
	}
	return ""
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

func (e *Engine) execAnalyze(s *sql.AnalyzeStmt) *Result {
	name := strings.TrimSpace(s.Name)
	// ANALYZE sqlite_master (or main.sqlite_master) — just ensures stats table exists.
	// In SQLite this loads stats into memory for the planner; we read from sqlite_stat1 directly.
	if isMasterStatName(name) {
		if err := e.stat1Writable(); err != nil {
			return &Result{Error: err}
		}
		// Ensure sqlite_stat1 table exists
		if err := e.ensureStatTable("sqlite_stat1", "tbl,idx,stat"); err != nil {
			return &Result{Error: err}
		}
		// sqlite_stat4 is also created by ANALYZE (SQLite creates both stat
		// tables together when statistics are collected).
		if err := e.ensureStatTable("sqlite_stat4", "tbl,idx,nEq,nLt,nDLt,sample"); err != nil {
			return &Result{Error: err}
		}
		return &Result{}
	}
	if name != "" {
		return e.execAnalyzeNamed(name)
	}
	// ANALYZE (no args): validate stat-table writability, then ensure stat
	// tables exist before analyzing all tables. SQLite's openStatTable is
	// called inside analyzeTable/analyzeDatabase after the table-existence
	// check passes; here the no-name form is a fast path that creates
	// sqlite_stat1/sqlite_stat4 once before the per-table walk.
	if err := e.stat1Writable(); err != nil {
		return &Result{Error: err}
	}
	if err := e.ensureStatTable("sqlite_stat1", "tbl,idx,stat"); err != nil {
		return &Result{Error: err}
	}
	if err := e.ensureStatTable("sqlite_stat4", "tbl,idx,nEq,nLt,nDLt,sample"); err != nil {
		return &Result{Error: err}
	}
	// Analyze all tables
	return e.analyzeAllTables()
}

// stat1Writable reports nil when sqlite_stat1 is absent (ANALYZE creates it)
// or is a real b-tree table; it reports "database disk image is malformed"
// when sqlite_stat1 exists as a virtual table (RootPage == 0), because
// ANALYZE cannot open a writable b-tree cursor on a virtual table — the
// SQLITE_CORRUPT path of analyze.c openStatTable/OP_Clear.
func (e *Engine) stat1Writable() error {
	entry, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return nil // absent: ensureStatTable creates a real table
	}
	if entry.RootPage == 0 {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}

// isMasterStatName reports whether an ANALYZE target names the schema table
// itself (ANALYZE sqlite_master / main.sqlite_master), which only ensures the
// stat tables exist.
func isMasterStatName(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "SQLITE_MASTER" || upper == "MAIN.SQLITE_MASTER" ||
		strings.HasSuffix(upper, ".SQLITE_MASTER")
}

// execAnalyzeNamed analyzes a named ANALYZE target: a schema (bare "main" /
// "temp"), a schema.table reference, a table, or an index.
func (e *Engine) execAnalyzeNamed(name string) *Result {
	// ANALYZE schema-name (bare "main" / "temp") analyzes all tables in
	// that schema; ANALYZE schema.table analyzes one table.
	upperName := strings.ToUpper(name)
	if upperName == "MAIN" || upperName == "TEMP" || upperName == "TEMPORARY" {
		if err := e.openStatTablesForAnalyze(); err != nil {
			return &Result{Error: err}
		}
		return e.analyzeAllTables()
	}
	// Handle schema.table prefix: validate the schema exists (SQLite
	// reports "unknown database %s" before looking up the table) and
	// strip it for the table lookup.
	tableName := name
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := tableName[:dotIdx]
		if e.getDB(prefix) == nil {
			return &Result{Error: fmt.Errorf("unknown database %s", prefix)}
		}
		tableName = tableName[dotIdx+1:]
	}
	// First try as a table name. The table-existence check must run BEFORE
	// openStatTablesForAnalyze so a non-existent target leaves no schema side
	// effects (analyze-1.4: SELECT count(*) FROM sqlite_master WHERE
	// name='sqlite_stat1' remains 0 after ANALYZE no_such_table).
	if _, tableErr := e.schema.FindTable(tableName); tableErr == nil {
		if err := e.openStatTablesForAnalyze(); err != nil {
			return &Result{Error: err}
		}
		return e.analyzeTable(tableName)
	}
	// Then try as an index name — ANALYZE index_name analyzes that index only.
	// Same ordering rule: index-existence first, stat tables second.
	idxEntry, idxErr := e.schema.FindIndex(name)
	if idxErr == nil {
		if err := e.openStatTablesForAnalyze(); err != nil {
			return &Result{Error: err}
		}
		return e.analyzeOneIndex(idxEntry)
	}
	return &Result{Error: fmt.Errorf("no such table: %s", tableName)}
}

// openStatTablesForAnalyze is the stat-table setup the analyzer must perform
// before walking tables or indexes (insertStatRow requires sqlite_stat1 to
// exist, and stat1Writable rejects shadowed virtual tables). On success both
// sqlite_stat1 and sqlite_stat4 are present in the schema.
func (e *Engine) openStatTablesForAnalyze() error {
	if err := e.stat1Writable(); err != nil {
		return err
	}
	if err := e.ensureStatTable("sqlite_stat1", "tbl,idx,stat"); err != nil {
		return err
	}
	return e.ensureStatTable("sqlite_stat4", "tbl,idx,nEq,nLt,nDLt,sample")
}

// InitStatTable ensures the sqlite_stat1 and sqlite_stat4 tables exist.
// It is called by ANALYZE (execAnalyze) after ensuring sqlite_stat1; stat4 is
// created on demand. The schema text matches SQLite's canonical definitions
// (no column types: CREATE TABLE sqlite_stat1(tbl,idx,stat)).
func (e *Engine) InitStatTable() error {
	if err := e.ensureStatTable("sqlite_stat1", "tbl,idx,stat"); err != nil {
		return err
	}
	return e.ensureStatTable("sqlite_stat4", "tbl,idx,nEq,nLt,nDLt,sample")
}
func (e *Engine) ensureStatTable(name, schemaSQL string) error {
	_, err := e.schema.FindTable(name)
	if err == nil {
		return nil // already exists
	}

	// Create table — allocate a new root page and add schema entry
	pg, perr := allocateRootPage(e.pager)
	if perr != nil {
		return err
	}
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafTable
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(int(e.pager.PageSize())-4))
	if err := e.pager.WritePage(pg); err != nil {
		return err
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     name,
		TblName:  name,
		RootPage: pg.PageNum,
		SQL:      fmt.Sprintf("CREATE TABLE %s(%s)", name, schemaSQL),
	}

	return e.schema.AddEntry(entry)
}

// EnsureStatTable ensures a sqlite_statN table exists on this connection's
// main database (used by the backup copy for ANALYZE statistics tables, whose
// CREATE TABLE DDL is reserved).
func (e *Engine) EnsureStatTable(name, schemaSQL string) error {
	return e.ensureStatTable(name, schemaSQL)
}

// EnsureStatTableIn ensures a sqlite_statN table exists in the named schema
// (main/temp/attached), using that schema's pager and schema manager. Used by
// the backup copy when the destination is not main.
func (e *Engine) EnsureStatTableIn(schemaName, name, schemaSQL string) error {
	ctx := e.GetDB(schemaName)
	if ctx == nil {
		return fmt.Errorf("unknown database %s", schemaName)
	}
	_, err := ctx.Schema.FindTable(name)
	if err == nil {
		return nil // already exists
	}
	pg, perr := allocateRootPage(ctx.Pager)
	if perr != nil {
		return err
	}
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafTable
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(int(ctx.Pager.PageSize())-4))
	if err := ctx.Pager.WritePage(pg); err != nil {
		return err
	}
	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     name,
		TblName:  name,
		RootPage: pg.PageNum,
		SQL:      fmt.Sprintf("CREATE TABLE %s(%s)", name, schemaSQL),
	}
	return ctx.Schema.AddEntry(entry)
}

// analyzeAllTables analyzes every user table and its indexes.
func (e *Engine) analyzeAllTables() *Result {
	entries, err := e.schema.GetEntries(schema.TypeTable)
	if err != nil {
		return &Result{Error: err}
	}

	// Clear entire stat table before re-populating
	e.clearAllStats()

	for _, entry := range entries {
		upper := strings.ToUpper(entry.Name)
		if upper == "SQLITE_SCHEMA" || upper == "SQLITE_MASTER" ||
			upper == "SQLITE_STAT1" || upper == "SQLITE_STAT4" ||
			upper == "SQLITE_SEQUENCE" {
			continue
		}
		if res := e.analyzeOneTable(entry); res.Error != nil {
			return res
		}
	}
	return &Result{}
}

// analyzeTable analyzes a specific table and its indexes.
func (e *Engine) analyzeTable(tableName string) *Result {
	entry, err := e.schema.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}
	// ANALYZE table-name must ensure sqlite_stat1/sqlite_stat4 exist before
	// inserting the per-index stat rows (insertStatRow requires the table to
	// exist). SQLite's openStatTable is invoked from analyzeTable after the
	// table-existence check; mirror that ordering here.
	if werr := e.stat1Writable(); werr != nil {
		return &Result{Error: werr}
	}
	if serr := e.ensureStatTable("sqlite_stat1", "tbl,idx,stat"); serr != nil {
		return &Result{Error: serr}
	}
	if serr := e.ensureStatTable("sqlite_stat4", "tbl,idx,nEq,nLt,nDLt,sample"); serr != nil {
		return &Result{Error: serr}
	}
	// Clear existing stats for this table then re-stats
	e.clearStatsForTable(tableName)
	return e.analyzeOneTable(entry)
}

// analyzeOneTable computes and stores statistics for one table and its indexes.
func (e *Engine) analyzeOneTable(entry *schema.Entry) *Result {
	// Build a unique key set of table names to match against index TblName.
	tableNames := map[string]bool{entry.Name: true, entry.TblName: true}

	// Enumerate all indexes
	allEntries, err := e.schema.GetEntries("")
	if err != nil {
		return &Result{Error: err}
	}

	// Count rows
	nRow := e.countTableRows(entry.RootPage)

	// SQLite skips analysis of empty tables: no stat1/stat4 rows are written
	// for a table (or its indexes) with zero rows.
	if nRow == 0 {
		return &Result{}
	}

	if res := e.analyzeWithoutRowidPK(entry, nRow); res.Error != nil {
		return res
	}

	nIdx, allPartial, res := e.analyzeTableIndexes(entry, tableNames, allEntries, nRow)
	if res.Error != nil {
		return res
	}

	// needTableCnt (analyze.c): the NULL-idx table-count row is emitted when
	// the table has NO indexes, or when EVERY index is partial — any
	// non-partial index clears the count (index6-1.10 "t1 {t1a {14 1} t1b
	// {10 1}} plus a NULL-idx row for the table scan"). WITHOUT ROWID tables
	// already emitted their table row above (the PRIMARY KEY row named after
	// the table).
	if nIdx > 0 && !allPartial {
		return &Result{}
	}
	if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return &Result{}
	}
	return e.insertTableScanStat(entry, nRow)
}

// analyzeWithoutRowidPK records a stat1/stat4 row for a WITHOUT ROWID table's
// PRIMARY KEY, which SQLite treats as an index named after the table (e.g.
// "t1 t1 {4 2 1}").
func (e *Engine) analyzeWithoutRowidPK(entry *schema.Entry, nRow int64) *Result {
	if !hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return &Result{}
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	pkCols := pkColumnNames(entry.SQL, colDefs)
	if len(pkCols) == 0 {
		return &Result{}
	}
	if stat := e.computePKStat(entry, pkCols, nRow); stat != "" {
		if res := e.insertStatRow(entry.Name, entry.Name, stat); res.Error != nil {
			return res
		}
		return e.insertStat4Row(entry.Name, entry.Name)
	}
	return &Result{}
}

// storageSlotsFor maps declared column indices to WITHOUT ROWID record slots
// (PK columns come first in the stored record). Rowid tables are the
// identity mapping.
func (e *Engine) storageSlotsFor(tableEntry *schema.Entry, colIdx []int) []int {
	if !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return colIdx
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	order := execdml.WithoutRowidStorageOrder(tableEntry.SQL, colDefs)
	out := make([]int, len(colIdx))
	for i, di := range colIdx {
		out[i] = -1
		for slot, d := range order {
			if d == di {
				out[i] = slot
				break
			}
		}
	}
	return out
}

// analyzeTableIndexes records stat1/stat4 rows for every index of a table.
// It returns the number of indexes analyzed and whether every index is
// partial (analyze.c's needTableCnt: any non-partial index suppresses the
// NULL-idx table-count row).
func (e *Engine) analyzeTableIndexes(entry *schema.Entry, tableNames map[string]bool, allEntries []*schema.Entry, nRow int64) (int, bool, *Result) {
	nIdx := 0
	allPartial := true
	for _, idx := range allEntries {
		if idx.Type != schema.TypeIndex {
			continue
		}
		if !tableNames[idx.TblName] {
			continue
		}
		nIdx++
		if partialIndexWhereSQL(idx.SQL) == "" {
			allPartial = false
		}

		statStr := e.computeIndexStat(entry, idx, nRow)
		if res := e.insertStatRow(entry.Name, idx.Name, statStr); res.Error != nil {
			return nIdx, allPartial, res
		}
		if res := e.insertStat4Row(entry.Name, idx.Name); res.Error != nil {
			return nIdx, allPartial, res
		}
	}
	return nIdx, allPartial, &Result{}
}

// insertTableScanStat records the single stat1/stat4 row a rowid table with no
// explicit indexes gets (idx NULL), describing the rowid scan.
func (e *Engine) insertTableScanStat(entry *schema.Entry, nRow int64) *Result {
	stat := fmt.Sprintf("%d", nRow)
	if res := e.insertStatRow(entry.Name, "", stat); res.Error != nil {
		return res
	}
	return e.insertStat4Row(entry.Name, "")
}

// insertStat4Row inserts a row into sqlite_stat4 (tbl, idx, nEq, nLt, nDLt,
// sample). The statistical samples are not computed; the row records that the
// index was analyzed so sqlite_stat4 introspection (DISTINCT tbl, idx) matches
// SQLite's output.
func (e *Engine) insertStat4Row(tbl, idx string) *Result {
	tableEntry, err := e.schema.FindTable("sqlite_stat4")
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs("sqlite_stat4", tableEntry.SQL)
	// A "" idx encodes SQLite's NULL idx (the stat4 row for a table with no
	// indexes); an actual index name is stored as a string.
	var idxVal interface{}
	if idx == "" {
		idxVal = nil
	} else {
		idxVal = idx
	}
	values := []interface{}{tbl, idxVal, nil, nil, nil, nil}
	return e.insertRow(e.mainDB.Pager, tableEntry, colDefs, values, nil, "")
}

// computePKStat computes the stat1 string for a WITHOUT ROWID table's PRIMARY
// KEY by scanning the table rows and counting distinct prefixes of the PK
// columns, in SQLite's format (nRow, ceil(nRow/distinct1), ...).
func (e *Engine) computePKStat(entry *schema.Entry, pkCols []string, nRow int64) string {
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	colIndex := buildColumnIndex(colDefs)
	var pkIdx []int
	for _, cn := range pkCols {
		if i, ok := colIndex[cn]; ok {
			pkIdx = append(pkIdx, i)
		}
	}
	if len(pkIdx) == 0 {
		return ""
	}
	distincts := e.countDistinctPrefixes(entry, e.storageSlotsFor(entry, pkIdx), func(v interface{}, j int) string {
		return fmt.Sprintf("%v", v)
	})
	return statAvgParts(nRow, distincts)
}

// countDistinctPrefixes scans a table's rows and returns, for each prefix
// position k, the number of distinct k+1-column value tuples. render converts
// a raw record value (and its position within the tuple) to its distinct-key
// form (collation-normalized for indexes, raw for the PRIMARY KEY). A nil
// return means the table scan failed (the caller then reports just the row
// count).
func (e *Engine) countDistinctPrefixes(tableEntry *schema.Entry, colIdx []int, render func(v interface{}, j int) string) []int {
	seen := make([]map[string]bool, len(colIdx))
	for i := range seen {
		seen[i] = make(map[string]bool)
	}
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		e.recordDistinctPrefixes(seen, rec, colIdx, render)
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	distincts := make([]int, len(seen))
	for k := range seen {
		distincts[k] = len(seen[k])
	}
	return distincts
}

// recordDistinctPrefixes records each prefix key of a record into the seen
// sets, stopping at the first indexed column missing from the record.
func (e *Engine) recordDistinctPrefixes(seen []map[string]bool, rec *storage.Record, colIdx []int, render func(v interface{}, j int) string) {
	for k := 0; k < len(colIdx); k++ {
		if colIdx[k] >= len(rec.Values) {
			return
		}
		seen[k][distinctPrefixKey(rec, colIdx, k, render)] = true
	}
}

// distinctPrefixKey builds the distinct-key string for prefix position k of a
// record's indexed columns.
func distinctPrefixKey(rec *storage.Record, colIdx []int, k int, render func(v interface{}, j int) string) string {
	var key strings.Builder
	for j := 0; j <= k; j++ {
		if j > 0 {
			key.WriteByte('|')
		}
		key.WriteString(render(rec.Values[colIdx[j]], j))
	}
	return key.String()
}

// statAvgParts renders SQLite's stat1 string: nRow followed by
// ceil(nRow/distinct_k) for each distinct prefix count.
func statAvgParts(nRow int64, distincts []int) string {
	parts := []string{fmt.Sprintf("%d", nRow)}
	for _, distinct := range distincts {
		avg := nRow
		if distinct > 0 {
			avg = (nRow + int64(distinct) - 1) / int64(distinct)
		}
		parts = append(parts, fmt.Sprintf("%d", avg))
	}
	return strings.Join(parts, " ")
}

// analyzeOneIndex analyzes a single index and stores its statistics.
// It finds the parent table, counts rows, computes the index stat, and inserts it.
func (e *Engine) analyzeOneIndex(idxEntry *schema.Entry) *Result {
	// Find the parent table
	tableEntry, err := e.schema.FindTable(idxEntry.TblName)
	if err != nil {
		return &Result{Error: fmt.Errorf("analyze: cannot find table for index %s: %w", idxEntry.Name, err)}
	}
	// Clear existing stat for this index
	e.clearStatsForIndex(idxEntry.TblName, idxEntry.Name)
	// Count rows and compute stat
	nRow := e.countTableRows(tableEntry.RootPage)
	statStr := e.computeIndexStat(tableEntry, idxEntry, nRow)
	return e.insertStatRow(tableEntry.Name, idxEntry.Name, statStr)
}

// countTableRows counts the number of rows in a table by traversing its b-tree.
func (e *Engine) countTableRows(rootPage uint32) int64 {
	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0
	}
	var count int64
	for {
		_, err := cursor.ReadCell()
		if err != nil {
			break
		}
		count++
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return count
}

// computeIndexStat computes the stat1 string for an index by scanning the
// parent TABLE and extracting the index column values (the engine does not
// maintain secondary index b-trees on INSERT, so the index tree may be empty).
// SQLite's format: nRow, ceil(nRow/distinct1), ceil(nRow/distinct2), ...
func (e *Engine) computeIndexStat(tableEntry *schema.Entry, idxEntry *schema.Entry, nRow int64) string {
	// Parse index columns from SQL
	colNames := parseIndexColumns(idxEntry.SQL)
	nCols := len(colNames)
	if nCols == 0 && idxEntry.SQL == "" && idxEntry.Type == "index" {
		// Autoindex entry (sqlite_autoindex_*): derive columns from the
		// table's UNIQUE / PRIMARY KEY constraints via SelectEngine.
		if autoCols := e.selectEngine.AutoindexColumnsForAnalyze(idxEntry.TblName, idxEntry.Name); len(autoCols) > 0 {
			colNames = autoCols
			nCols = len(colNames)
		}
	}
	if nCols == 0 {
		return fmt.Sprintf("%d", nRow)
	}

	// Map index column names to table column indices.
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	colIndex := buildColumnIndex(colDefs)
	var colIdx []int
	for _, cn := range colNames {
		if i, ok := colIndex[cn]; ok {
			colIdx = append(colIdx, i)
		}
	}
	if len(colIdx) == 0 {
		return fmt.Sprintf("%d", nRow)
	}

	// Effective collation per index column: an explicit COLLATE in the index
	// SQL wins; otherwise the table column's declared collation applies
	// (SQLite: "CREATE INDEX t2a ON t2(a)" inherits a's COLLATE nocase).
	explicitColls := parseIndexColumnCollations(idxEntry.SQL)
	colls := indexStatCollations(colIdx, colDefs, explicitColls)

	// A PARTIAL index's stat row counts only rows satisfying the partial
	// WHERE clause: analyze.c scans the INDEX b-tree, whose entries exist
	// only for indexed rows (index6-1.10: t1a on t1(a) WHERE a IS NOT NULL
	// over 20 rows reports "14 1", not "20 ...").
	if pred := partialIndexPredicate(idxEntry.SQL); pred != nil {
		nRowIdx, distincts := e.scanPartialIndexStats(tableEntry, colIdx, colls, pred)
		return statAvgParts(nRowIdx, distincts)
	}

	// Scan the table rows, counting distinct prefixes of the index columns
	// under each column's collation. WITHOUT ROWID records store PK columns
	// first, so declared column indices must map to storage slots first.
	distincts := e.countDistinctPrefixes(tableEntry, e.storageSlotsFor(tableEntry, colIdx), func(v interface{}, j int) string {
		return normalizeCollationKey(v, colls[j])
	})
	return statAvgParts(nRow, distincts)
}

// partialIndexWhereSQL extracts the partial-index predicate text from a
// CREATE INDEX statement ("... ON t(a) WHERE a IS NOT NULL"), or "" when the
// index is not partial.
func partialIndexWhereSQL(indexSQL string) string {
	m := indexWhereRe.FindStringSubmatch(indexSQL)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// partialIndexPredicate parses a partial index's WHERE clause into an
// expression, returning nil for a full index (or an unparseable predicate,
// which the membership machinery also treats as always-true).
func partialIndexPredicate(indexSQL string) sql.Expr {
	whereSQL := partialIndexWhereSQL(indexSQL)
	if whereSQL == "" {
		return nil
	}
	stmts, err := parse.ParseSQL("SELECT " + whereSQL)
	if err != nil || len(stmts) == 0 {
		return nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return nil
	}
	return sel.Columns[0].Expr
}

// scanPartialIndexStats scans the parent table once and returns the partial
// index's own row count plus distinct key-prefix counts, skipping rows that
// fail the partial predicate.
func (e *Engine) scanPartialIndexStats(tableEntry *schema.Entry, colIdx []int, colls []string, pred sql.Expr) (int64, []int) {
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	seen := make([]map[string]bool, len(colIdx))
	for i := range seen {
		seen[i] = make(map[string]bool)
	}
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, make([]int, len(colIdx))
	}
	order := e.wrRecordOrder(tableEntry, colDefs)
	nRow := e.collectPartialIndexKeys(cursor, colDefs, order, seen, colIdx, colls, pred)
	distincts := make([]int, len(seen))
	for k := range seen {
		distincts[k] = len(seen[k])
	}
	return nRow, distincts
}

// wrRecordOrder returns the slot-to-declared-column order for a WITHOUT ROWID
// table (PK columns come first in the stored record), or nil for a rowid
// table whose records are already in declared order.
func (e *Engine) wrRecordOrder(tableEntry *schema.Entry, colDefs []sql.ColumnDef) []int {
	if !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return nil
	}
	return execdml.WithoutRowidStorageOrder(tableEntry.SQL, colDefs)
}

// declaredRecord remaps a WITHOUT ROWID record's slot values to declared
// column order (order == nil means the record is already in declared order).
func declaredRecord(rec *storage.Record, order []int) *storage.Record {
	if order == nil {
		return rec
	}
	declared := make([]interface{}, len(order))
	for slot, di := range order {
		if slot < len(rec.Values) && di < len(declared) {
			declared[di] = rec.Values[slot]
		}
	}
	return &storage.Record{Values: declared}
}

// collectPartialIndexKeys walks the table once, counting rows and recording
// index-key prefixes for rows that satisfy the partial-index predicate.
func (e *Engine) collectPartialIndexKeys(cursor *btree.Cursor, colDefs []sql.ColumnDef, order []int, seen []map[string]bool, colIdx []int, colls []string, pred sql.Expr) int64 {
	var nRow int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return nRow
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			return nRow
		}
		rec = declaredRecord(rec, order)
		if e.recordSatisfiesPredicate(rec, colDefs, pred) {
			nRow++
			e.recordDistinctPrefixes(seen, rec, colIdx, func(v interface{}, j int) string {
				return normalizeCollationKey(v, colls[j])
			})
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return nRow
		}
	}
}

// recordSatisfiesPredicate evaluates pred against one stored record rendered
// as a row map. A row whose predicate evaluation errors stays indexed,
// matching the membership machinery's always-true fallback.
func (e *Engine) recordSatisfiesPredicate(rec *storage.Record, colDefs []sql.ColumnDef, pred sql.Expr) bool {
	rm := buildRowMapFromValues(rec.Values, colDefs, 0)
	v, err := e.EvalExpr(pred, rm)
	if err != nil {
		return true
	}
	if v == nil {
		return false
	}
	return execexpr.ToBool(v)
}

// indexStatCollations resolves the effective collation per index column: an
// explicit COLLATE in the index SQL wins; otherwise the table column's
// declared collation applies.
func indexStatCollations(colIdx []int, colDefs []sql.ColumnDef, explicitColls []string) []string {
	colls := make([]string, len(colIdx))
	for i, ci := range colIdx {
		colls[i] = "BINARY"
		if i < len(explicitColls) && explicitColls[i] != "" {
			colls[i] = strings.ToUpper(explicitColls[i])
		} else if ci < len(colDefs) && colDefs[ci].Collate != "" &&
			!strings.EqualFold(colDefs[ci].Collate, "BINARY") {
			colls[i] = strings.ToUpper(colDefs[ci].Collate)
		}
	}
	return colls
}

// normalizeCollationKey renders a value as its collation-normalized form for
// distinct-key counting: NOCASE lowercases, RTRIM strips trailing spaces, and
// BINARY keeps the raw rendering. Non-string values are rendered as-is (their
// equality is storage-class based, unaffected by text collations).
func normalizeCollationKey(v interface{}, collation string) string {
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	switch strings.ToUpper(collation) {
	case "NOCASE":
		// sqlite3UpperToLower folds ASCII A-Z down; the direction only
		// matters for ordering, but ASCII-only folding also keeps the
		// non-ASCII bytes distinct like SQLite's NOCASE.
		return value.SQLiteAsciiToLower(s)
	case "RTRIM":
		return strings.TrimRight(s, " ")
	default:
		return s
	}
}

// indexColumnCount returns the number of indexed columns for a given index name.
func (e *Engine) indexColumnCount(idxName string) int {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.Name == idxName {
			return len(parseIndexColumns(entry.SQL))
		}
	}
	return 0
}

// insertStatRow inserts a single row into sqlite_stat1.
func (e *Engine) insertStatRow(tbl, idx, stat string) *Result {
	tableEntry, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs("sqlite_stat1", tableEntry.SQL)
	// A "" idx encodes SQLite's NULL idx (the stat1 row for a table with no
	// indexes); an actual index name is stored as a string.
	var idxVal interface{}
	if idx == "" {
		idxVal = nil
	} else {
		idxVal = idx
	}
	values := []interface{}{tbl, idxVal, stat}
	return e.insertRow(e.mainDB.Pager, tableEntry, colDefs, values, nil, "")
}

// clearAllStats deletes all rows from sqlite_stat1 and sqlite_stat4.
func (e *Engine) clearAllStats() *Result {
	if _, err := e.schema.FindTable("sqlite_stat1"); err == nil {
		d := &sql.DeleteStmt{Table: "sqlite_stat1"}
		if res := e.dml.Delete(d); res.Error != nil {
			return res
		}
	}
	if _, err := e.schema.FindTable("sqlite_stat4"); err == nil {
		d := &sql.DeleteStmt{Table: "sqlite_stat4"}
		return e.dml.Delete(d)
	}
	return &Result{}
}

// clearStatsForTable deletes rows from sqlite_stat1 for a specific table.
// ClearStatsForTable is the package-public form of clearStatsForTable: DDL
// (DROP TABLE) calls it so sqlite_stat1 entries for the dropped table are
// removed in the same transaction (SQLite src/build.c sqlite3ClearStatTables).
// Silently no-ops when sqlite_stat1 does not yet exist.
func (e *Engine) ClearStatsForTable(tblName string) {
	e.clearStatsForTable(tblName)
}

// ClearStatsForIndex is the package-public form of clearStatsForIndex: DDL
// (DROP INDEX) calls it so the sqlite_stat1 entry for the dropped index is
// removed (SQLite src/build.c sqlite3ClearStatTables). Silently no-ops when
// sqlite_stat1 does not yet exist.
func (e *Engine) ClearStatsForIndex(tblName, idxName string) {
	e.clearStatsForIndex(tblName, idxName)
}

func (e *Engine) clearStatsForTable(tblName string) *Result {
	return e.deleteStatRows(func(row RowMap) bool {
		return stat1RowMatchesTbl(row, tblName)
	})
}
