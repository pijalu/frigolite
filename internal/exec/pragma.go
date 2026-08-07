// Package exec implements query execution.
package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- ANALYZE ---

// execReindex implements REINDEX. REINDEX rebuilds indexes; when the schema
// is corrupt (e.g. two index entries share a name after a writable_schema
// edit) SQLite reports "database disk image is malformed" before doing any
// work. Frigolite validates the schema and reports the same corruption error.
func (e *Engine) execReindex(s *sql.ReindexStmt) *Result {
	seen := make(map[string]string) // index name -> table
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if prev, ok := seen[strings.ToUpper(ent.Name)]; ok {
				if !strings.EqualFold(prev, ent.TblName) {
					return &Result{Error: fmt.Errorf("database disk image is malformed")}
				}
			}
			seen[strings.ToUpper(ent.Name)] = ent.TblName
		}
	}
	return &Result{}
}

func (e *Engine) execAnalyze(s *sql.AnalyzeStmt) *Result {
	// Ensure sqlite_stat1 table exists
	if err := e.ensureStatTable("sqlite_stat1", "tbl,idx,stat"); err != nil {
		return &Result{Error: err}
	}
	// sqlite_stat4 is also created by ANALYZE (SQLite creates both stat
	// tables together when statistics are collected).
	if err := e.ensureStatTable("sqlite_stat4", "tbl,idx,nEq,nLt,nDLt,sample"); err != nil {
		return &Result{Error: err}
	}

	// ANALYZE sqlite_master (or main.sqlite_master) — just ensures stats table exists.
	// In SQLite this loads stats into memory for the planner; we read from sqlite_stat1 directly.
	name := strings.TrimSpace(s.Name)
	upper := strings.ToUpper(name)
	if upper == "SQLITE_MASTER" || upper == "MAIN.SQLITE_MASTER" ||
		strings.HasSuffix(upper, ".SQLITE_MASTER") {
		return &Result{}
	}

	if name != "" {
		// ANALYZE schema-name (bare "main" / "temp") analyzes all tables in
		// that schema; ANALYZE schema.table analyzes one table.
		upperName := strings.ToUpper(name)
		if upperName == "MAIN" || upperName == "TEMP" || upperName == "TEMPORARY" {
			return e.analyzeAllTables()
		}
		// Handle schema.table prefix
		tableName := name
		if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
			prefix := strings.ToUpper(tableName[:dotIdx])
			if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
				tableName = tableName[dotIdx+1:]
			}
		}
		// First try as a table name
		if _, tableErr := e.schema.FindTable(tableName); tableErr == nil {
			return e.analyzeTable(tableName)
		}
		// Then try as an index name — ANALYZE index_name analyzes that index only
		idxEntry, idxErr := e.schema.FindIndex(name)
		if idxErr == nil {
			return e.analyzeOneIndex(idxEntry)
		}
		return &Result{Error: fmt.Errorf("no such table or index: %s", name)}
	}

	// Analyze all tables
	return e.analyzeAllTables()
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
	pg := e.pager.AllocatePage()
	pg.Data[0] = storage.PageTypeLeafTable
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

	// WITHOUT ROWID tables store their PRIMARY KEY as the row key; SQLite
	// ANALYZE records a stat1 row for the PK as if it were an index named
	// after the table (e.g. "t1 t1 {4 2 1}").
	if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
		pkCols := pkColumnNames(entry.SQL, colDefs)
		if len(pkCols) > 0 {
			if stat := e.computePKStat(entry, pkCols, nRow); stat != "" {
				if res := e.insertStatRow(entry.Name, entry.Name, stat); res.Error != nil {
					return res
				}
				if res := e.insertStat4Row(entry.Name, entry.Name); res.Error != nil {
					return res
				}
			}
		}
	}

	for _, idx := range allEntries {
		if idx.Type != schema.TypeIndex {
			continue
		}
		if !tableNames[idx.TblName] {
			continue
		}

		statStr := e.computeIndexStat(entry, idx, nRow)
		if res := e.insertStatRow(entry.Name, idx.Name, statStr); res.Error != nil {
			return res
		}
		if res := e.insertStat4Row(entry.Name, idx.Name); res.Error != nil {
			return res
		}
	}

	return &Result{}
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
	values := []interface{}{tbl, idx, nil, nil, nil, nil}
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
	seen := make([]map[string]bool, len(pkIdx))
	for i := range seen {
		seen[i] = make(map[string]bool)
	}
	tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return fmt.Sprintf("%d", nRow)
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
		for k := 0; k < len(pkIdx); k++ {
			if pkIdx[k] >= len(rec.Values) {
				break
			}
			var key strings.Builder
			for j := 0; j <= k; j++ {
				if j > 0 {
					key.WriteByte('|')
				}
				key.WriteString(fmt.Sprintf("%v", rec.Values[pkIdx[j]]))
			}
			seen[k][key.String()] = true
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%d", nRow))
	for k := range seen {
		distinct := len(seen[k])
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

	// Scan the table rows, counting distinct prefixes of the index columns.
	seen := make([]map[string]bool, len(colIdx))
	for i := range seen {
		seen[i] = make(map[string]bool)
	}
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return fmt.Sprintf("%d", nRow)
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
		for k := 0; k < len(colIdx); k++ {
			if colIdx[k] >= len(rec.Values) {
				break
			}
			var key strings.Builder
			for j := 0; j <= k; j++ {
				if j > 0 {
					key.WriteByte('|')
				}
				key.WriteString(fmt.Sprintf("%v", rec.Values[colIdx[j]]))
			}
			seen[k][key.String()] = true
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("%d", nRow))
	for k := range seen {
		distinct := len(seen[k])
		avg := nRow
		if distinct > 0 {
			avg = (nRow + int64(distinct) - 1) / int64(distinct)
		}
		parts = append(parts, fmt.Sprintf("%d", avg))
	}
	return strings.Join(parts, " ")
}

// formatPrefixKey creates a string key from a slice of values.
func formatPrefixKey(vals []interface{}) string {
	if len(vals) == 0 {
		return ""
	}
	var b strings.Builder
	for i, v := range vals {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(fmt.Sprintf("%v", v))
	}
	return b.String()
}

// parseIndexColumns extracts indexed column names from a CREATE INDEX SQL.
func parseIndexColumns(sqlStr string) []string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return nil
	}
	colsStr := sqlStr[start+1 : end]
	var cols []string
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
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
	values := []interface{}{tbl, idx, stat}
	return e.insertRow(e.mainDB.Pager, tableEntry, colDefs, values, nil, "")
}

