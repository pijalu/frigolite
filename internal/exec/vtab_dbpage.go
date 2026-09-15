package exec

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

// dbpagePageSource adapts a single database's pager to vtab.PageSource
// (src/dbpage.c reads whole pages through the pager).
type dbpagePageSource struct {
	p *pager.Pager
}

// PageCount reports the number of pages in the file.
func (s dbpagePageSource) PageCount() uint32 { return s.p.NumPages() }

// PageSize reports the page size in bytes.
func (s dbpagePageSource) PageSize() uint32 { return s.p.PageSize() }

// ReadPage returns a copy of the raw page bytes: ReadPage hands back the
// cached page itself, which callers must not alias.
func (s dbpagePageSource) ReadPage(pgno uint32) ([]byte, error) {
	pg, err := s.p.ReadPage(pgno)
	if err != nil {
		return nil, err
	}
	data := make([]byte, len(pg.Data))
	copy(data, pg.Data)
	return data, nil
}

// WritePage replaces the raw bytes of page pgno.
func (s dbpagePageSource) WritePage(pgno uint32, data []byte) error {
	if int(s.p.PageSize()) == 0 || uint32(len(data)) != s.p.PageSize() {
		return fmt.Errorf("sqlite_dbpage: %d-byte page is not %d bytes", len(data), s.p.PageSize())
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	return s.p.WritePage(&pager.Page{Data: buf, PageNum: pgno})
}

// TruncatePages drops all pages after n (src/dbpage.c INSERT with NULL data).
func (s dbpagePageSource) TruncatePages(n uint32) error { return s.p.Truncate(n) }

// enginePageSources resolves ATTACHed schema names to their pagers.
type enginePageSources struct {
	e *Engine
}

// PageSourceFor implements vtab.PageSourceProvider. Schema names are matched
// case-insensitively against the connection's databases ("main", "temp",
// ATTACH aliases).
func (p enginePageSources) PageSourceFor(schema string) (vtab.PageSource, bool) {
	ctx, ok := p.e.databases[strings.ToUpper(schema)]
	if !ok || ctx == nil || ctx.Pager == nil {
		return nil, false
	}
	return dbpagePageSource{ctx.Pager}, true
}

// AllPageSources implements vtab.PageSourceProvider: every database of the
// connection in attachment order (e.dbList is built main-first).
func (p enginePageSources) AllPageSources() []vtab.NamedPageSource {
	out := make([]vtab.NamedPageSource, 0, len(p.e.dbList))
	for _, ctx := range p.e.dbList {
		if ctx == nil || ctx.Pager == nil {
			continue
		}
		name := strings.ToLower(ctx.Name)
		if ctx.IsTemp {
			name = "temp"
		}
		out = append(out, vtab.NamedPageSource{Schema: name, Src: dbpagePageSource{ctx.Pager}})
	}
	return out
}

// VTabUpdaterInstance resolves a table name to an updatable virtual-table
// instance (DMLContext). Two forms resolve:
//
//   - an eponymous module's implicit instance (FROM-usable module name used
//     directly as an UPDATE/INSERT target, e.g. UPDATE sqlite_dbpage ...);
//   - a CREATE VIRTUAL TABLE entry whose stored SQL names a registered
//     module (CREATE VIRTUAL TABLE t1 USING sqlite_dbpage).
//
// ok is false when the name is neither; err reports instance creation
// failures for names that DO resolve to a vtab.
func (e *Engine) VTabUpdaterInstance(name string) (vtab.VirtualTable, []sql.ColumnDef, bool, error) {
	lower := strings.ToLower(name)
	var module vtab.Module
	var args []string
	var entry *schema.Entry
	var ctx *DatabaseContext
	if m, ok := e.vtabs.Find(lower); ok && vtab.ModuleIsEponymous(m) {
		module = m
	} else if e2, c2, terr := e.findTable(name); terr == nil && e2 != nil {
		entry = e2
		ctx = c2
		if os.Getenv("CL_DBG") != "" {
			fmt.Fprintf(os.Stderr, "VU DBG sql=%q type=%q root=%d\n", entry.SQL, entry.Type, entry.RootPage)
		}
		mod2, args2, ok2 := vtabModuleFromSQL(entry.SQL)
		if ok2 {
			m3, found := e.vtabs.Find(mod2)
			if !found {
				return nil, nil, true, fmt.Errorf("no such module: %s", mod2)
			}
			module, args = m3, args2
		}
	}
	if module == nil {
		if os.Getenv("CL_DBG") != "" {
			fmt.Fprintf(os.Stderr, "VU DBG module nil\n")
		}
		return nil, nil, false, nil
	}
	vt, err := createVtabModule(module, args, nil)
	if err != nil {
		if os.Getenv("CL_DBG") != "" {
			fmt.Fprintf(os.Stderr, "VU DBG create err=%v\n", err)
		}
		return nil, nil, true, err
	}
	// Schema-bound modules (rtree, dbdata, dbstat, ...) need their resolved
	// db + table name to name their shadow tables. BindSchema is already called
	// at CREATE time and during SELECT export, but the write path
	// (execVTabInsert/Update/Delete) reaches the instance only here, so bind it
	// now. It is idempotent: shadow DDL + root-node creation are no-ops if the
	// tables already exist.
	if sb, ok := vt.(vtab.SchemaBoundVTab); ok {
		if err := sb.BindSchema(ctx.Name, entry.Name); err != nil {
			return nil, nil, true, err
		}
	}
	// Only generic updatable instances are claimed here; FTS and other
	// special-purpose modules keep their dedicated write paths.
	if _, isUp := vt.(vtab.RowUpdater); !isUp {
		if os.Getenv("CL_DBG") != "" {
			fmt.Fprintf(os.Stderr, "VU DBG not RowUpdater type=%T\n", vt)
		}
		return nil, nil, false, nil
	}
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil, nil, true, fmt.Errorf("virtual table %s has no columns", name)
	}
	defs := make([]sql.ColumnDef, 0)
	for _, c := range ci.Columns() {
		defs = append(defs, sql.ColumnDef{Name: c})
	}
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden := hc.HiddenColumns()
		for i := range defs {
			defs[i].Hidden = hidden[i]
		}
	}
	return vt, defs, true, nil
}

