package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// pragmaTableFuncs is the set of table-valued pragma function names Frigolite
// supports. isPragmaTableFunc checks against this set (not a prefix match) so
// user tables named pragma_* (e.g. CREATE TABLE pragma_t4 AS ...) are not
// shadowed.
var pragmaTableFuncs = map[string]bool{
	"pragma_table_info":        true,
	"pragma_table_xinfo":       true,
	"pragma_table_list":        true,
	"pragma_index_info":        true,
	"pragma_index_xinfo":       true,
	"pragma_index_list":        true,
	"pragma_foreign_key_list":  true,
	"pragma_foreign_key_check": true,
	"pragma_function_list":     true,
	"pragma_module_list":       true,
	"pragma_pragma_list":       true,
	"pragma_integrity_check":   true,
	"pragma_quick_check":       true,
	"pragma_cache_size":        true,
	"pragma_database_list":     true,
	"pragma_collation_list":    true,
	"pragma_compile_options":   true,
}

func isPragmaTableFunc(name string) bool {
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	return pragmaTableFuncs[lower]
}

// execPragmaTableValued executes a SELECT whose FROM clause is a table-valued
// pragma function. The pragma is materialized into column definitions and rows
// and the outer SELECT pipeline runs over them.
func (e *Engine) execPragmaTableValued(s *sql.SelectStmt) *Result {
	colDefs, rows, err := e.materializePragmaTable(s.From)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execSelectOverMaterialized(s, colDefs, rows)
}

// materializePragmaTable converts a table-valued pragma reference into column
// definitions and rows, mirroring SQLite's pragma table-valued functions.
func (e *Engine) materializePragmaTable(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializePragmaTableWithRowImpl(ref, nil)
}

// materializePragmaTableWithRowImpl converts a table-valued pragma reference
// into column definitions and rows. When row is non-nil, column-reference
// arguments are evaluated against it (correlated table-valued pragmas).
func (e *Engine) materializePragmaTableWithRowImpl(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	lower := strings.ToLower(ref.Name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	pragma := strings.TrimPrefix(lower, "pragma_")
	switch pragma {
	case "table_info", "table_xinfo":
		return e.materializeTableInfoWithRow(ref, row)
	case "foreign_key_check":
		return e.materializeForeignKeyCheckWithRow(ref, row)
	case "foreign_key_list":
		return e.materializeForeignKeyListWithRow(ref, row)
	case "table_list":
		return e.materializeTableList(ref)
	case "cache_size":
		// pragma_cache_size is a table-valued form of PRAGMA cache_size:
		// a single row with the setting value.
		return []sql.ColumnDef{{Name: "cache_size"}}, [][]interface{}{{int64(2000)}}, nil
	case "index_info", "index_xinfo":
		return e.materializePragmaIndexInfo(ref)
	case "index_list", "function_list", "module_list", "pragma_list":
		return e.materializePragmaList(pragma, ref)
	case "compile_options":
		// pragma_compile_options TVF: one compile_options column row per
		// compile-time option (sqlite parity; json101 21.1 queries it via
		// WHERE compile_options LIKE '%legacy_json_valid%').
		cols := []sql.ColumnDef{{Name: "compile_options"}}
		opts := e.CompileOptions()
		rows := make([][]interface{}, 0, len(opts))
		for _, o := range opts {
			rows = append(rows, []interface{}{o})
		}
		return cols, rows, nil
	case "integrity_check", "quick_check":
		return e.materializePragmaIntegrityCheck(ref)
	default:
		return nil, nil, fmt.Errorf("no such table-valued pragma: %s", ref.Name)
	}
}

// materializePragmaList dispatches the simple "list" table-valued pragmas
// (index_list, function_list, module_list, pragma_list) to their matchers.
func (e *Engine) materializePragmaList(pragma string, ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	switch pragma {
	case "index_list":
		return e.materializePragmaIndexList(ref)
	case "function_list":
		return e.materializePragmaFunctionList(ref)
	case "module_list":
		return e.materializePragmaModuleList(ref)
	case "pragma_list":
		return e.materializePragmaPragmaList(ref)
	}
	return nil, nil, fmt.Errorf("no such table-valued pragma: %s", ref.Name)
}

// materializePragmaIndexInfo materializes pragma_index_info(name) /
// pragma_index_xinfo(name) as a table-valued function. The pragma's first
// argument is the index name (or a WITHOUT ROWID table name for its implicit
// PRIMARY KEY index).
func (e *Engine) materializePragmaIndexInfo(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "seqno"},
		{Name: "cid"},
		{Name: "name"},
	}
	xinfo := strings.HasSuffix(strings.ToLower(ref.Name), "index_xinfo")
	if xinfo {
		cols = append(cols, sql.ColumnDef{Name: "desc"}, sql.ColumnDef{Name: "coll"}, sql.ColumnDef{Name: "key"})
	}
	if len(ref.Args) == 0 {
		return cols, nil, nil
	}
	argVal, err := e.evalExpr(ref.Args[0], nil)
	if err != nil {
		return nil, nil, err
	}
	arg, ok := util.UnwrapColumnValue(argVal).(string)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}
	res := e.execPragmaIndexInfo(arg, xinfo)
	if res.Error != nil {
		return nil, nil, res.Error
	}
	return cols, res.Rows, nil
}