// clearAllStats deletes all rows from sqlite_stat1 and sqlite_stat4.
func (e *Engine) clearAllStats() *Result {
	if _, err := e.schema.FindTable("sqlite_stat1"); err == nil {
		d := &sql.DeleteStmt{Table: "sqlite_stat1"}
		if res := e.execDelete(d); res.Error != nil {
			return res
		}
	}
	if _, err := e.schema.FindTable("sqlite_stat4"); err == nil {
		d := &sql.DeleteStmt{Table: "sqlite_stat4"}
		return e.execDelete(d)
	}
	return &Result{}
}

// clearStatsForTable deletes rows from sqlite_stat1 for a specific table.
func (e *Engine) clearStatsForTable(tblName string) *Result {
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
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if v, ok := row["tbl"]; ok {
			if s, ok := v.(string); ok && s == tblName {
				return true
			}
		}
		return false
	})
	if err != nil {
		return &Result{Error: err}
	}
	return &Result{Changes: deleted}
}

// clearStatsForIndex deletes rows from sqlite_stat1 for a specific index.
func (e *Engine) clearStatsForIndex(tblName, idxName string) *Result {
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
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if v, ok := row["tbl"]; ok {
			if s, ok := v.(string); ok && s == tblName {
				if v2, ok := row["idx"]; ok {
					if s2, ok := v2.(string); ok && s2 == idxName {
						return true
					}
				}
			}
		}
		return false
	})
	if err != nil {
		return &Result{Error: err}
	}
	return &Result{Changes: deleted}
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
		tblVal, ok := row["tbl"]
		if !ok {
			continue
		}
		idxVal, ok := row["idx"]
		if !ok {
			continue
		}
		tblStr, _ := tblVal.(string)
		idxStr, _ := idxVal.(string)
		if tblStr == tbl && idxStr == idx {
			statVal, ok := row["stat"]
			if !ok {
				return ""
			}
			statStr, _ := statVal.(string)
			return statStr
		}
		ok, err = cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return ""
}

// --- PRAGMA ---

