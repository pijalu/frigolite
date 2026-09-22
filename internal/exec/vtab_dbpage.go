package exec

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
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
	module, args, entry, ctx, rerr := e.resolveUpdaterVtabTarget(name)
	if rerr != nil {
		return nil, nil, true, rerr
	}
	if module == nil {
		if debugUpdater {
			fmt.Fprintf(os.Stderr, "VU DBG module nil\n")
		}
		return nil, nil, false, nil
	}
	vt, err := createVtabModule(module, args, nil)
	if err != nil {
		if debugUpdater {
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
		if debugUpdater {
			fmt.Fprintf(os.Stderr, "VU DBG not RowUpdater type=%T\n", vt)
		}
		return nil, nil, false, nil
	}
	defs, derr := updaterColumnDefs(vt)
	if derr != nil {
		return nil, nil, true, fmt.Errorf("virtual table %s has no columns", name)
	}
	return vt, defs, true, nil
}

// debugUpdater toggles verbose tracing of the vtab updater resolution path.
var debugUpdater = os.Getenv("CL_DBG") != ""

// resolveUpdaterVtabTarget resolves a DML target name to its vtab module.
// module is nil (rerr nil) when the name does not resolve to a vtab at all;
// rerr non-nil reports a claimed name whose module is not registered.
func (e *Engine) resolveUpdaterVtabTarget(name string) (module vtab.Module, args []string, entry *schema.Entry, ctx *DatabaseContext, rerr error) {
	lower := strings.ToLower(name)
	if m, ok := e.vtabs.Find(lower); ok && vtab.ModuleIsEponymous(m) {
		return m, nil, nil, nil, nil
	}
	e2, c2, terr := e.findTable(name)
	if terr != nil || e2 == nil {
		return nil, nil, nil, nil, nil
	}
	entry, ctx = e2, c2
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "VU DBG sql=%q type=%q root=%d\n", entry.SQL, entry.Type, entry.RootPage)
	}
	mod2, args2, ok2 := vtabModuleFromSQL(entry.SQL)
	if !ok2 {
		return nil, nil, entry, ctx, nil
	}
	m3, found := e.vtabs.Find(mod2)
	if !found {
		return nil, nil, entry, ctx, fmt.Errorf("no such module: %s", mod2)
	}
	return m3, args2, entry, ctx, nil
}

// updaterColumnDefs builds name-only column defs (HIDDEN flags applied) of
// an updater instance; an error means the instance declares no columns.
func updaterColumnDefs(vt vtab.VirtualTable) ([]sql.ColumnDef, error) {
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil, fmt.Errorf("virtual table has no columns")
	}
	defs := make([]sql.ColumnDef, 0)
	for _, c := range ci.Columns() {
		defs = append(defs, sql.ColumnDef{Name: c})
	}
	applyHiddenColumnFlags(vt, defs)
	return defs, nil
}

// applyHiddenColumnFlags copies an instance's HIDDEN column flags onto defs.
func applyHiddenColumnFlags(vt vtab.VirtualTable, defs []sql.ColumnDef) {
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden := hc.HiddenColumns()
		for i := range defs {
			defs[i].Hidden = hidden[i]
		}
	}
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
	if debugClosure {
		fmt.Fprintf(os.Stderr, "MCVT name=%s mod=%q args=%q\n", name, modName, modArgs)
	}
	if skip {
		return nil, nil, nil, nil, false
	}
	if isEchoModule(modName, modArgs) {
		return e.materializeEchoVTabModule(entry, modName, modArgs, opts)
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
	bindSchema := e.createdVtabBindSchema(entry, ctx)
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
	return createdVtabScanResult(module, modArgs, rows, rowids)
}

// createdVtabScanResult finishes a materialized created-vtab scan by
// building the projected column definitions of a representative instance.
// handled=false marks instances without declared columns (not a generic
// read path); err carries instance-creation failures.
func createdVtabScanResult(module vtab.Module, modArgs []string, rows [][]interface{}, rowids []int64) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	defs, cerr, defsOK := createdVtabDefsForScan(module, modArgs)
	if cerr != nil {
		return nil, nil, nil, cerr, true
	}
	if !defsOK {
		return nil, nil, nil, nil, false // no declared columns: not a generic read path
	}
	return defs, rows, rowids, nil, true
}

