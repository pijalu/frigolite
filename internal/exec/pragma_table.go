package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
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

// materializeVtabTableFunc materializes a table-valued virtual-table function
// reference (e.g. FROM generate_series(1,256)) into column definitions and
// rows. where carries the enclosing WHERE clause so hidden-column equality /
// IN constraints can be pushed into the instance before row generation
// (series.c xBestIndex/xFilter parity). It returns an error wrapping "no such
// module" when the name is not a registered vtab module, so callers can fall
// back to ordinary table lookup.
func (e *Engine) materializeVtabTableFunc(ref sql.TableRef, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	args, err := evalVtabArgs(e, ref, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	valArgs, verr := evalVtabArgValues(e, ref, nil)
	if verr != nil {
		return nil, nil, nil, verr
	}
	rows, rowids, err := e.materializeVtabModule(module, args, valArgs, opts, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	vt, err := createVtabModuleConn(module, args, valArgs)
	if err != nil {
		return nil, nil, nil, err
	}
	return vtabColumnDefs(vt, rows), rows, rowids, nil
}

// materializeVtabTableFuncInRow is MaterializeVtabTableFunc with argument
// expressions evaluated against a specific outer row (correlation).
func (e *Engine) materializeVtabTableFuncInRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	args, err := evalVtabArgs(e, ref, row)
	if err != nil {
		return nil, nil, err
	}
	valArgs, verr := evalVtabArgValues(e, ref, row)
	if verr != nil {
		return nil, nil, verr
	}
	vt, err := createVtabModuleConn(module, args, valArgs)
	if err != nil {
		return nil, nil, err
	}
	rows, err := readVtabRows(vt)
	if err != nil {
		return nil, nil, err
	}
	return vtabColumnDefs(vt, rows), rows, nil
}

// materializeCorrelatedVTabFunc materializes a table-valued vtab function
// whose arguments reference left-side columns (e.g.
// FROM t, json_each(t.json) AS jx): one Connect per left row with that row
// as the argument evaluation context, all right rows concatenated with the
// index of the left row each batch came from (SQLite correlation).
func (e *Engine) materializeCorrelatedVTabFunc(ref sql.TableRef, leftRows []RowMap, where sql.Expr) ([]sql.ColumnDef, []RowMap, []int, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	// WHERE pushdown parity: conjuncts referencing only outer-side columns
	// gate TVF materialization per row. sqlite evaluates such terms before
	// entering the inner loop, so a failing json_valid() check prevents
	// json_each from ever seeing an invalid argument (json102-1011). A
	// conjunct that cannot be evaluated against the outer row alone
	// (references TVF or later-join columns) is not a gating term; it still
	// applies in the outer WHERE pass.
	conjuncts := splitAndConjuncts(where)
	var colDefs []sql.ColumnDef
	var allMaps []RowMap
	var leftIdx []int
	for li, left := range leftRows {
		if !e.outerConjunctsPass(conjuncts, left) {
			continue
		}
		args, err := evalVtabArgs(e, ref, left)
		if err != nil {
			return nil, nil, nil, err
		}
		valArgs, verr := evalVtabArgValues(e, ref, left)
		if verr != nil {
			return nil, nil, nil, verr
		}
		vt, err := createVtabModuleConn(module, args, valArgs)
		if err != nil {
			return nil, nil, nil, err
		}
		rows, rerr := readVtabRows(vt)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		defs := vtabColumnDefs(vt, rows)
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

// splitAndConjuncts flattens an AND-tree into its conjuncts.
func splitAndConjuncts(expr sql.Expr) []sql.Expr {
	if expr == nil {
		return nil
	}
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		return append(splitAndConjuncts(bin.Left), splitAndConjuncts(bin.Right)...)
	}
	return []sql.Expr{expr}
}

// outerConjunctsPass reports whether every conjunct fully bound by the
// outer row passes. sqlite pushes only WHERE terms whose column
// references are ALL provided by the outer loop (whereLoop term masking):
// a conjunct referencing TVF or later-join columns resolves to NULL on
// the left row (evaluator fallback) and must NOT gate materialization.
func (e *Engine) outerConjunctsPass(conjuncts []sql.Expr, left RowMap) bool {
	for _, c := range conjuncts {
		if !exprBoundInRow(c, left) {
			continue // references unavailable columns: not a gating term
		}
		pass, err := e.evalBool(c, left)
		if err != nil {
			continue // evaluation against the outer row alone failed
		}
		if !pass {
			return false
		}
	}
	return true
}

// exprBoundInRow reports whether every column reference in expr resolves
// to a key present in row. Subqueries (scalar or EXISTS) are conservatively
// unbound. This mirrors sqlite's rule that a pushed-down WHERE term must be
// completely determined by the outer loop's cursor set.
func exprBoundInRow(expr sql.Expr, row RowMap) bool {
	bound := true
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if !bound {
			return
		}
		switch v := n.(type) {
		case *sql.ColumnRef:
			if !rowHasColumn(v, row) {
				bound = false
			}
		case *sql.Subquery, *sql.ExistsExpr:
			bound = false
		}
	})
	return bound
}