func (e *Engine) execPragma(s *sql.PragmaStmt) *Result {
	name := strings.ToUpper(s.Name)

	// PRAGMA foreign_key_check / foreign_key_check(table): report every
	// foreign key violation as rows (child_table, rowid, parent_table, fkid).
	// Runs regardless of the foreign_keys setting. A schema qualifier
	// (PRAGMA main.foreign_key_check) restricts the scan to that schema.
	if name == "FOREIGN_KEY_CHECK" {
		viols, err := e.findFKViolations(s.Value, s.Schema)
		if err != nil {
			return &Result{Error: err}
		}
		rows := make([][]interface{}, 0, len(viols))
		for _, v := range viols {
			rows = append(rows, []interface{}{v.childTable, v.rowID, v.parentTable, int64(v.fkID)})
		}
		return &Result{Columns: []string{"table", "rowid", "parent", "fkid"}, Rows: rows}
	}

	// PRAGMA foreign_key_list(table): report each FK of the table as rows
	// (id, seq, table, from, to, on_update, on_delete, match). id is the FK
	// index, seq the column position within the FK.
	if name == "FOREIGN_KEY_LIST" {
		entry, _, err := e.findTable(s.Value)
		if err != nil {
			return &Result{Error: err}
		}
		colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
		fks := e.tableFKConstraints(entry, colDefs)
		var rows [][]interface{}
		for id, fk := range fks {
			// For an implicit "REFERENCES t" the parent column is not named;
			// SQLite reports NULL in the "to" column.
			parentCols := fk.parentCols
			for seq, childCol := range fk.childCols {
				to := ""
				if seq < len(parentCols) {
					to = parentCols[seq]
				}
				upd := fk.onUpdate
				if upd == "" {
					upd = "NO ACTION"
				}
				del := fk.onDelete
				if del == "" {
					del = "NO ACTION"
				}
				rows = append(rows, []interface{}{int64(id), int64(seq), fk.parentRef, childCol, to, upd, del, "NONE"})
			}
		}
		return &Result{Columns: []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}, Rows: rows}
	}

	// PRAGMA index_info(name) / index_xinfo(name) take an argument: an index
	// name or (for WITHOUT ROWID tables) the table name referring to its
	// implicit PRIMARY KEY index. Handled before the value-return shortcut
	// below (which would otherwise swallow any pragma with a value).
	if name == "INDEX_INFO" || name == "INDEX_XINFO" {
		return e.execPragmaIndexInfo(s.Value, name == "INDEX_XINFO")
	}
	if name == "INDEX_LIST" {
		return e.execPragmaIndexList(s.Value)
	}

	// PRAGMA table_info(tbl) / table_xinfo(tbl) materialize the table's
	// column metadata as rows (cid, name, type, notnull, dflt_value, pk).
	if name == "TABLE_INFO" || name == "TABLE_XINFO" {
		cols, rows, err := e.materializeTableInfo(sql.TableRef{Name: "pragma_" + strings.ToLower(name), Args: []sql.Expr{&sql.StringLit{Value: s.Value}}})
		if err != nil {
			return &Result{Error: err}
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return &Result{Columns: names, Rows: rows}
	}

	// Handle PRAGMA ... = value for known pragmas
	if s.Value != "" && name != "QUICK_CHECK" && name != "INTEGRITY_CHECK" {
		switch name {
		case "LEGACY_ALTER_TABLE":
			e.legacyAlterTable = s.Value == "1"
		case "RECURSIVE_TRIGGERS":
			e.recursiveTriggers = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
		case "IGNORE_CHECK_CONSTRAINTS":
			e.ignoreCheckConstraints = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
		case "FOREIGN_KEYS":
			e.foreignKeys = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
		case "DEFER_FOREIGN_KEYS":
			// SQLite refuses to change defer_foreign_keys from ON to ON (or
			// set it at all) outside a transaction once it is already ON
			// ("defer_foreign_keys only supported inside a transaction").
			// The first 0→1 transition outside a transaction is allowed.
			if e.deferForeignKeys && !e.inTransaction {
				return &Result{Error: fmt.Errorf("defer_foreign_keys only supported inside a transaction")}
			}
			e.deferForeignKeys = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
		case "WRITABLE_SCHEMA":
			e.writableSchema = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
		case "ENCODING":
			// Accept UTF-8, UTF-16, UTF-16le, UTF-16be (case-insensitive)
			switch strings.ToUpper(s.Value) {
			case "UTF-8", "UTF8":
				e.encoding = "UTF-8"
			case "UTF-16LE", "UTF16LE":
				e.encoding = "UTF-16le"
			case "UTF-16BE", "UTF16BE", "UTF-16", "UTF16":
				e.encoding = "UTF-16be"
			default:
				return &Result{Error: fmt.Errorf("unsupported encoding: %s", s.Value)}
			}
		case "JOURNAL_MODE":
			// SQLite returns the resulting journal mode after assignment
			// (e.g. "PRAGMA journal_mode=off" yields "off"). Frigolite uses
			// rollback journal only; report the requested mode normalized.
			mode := strings.ToLower(s.Value)
			switch mode {
			case "delete", "truncate", "persist", "memory", "off", "wal", "wal2":
				// accepted modes (WAL modes accepted but not implemented:
				// report the requested mode as SQLite does)
			default:
				return &Result{Error: fmt.Errorf("unsupported journal mode: %s", s.Value)}
			}
			return &Result{Rows: [][]interface{}{{mode}}}
		case "RECURSIVE_CTE_LIMIT":
			if n, err := strconv.Atoi(s.Value); err == nil && n >= 0 {
				e.recursiveCTELimit = n
			}
			return &Result{Rows: [][]interface{}{{int64(e.recursiveCTELimit)}}}
		case "REVERSE_UNORDERED_SELECTS":
			e.reverseUnordered = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
			// SQLite: PRAGMA reverse_unordered_selects=1 (set form) returns
			// no row; only the bare PRAGMA (getter) returns the value.
		case "COUNT_CHANGES":
			e.countChanges = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
		case "MMAP_SIZE":
			// SQLite returns the effective mmap size after assignment. In a
			// build with SQLITE_MAX_MMAP_SIZE=0 (mmap compiled out) the
			// result is always 0; Frigolite has no mmap support, so report
			// 0 (matching the SQLite test-suite expectation).
			return &Result{Rows: [][]interface{}{{int64(0)}}}
		}
		// When setting a PRAGMA value, don't also return the value
		return &Result{}
	}

	// Handle quick_check / integrity_check
	if name == "QUICK_CHECK" || name == "INTEGRITY_CHECK" {
		return e.execQuickCheck(s.Value)
	}

	if fn, ok := pragmaHandlers[name]; ok {
		return fn(e)
	}
	return &Result{}
}

