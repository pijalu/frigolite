package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// isUnionVtabModule reports whether module is the engine-bound unionvtab or
// swarmvtab module (both are *vtab.UnionVtabModule). These modules build
// instances that implement vtab.Disconnecter and must persist per created
// table, unlike the stateless per-statement modules.
func isUnionVtabModule(module vtab.Module) bool {
	if mn, ok := module.(vtab.ModuleNamer); ok {
		name := mn.ModuleName()
		return name == "unionvtab" || name == "swarmvtab"
	}
	return false
}

// materializeUnionVtab materializes a created unionvtab/swarmvtab table's
// rows, reusing the cached per-table instance so the swarm maxopen LRU and
// open file handles persist across statements (unionvtab.c UnionTab). On
// first use the instance is created and cached under the lowercased table
// name; later statements reuse it (xConnect runs once per table in C).
func (e *Engine) materializeUnionVtab(name string, module vtab.Module, modArgs []string, opts execquery.VtabScanOptions, bindSchema func(vtab.VirtualTable) error) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	vt, ok := e.unionVtabInstances[strings.ToLower(name)]
	if !ok {
		var cerr error
		vt, cerr = createVtabModule(module, modArgs, nil)
		if cerr != nil {
			return nil, nil, nil, cerr, true
		}
		e.unionVtabInstances[strings.ToLower(name)] = vt
	}
	if bindSchema != nil {
		if err := bindSchema(vt); err != nil {
			return nil, nil, nil, err, true
		}
	}
	e.rearmUnionRowidRange(vt, &opts)
	rows, rowids, rerr := readVtabRowsWithRowids(vt, opts.MaxRows)
	if rerr != nil {
		return nil, nil, nil, rerr, true
	}
	defs, dok := unionVtabColumnDefs(vt)
	if !dok {
		return nil, nil, nil, nil, false
	}
	return defs, rows, rowids, nil, true
}

// rearmUnionRowidRange re-arms this statement's rowid-interval source
// selection on the cached per-table instance (xFilter runs once per
// statement in C; the consumed range lives on the cursor, not the table).
// ConsumeRowidRange is invoked on EVERY statement — also when nothing was
// consumed (idxNum==0 in C: the unconstrained full range) — so no previous
// statement's selection can leak into this scan.
func (e *Engine) rearmUnionRowidRange(vt vtab.VirtualTable, opts *execquery.VtabScanOptions) {
	rc, isRC := vt.(vtab.RowidRangeConsumer)
	if !isRC {
		return
	}
	if opts.Where == nil {
		rc.ConsumeRowidRange(nil, false, nil, false)
		return
	}
	e.consumeVTabRowidRange(vt, opts.Where)
	if cleaned, changed := e.dropVTabRowidConjuncts(vt, opts.Where); changed {
		opts.Where = cleaned
		if opts.Residual != nil {
			*opts.Residual = cleaned
		}
	}
}

// unionVtabColumnDefs builds the declared column definitions of a
// unionvtab/swarmvtab instance from its ColumnInfo/ColumnTypeInfo.
func unionVtabColumnDefs(vt vtab.VirtualTable) ([]sql.ColumnDef, bool) {
	ci, ciOK := vt.(vtab.ColumnInfo)
	if !ciOK {
		return nil, false
	}
	defs := make([]sql.ColumnDef, 0, len(ci.Columns()))
	for _, c := range ci.Columns() {
		defs = append(defs, sql.ColumnDef{Name: c})
	}
	if ct, isCT := vt.(vtab.ColumnTypeInfo); isCT {
		types := ct.ColumnTypes()
		for i := range defs {
			if i < len(types) && types[i] != "" {
				defs[i].Type = types[i]
			}
		}
	}
	return defs, true
}

// DisconnectUnionVtabs releases every cached unionvtab/swarmvtab instance
// (unionDisconnect on connection close) and clears the cache.
func (e *Engine) DisconnectUnionVtabs() {
	for key, vt := range e.unionVtabInstances {
		if d, ok := vt.(vtab.Disconnecter); ok {
			d.Disconnect()
		}
		delete(e.unionVtabInstances, key)
	}
}

// DropUnionVtabInstance disconnects and evicts the cached instance of one
// dropped virtual table (unionDisconnect on DROP TABLE), if any.
func (e *Engine) DropUnionVtabInstance(tableName string) {
	key := strings.ToLower(tableName)
	if vt, ok := e.unionVtabInstances[key]; ok {
		if d, isD := vt.(vtab.Disconnecter); isD {
			d.Disconnect()
		}
		delete(e.unionVtabInstances, key)
	}
}