// DirectOnlyVTab reports whether name resolves to an eponymous module
// registered SQLITE_VTAB_DIRECTONLY (DMLContext).
func (e *Engine) DirectOnlyVTab(name string) bool {
	m, ok := e.vtabs.Find(strings.ToLower(name))
	return ok && vtab.ModuleIsEponymous(m) && vtab.ModuleIsDirectOnly(m)
}

// MaterializeCreatedVTab materializes a CREATE VIRTUAL TABLE instance's rows
// for SELECT execution: the schema entry has RootPage 0 and its stored SQL
// names the module (e.g. csv). ok is false when name is not such a table.
func (e *Engine) MaterializeCreatedVTab(name string, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	entry, ctx, err := e.findTable(name)
	if err != nil || entry == nil || entry.RootPage != 0 {
		return nil, nil, nil, nil, false
	}
	modName, modArgs, isVtab, skip := createdVTabModuleKind(e, entry, name)
	if skip {
		return nil, nil, nil, nil, false
	}
	if debugClosure {
		fmt.Fprintf(os.Stderr, "MCVT name=%s mod=%q args=%q\n", name, modName, modArgs)
	}
	if isEchoModule(modName, modArgs) {
		return e.materializeEchoVTab(entry, modArgs[0])
	}
	if !isVtab {
		return nil, nil, nil, nil, false
	}
	module, found := e.vtabs.Find(modName)
	if !found {
		return nil, nil, nil, fmt.Errorf("no such module: %s", modName), true
	}
	// Schema-bound modules (rtree) name shadow tables after the vtab; give
	// every scan instance the resolved db + table identity before its first
	// read. Table-valued/eponymous modules are unaffected (binder no-op).
	bindSchema := func(vt vtab.VirtualTable) error {
		if sb, ok := vt.(vtab.SchemaBoundVTab); ok && entry != nil && ctx != nil {
			return sb.BindSchema(ctx.Name, entry.Name)
		}
		return nil
	}
	// unionvtab/swarmvtab keep per-table persistent state (the swarm source
	// handles and maxopen LRU — unionvtab.c UnionTab) that must survive
	// across statements. Reuse the cached instance when present so the LRU
	// is a table-lifetime invariant; other modules are re-created per
	// statement as before.
	if isUnionVtabModule(module) {
		return e.materializeUnionVtab(entry.Name, module, modArgs, opts, bindSchema)
	}
	rows, rowids, rerr := e.materializeVtabModule(module, modArgs, nil, opts, bindSchema)
	if rerr != nil {
		return nil, nil, nil, rerr, true
	}
	vt, cerr := createVtabModule(module, modArgs, nil)
	if cerr != nil {
		return nil, nil, nil, cerr, true
	}
	ci, ciOK := vt.(vtab.ColumnInfo)
	if !ciOK {
		return nil, nil, nil, nil, false // no declared columns: not a generic read path
	}
	defs := make([]sql.ColumnDef, 0, len(ci.Columns()))
	for _, c := range ci.Columns() {
		defs = append(defs, sql.ColumnDef{Name: c})
	}
	if ct, ok := vt.(vtab.ColumnTypeInfo); ok {
		types := ct.ColumnTypes()
		for i := range defs {
			if i < len(types) && types[i] != "" {
				defs[i].Type = types[i]
			}
		}
	}
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden := hc.HiddenColumns()
		for i := range defs {
			defs[i].Hidden = hidden[i]
		}
	}
	return defs, rows, rowids, nil, true
}