// autoindexDef describes one implicit autoindex of a WITHOUT ROWID table.
type autoindexDef struct {
	name   string
	cols   []string
	origin string // "pk" or "u"
}

// execPragmaIndexList implements PRAGMA index_list(table). Columns:
// (seq, name, unique, origin, partial). Explicit indexes are listed first in
// creation order; implicit PRIMARY KEY/UNIQUE autoindexes of a WITHOUT ROWID
// table follow in reverse order, matching SQLite.
func (e *Engine) execPragmaIndexList(arg string) *Result {
	cols := []string{"seq", "name", "unique", "origin", "partial"}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return &Result{Columns: cols}
	}
	tableEntry, ctx, err := e.findTable(arg)
	if err != nil {
		return &Result{Columns: cols} // unknown table → zero rows
	}
	var rows [][]interface{}
	seq := 0

	// Explicit indexes on the table.
	indexes, _ := ctx.Schema.FindIndexesForTable(tableEntry.Name)
	for _, idx := range indexes {
		unique := int64(0)
		if uniqueIndexColsRe.MatchString(idx.SQL) {
			unique = 1
		}
		partial := int64(0)
		if indexWhereRe.MatchString(idx.SQL) {
			partial = 1
		}
		rows = append(rows, []interface{}{int64(seq), idx.Name, unique, "c", partial})
		seq++
	}

	// Implicit autoindexes for WITHOUT ROWID tables, in reverse creation order.
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		defs := e.withoutRowidAutoindexes(tableEntry.Name, tableEntry)
		for i := len(defs) - 1; i >= 0; i-- {
			rows = append(rows, []interface{}{int64(seq), defs[i].name, int64(1), defs[i].origin, int64(0)})
			seq++
		}
	}
	return &Result{Columns: cols, Rows: rows}
}

// withoutRowidAutoindexes computes the implicit UNIQUE/PRIMARY KEY autoindexes
// of a WITHOUT ROWID table and names them sqlite_autoindex_<table>_<N>.
// Autoindexes are numbered sequentially in creation order: column-level UNIQUE
// and PRIMARY KEY constraints first (in column order), then table-level UNIQUE
// and PRIMARY KEY constraints (in declaration order). An index whose column
// set already exists is merged into that existing index; a PRIMARY KEY wins
// the "pk" origin.
func (e *Engine) withoutRowidAutoindexes(tableName string, tableEntry *schema.Entry) []autoindexDef {
	colDefs := e.parseColumnDefs(tableName, tableEntry.SQL)
	colIndex := buildColumnIndex(colDefs)
	var defs []autoindexDef
	addIndex := func(cols []string, origin string) {
		for i := range defs {
			if sameColumnSet(defs[i].cols, cols) {
				if origin == "pk" {
					defs[i].origin = "pk"
				}
				return
			}
		}
		defs = append(defs, autoindexDef{cols: cols, origin: origin})
	}
	// Column-level constraints, in column order.
	for _, cd := range colDefs {
		if cd.Unique {
			addIndex([]string{cd.Name}, "u")
		}
		if cd.PrimaryKey {
			addIndex([]string{cd.Name}, "pk")
		}
	}
	// Table-level constraints, in declaration order.
	for _, tc := range e.tableConstraints(tableName, tableEntry.SQL) {
		var constraintCols []string
		switch tc.Type {
		case sql.ConstraintUnique:
			constraintCols = constraintColumnNames(tc, colIndex, colDefs)
			addIndex(constraintCols, "u")
		case sql.ConstraintPrimaryKey:
			constraintCols = constraintColumnNames(tc, colIndex, colDefs)
			addIndex(constraintCols, "pk")
		}
	}
	for i := range defs {
		defs[i].name = fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, i+1)
	}
	return defs
}