// rowHasColumn reports whether the row map provides the column reference.
// Row maps are keyed by bare column names; a qualified "t.c" (or "db.t.c")
// resolves through the evaluator's unqualified-name fallback for the table
// currently being scanned, so the bare key is checked as a last resort.
func rowHasColumn(v *sql.ColumnRef, row RowMap) bool {
	if _, ok := row[v.Name]; ok {
		return true
	}
	if v.Table == "" {
		return false
	}
	if _, ok := row[v.Table+"."+v.Name]; ok {
		return true
	}
	parts := strings.SplitN(v.Table, ".", 2)
	if len(parts) == 2 {
		if _, ok := row[parts[1]+"."+v.Name]; ok {
			return true
		}
	}
	return false
}

// evalVtabArgs evaluates a vtab reference's argument expressions to strings.
// A NULL argument becomes the empty string (e.g. json_each(NULL) yields no
// rows, matching SQLite's NULL handling).
func evalVtabArgs(e *Engine, ref sql.TableRef, row Row) ([]string, error) {
	args := make([]string, 0, len(ref.Args))
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, row)
		if err != nil {
			return nil, err
		}
		u := util.UnwrapColumnValue(v)
		if u == nil {
			args = append(args, "")
			continue
		}
		if b, ok := u.([]byte); ok {
			// BLOB argument: pass the raw image through verbatim
			// (zipfile(X'504b...') binds an in-memory archive).
			args = append(args, string(b))
			continue
		}
		args = append(args, fmt.Sprintf("%v", u))
	}
	return args, nil
}

// evalVtabArgValues evaluates a vtab reference's argument expressions to SQL
// values, preserving types (BLOB/JSONB inputs survive intact).
func evalVtabArgValues(e *Engine, ref sql.TableRef, row Row) ([]interface{}, error) {
	args := make([]interface{}, 0, len(ref.Args))
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, row)
		if err != nil {
			return nil, err
		}
		args = append(args, util.UnwrapColumnValue(v))
	}
	return args, nil
}

// createVtabModule materializes vt rows via the ValueModule interface when
// typed argument VALUES are available; created-virtual-table re-instantiation
// (DML target resolution, schema SQL re-parse) supplies only the stored TEXT
// argv and must take the xCreate(string argv) path — preferring the typed
// constructor with a nil value list would drop every argument
// (zipfile INSERT reported "constructor requires one argument").
//
// Only a NON-NIL value list indicates runtime-typed arguments (table-valued
// function call sites); a created virtual table re-instantiated from stored
// schema SQL has TEXT argv only.
func createVtabModule(module vtab.Module, strArgs []string, valArgs []interface{}) (vtab.VirtualTable, error) {
	if vm, ok := module.(vtab.ValueModule); ok && valArgs != nil {
		return vm.CreateWithValues(valArgs)
	}
	return module.Create(strArgs)
}

// createVtabModuleConn is the xConnect-side instance constructor: like
// createVtabModule it prefers typed argument VALUES when available and falls
// back to the stored TEXT argv otherwise (preferring the ValueModule
// value constructor with a nil value list would drop every argument).
// CREATE-VIRTUAL-TABLE-form connections bind through Create so the module can
// give create-specific diagnostics (e.g. missing-file archives are legal on
// create, an error on plain connect), while table-valued-function contexts go
// through Connect.
func createVtabModuleConn(module vtab.Module, strArgs []string, valArgs []interface{}) (vtab.VirtualTable, error) {
	if vm, ok := module.(vtab.ValueModule); ok && valArgs != nil {
		// Connect-side sites use the xConnect analogue even for typed args,
		// so a module can give function-specific diagnostics
		// (zipfile: FROM zipfile() must report the function-arity message).
		return vm.ConnectWithValues(valArgs)
	}
	if strArgs != nil {
		return module.Connect(strArgs)
	}
	return module.Create(strArgs)
}

// readVtabRows reads every row from an opened virtual table.
func readVtabRows(vt vtab.VirtualTable) ([][]interface{}, error) {
	rows, _, err := readVtabRowsWithRowids(vt, -1)
	return rows, err
}