// materializePragmaIndexList materializes pragma_index_list(table) as a
// table-valued function with SQLite's columns: (seq, name, unique, origin,
// partial).
func (e *Engine) materializePragmaIndexList(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "seq"},
		{Name: "name"},
		{Name: "unique"},
		{Name: "origin"},
		{Name: "partial"},
	}
	if len(ref.Args) == 0 {
		return cols, nil, nil
	}
	argVal, err := e.evalExpr(ref.Args[0], nil)
	if err != nil {
		return nil, nil, err
	}
	arg, ok := util.UnwrapColumnValue(argVal).(string)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}
	res := e.execPragmaIndexList(arg)
	if res.Error != nil {
		return nil, nil, res.Error
	}
	// execPragmaIndexList may return 3-column rows (seq,name,unique); the
	// table-valued form has 5 columns. Extend the shorter rows.
	var rows [][]interface{}
	for _, r := range res.Rows {
		if len(r) == 3 {
			rows = append(rows, []interface{}{r[0], r[1], r[2], "c", int64(0)})
		} else {
			rows = append(rows, r)
		}
	}
	return cols, rows, nil
}

// materializePragmaFunctionList materializes pragma_function_list as a
// table-valued function with SQLite's columns: (name, builtin, type, enc,
// narg, flags).
func (e *Engine) materializePragmaFunctionList(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "name"},
		{Name: "builtin"},
		{Name: "type"},
		{Name: "enc"},
		{Name: "narg"},
		{Name: "flags"},
	}
	var rows [][]interface{}
	for _, f := range e.funcs.List() {
		builtin := int64(1)
		if !f.Builtin {
			builtin = 0
		}
		typ := "s"
		if f.Type == function.TypeAggregate {
			typ = "a"
		}
		narg := int64(f.MinArgs)
		if f.MaxArgs != f.MinArgs {
			narg = int64(-1) // variable arity
		}
		rows = append(rows, []interface{}{strings.ToLower(f.Name), builtin, typ, "utf8", narg, int64(0)})
	}
	return cols, rows, nil
}

// materializePragmaModuleList materializes pragma_module_list: one column
// (name) listing every registered virtual-table module.
func (e *Engine) materializePragmaModuleList(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{{Name: "name"}}
	var rows [][]interface{}
	for _, m := range e.vtabs.List() {
		rows = append(rows, []interface{}{m})
	}
	return cols, rows, nil
}