// sameColumnSet reports whether two lists name the same columns in the same
// order (case-insensitively).
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// constraintColumnNames resolves a table-level UNIQUE/PRIMARY KEY constraint's
// indexed columns to their names, honoring integer column positions.
func constraintColumnNames(tc sql.TableConstraint, colIndex map[string]int, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, ic := range tc.Columns {
		if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
			names = append(names, colDefs[n-1].Name)
			continue
		}
		if idx, ok := colIndex[ic.Name]; ok {
			names = append(names, colDefs[idx].Name)
		} else {
			names = append(names, ic.Name)
		}
	}
	return names
}

// indexPragmaColumn describes one column of an index for index_info/xinfo.
type indexPragmaColumn struct {
	name  string
	desc  bool
	coll  string // resolved collation ("" for BINARY)
	cid   int64  // table column ordinal (-1 for rowid)
	key   int64  // 1 for key columns, 0 for payload columns
	rowid bool   // synthetic rowid column
}

// execPragmaIndexInfo implements PRAGMA index_info(name) and
// PRAGMA index_xinfo(name). The argument may name an explicit index or, for a
// WITHOUT ROWID table, the table itself (its implicit PRIMARY KEY index).
// Mirrors SQLite's output:
//
//	index_info:  (seqno, cid, name)
//	index_xinfo: (seqno, cid, name, desc, coll, key)
func (e *Engine) execPragmaIndexInfo(arg string, xinfo bool) *Result {
	cols := []string{"seqno", "cid", "name"}
	if xinfo {
		cols = []string{"seqno", "cid", "name", "desc", "coll", "key"}
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return &Result{Columns: cols}
	}

	var columns []indexPragmaColumn

	// 1. Named index.
	if idxEntry, ctx, err := e.findIndex(arg); err == nil {
		var tableEntry *schema.Entry
		var colDefs []sql.ColumnDef
		if te, _, terr := e.findTable(idxEntry.TblName); terr == nil {
			tableEntry = te
			colDefs = e.parseColumnDefs(te.Name, te.SQL)
		}
		columns = e.indexColumnsFromSQL(idxEntry.SQL, ctx, tableEntry, colDefs)
	} else if tableEntry, _, terr := e.findTable(arg); terr == nil && hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		// 2. WITHOUT ROWID table name: implicit PRIMARY KEY index.
		colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
		columns = e.withoutRowidPKColumns(arg, tableEntry, colDefs, xinfo)
	} else {
		// Unknown index/table: SQLite returns zero rows.
		return &Result{Columns: cols}
	}

	rows := make([][]interface{}, 0, len(columns))
	for i, c := range columns {
		if !xinfo && c.rowid {
			continue // index_info omits the trailing rowid column
		}
		if xinfo {
			var name interface{}
			if c.name != "" {
				name = c.name
			}
			coll := c.coll
			if coll == "" {
				coll = "BINARY"
			}
			rows = append(rows, []interface{}{int64(i), c.cid, name, int64(boolToInt(c.desc)), coll, c.key})
		} else {
			rows = append(rows, []interface{}{int64(i), c.cid, c.name})
		}
	}
	return &Result{Columns: cols, Rows: rows}
}