// CacheUnionVtabInstance registers the unionvtab/swarmvtab instance created
// at CREATE VIRTUAL TABLE time so later statements reuse it (unionvtab.c:
// the UnionTab, incl. open source handles + LRU state, lives for the
// table's whole lifetime). A pre-existing cached instance (CREATE OR
// REPLACE-style churn) is disconnected first.
func (e *Engine) CacheUnionVtabInstance(tableName string, vt vtab.VirtualTable) {
	key := strings.ToLower(tableName)
	if old, ok := e.unionVtabInstances[key]; ok {
		if d, isD := old.(vtab.Disconnecter); isD {
			d.Disconnect()
		}
	}
	e.unionVtabInstances[key] = vt
}

// UnionPrepareSources implements vtab.UnionVtabSource: syntax-validate the
// source statement without evaluating it (unionConnect's early prepare,
// before the aux options are parsed — unionConnect lines 912 vs 927 in
// unionvtab.c). Parse errors wrap as "sql error: %s" (unionPrepare).
func (e *Engine) UnionPrepareSources(query string) error {
	_, wrapped, err := wrapUnionSourceQuery(query)
	if err != nil {
		return err
	}
	if _, perr := parse.ParseSQL(wrapped); perr != nil {
		return fmt.Errorf("sql error: %s", perr)
	}
	return nil
}

// wrapUnionSourceQuery turns a unionvtab source specification into the
// statement C prepares: bare names (anything not starting with SELECT or
// VALUES) become a SELECT over that table, then the text is wrapped as
// "SELECT * FROM (...) ORDER BY 3" (unionPreparePrintf in unionConnect).
// inner is the caller's statement part (where :param bindings apply).
func wrapUnionSourceQuery(query string) (inner, wrapped string, err error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", "", fmt.Errorf("unionvtab: missing source specification")
	}
	inner = q
	upper := strings.ToUpper(q)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "VALUES") {
		inner = `SELECT db, tbl, min, max FROM "` + q + `"`
	}
	return inner, `SELECT * FROM (` + inner + `) ORDER BY 3`, nil
}

// UnionResolveSources implements vtab.UnionVtabSource: it turns a unionvtab
// constructor argument — either SQL text returning db,tbl,min,max rows or a
// bare source-table name — into source definitions ordered by Min (C wraps
// the statement "SELECT * FROM (%z) ORDER BY 3"). Swarm :parameter bindings
// (unionConfigureVtab) are applied before the statement is parsed.
func (e *Engine) UnionResolveSources(query string, cfg *vtab.UnionSwarmConfig) ([]vtab.UnionSourceDef, error) {
	inner, wrapped, err := wrapUnionSourceQuery(query)
	if err != nil {
		return nil, err
	}
	if cfg != nil && len(cfg.Params) > 0 {
		// Substitute the :name tokens in the caller's statement, then re-wrap
		// (C prepares the wrapped statement and binds the values afterwards).
		var berr error
		if inner, berr = bindUnionSwarmParams(inner, cfg.Params); berr != nil {
			return nil, berr
		}
		wrapped = `SELECT * FROM (` + inner + `) ORDER BY 3`
	}
	// unionPreparePrintf("SELECT * FROM (%z) ORDER BY 3") — prepare errors
	// surface as "sql error: %s" (unionPrepare).
	stmts, perr := parse.ParseSQL(wrapped)
	if perr != nil {
		return nil, fmt.Errorf("sql error: %s", perr)
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("sql error: statement is not executable")
	}
	res := e.Exec(stmts[0])
	if res.Error != nil {
		return nil, res.Error
	}
	if cfg != nil {
		// unionConfigureVtab: bHasContext = column_count > 4.
		cfg.HasContext = len(res.Columns) > 4
	}
	out := make([]vtab.UnionSourceDef, 0, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < 4 {
			return nil, fmt.Errorf("unionvtab: source query must return 4 columns")
		}
		out = append(out, unionSourceDefFromRow(row))
	}
	return out, nil
}