// createdVtabBindSchema returns the schema-binding callback handed to vtab
// materializers: schema-bound modules (rtree) name shadow tables after the
// vtab and need the resolved db + table identity; the binder is a no-op for
// table-valued/eponymous modules and unresolvable identities.
func (e *Engine) createdVtabBindSchema(entry *schema.Entry, ctx *DatabaseContext) func(vtab.VirtualTable) error {
	return func(vt vtab.VirtualTable) error {
		if sb, ok := vt.(vtab.SchemaBoundVTab); ok && entry != nil && ctx != nil {
			return sb.BindSchema(ctx.Name, entry.Name)
		}
		return nil
	}
}

// createdVtabDefsForScan builds the projected column definitions (with
// declared types and HIDDEN flags) of a created vtab's representative
// instance. defsOK is false for instances without declared columns (not a
// generic read path); err carries instance-creation failures.
func createdVtabDefsForScan(module vtab.Module, modArgs []string) (defs []sql.ColumnDef, err error, defsOK bool) {
	vt, cerr := createVtabModule(module, modArgs, nil)
	if cerr != nil {
		return nil, cerr, true
	}
	if _, ciOK := vt.(vtab.ColumnInfo); !ciOK {
		return nil, nil, false
	}
	return typedColumnDefsWithMeta(vt), nil, true
}

// typedColumnDefsWithMeta builds column defs carrying names, declared types
// and HIDDEN flags of a representative instance.
func typedColumnDefsWithMeta(vt vtab.VirtualTable) []sql.ColumnDef {
	ci := vt.(vtab.ColumnInfo)
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
	applyHiddenColumnFlags(vt, defs)
	return defs
}

// debugClosure toggles verbose tracing of created-vtab materialization.
var debugClosure = os.Getenv("CL_DBG") != ""

// isEchoModule reports whether a created virtual table uses the echo module
// with a source-table argument. The echo module mirrors its underlying source
// table (SQLite test8.c: echoConnect declares the source table's columns and
// echoCursor steps through its b-tree).
func isEchoModule(modName string, modArgs []string) bool {
	return strings.EqualFold(modName, "echo")
}

// firstIndexColumn extracts a CREATE INDEX statement's first key column name
// (test8.c getIndexArray reads PRAGMA index_info's left-most entry).
func firstIndexColumn(indexSQL, _ string) string {
	up := strings.ToUpper(indexSQL)
	onIdx := strings.Index(up, " ON ")
	if onIdx < 0 {
		return ""
	}
	rest := indexSQL[onIdx+4:]
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return ""
	}
	list := rest[open+1:]
	if end := strings.IndexByte(list, ')'); end >= 0 {
		list = list[:end]
	}
	// The first key column is the first whitespace-delimited token of the
	// list (index columns may carry COLLATE/ASC/DESC suffixes).
	fields := strings.Fields(strings.TrimSpace(list))
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], "'\"`")
}

// createdVTabModuleKind resolves a created (rootpage-0) virtual table's module
// name/arguments. skip=true marks tables that never take the generic vtab
// materialization path: FTS/fts5 keep their dedicated scan paths.
func createdVTabModuleKind(e *Engine, entry *schema.Entry, name string) (modName string, modArgs []string, isVtab bool, skip bool) {
	modName, modArgs, isVtab = vtabModuleFromSQL(entry.SQL)
	return modName, modArgs, isVtab, e.createdVtabScanBlocked(entry)
}

// materializeEchoVTabModule runs the echo module's scan-time observable
// contract before materializing the source table. The echo module must be
// registered on this connection (register_echo_module / sqlite3_create_module
// parity): a connection without it reports "no such module: echo" for any
// reference to the vtab (vtab1-1.10, vtab1.2.6). The xBestIndex call
// (test8.c echoBestIndex) then observes the planner's constraint set: a
// constraint claimed despite usable==0 is a malfunction naming the vtab
// (where.c:4364-4366), and any module error aborts the statement. Row
// materialization reads the source table (materializeEchoVTab), so argv
// bindings and omit flags carry no residual effect here — the core
// re-checks every constraint, which is observationally the echo module's
// own re-run of the WHERE against the source table.
func (e *Engine) materializeEchoVTabModule(entry *schema.Entry, modName string, modArgs []string, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	if len(modArgs) == 0 {
		// A source-less echo table cannot be created (the constructor never
		// declares a schema), so nothing reaches materialization for it.
		return nil, nil, nil, nil, false
	}
	module, found := e.vtabs.Find(modName)
	if !found {
		return nil, nil, nil, fmt.Errorf("no such module: %s", modName), true
	}
	vt, cerr := createVtabModule(module, modArgs, nil)
	if cerr != nil {
		return nil, nil, nil, cerr, true
	}
	if err := planEchoVTabBestIndex(vt, entry.Name, opts); err != nil {
		return nil, nil, nil, err, true
	}
	return e.materializeEchoVTab(entry, modArgs[0])
}