// indexColumnsFromSQL resolves an explicit index's columns from its CREATE
// INDEX SQL: names and DESC flags from the AST, collations from the table
// column definitions (or an explicit COLLATE in the index column list).
func (e *Engine) indexColumnsFromSQL(sqlStr string, ctx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef) []indexPragmaColumn {
	stmts, perr := parse.ParseSQL(sqlStr)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ci, ok := stmts[0].(*sql.CreateIndexStmt)
	if !ok {
		return nil
	}
	explicitColls := parseIndexColumnCollations(sqlStr)
	colIndex := buildColumnIndex(colDefs)
	var out []indexPragmaColumn
	for i, ic := range ci.Columns {
		cid := int64(-1)
		coll := ""
		if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
			cid = int64(n - 1)
			coll = colDefs[n-1].Collate
		} else if idx, ok := colIndex[ic.Name]; ok {
			cid = int64(idx)
			coll = colDefs[idx].Collate
		}
		if i < len(explicitColls) && explicitColls[i] != "" {
			coll = explicitColls[i]
		}
		out = append(out, indexPragmaColumn{name: ic.Name, desc: ic.Desc, coll: coll, cid: cid, key: 1})
	}
	if tableEntry != nil && !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		// Rowid tables store a trailing rowid in every index record.
		out = append(out, indexPragmaColumn{cid: -1, key: 0, rowid: true})
	}
	return out
}

// withoutRowidPKColumns builds the implicit PRIMARY KEY index columns of a
// WITHOUT ROWID table. Key columns come from the PRIMARY KEY constraint; the
// remaining table columns appear as payload (key=0) columns in index_xinfo.
func (e *Engine) withoutRowidPKColumns(tableName string, tableEntry *schema.Entry, colDefs []sql.ColumnDef, xinfo bool) []indexPragmaColumn {
	colIndex := buildColumnIndex(colDefs)
	inPK := make(map[int]bool)
	var out []indexPragmaColumn

	// Column-level PRIMARY KEY constraints (e.g. "b PRIMARY KEY") are treated
	// by SQLite as a PRIMARY KEY constraint on that single column.
	for i, cd := range colDefs {
		if !cd.PrimaryKey || inPK[i] {
			continue
		}
		inPK[i] = true
		out = append(out, indexPragmaColumn{name: cd.Name, cid: int64(i), coll: cd.Collate, key: 1})
	}

	for _, tc := range e.tableConstraints(tableName, tableEntry.SQL) {
		if tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		for _, ic := range tc.Columns {
			idx := -1
			if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
				idx = n - 1
			} else if i, ok := colIndex[ic.Name]; ok {
				idx = i
			}
			if idx < 0 {
				continue
			}
			inPK[idx] = true
			coll := ic.Collate
			if coll == "" {
				coll = colDefs[idx].Collate
			}
			out = append(out, indexPragmaColumn{name: colDefs[idx].Name, desc: ic.Desc, coll: coll, cid: int64(idx), key: 1})
		}
	}
	if xinfo {
		for i, cd := range colDefs {
			if inPK[i] {
				continue
			}
			out = append(out, indexPragmaColumn{name: cd.Name, cid: int64(i), coll: cd.Collate, key: 0})
		}
	}
	return out
}

// parseIndexColumnCollations extracts per-column COLLATE names from a CREATE
// INDEX column list ("CREATE INDEX i ON t(a, b COLLATE rtrim)" -> ["", "rtrim"]).
func parseIndexColumnCollations(sqlStr string) []string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return nil
	}
	colsStr := sqlStr[start+1 : end]
	var colls []string
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		cu := strings.ToUpper(col)
		if idx := strings.Index(cu, " COLLATE "); idx >= 0 {
			rest := strings.TrimSpace(col[idx+len(" COLLATE "):])
			// Strip trailing ASC/DESC.
			ru := strings.ToUpper(rest)
			if di := strings.Index(ru, " DESC"); di >= 0 {
				rest = strings.TrimSpace(rest[:di])
			} else if ai := strings.Index(ru, " ASC"); ai >= 0 {
				rest = strings.TrimSpace(rest[:ai])
			}
			colls = append(colls, rest)
		} else {
			colls = append(colls, "")
		}
	}
	return colls
}