// unionSourceDefFromRow converts one source-statement row (db, tbl, imin,
// imax [, context]) into a source definition. Values coerce like C's
// sqlite3_column_* reads in unionConnect: a NULL table name stays empty
// (the later table lookup fails: "no such rowid table: main."), and
// imin/imax go through sqlite3_column_int64 (NULL or non-numeric text → 0).
func unionSourceDefFromRow(row []interface{}) vtab.UnionSourceDef {
	var def vtab.UnionSourceDef
	if s, ok := util.UnwrapColumnValue(row[0]).(string); ok {
		def.Schema = s
	}
	t, _ := util.UnwrapColumnValue(row[1]).(string)
	def.Table = t
	def.Min, _ = vtab.AsVtabInt64(util.UnwrapColumnValue(row[2]))
	def.Max, _ = vtab.AsVtabInt64(util.UnwrapColumnValue(row[3]))
	if len(row) > 4 {
		if s, ok := util.UnwrapColumnValue(row[4]).(string); ok {
			def.Context = s
		}
	}
	return def
}

// bindUnionSwarmParams substitutes the :name parameter tokens of the source
// statement with text literals (C binds the values with sqlite3_bind_text
// after prepare; textual substitution before parsing is equivalent for
// these values). A parameter named by an option but absent from the
// statement is the unionConfigureVtab "no such SQL parameter" error, raised
// in option order.
func bindUnionSwarmParams(sqlText string, params []vtab.UnionSwarmParam) (string, error) {
	present := map[string]bool{}
	tk := sql.NewTokenizer(sqlText)
	for {
		tok := tk.Next()
		if tok.Type == sql.TokenEOF {
			break
		}
		if tok.Type == sql.TokenParam && strings.HasPrefix(tok.Value, ":") {
			present[tok.Value] = true
		}
	}
	for _, p := range params {
		if !present[p.Name] {
			return "", fmt.Errorf("swarmvtab: no such SQL parameter: %s", p.Name)
		}
	}
	byName := make(map[string]string, len(params))
	for _, p := range params {
		byName[p.Name] = p.Value
	}
	var b strings.Builder
	pos := 0
	tk2 := sql.NewTokenizer(sqlText)
	for {
		tok := tk2.Next()
		if tok.Type == sql.TokenEOF {
			break
		}
		if tok.Type == sql.TokenParam && tok.Pos >= pos {
			if v, ok := byName[tok.Value]; ok {
				b.WriteString(sqlText[pos:tok.Pos])
				b.WriteString("'" + strings.ReplaceAll(v, "'", "''") + "'")
				pos = tok.Pos + len(tok.Value)
			}
		}
	}
	b.WriteString(sqlText[pos:])
	return b.String(), nil
}

// unionSourceEntry locates and validates one unionvtab source table: it
// must exist and be a rowid table (unionIsIntkeyTable: views and WITHOUT
// ROWID tables are rejected; the implicit rowid of a plain rowid table
// qualifies). Error display carries the schema prefix only when the source
// row named one (C formats zDb "." zTab with NULL zDb rendering empty).
func (e *Engine) unionSourceEntry(defSchema, table string) (*schema.Entry, error) {
	name := strings.ToLower(table)
	display := table
	if defSchema != "" {
		display = defSchema + "." + table
	}
	entry := e.lookupUnionSource(defSchema, name)
	if entry == nil || entry.Type != schema.TypeTable {
		return nil, fmt.Errorf("no such rowid table: %s", display)
	}
	if e.WithoutRowidVTab(entry.Name) || strings.Contains(strings.ToUpper(entry.SQL), "WITHOUT ROWID") {
		return nil, fmt.Errorf("no such rowid table: %s", display)
	}
	return entry, nil
}

// lookupUnionSource resolves one source table across schemas. An
// unqualified source follows SQLite's default name resolution (TEMP, then
// MAIN, then attached databases in attach order); a named schema resolves
// within that schema with a TEMP fallback.
func (e *Engine) lookupUnionSource(defSchema, name string) *schema.Entry {
	var entry *schema.Entry
	lookup := func(ctx *DatabaseContext) bool {
		if ctx == nil {
			return false
		}
		var ferr error
		entry, ferr = ctx.Schema.FindTable(name)
		return ferr == nil && entry != nil
	}
	switch {
	case defSchema == "":
		if lookup(e.getDB("TEMP")) {
			return entry
		}
		if lookup(e.getDB("main")) {
			return entry
		}
		for _, ctx := range e.dbList {
			up := strings.ToUpper(ctx.Name)
			if up == "MAIN" || up == "TEMP" || up == "TEMPORARY" {
				continue
			}
			if lookup(ctx) {
				return entry
			}
		}
	case strings.EqualFold(defSchema, "main"):
		if !lookup(e.getDB("main")) {
			lookup(e.mainDB)
		}
	case !strings.EqualFold(defSchema, "temp"):
		if !lookup(e.getDB(defSchema)) {
			lookup(e.getDB("TEMP"))
		}
	default:
		lookup(e.getDB("TEMP"))
	}
	return entry
}