// readVtabRowsWithRowids reads every row plus native rowids when the cursor
// exposes them (vtab xRowid parity, e.g. generate_series rowid == value).
// rowids is nil when the cursor has no rowid support. maxRows caps the row
// count (LIMIT pushdown parity); negative means unlimited.
func readVtabRowsWithRowids(vt vtab.VirtualTable, maxRows int64) ([][]interface{}, []int64, error) {
	cur, err := vt.Open()
	if err != nil {
		return nil, nil, err
	}
	defer cur.Close()
	ridCur, hasRowids := cur.(vtab.RowidCursor)
	var rows [][]interface{}
	var rowids []int64
	for cur.Next() {
		var row []interface{}
		for i := 0; ; i++ {
			val, err := cur.Column(i)
			if err != nil {
				break
			}
			row = append(row, val)
		}
		rows = append(rows, row)
		if hasRowids {
			rowids = append(rowids, ridCur.Rowid())
		}
		if maxRows >= 0 && int64(len(rows)) >= maxRows {
			break
		}
	}
	return rows, rowids, nil
}

// vtabColumnDefs builds column definitions from the vtab's declared columns,
// falling back to c0/c1/... names derived from the first row's width.
// HIDDEN columns are included but flagged Hidden (excluded from SELECT *
// and PRAGMA table_info, still resolvable by explicit references — this
// matches SQLite's series.c tests compiled with
// SQLITE_SERIES_CONSTRAINT_VERIFY=1, where the core re-verifies hidden
// constraints per row). Hidden columns are only declared when the cursor
// actually serves their values (row width == full declared schema).
func vtabColumnDefs(vt vtab.VirtualTable, rows [][]interface{}) []sql.ColumnDef {
	var colDefs []sql.ColumnDef
	if ci, ok := vt.(vtab.ColumnInfo); ok {
		var hidden map[int]bool
		if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
			hidden = hc.HiddenColumns()
		}
		// The cursor must provide one value per declared column (including
		// HIDDEN ones) for the hidden defs to be backed by data.
		fullWidth := len(rows) > 0 && len(rows[0]) == len(ci.Columns())
		for i, c := range ci.Columns() {
			cd := sql.ColumnDef{Name: c}
			if hidden[i] {
				if !fullWidth {
					continue
				}
				cd.Hidden = true
			}
			colDefs = append(colDefs, cd)
		}
	}
	if len(colDefs) == 0 && len(rows) > 0 {
		for i := range rows[0] {
			colDefs = append(colDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d", i)})
		}
	}
	return colDefs
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

// materializeTableInfo builds the rows of pragma_table_info / pragma_table_xinfo
// for the table or view named by the first function argument. The result has
// columns (cid, name, type, notnull, dflt_value, pk), one row per column.
func (e *Engine) materializeTableInfo(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializeTableInfoWithRow(ref, nil)
}

// materializeTableInfoWithRow is materializeTableInfo with a row context for
// column-reference arguments (correlated pragma_table_info) and an optional
// second schema argument (pragma_table_info(table, schema)).
func (e *Engine) materializeTableInfoWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "cid"},
		{Name: "name"},
		{Name: "type"},
		{Name: "notnull"},
		{Name: "dflt_value"},
		{Name: "pk"},
	}
	xinfo := strings.HasSuffix(strings.ToLower(ref.Name), "table_xinfo")
	if xinfo {
		cols = append(cols, sql.ColumnDef{Name: "hidden"})
	}
	tableName, err := tableInfoTableName(e, ref, row)
	if err != nil {
		return nil, nil, err
	}

	colDefs, found, err := e.tableInfoColDefs(tableName)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		// Unknown table or view: pragma_table_info returns zero rows.
		return cols, nil, nil
	}
	return cols, tableInfoRows(colDefs, xinfo), nil
}

// tableInfoTableName resolves the first argument of pragma_table_info(xinfo)
// to a table name, honoring the optional second (schema) argument.
func tableInfoTableName(e *Engine, ref sql.TableRef, row Row) (string, error) {
	if len(ref.Args) == 0 {
		return "", fmt.Errorf("wrong number of arguments to function %s()", ref.Name)
	}
	argVal, err := e.evalExpr(ref.Args[0], row)
	if err != nil {
		return "", err
	}
	tableName, ok := argVal.(string)
	if !ok {
		return "", fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}
	// Optional second argument: schema name. Resolve the table within that
	// schema (SQLite pragma_table_info(table, schema)).
	if len(ref.Args) >= 2 {
		schemaVal, err := e.evalExpr(ref.Args[1], row)
		if err != nil {
			return "", err
		}
		if schema, ok := schemaVal.(string); ok && schema != "" {
			tableName = schema + "." + tableName
		}
	}
	return tableName, nil
}