var pragmaHandlers = map[string]func(e *Engine) *Result{
	"TABLE_INFO": func(e *Engine) *Result {
		return &Result{Columns: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}}
	},
	"INDEX_INFO": func(e *Engine) *Result { return &Result{Columns: []string{"seqno", "cid", "name"}} },
	"INDEX_LIST": func(e *Engine) *Result { return &Result{Columns: []string{"seq", "name", "unique"}} },
	"DATABASE_VERSION": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"PAGE_SIZE":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(e.pager.PageSize())}}} },
	"PAGE_COUNT":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"FREELIST_COUNT":   func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"SCHEMA_VERSION":   func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"USER_VERSION":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"APPLICATION_ID":   func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"AUTO_VACUUM":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"REVERSE_UNORDERED_SELECTS": func(e *Engine) *Result {
		return &Result{Rows: [][]interface{}{{boolToInt(e.reverseUnordered)}}}
	},
	"JOURNAL_MODE":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"memory"}}} },
	"SYNCHRONOUS":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"CACHE_SIZE":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(2000)}}} },
	"TEMP_STORE":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"LOCKING_MODE":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"normal"}}} },
	"DATABASE_LIST": func(e *Engine) *Result {
		var rows [][]interface{}
		seq := int64(0)
		// Main database first (seq 0), then attached databases
		rows = append(rows, []interface{}{seq, "main", e.mainDB.FilePath})
		seq++
		for _, ctx := range e.databases {
			upper := strings.ToUpper(ctx.Name)
			if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
				continue
			}
			rows = append(rows, []interface{}{seq, ctx.Name, ctx.FilePath})
			seq++
		}
		return &Result{Columns: []string{"seq", "name", "file"}, Rows: rows}
	},
	"INTEGRITY_CHECK": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"ok"}}} },
	"LEGACY_ALTER_TABLE": func(e *Engine) *Result {
		val := int64(0)
		if e.legacyAlterTable {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"TABLE_X": func(e *Engine) *Result {
		return &Result{Columns: []string{"oid", "colX"}, Rows: [][]interface{}{{int64(0), ""}}}
	},
	"COUNT_CHANGES":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{boolToInt(e.countChanges)}}} },
	"CASE_SENSITIVE_LIKE": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"RECURSIVE_TRIGGERS": func(e *Engine) *Result {
		val := int64(0)
		if e.recursiveTriggers {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"FOREIGN_KEYS": func(e *Engine) *Result {
		val := int64(0)
		if e.foreignKeys {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"DEFER_FOREIGN_KEYS": func(e *Engine) *Result {
		val := int64(0)
		if e.deferForeignKeys {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"READ_UNCOMMITTED": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"ENCODING":         func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{e.encoding}}} },
	"SCHEMA_TABLE": func(e *Engine) *Result {
		return &Result{Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"}}
	},
	"SOFT_HEAP_LIMIT": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"THREADS":         func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"COMPILE_OPTIONS": func(e *Engine) *Result {
		return &Result{Columns: []string{"compile_options"}, Rows: [][]interface{}{{"THREADSAFE=1"}}}
	},
}

// execQuickCheck implements PRAGMA quick_check('table_name') and
// PRAGMA integrity_check('table_name'). For STRICT tables, it scans all rows
// and validates that each value's type matches the column's declared type.
// Returns "ok" if no violations, or a description of the first violation.
func (e *Engine) execQuickCheck(tableName string) *Result {
	if tableName == "" {
		// No table name: check all tables. Besides STRICT-type and NULL
		// checks (per-table below), SQLite's integrity_check also verifies
		// every CHECK constraint against the stored rows and reports the
		// first table with a violation as "CHECK constraint failed in T"
		// (this is how rows written under PRAGMA
		// ignore_check_constraints=ON are caught).
		if bad := e.firstCheckViolationTable(); bad != "" {
			return &Result{Columns: []string{"integrity_check"}, Rows: [][]interface{}{{fmt.Sprintf("CHECK constraint failed in %s", bad)}}}
		}
		return &Result{Columns: []string{"integrity_check"}, Rows: [][]interface{}{{"ok"}}}
	}

	// Strip quotes from table name
	tableName = strings.Trim(tableName, "'\"")

	te, dbCtx, err := e.findTable(tableName)
	if err != nil {
		return &Result{Columns: []string{"integrity_check"}, Rows: [][]interface{}{{"ok"}}}
	}

	// Only STRICT tables need checking
	if !hasStrictKeyword(strings.ToUpper(te.SQL)) {
		return &Result{Columns: []string{"integrity_check"}, Rows: [][]interface{}{{"ok"}}}
	}

	colDefs := e.parseColumnDefs(te.Name, te.SQL)

	// Scan all rows and check STRICT types
	tree := e.tableBTreePg(dbCtx.Pager, te.Name, te.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Columns: []string{"integrity_check"}, Rows: [][]interface{}{{"ok"}}}
	}

	for {
		payload, _, err := cursor.ReadCellData()
		if err != nil {
			break
		}
		// Decode the record
		rec, err := storage.DecodeRecord(payload)
		if err != nil {
			break
		}
		// Check each column value against STRICT type
		for i, val := range rec.Values {
			if i >= len(colDefs) {
				break
			}
			cd := colDefs[i]
			if cd.Generated != nil {
				continue
			}
			if val == nil {
				// Check NOT NULL. PRIMARY KEY columns are implicitly NOT NULL
				// in STRICT tables (matches sqlite3AddPrimaryKey setting
				// pCol->notNull; quick_check reports "NULL value in t.c").
				if cd.NotNull || cd.PrimaryKey {
					return &Result{
						Columns: []string{"integrity_check"},
						Rows:    [][]interface{}{{fmt.Sprintf("NULL value in %s.%s", te.Name, cd.Name)}},
					}
				}
				continue
			}
			if err := checkStrictValueForQuickCheck(te.Name, cd.Name, cd.Type, val); err != nil {
				return &Result{
					Columns: []string{"integrity_check"},
					Rows:    [][]interface{}{{err.Error()}},
				}
			}
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	return &Result{Columns: []string{"integrity_check"}, Rows: [][]interface{}{{"ok"}}}
}

// firstCheckViolationTable scans every user table and returns the name of the
// first one whose CHECK constraints are violated by a stored row, or "" if all
// tables satisfy their CHECKs. Column-level and table-level CHECK constraints
// are evaluated against each row's stored values (SQLite integrity_check
// reports the FIRST violating table only).
func (e *Engine) firstCheckViolationTable() string {
	for _, dbCtx := range e.databases {
		entries, err := dbCtx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, te := range entries {
			if isSchemaTable(te.Name) {
				continue
			}
			colDefs := e.parseColumnDefs(te.Name, te.SQL)
			tcs := e.tableConstraints(te.Name, te.SQL)
			hasCheck := false
			for _, cd := range colDefs {
				if cd.Check != nil {
					hasCheck = true
					break
				}
			}
			if !hasCheck {
				for _, tc := range tcs {
					if tc.Type == sql.ConstraintCheck {
						hasCheck = true
						break
					}
				}
			}
			if !hasCheck {
				continue
			}
			tree := e.tableBTreePg(dbCtx.Pager, te.Name, te.RootPage, true)
			cursor, err := tree.OpenCursor()
			if err != nil {
				continue
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
				row := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
				for _, cd := range colDefs {
					if cd.Check == nil {
						continue
					}
					checkVal, err := e.evalExpr(cd.Check, row)
					if err == nil && checkVal != nil && !toBool(checkVal) {
						return te.Name
					}
				}
				for _, tc := range tcs {
					if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
						continue
					}
					checkVal, err := e.evalExpr(tc.Expr, row)
					if err == nil && checkVal != nil && !toBool(checkVal) {
						return te.Name
					}
				}
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
			}
		}
	}
	return ""
}

// checkStrictValueForQuickCheck validates a value against a STRICT column type
// using SQLite's quick_check error format: "non-DECLARED value in table.column".
// The "non-X" is the DECLARED type name (sqlite3StdType[eCType-1]), not the
// actual value type. Allowed actual types follow pragma.c's aStdTypeMask:
//
//	ANY:     any type
//	BLOB:    BLOB only
//	INT:     INT only
//	INTEGER: INT only
//	REAL:    INT or REAL
//	TEXT:    TEXT only
//
// Unlike enforceStrictType, this does NOT apply affinity — it checks the raw
// stored value type (used for detecting corruption).
func checkStrictValueForQuickCheck(tableName, colName, declaredType string, v interface{}) error {
	if v == nil {
		return nil
	}
	upper := strings.ToUpper(strings.TrimSpace(declaredType))
	// The declared type in the error message uses the canonical STRICT name
	// (e.g. "INT", "INTEGER", "REAL", "TEXT", "BLOB"). sqlite3StdType is
	// {"ANY","BLOB","INT","INTEGER","REAL","TEXT"} indexed by eCType-1.
	declaredName := upper
	if declaredName != "INT" && declaredName != "INTEGER" &&
		declaredName != "REAL" && declaredName != "TEXT" &&
		declaredName != "BLOB" && declaredName != "ANY" {
		return nil
	}
	v = util.UnwrapColumnValue(v)
	var actualType string
	switch v.(type) {
	case int64:
		actualType = "INT"
	case float64:
		actualType = "REAL"
	case string:
		actualType = "TEXT"
	case []byte:
		actualType = "BLOB"
	default:
		return nil
	}
	switch upper {
	case "TEXT":
		if actualType != "TEXT" {
			return fmt.Errorf("non-%s value in %s.%s", declaredName, tableName, colName)
		}
	case "INT", "INTEGER":
		if actualType != "INT" {
			return fmt.Errorf("non-%s value in %s.%s", declaredName, tableName, colName)
		}
	case "REAL":
		if actualType != "REAL" && actualType != "INT" {
			return fmt.Errorf("non-%s value in %s.%s", declaredName, tableName, colName)
		}
	case "BLOB":
		if actualType != "BLOB" {
			return fmt.Errorf("non-%s value in %s.%s", declaredName, tableName, colName)
		}
	case "ANY":
		// any type is OK
	}
	return nil
}