// debugClosure toggles verbose tracing of created-vtab materialization.
var debugClosure = os.Getenv("CL_DBG") != ""

// isEchoModule reports whether a created virtual table uses the echo module
// with a source-table argument. The echo module mirrors its underlying source
// table (SQLite test8.c: echoConnect declares the source table's columns and
// echoCursor steps through its b-tree); the registered echo stub declares
// nothing, so materialization reads the source table directly.
func isEchoModule(modName string, modArgs []string) bool {
	return strings.EqualFold(modName, "echo") && len(modArgs) > 0
}

// createdVTabModuleKind resolves a created (rootpage-0) virtual table's module
// name/arguments. skip=true marks tables that never take the generic vtab
// materialization path: FTS/fts5 keep their dedicated scan paths.
func createdVTabModuleKind(e *Engine, entry *schema.Entry, name string) (modName string, modArgs []string, isVtab bool, skip bool) {
	modName, modArgs, isVtab = vtabModuleFromSQL(entry.SQL)
	if _, isFTS := e.ftsTables[entry.Name]; isFTS {
		return modName, modArgs, isVtab, true
	}
	if _, isFTS5 := e.fts5Tables[entry.Name]; isFTS5 {
		return modName, modArgs, isVtab, true
	}
	return modName, modArgs, isVtab, false
}

// materializeEchoVTab materializes an echo virtual table by scanning its
// source table: echoConnect (SQLite test8.c) declares the source table's
// columns and echoCursor reads the source b-tree rows, rowid included.
// Returns ok=false when the source table or its columns cannot be resolved.
func (e *Engine) materializeEchoVTab(entry *schema.Entry, srcArg string) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	defs := e.echoColumnDefs(entry.Name, srcArg)
	if len(defs) == 0 {
		return nil, nil, nil, nil, false
	}
	srcName := strings.Trim(srcArg, "'\"")
	srcEntry, ctx, ferr := e.findTable(srcName)
	if ferr != nil || srcEntry == nil {
		return nil, nil, nil, fmt.Errorf("no such table: %s", srcName), true
	}
	tree := e.TableBTreePg(ctx.Pager, srcEntry.Name, srcEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil, nil, nil, cerr, true
	}
	var rows [][]interface{}
	var rowids []int64
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		rows = append(rows, rec.Values)
		rowids = append(rowids, cell.RowID)
		if okN, nerr := cursor.Next(); nerr != nil || !okN {
			break
		}
	}
	return defs, rows, rowids, nil, true
}