// UnionSourceSchema implements vtab.UnionVtabSource: validate one source
// (intkey rowid table) and return its schema fingerprint plus declared
// column names/types and the INTEGER-PK column index (pTab->iPK).
func (e *Engine) UnionSourceSchema(def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig) (string, []string, []string, int, error) {
	if def.File != "" {
		return e.unionFileSourceSchema(def, cfg)
	}
	entry, err := e.unionSourceEntry(def.Schema, def.Table)
	if err != nil {
		return "", nil, nil, -1, err
	}
	fp, cols, types, pk := e.unionFingerprint(entry)
	return fp, cols, types, pk, nil
}

// unionFingerprint builds the pragma_table_info parity string
// (group_concat(quote(name)||'.'||quote(type))) from a table entry's column
// definitions; only cross-source equality of the string is observable.
// The pk index mirrors unionConnect's declare-time query
// max((cid+1)*(type='INTEGER' AND pk=1))-1: the LAST declared column that
// is an INTEGER primary key, or -1 when none.
func (e *Engine) unionFingerprint(entry *schema.Entry) (string, []string, []string, int) {
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	cols := make([]string, len(colDefs))
	types := make([]string, len(colDefs))
	parts := make([]string, len(colDefs))
	pk := -1
	for i, cd := range colDefs {
		cols[i] = cd.Name
		types[i] = cd.Type
		parts[i] = "'" + cd.Name + "'.'" + cd.Type + "'"
		if cd.PrimaryKey && strings.EqualFold(cd.Type, "INTEGER") {
			pk = i
		}
	}
	return strings.Join(parts, ","), cols, types, pk
}

// UnionReadRows implements vtab.UnionVtabSource: one source's rows in rowid
// order (declared columns only) with the parallel rowid list (C reads
// "SELECT rowid, * FROM tbl ORDER BY rowid" per source statement).
func (e *Engine) UnionReadRows(def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig) ([][]interface{}, []int64, error) {
	ex := e
	if def.File != "" {
		fe, err := e.unionFileDB(def.File, def, cfg)
		if err != nil {
			return nil, nil, err
		}
		ex = fe
	}
	qual := `"` + def.Table + `"`
	if def.File == "" && def.Schema != "" {
		qual = `"` + def.Schema + `"."` + def.Table + `"`
	}
	stmts, perr := parse.ParseSQL(`SELECT rowid, * FROM ` + qual + ` ORDER BY rowid`)
	if perr != nil || len(stmts) == 0 {
		return nil, nil, fmt.Errorf("unionvtab: invalid source read")
	}
	res := ex.Exec(stmts[0])
	if res.Error != nil {
		return nil, nil, res.Error
	}
	rows := make([][]interface{}, len(res.Rows))
	rowids := make([]int64, len(res.Rows))
	for i, r := range res.Rows {
		if len(r) == 0 {
			rowids[i] = 0
			rows[i] = nil
			continue
		}
		if n, ok := vtab.AsVtabInt64(util.UnwrapColumnValue(r[0])); ok {
			rowids[i] = n
		}
		rows[i] = r[1:]
	}
	return rows, rowids, nil
}

// UnionReleaseFile implements vtab.UnionVtabSource: drop a swarm source's
// open database handle and fire the openclose UDF with bClose=1.
func (e *Engine) UnionReleaseFile(def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig) {
	key := unionFileKey{path: def.File}
	h, ok := e.unionFileDBs[key]
	if !ok {
		return
	}
	delete(e.unionFileDBs, key)
	e.unionInvokeOpenClose(h.def, h.cfg, 1)
	h.eng.Close()
}

// UnionFunctionExists implements vtab.UnionVtabSource: whether a scalar
// function of that name is registered (the missing=/openclose= UDF
// statements are prepared at option-parse time).
func (e *Engine) UnionFunctionExists(name string) bool {
	_, ok := e.funcs.Find(name)
	return ok
}

// RegisterUnionVtab binds the engine-backed unionvtab and swarmvtab
// modules (createUnionVtab registers the same module object twice).
func (e *Engine) RegisterUnionVtab() {
	e.vtabs.Register("unionvtab", vtab.NewUnionVtabModule(e))
	e.vtabs.Register("swarmvtab", vtab.NewSwarmVtabModule(e))
}