// materializePragmaPragmaList materializes pragma_pragma_list: one column
// (name) listing every supported PRAGMA name.
func (e *Engine) materializePragmaPragmaList(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{{Name: "name"}}
	names := []string{
		"pragma_list", "function_list", "module_list", "table_list",
		"table_info", "table_xinfo", "index_info", "index_xinfo", "index_list",
		"foreign_key_list", "foreign_key_check", "collation_list", "database_list",
		"compile_options", "integrity_check", "quick_check", "encoding",
		"journal_mode", "page_size", "cache_size", "cache_spill", "auto_vacuum",
		"user_version", "application_id", "case_sensitive_like", "recursive_triggers",
		"foreign_keys", "defer_foreign_keys", "writable_schema", "data_version",
		"lock_status", "count_changes", "reverse_unordered_selects", "synchronous",
		"temp_store", "locking_mode", "mmap_size", "soft_heap_limit", "threads",
		"read_uncommitted", "recursive_cte_limit", "default_cache_size",
		"ignore_check_constraints", "query_only", "schema_version", "freelist_count",
		"page_count", "legacy_alter_table", "fullfsync", "checkpoint_fullfsync",
	}
	var rows [][]interface{}
	for _, n := range names {
		rows = append(rows, []interface{}{n})
	}
	return cols, rows, nil
}

// materializePragmaIntegrityCheck materializes pragma_integrity_check /
// pragma_quick_check as table-valued functions with one column named after
// the pragma ("integrity_check" or "quick_check"). Each row is a line of
// the check output; a clean database yields a single "ok" row.
func (e *Engine) materializePragmaIntegrityCheck(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	colName := "integrity_check"
	if strings.HasSuffix(strings.ToLower(ref.Name), "quick_check") {
		colName = "quick_check"
	}
	cols := []sql.ColumnDef{{Name: colName}}
	var tableName string
	if len(ref.Args) > 0 {
		argVal, err := e.evalExpr(ref.Args[0], nil)
		if err != nil {
			return nil, nil, err
		}
		if s, ok := util.UnwrapColumnValue(argVal).(string); ok {
			tableName = s
		}
	}
	res := e.execQuickCheck(tableName)
	if res.Error != nil {
		return nil, nil, res.Error
	}
	return cols, res.Rows, nil
}

// pragmaArgsCorrelated reports whether a table-valued pragma reference has
// an argument containing a column reference (an outer-row correlation, e.g.
// pragma_foreign_key_check(name) joined against sqlite_schema, or nested in
// a function call like json_tree(jsonb(big.json))). Delegates to the shared
// execquery implementation.
func pragmaArgsCorrelated(ref sql.TableRef) bool {
	return execquery.PragmaArgsCorrelated(ref)
}

// materializeCorrelatedPragma materializes a table-valued pragma once per left
// row, evaluating column-reference arguments against that row (SQLite
// correlation for table-valued pragma functions). It returns the pragma column
// definitions, the materialized row maps, and for each row map the index of the
// left row it was materialized for (so the join pairs each right row with its
// own left row instead of cross-joining).
func (e *Engine) materializeCorrelatedPragma(ref sql.TableRef, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error) {
	var colDefs []sql.ColumnDef
	var allMaps []RowMap
	var leftIdx []int
	for li, left := range leftRows {
		defs, rows, err := e.materializePragmaTableWithRow(ref, left)
		if err != nil {
			return nil, nil, nil, err
		}
		if colDefs == nil {
			colDefs = defs
		}
		for _, row := range rows {
			m := make(RowMap)
			for i, val := range row {
				if i < len(defs) {
					m[defs[i].Name] = val
				}
			}
			allMaps = append(allMaps, m)
			leftIdx = append(leftIdx, li)
		}
	}
	return colDefs, allMaps, leftIdx, nil
}

// materializePragmaTableWithRow is materializePragmaTable with a row context
// for column-reference arguments.
func (e *Engine) materializePragmaTableWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializePragmaTableWithRowImpl(ref, row)
}