// MaterializeCreatedVTabFunc materializes the table-valued form of a CREATED
// virtual table (FROM t('x')): the FROM arguments bind to the leftmost
// HIDDEN columns as equality constraints (SQLite's vtab TVF form). ok is
// false when ref does not name a created vtab (the caller falls back to
// "'t' is not a function" handling).
func (e *Engine) MaterializeCreatedVTabFunc(ref sql.TableRef, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	entry, ctx, err := e.findTable(ref.Name)
	if err != nil || entry == nil || entry.RootPage != 0 {
		return nil, nil, nil, nil, false
	}
	if _, isFTS := e.ftsTables[entry.Name]; isFTS {
		return nil, nil, nil, nil, false // FTS keeps its dedicated scan path
	}
	if _, isFTS5 := e.fts5Tables[entry.Name]; isFTS5 {
		return nil, nil, nil, nil, false // fts5 has its own TVF path
	}
	modName, modArgs, isVtab := vtabModuleFromSQL(entry.SQL)
	if !isVtab {
		return nil, nil, nil, nil, false
	}
	module, found := e.vtabs.Find(modName)
	if !found {
		return nil, nil, nil, fmt.Errorf("no such module: %s", modName), true
	}
	// Discover the hidden columns from a representative instance; without
	// declared columns or hidden columns the TVF form cannot bind arguments
	// (handled=false falls through to the not-a-function error).
	vt, cerr := createVtabModuleConn(module, modArgs, nil)
	if cerr != nil {
		return nil, nil, nil, cerr, true
	}
	hidden := tvfHiddenColumns(vt)
	if len(ref.Args) > 0 && len(hidden) == 0 {
		return nil, nil, nil, nil, false
	}
	for i, argExpr := range ref.Args {
		if i >= len(hidden) {
			break
		}
		conj := &sql.BinaryOp{
			Left:     &sql.ColumnRef{Name: hidden[i]},
			Operator: "=",
			Right:    argExpr,
		}
		opts.Where = andExpr(opts.Where, conj)
	}
	rows, rowids, rerr := e.materializeVtabModule(module, modArgs, nil, opts, func(vt vtab.VirtualTable) error {
		if sb, ok := vt.(vtab.SchemaBoundVTab); ok && entry != nil && ctx != nil {
			return sb.BindSchema(ctx.Name, entry.Name)
		}
		return nil
	})
	if rerr != nil {
		return nil, nil, nil, rerr, true
	}
	return createdVtabColumnDefs(module, modArgs), rows, rowids, nil, true
}

// tvfHiddenColumns lists the leftmost-hidden-column names of an instance in
// declaration order.
func tvfHiddenColumns(vt vtab.VirtualTable) []string {
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil
	}
	hc, ok := vt.(vtab.HiddenColumnInfo)
	if !ok || len(hc.HiddenColumns()) == 0 {
		return nil
	}
	var out []string
	for i, c := range ci.Columns() {
		if hc.HiddenColumns()[i] {
			out = append(out, c)
		}
	}
	return out
}

// andExpr ANDs a conjunction onto a WHERE expression.
func andExpr(where, conj sql.Expr) sql.Expr {
	if where == nil {
		return conj
	}
	return &sql.BinaryOp{Left: where, Right: conj, Operator: "AND"}
}

// createdVtabColumnDefs builds the projected column definitions (with types
// and HIDDEN flags) of a created vtab's representative instance.
func createdVtabColumnDefs(module vtab.Module, modArgs []string) []sql.ColumnDef {
	vt, err := createVtabModule(module, modArgs, nil)
	if err != nil {
		return nil
	}
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil
	}
	defs := make([]sql.ColumnDef, 0, len(ci.Columns()))
	for _, c := range ci.Columns() {
		defs = append(defs, sql.ColumnDef{Name: c})
	}
	if ct, ok := vt.(vtab.ColumnTypeInfo); ok {
		types := ct.ColumnTypes()
		for i := range defs {
			if i < len(types) && types[i] != "" {
				defs[i].Type = types[i]
			}
		}
	}
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden := hc.HiddenColumns()
		for i := range defs {
			defs[i].Hidden = hidden[i]
		}
	}
	return defs
}

// VtabPlanInstance resolves a created virtual table (CREATE VIRTUAL TABLE
// schema entry, RootPage 0) to a representative instance plus its declared
// column names, for prepare-time xBestIndex calls (EQP parity, wherecode.c).
// ok is false when name is not such a vtab, when the instance lacks declared
// columns, or when planning cannot proceed without the runtime materializer's
// diagnostics/errors: a missing module ("no such module" is a runtime error,
// not a plan) falls back to a plain SCAN here, as do unionvtab/swarmvtab —
// instantiating one outside materialization would re-open swarm source 0 (see
// WithoutRowidVTab for the side-effect rationale).
func (e *Engine) VtabPlanInstance(name string) (vtab.VirtualTable, []string, bool) {
	entry, ctx, err := e.findTable(name)
	if err != nil || entry == nil || entry.RootPage != 0 {
		return nil, nil, false
	}
	if _, isFTS := e.ftsTables[entry.Name]; isFTS {
		return nil, nil, false // FTS keeps its dedicated scan path
	}
	if _, isFTS5 := e.fts5Tables[entry.Name]; isFTS5 {
		return nil, nil, false // fts5 keeps its dedicated scan path
	}
	modName, modArgs, isVtab := vtabModuleFromSQL(entry.SQL)
	if !isVtab {
		return nil, nil, false
	}
	module, found := e.vtabs.Find(modName)
	if !found || isUnionVtabModule(module) {
		return nil, nil, false
	}
	vt, cerr := createVtabModule(module, modArgs, nil)
	if cerr != nil {
		return nil, nil, false
	}
	// Schema-bound modules (rtree) name shadow tables after the vtab; the
	// binding is idempotent (already done at CREATE time) and a failure only
	// downgrades planning — materialization re-surfaces the real error.
	if sb, ok := vt.(vtab.SchemaBoundVTab); ok {
		if berr := sb.BindSchema(ctx.Name, entry.Name); berr != nil {
			return nil, nil, false
		}
	}
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil, nil, false
	}
	return vt, ci.Columns(), true
}

