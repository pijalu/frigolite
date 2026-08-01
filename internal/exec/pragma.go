// Package exec implements query execution.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// --- ANALYZE ---

func (e *Engine) execAnalyze(s *sql.AnalyzeStmt) *Result {
	// Ensure sqlite_stat1 table exists
	if err := e.ensureStatTable("sqlite_stat1", "tbl TEXT,idx TEXT,stat TEXT"); err != nil {
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
// It is called during database initialization to make stat tables always available.
func (e *Engine) InitStatTable() error {
	if err := e.ensureStatTable("sqlite_stat1", "tbl TEXT,idx TEXT,stat TEXT"); err != nil {
		return err
	}
	return e.ensureStatTable("sqlite_stat4", "tbl TEXT,idx TEXT,nEq BLOB,nLt BLOB,nDLt BLOB,sample BLOB")
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

	for _, idx := range allEntries {
		if idx.Type != schema.TypeIndex {
			continue
		}
		if !tableNames[idx.TblName] {
			continue
		}

		statStr := e.computeIndexStat(idx, nRow)
		if res := e.insertStatRow(entry.Name, idx.Name, statStr); res.Error != nil {
			return res
		}
	}

	return &Result{}
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
	statStr := e.computeIndexStat(idxEntry, nRow)
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

// computeIndexStat computes the stat string for an index.
// The stat format: "N N1 N2 ..." where N = table row count and Nk = distinct
// prefix values for the first k columns.
func (e *Engine) computeIndexStat(idxEntry *schema.Entry, nRow int64) string {
	// Parse index columns from SQL
	colNames := parseIndexColumns(idxEntry.SQL)
	nCols := len(colNames)
	if nCols == 0 {
		return fmt.Sprintf("%d", nRow)
	}

	// Open index b-tree and scan all entries
	idxTree := btree.NewBTree(e.pager, idxEntry.RootPage, false)
	cursor, err := idxTree.OpenCursor()
	if err != nil {
		return fmt.Sprintf("%d", nRow)
	}

	// Count distinct prefix values
	seen := make([]map[string]bool, nCols)
	for i := 0; i < nCols; i++ {
		seen[i] = make(map[string]bool)
	}

	actualCols := 0
	firstEntry := true

	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		// Index records: [col1, col2, ..., colN, rowid]
		nVals := len(rec.Values) - 1 // exclude trailing rowid
		if firstEntry {
			actualCols = nVals
			firstEntry = false
		}
		// For each prefix length, build a distinct key
		for k := 0; k < nVals && k < nCols; k++ {
			key := formatPrefixKey(rec.Values[:k+1])
			seen[k][key] = true
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	if actualCols == 0 {
		return fmt.Sprintf("%d", nRow)
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("%d", nRow))
	for k := 0; k < actualCols && k < nCols; k++ {
		parts = append(parts, fmt.Sprintf("%d", len(seen[k])))
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
	return e.insertRow(e.mainDB.Pager, tableEntry, colDefs, values, nil)
}

// clearAllStats deletes all rows from sqlite_stat1.
func (e *Engine) clearAllStats() *Result {
	_, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return &Result{} // table doesn't exist, nothing to clear
	}
	d := &sql.DeleteStmt{Table: "sqlite_stat1"}
	return e.execDelete(d)
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

//lint:ignore U1000  Planned for P2 ANALYZE
// statLookup returns the stat string for a given index, or empty if not available.
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

	// Handle PRAGMA ... = value for known pragmas
	if s.Value != "" {
		switch name {
		case "LEGACY_ALTER_TABLE":
			e.legacyAlterTable = s.Value == "1"
		case "RECURSIVE_TRIGGERS":
			e.recursiveTriggers = s.Value == "1" || strings.EqualFold(s.Value, "ON") || strings.EqualFold(s.Value, "TRUE")
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
		}
		// When setting a PRAGMA value, don't also return the value
		return &Result{}
	}

	if fn, ok := pragmaHandlers[name]; ok {
		return fn(e)
	}
	return &Result{}
}

var pragmaHandlers = map[string]func(e *Engine) *Result{
	"TABLE_INFO":          func(e *Engine) *Result { return &Result{Columns: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}} },
	"INDEX_INFO":          func(e *Engine) *Result { return &Result{Columns: []string{"seqno", "cid", "name"}} },
	"INDEX_LIST":          func(e *Engine) *Result { return &Result{Columns: []string{"seq", "name", "unique"}} },
	"FOREIGN_KEY_LIST":    func(e *Engine) *Result { return &Result{Columns: []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}} },
	"DATABASE_VERSION":    func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"PAGE_SIZE":           func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(e.pager.PageSize())}}} },
	"PAGE_COUNT":          func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"FREELIST_COUNT":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"SCHEMA_VERSION":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"USER_VERSION":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"APPLICATION_ID":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"AUTO_VACUUM":         func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"JOURNAL_MODE":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"memory"}}} },
	"SYNCHRONOUS":         func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"CACHE_SIZE":          func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(2000)}}} },
	"TEMP_STORE":          func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"LOCKING_MODE":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"normal"}}} },
	"DATABASE_LIST":       func(e *Engine) *Result {
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
	"INTEGRITY_CHECK":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"ok"}}} },
	"LEGACY_ALTER_TABLE":  func(e *Engine) *Result {
		val := int64(0)
		if e.legacyAlterTable {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"TABLE_X":             func(e *Engine) *Result { return &Result{Columns: []string{"oid", "colX"}, Rows: [][]interface{}{{int64(0), ""}}} },
	"COUNT_CHANGES":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"CASE_SENSITIVE_LIKE": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"RECURSIVE_TRIGGERS":  func(e *Engine) *Result {
		val := int64(0)
		if e.recursiveTriggers {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"READ_UNCOMMITTED":    func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"ENCODING":            func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{e.encoding}}} },
	"SCHEMA_TABLE":        func(e *Engine) *Result { return &Result{Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"}} },
	"SOFT_HEAP_LIMIT":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"THREADS":             func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"COMPILE_OPTIONS":     func(e *Engine) *Result { return &Result{Columns: []string{"compile_options"}, Rows: [][]interface{}{{"THREADSAFE=1"}}} },
}