// EchoJoinBestIndexPlan implements execquery.SelectContext: it offers the
// join's effective ON terms to an echo vtab operand's xBestIndex (where.c
// offers ON + WHERE terms per table). ok is false when name is not an echo
// vtab; err carries a claimed-unusable-constraint malfunction or module
// error.
func (e *Engine) EchoJoinBestIndexPlan(name string, on sql.Expr) (error, bool) {
	entry, _, err := e.findTable(name)
	if err != nil || entry == nil || entry.RootPage != 0 {
		return nil, false
	}
	modName, modArgs, isVtab := vtabModuleFromSQL(entry.SQL)
	if !isVtab || !isEchoModule(modName, modArgs) {
		return nil, false
	}
	module, found := e.vtabs.Find(modName)
	if !found {
		return fmt.Errorf("no such module: %s", modName), true
	}
	vt, cerr := createVtabModule(module, modArgs, nil)
	if cerr != nil {
		return cerr, true
	}
	return planEchoBestIndexWithWhere(vt, entry.Name, on), true
}

// planEchoBestIndexWithWhere runs the echo instance's BestIndexPlan over the
// given constraint expression and validates the plan (validateVtabArgvSlots);
// the vtab name personalizes the malfunction error.
func planEchoBestIndexWithWhere(vt vtab.VirtualTable, name string, where sql.Expr) error {
	pbi, ok := vt.(vtab.PlanBestIndexer)
	if !ok {
		return nil
	}
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok || len(ci.Columns()) == 0 {
		return nil
	}
	var fo vtab.FunctionOverloader
	if f, isFo := vt.(vtab.FunctionOverloader); isFo {
		fo = f
	}
	opts := execquery.VtabScanOptions{Where: where, MaxRows: -1}
	ii, _, err := execquery.BuildVtabIndexInfoWithInstance(&opts, name, ci.Columns(), fo)
	if err != nil {
		return err
	}
	if berr := pbi.BestIndexPlan(ii); berr != nil {
		return berr
	}
	if _, _, merr := validateVtabArgvSlots(ii); merr != nil {
		return fmt.Errorf("%s.xBestIndex malfunction", name)
	}
	return nil
}

// planEchoVTabBestIndex offers the scan's constraints to the echo instance's
// xBestIndex and validates the returned plan. The vtab name personalizes the
// malfunction error ("<name>.xBestIndex malfunction", where.c:4366).
func planEchoVTabBestIndex(vt vtab.VirtualTable, name string, opts execquery.VtabScanOptions) error {
	return planEchoBestIndexWithWhere(vt, name, opts.Where)
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
	// Pattern source form (echo('*_base')): the real table is
	// <this-name><suffix> (test8.c echoConstructor isPattern branch).
	if strings.HasPrefix(srcName, "*") {
		srcName = entry.Name + srcName[1:]
	}
	srcEntry, ctx, ferr := e.findTable(srcName)
	if ferr != nil || srcEntry == nil {
		return nil, nil, nil, fmt.Errorf("no such table: %s", srcName), true
	}
	tree := e.TableBTreePg(ctx.Pager, srcEntry.Name, srcEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil, nil, nil, cerr, true
	}
	rows, rowids := scanBTreeRecords(cursor)
	return defs, rows, rowids, nil, true
}