// vtabModuleFromSQL extracts the module name and arguments from a stored
// "CREATE VIRTUAL TABLE ... USING module(args)" statement.
func vtabModuleFromSQL(sqlStr string) (module string, args []string, ok bool) {
	up := strings.ToUpper(sqlStr)
	idx := strings.Index(up, " USING ")
	if idx < 0 {
		return "", nil, false
	}
	rest := strings.TrimSpace(sqlStr[idx+len(" USING "):])
	end := strings.IndexAny(rest, "( \t\n\r,")
	if end < 0 {
		return strings.ToLower(strings.TrimSuffix(rest, ";")), nil, rest != ""
	}
	module = strings.ToLower(strings.TrimSpace(rest[:end]))
	// Allow whitespace between module name and '(' ("USING rtree (...)").
	j := end
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r') {
		j++
	}
	if j >= len(rest) || (rest[j] != '(' && strings.ContainsAny(module, " \t\n")) {
		// Trailing junk without an argument list.
		return module, nil, true
	}
	if rest[j] != '(' {
		return module, nil, true
	}
	close := strings.LastIndex(rest, ")")
	if close < 0 {
		return module, nil, true
	}
	args = vtab.SplitModuleArgs(rest[j+1 : close])
	return module, args, true
}

// WithoutRowidVTab reports whether the named created virtual table's stored
// schema declares WITHOUT ROWID (DMLContext/SelectContext).
func (e *Engine) WithoutRowidVTab(name string) bool {
	entry, _, err := e.findTable(name)
	if err != nil || entry == nil {
		return false
	}
	// The declaration may live in the stored SQL (csv schema=... forms) or
	// only in the module's own declared schema (zipfile) — ask both.
	if strings.Contains(strings.ToUpper(entry.SQL), "WITHOUT ROWID") {
		return true
	}
	if entry.RootPage != 0 {
		return false
	}
	modName, modArgs, isVtab := vtabModuleFromSQL(entry.SQL)
	if !isVtab {
		return false
	}
	module, found := e.vtabs.Find(modName)
	if !found {
		return false
	}
	// unionvtab/swarmvtab are always rowid vtabs (WithoutRowid() false);
	// short-circuit WITHOUT instantiating — Create would re-open swarm
	// source 0 (unionOpenDatabase), a side effect this probe must not have.
	if isUnionVtabModule(module) {
		return false
	}
	vt, cerr := createVtabModuleConn(module, modArgs, nil)
	if cerr != nil {
		return false
	}
	if wr, ok := vt.(interface{ WithoutRowid() bool }); ok {
		return wr.WithoutRowid()
	}
	return false
}

// closureEdgeSource adapts the engine's query machinery to
// vtab.ClosureEdgeSource: it runs an internal SELECT over the configured base
// table and returns (id, parent) pairs, skipping NULL parents.
type closureEdgeSource struct {
	e *Engine
}