// tableInfoColDefs resolves the column definitions of a table-info target: a
// pragma table-valued function, an ordinary table, or a view. found is false
// when the name is not a known object (zero rows).
func (e *Engine) tableInfoColDefs(tableName string) (colDefs []sql.ColumnDef, found bool, err error) {
	// A pragma table-valued function name (e.g. pragma_function_list) is
	// materialized as a virtual table; PRAGMA table_info(pragma_function_list)
	// must report the FUNCTION's columns, not the synthetic schema entry that
	// findTable synthesizes for PRAGMA_* names.
	if isPragmaTableFunc(tableName) {
		if defs, _, err := e.materializePragmaTableWithRowImpl(sql.TableRef{Name: tableName}, nil); err == nil {
			return defs, true, nil
		}
	}
	if te, _, err := e.findTable(tableName); err == nil {
		return e.parseColumnDefs(te.Name, te.SQL), true, nil
	}
	if ve, _, err := e.findView(tableName); err == nil {
		defs, err := e.viewColumnDefs(ve)
		if err != nil {
			return nil, true, err
		}
		return defs, true, nil
	}
	// Eponymous-only module implicit tables (generate_series): visible to
	// PRAGMA table_info/table_xinfo with hidden columns flagged (tabfunc01-1.1b).
	if defs, found, err := e.eponymousVtabColDefs(tableName); found {
		return defs, true, err
	}
	return nil, false, nil
}

// tableInfoRows renders column definitions as pragma_table_info(xinfo) rows.
func tableInfoRows(colDefs []sql.ColumnDef, xinfo bool) [][]interface{} {
	rows := make([][]interface{}, 0, len(colDefs))
	cid := int64(0)
	for _, cd := range colDefs {
		row, skip := tableInfoRow(cd, cid, xinfo)
		if skip {
			continue
		}
		rows = append(rows, row)
		cid++
	}
	return rows
}

// tableInfoRow renders one column definition as a pragma_table_info(xinfo)
// row. skip is true for dropped columns and (in table_info) hidden columns.
func tableInfoRow(cd sql.ColumnDef, cid int64, xinfo bool) (row []interface{}, skip bool) {
	// Skip dropped columns (removed via ALTER TABLE DROP COLUMN).
	if cd.Dropped {
		return nil, true
	}
	// PRAGMA table_info excludes hidden columns; table_xinfo includes
	// them with a nonzero hidden flag (SQLite pragma.c).
	if !xinfo && isHiddenColumnDef(cd) {
		return nil, true
	}
	notnull, pk, typeName, dflt := tableInfoRowFields(cd)
	if xinfo {
		hiddenFlag := int64(0)
		if isHiddenColumnDef(cd) {
			hiddenFlag = 1
		}
		return []interface{}{cid, cd.Name, typeName, notnull, dflt, pk, hiddenFlag}, false
	}
	return []interface{}{cid, cd.Name, typeName, notnull, dflt, pk}, false
}

// tableInfoRowFields renders a column definition's row fields: notnull and pk
// as 0/1, the declared type (NONE-affinity sentinel rendered as empty), and
// the rendered DEFAULT expression.
func tableInfoRowFields(cd sql.ColumnDef) (notnull, pk int64, typeName string, dflt interface{}) {
	if cd.NotNull {
		notnull = 1
	}
	if cd.PrimaryKey {
		pk = 1
	}
	// The NONE-affinity sentinel (an expression-derived view column with
	// no declared type) renders as an empty type, matching SQLite.
	typeName = cd.Type
	if typeName == util.AffinityNone {
		typeName = ""
	}
	if cd.Default != nil {
		dflt = renderDefaultValue(cd.Default)
	}
	return
}

// renderDefaultValue renders a column DEFAULT expression as SQLite's
// dflt_value text. Numeric unary signs are glued to the number ("-1", "+4.0")
// and string literals keep their quotes.
func renderDefaultValue(d sql.Expr) string {
	if un, ok := d.(*sql.UnaryOp); ok {
		switch un.Operator {
		case "-", "+":
			if nl, ok := un.Operand.(*sql.NumericLit); ok {
				return un.Operator + nl.Value
			}
		}
	}
	return sql.ExprString(d)
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

// materializeForeignKeyListWithRow builds the rows of pragma_foreign_key_list
// (table-valued PRAGMA foreign_key_list) with a row context for
// column-reference arguments (correlated pragma_foreign_key_list). Columns:
// (id, seq, table, from, to, on_update, on_delete, match).