// scanBTreeRecords reads every remaining record of an open cursor,
// collecting row values and rowids; decoding or stepping failures stop the
// scan (the rows read so far are kept).
func scanBTreeRecords(cursor *btree.Cursor) ([][]interface{}, []int64) {
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
	return rows, rowids
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
	if e.createdVtabScanBlocked(entry) {
		return nil, nil, nil, nil, false // FTS/fts5 keep their dedicated TVF paths
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
	opts.Where = tvfBindArgs(opts.Where, ref.Args, hidden)
	rows, rowids, rerr := e.materializeVtabModule(module, modArgs, nil, opts, e.createdVtabBindSchema(entry, ctx))
	if rerr != nil {
		return nil, nil, nil, rerr, true
	}
	return createdVtabColumnDefs(module, modArgs), rows, rowids, nil, true
}

// tvfBindArgs ANDs the FROM-arguments onto the scan's WHERE as equality
// constraints against the leftmost HIDDEN columns (SQLite's vtab TVF form).
func tvfBindArgs(where sql.Expr, args []sql.Expr, hidden []string) sql.Expr {
	for i, argExpr := range args {
		if i >= len(hidden) {
			break
		}
		conj := &sql.BinaryOp{
			Left:     &sql.ColumnRef{Name: hidden[i]},
			Operator: "=",
			Right:    argExpr,
		}
		where = andExpr(where, conj)
	}
	return where
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
	if _, ok := vt.(vtab.ColumnInfo); !ok {
		return nil
	}
	return typedColumnDefsWithMeta(vt)
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
	if e.createdVtabScanBlocked(entry) {
		return nil, nil, false // FTS/fts5 keep their dedicated scan paths
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

// createdVtabScanBlocked reports whether a rootpage-0 schema entry keeps its
// dedicated scan path (FTS or fts5) instead of generic vtab materialization.
func (e *Engine) createdVtabScanBlocked(entry *schema.Entry) bool {
	if _, isFTS := e.ftsTables[entry.Name]; isFTS {
		return true
	}
	_, isFTS5 := e.fts5Tables[entry.Name]
	return isFTS5
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
	return module, vtabModuleArgs(rest, end, module), true
}

// vtabModuleArgs extracts the module argument list starting the scan at the
// first character after the module name. nil means the statement has no
// argument list (trailing junk, bare module name, or an unterminated '(').
func vtabModuleArgs(rest string, end int, module string) []string {
	// Allow whitespace between module name and '(' ("USING rtree (...)").
	j := end
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r') {
		j++
	}
	if j >= len(rest) || (rest[j] != '(' && strings.ContainsAny(module, " \t\n")) {
		// Trailing junk without an argument list.
		return nil
	}
	if rest[j] != '(' {
		return nil
	}
	close := strings.LastIndex(rest, ")")
	if close < 0 {
		return nil
	}
	return vtab.SplitModuleArgs(rest[j+1 : close])
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
		if !closureCleanIdent(col) {
			continue
		}
		if err := closureColumnExists(s.e, table, col); err != nil {
			return nil, err
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
		return nil, closureQualifyNoSuchColumn(res.Error, table, idCol, parentCol)
	}
	if debugClosure {
		fmt.Fprintf(os.Stderr, "CE rows=%d first=%T %#v\n", len(res.Rows), rowType(res.Rows), rowFirst(res.Rows))
	}
	return closureRowsFrom(res.Rows)
}

// closureCleanIdent reports whether col is a clean SQL identifier (letters,
// underscores, and — not in first position — digits). Malformed configured
// values fall through to natural evaluation.
func closureCleanIdent(col string) bool {
	if len(col) == 0 {
		return false
	}
	for i := 0; i < len(col); i++ {
		c := col[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// closureColumnExists verifies one configured column is declared by the base
// table ("no such table: T" / "no such column: T.C").
func closureColumnExists(e *Engine, table, col string) error {
	entry, _, terr := e.findTable(table)
	if terr != nil || entry == nil {
		return fmt.Errorf("no such table: %s", table)
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, col) {
			return nil
		}
	}
	return fmt.Errorf("no such column: %s.%s", table, col)
}

// closureQualifyNoSuchColumn rewrites a raw "no such column" failure,
// qualifying the failing configured column with the base table (closure01
// 4.2/4.3: "no such column: t2.xyz" / "t2.pqr").
func closureQualifyNoSuchColumn(err error, table, idCol, parentCol string) error {
	if strings.Contains(err.Error(), "no such column") {
		// Qualify the failing configured column with the base table
		// (closure01 4.2/4.3: "no such column: t2.xyz" / "t2.pqr").
		if strings.Contains(err.Error(), idCol) {
			return fmt.Errorf("no such column: %s.%s", table, idCol)
		}
		if strings.Contains(err.Error(), parentCol) {
			return fmt.Errorf("no such column: %s.%s", table, parentCol)
		}
	}
	return err
}

// closureRowsFrom converts the internal SELECT's rows to (id, parent) edge
// pairs, skipping rows whose cells do not convert to integers.
func closureRowsFrom(rows [][]interface{}) ([][2]int64, error) {
	out := make([][2]int64, 0, len(rows))
	for _, row := range rows {
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