// ClosureEdges implements vtab.ClosureEdgeSource.
func (s closureEdgeSource) ClosureEdges(table, idCol, parentCol string) ([][2]int64, error) {
	// Validate the configured columns against the base table so overrides
	// naming missing columns report SQLite's message (closure01 4.2/4.3).
	table = strings.Trim(table, "'\"")
	// Clean identifiers naming missing columns report SQLite's prepare error
	// up front (closure01 4.1/4.2: "no such column: t2.xyz" / "t2.pqr").
	// Malformed values (e.g. "'abc'x") fall through to natural evaluation.
	for _, col := range []string{idCol, parentCol} {
		clean := len(col) > 0
		for i := 0; i < len(col); i++ {
			c := col[i]
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
				clean = false
				break
			}
		}
		if !clean {
			continue
		}
		entry, _, terr := s.e.findTable(table)
		if terr != nil || entry == nil {
			return nil, fmt.Errorf("no such table: %s", table)
		}
		colDefs := s.e.parseColumnDefs(entry.Name, entry.SQL)
		found := false
		for _, cd := range colDefs {
			if strings.EqualFold(cd.Name, col) {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no such column: %s.%s", table, col)
		}
	}
	q := fmt.Sprintf("SELECT %q, %q FROM %q", idCol, parentCol, table)
	stmts, err := parse.ParseSQL(q)
	if err != nil {
		return nil, err
	}
	if len(stmts) == 0 {
		return nil, nil
	}
	res := s.e.Exec(stmts[0])
	if res.Error != nil {
		if strings.Contains(res.Error.Error(), "no such column") {
			// Qualify the failing configured column with the base table
			// (closure01 4.2/4.3: "no such column: t2.xyz" / "t2.pqr").
			if strings.Contains(res.Error.Error(), idCol) {
				return nil, fmt.Errorf("no such column: %s.%s", table, idCol)
			}
			if strings.Contains(res.Error.Error(), parentCol) {
				return nil, fmt.Errorf("no such column: %s.%s", table, parentCol)
			}
		}
		return nil, res.Error
	}
	if debugClosure {
		fmt.Fprintf(os.Stderr, "CE rows=%d first=%T %#v\n", len(res.Rows), rowType(res.Rows), rowFirst(res.Rows))
	}
	out := make([][2]int64, 0, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		id, iok := toClosureInt(row[0])
		parent, pok := toClosureInt(row[1])
		if debugClosure && len(out) == 0 {
			fmt.Fprintf(os.Stderr, "CE first conv id=%v/%v par=%v/%v\n", row[0], iok, row[1], pok)
		}
		if !iok || !pok {
			continue
		}
		out = append(out, [2]int64{id, parent})
	}
	return out, nil
}

func rowType(rows [][]interface{}) interface{} {
	if len(rows) > 0 && len(rows[0]) > 0 {
		return rows[0][0]
	}
	return nil
}
func rowFirst(rows [][]interface{}) []interface{} {
	if len(rows) > 0 {
		return rows[0]
	}
	return nil
}

// toClosureInt coerces a query cell to int64.
func toClosureInt(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// engineVocabSource adapts the engine's query machinery to
// vtab.VocabSource (approximate_match vocabulary + cost tables).
type engineVocabSource struct {
	e *Engine
}

// VocabWords implements vtab.VocabSource.
func (s engineVocabSource) VocabWords(table, wordCol string) ([]string, error) {
	rows, err := s.queryRows(fmt.Sprintf("SELECT %q FROM %q", wordCol, table))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if len(row) == 0 || row[0] == nil {
			continue
		}
		w := fmt.Sprintf("%v", row[0])
		if seen[w] {
			continue // vocabulary tables are set-semantics (amatch.c UNIQUE)
		}
		seen[w] = true
		out = append(out, w)
	}
	return out, nil
}

// CostRules implements vtab.VocabSource.
func (s engineVocabSource) CostRules(table string) ([]vtab.AmatchCostRule, error) {
	rows, err := s.queryRows(fmt.Sprintf("SELECT iLang, cFrom, cTo, Cost FROM %q", table))
	if err != nil {
		return nil, err
	}
	out := make([]vtab.AmatchCostRule, 0, len(rows))
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		lang, ok1 := toClosureInt(row[0])
		cost, ok2 := toClosureInt(row[3])
		from := cellString(row[1])
		to := cellString(row[2])
		if !ok1 || !ok2 || from == "" && to == "" {
			continue
		}
		out = append(out, vtab.AmatchCostRule{Lang: lang, From: from, To: to, Cost: cost})
	}
	return out, nil
}

// queryRows runs an internal SELECT and returns raw rows.
func (s engineVocabSource) queryRows(q string) ([][]interface{}, error) {
	stmts, err := parse.ParseSQL(q)
	if err != nil {
		return nil, err
	}
	if len(stmts) == 0 {
		return nil, nil
	}
	res := s.e.Exec(stmts[0])
	if res.Error != nil {
		return nil, res.Error
	}
	return res.Rows, nil
}

func cellString(v interface{}) string {
	if v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprintf("%v", v)
}
