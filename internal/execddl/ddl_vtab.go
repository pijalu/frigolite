// Package execddl: CREATE VIRTUAL TABLE execution (module lookup, authorizer
// contract, FTS registration) and its transaction-rollback undo path. Split
// from ddl_trigger.go; behavior unchanged.
package execddl

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// execCreateVirtualTable implements CREATE VIRTUAL TABLE. The check order
// mirrors SQLite: reserved-name check and existing-object handling run at
// prepare/name-resolution time, the module lookup before any authorizer
// action, and the constructor contract last (vtab.c vtabCallConstructor):
//
//	"object name reserved for internal use: <name>"
//	success no-op                                    (IF NOT EXISTS + exists)
//	"table <name> already exists"
//	"no such module: <module>"
//	authorizer: SQLITE_INSERT sqlite_master, then
//	            SQLITE_CREATE_VTABLE <name> <module> <db>
//	"vtable constructor failed: <name>"              (xCreate error, no message)
//	"vtable constructor did not declare schema: <name>" (no declare_vtab)
func (e *DDLExecutor) execCreateVirtualTable(s *sql.CreateVirtualTableStmt) *Result {
	if res := e.validateReservedName(s.Name); res != nil {
		return res
	}
	ctx0, tableName0 := resolveVTabContext(e, s.Name)
	if existing, ferr := ctx0.Schema.FindTable(tableName0); ferr == nil && existing != nil {
		if s.IfNotExists {
			// IF NOT EXISTS makes an existing object a silent no-op — even a
			// real table under the same name, and even when the module is
			// unknown: SQLite resolves the name before the module
			// (vtab1-1.8.2, oracle-verified).
			return &Result{}
		}
		// A table of the same name (including a prior virtual table's schema
		// entry) makes the CREATE fail BEFORE any shadow table is touched
		// (SQLite raises "table t1 already exists" from the schema insert;
		// fts3expr-6.1 re-CREATEs t1 in the same session).
		return &Result{Error: fmt.Errorf("table %s already exists", tableName0)}
	}
	module, ok := e.ctx.VTables().Find(s.Module)
	if !ok {
		return &Result{Error: fmt.Errorf("no such module: %s", s.Module)}
	}
	// Eponymous-only modules (series.c) register without xCreate: the name
	// is usable in FROM but CREATE VIRTUAL TABLE reports "no such module"
	// (tabfunc01-1.3).
	if eo, ok := module.(vtab.EponymousOnlyModule); ok && eo.EponymousOnly() {
		return &Result{Error: fmt.Errorf("no such module: %s", s.Module)}
	}
	// TEMP-only modules (unionvtab.c): creation outside the TEMP schema
	// fails before any source resolution (unionvtab.test 2.1.*). The error
	// names the connected module (unionConnect's zVtab: "unionvtab" or
	// "swarmvtab").
	if to, ok := module.(vtab.TempSchemaOnly); ok && to.TempSchemaOnly() {
		target := ""
		if idx := strings.LastIndexByte(s.Name, '.'); idx >= 0 {
			target = s.Name[:idx]
		}
		if !strings.EqualFold(target, "temp") {
			name := s.Module
			if mn, ok := module.(vtab.ModuleNamer); ok {
				name = mn.ModuleName()
			}
			return &Result{Error: fmt.Errorf("%s tables must be created in TEMP schema", name)}
		}
	}
	// Authorizer actions (sqlite3AuthCheck at prepare/codegen parity): the
	// sqlite_schema row insert is authorized first, then the vtab creation
	// with the module name as arg2 (vtab3-1.2 trace order: SQLITE_INSERT
	// sqlite_master, SQLITE_CREATE_VTABLE <name> <module> <db>).
	if err := e.ctx.Authorize(auth.ActionInsert, "sqlite_master", "", ctx0.Name, ""); err != nil {
		return &Result{Error: err}
	}
	if err := e.ctx.Authorize(auth.ActionCreateVTable, tableName0, s.Module, ctx0.Name, ""); err != nil {
		return &Result{Error: err}
	}
	// Module arguments: the parser AST joins argument tokens with spaces
	// (rule 405), but SQLite hands the module the VERBATIM argument text
	// (sqlite3VtabArgExtend concatenation). When RawSQL is available, re-split
	// the original argument text so spellfix1's "edit_cost_table=x" and FTS4's
	// option syntax arrive exactly as written.
	createArgs := s.Args
	if strings.TrimSpace(s.RawSQL) != "" {
		if _, rargs, perr := parseVTabSQL(s.RawSQL); perr == nil && rargs != nil {
			createArgs = rargs
		}
	}
	// The constructor receives the vtab's own name (xCreate's argv[2]):
	// the echo module's '*'-pattern source resolves <name><suffix> during
	// xCreate (test8.c echoConstructor).
	if bn, ok := module.(vtab.CreateNameSetter); ok {
		bn.SetCreateName(tableName0)
	}
	vt, err := module.Create(createArgs)
	if err != nil {
		// A constructor error WITHOUT a message becomes
		// "vtable constructor failed: <table>" (vtab.c vtabCallConstructor:
		// zErr==0 → sqlite3MPrintf "vtable constructor failed: %s"); an
		// error with a message passes through verbatim (vtab1-1.5.x vs
		// unionvtab's own diagnostics).
		var silent *vtab.SilentConstructorError
		if errors.As(err, &silent) {
			return &Result{Error: fmt.Errorf("vtable constructor failed: %s", tableName0)}
		}
		return &Result{Error: err}
	}
	// A constructor that never declared the instance schema fails: SQLite
	// tracks sqlite3_declare_vtab during xCreate (sCtx.bDeclared) and reports
	// "vtable constructor did not declare schema: <name>" otherwise
	// (vtab1-1.3.x: the echo module with zero arguments). Declared columns
	// imply the declare; a module whose constructor declares even an empty
	// schema opts in via SchemaDeclaredMarker (fts3()/fts5()).
	declared := false
	if ci, ok := vt.(vtab.ColumnInfo); ok && len(ci.Columns()) > 0 {
		declared = true
	}
	if m, ok := vt.(vtab.SchemaDeclaredMarker); ok && m.SchemaDeclared() {
		declared = true
	}
	if !declared {
		e.disconnectVtabOnCreateFailure(vt)
		return &Result{Error: fmt.Errorf("vtable constructor did not declare schema: %s", tableName0)}
	}
	// The schema entry is written BEFORE the module binds its schema: SQLite
	// inserts the sqlite_schema row at prepare/codegen time and OP_VCreate
	// only then invokes xCreate, so the vtab row precedes every shadow-table
	// row in sqlite_master (oracle: CREATE VIRTUAL TABLE rt USING rtree(...) →
	// rowids rt=1, rt_rowid=2, rt_node=3, rt_parent=4).
	ctx, tableName := resolveVTabContext(e, s.Name)
	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      e.vtabSQL(s, tableName),
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		e.disconnectVtabOnCreateFailure(vt)
		return &Result{Error: err}
	}
	// Bind the resolved schema/table name so the module can create its shadow
	// tables (rtree/dbdata/dbstat name backing tables after the vtab name).
	// A binding failure (shadow-name collision) aborts the CREATE — the
	// schema entry is rolled back like a failed statement (the master row
	// C wrote before xCreate fails is removed by the statement abort).
	if sb, ok := vt.(vtab.SchemaBoundVTab); ok {
		if err := sb.BindSchema(ctx0.Name, tableName0); err != nil {
			ctx.Schema.RemoveEntry(entry.Name)
			e.disconnectVtabOnCreateFailure(vt)
			return &Result{Error: err}
		}
	}
	e.cachePersistentVtabInstance(tableName, vt)

	// Transaction rollback undoes the pager writes (sqlite_schema row and
	// the shadow family) but not the module's LIVE instance registration:
	// without an undo, a later CREATE of the same name inside a new
	// transaction re-binds the stale instance and silently skips the shadow
	// family (fts5tokenizer 8.2: CREATE e6 -> ROLLBACK -> CREATE e6 ->
	// "no such table: e6_content").
	if e.ctx.InTransaction() {
		e.ctx.AppendDDLBuffer(func() {
			e.forgetCreatedVTab(tableName, vt)
		})
	}

	// fts5 owns its module lifecycle: the instance bound by BindSchema above
	// registered the table in the fts5 module; record it in the engine map so
	// DML/SELECT route to the fts5 machinery.
	if strings.EqualFold(s.Module, "fts5") {
		if err := e.registerFTS5VTab(tableName); err != nil {
			ctx.Schema.RemoveEntry(entry.Name)
			return &Result{Error: err}
		}
		return &Result{}
	}
	// If this is an FTS module, create and store the FTS table. The args
	// are re-parsed from the stored SQL text (which preserves the original
	// spacing) so that module validation matches SQLite: "xyz=abc" fails
	// FTS4 validation with "unrecognized parameter: xyz=abc" while
	// "xyz = abc" reports "unrecognized parameter: xyz = abc" (the vtab
	// arg span, not a space-joined reconstruction).
	if e.getFTSModule(s.Module) != nil {
		_, args, perr := parseVTabSQL(entry.SQL)
		if perr != nil {
			ctx.Schema.RemoveEntry(entry.Name)
			return &Result{Error: perr}
		}
		if err := e.registerFTSVTab(s.Module, tableName, args); err != nil {
			// The CREATE failed: roll back the schema entry so a retry or a
			// subsequent DROP does not see a half-created table.
			ctx.Schema.RemoveEntry(entry.Name)
			return &Result{Error: err}
		}
	}
	return &Result{}
}

// forgetCreatedVTab is the transaction-rollback undo of a CREATE VIRTUAL
// TABLE: it drops the engine's fts5 registration and the module's live
// instance for the table (the sqlite_schema row and shadow family are
// reverted by the pager restore).
func (e *DDLExecutor) forgetCreatedVTab(tableName string, vt vtab.VirtualTable) {
	if _, ok := e.ctx.FTS5Tables()[tableName]; ok {
		delete(e.ctx.FTS5Tables(), tableName)
		if mod, ok := e.fts5Module(); ok {
			mod.DropTable(tableName)
		}
	}
	if d, ok := vt.(vtab.Disconnecter); ok {
		d.Disconnect()
	}
}

// disconnectVtabOnCreateFailure rolls back a module instance whose CREATE
// failed after module.Create succeeded (unionvtab.c: xConnect followed by a
// schema-insert failure runs xDisconnect).
func (e *DDLExecutor) disconnectVtabOnCreateFailure(vt vtab.VirtualTable) {
	if d, ok := vt.(vtab.Disconnecter); ok {
		d.Disconnect()
	}
}

// cachePersistentVtabInstance keeps a unionvtab/swarmvtab CREATE-time
// instance alive for the table's whole lifetime (unionvtab.c UnionTab): its
// open source handles and maxopen LRU state must persist across statements.
// Other modules are re-materialized per statement and need no cache.
func (e *DDLExecutor) cachePersistentVtabInstance(tableName string, vt vtab.VirtualTable) {
	if _, ok := vt.(vtab.Disconnecter); ok {
		e.ctx.CacheUnionVtabInstance(tableName, vt)
	}
}

// vtabSQL renders the sqlite_schema SQL text for a CREATE VIRTUAL TABLE.
// When the parser captured the original statement text it is stored verbatim
// (SQLite preserves module argument punctuation, e.g. "varchar(32)"); a
// reconstruction from the parsed argument list is the fallback. The same
// IF NOT EXISTS / TEMP stripping rules as CREATE TABLE apply.
func (e *DDLExecutor) vtabSQL(s *sql.CreateVirtualTableStmt, tableName string) string {
	if strings.TrimSpace(s.RawSQL) != "" {
		return stripIfNotExists(stripCreateTempKeyword(strings.TrimSpace(s.RawSQL)))
	}
	return fmt.Sprintf("CREATE VIRTUAL TABLE %s USING %s(%s)", tableName, s.Module, strings.Join(s.Args, ","))
}

// resolveVTabContext resolves the schema prefix and database context for a
// CREATE VIRTUAL TABLE (mirroring execCreateTable): CREATE VIRTUAL TABLE
// temp.x stores the entry in the TEMP schema.
func resolveVTabContext(e *DDLExecutor, rawName string) (*DatabaseContext, string) {
	ctx := e.ctx.MainDB()
	tableName := rawName
	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
			if tc := e.ctx.GetDB("temp"); tc != nil {
				ctx = tc
			}
		} else if schemaUpper != "MAIN" {
			if db := e.ctx.GetDB(prefix); db != nil {
				ctx = db
			}
		}
		tableName = rawName[dotIdx+1:]
	}
	return ctx, tableName
}

// registerFTSVTab creates and stores the FTS table for an FTS virtual table
// module. The CREATE VIRTUAL TABLE args become the FTS column names. Returns
// the module argument-validation error (e.g. "unrecognized parameter" for an
// unknown FTS4 option) so CREATE VIRTUAL TABLE fails like SQLite does.
func (e *DDLExecutor) registerFTSVTab(moduleName, tableName string, args []string) error {
	ftsMod := e.getFTSModule(moduleName)
	if ftsMod == nil {
		return nil
	}
	ftsTable, err := ftsMod.GetOrCreateTable(tableName, moduleName, args)
	if err != nil {
		return err
	}
	// A self-referential content source with NO explicit columns (CREATE
	// VIRTUAL TABLE t1 USING fts4(content=t1)) is rejected at CREATE:
	// SQLite's xCreate must read the content table to derive the columns and
	// recurses into the not-yet-created vtab, failing with "vtable
	// constructor called recursively: t1" (fts4content 11.1). With explicit
	// columns (fts4(a, content=t1)) the CREATE succeeds and every read fails
	// with "SQL logic error" (12.x).
	if strings.EqualFold(ftsTable.ContentTable(), tableName) && len(ftsTable.ColumnNames()) == 0 {
		return fmt.Errorf("vtable constructor called recursively: %s", tableName)
	}
	e.ctx.FTSTables()[tableName] = ftsTable
	// A later validation failure must not leave the FTS table registered:
	// the CREATE fails and the schema entry is rolled back, but a stale
	// FTSTables entry would leak into the next CREATE of the same name
	// (fts4noti 1.8 then 1.9: the failed notindexed=d content=cc CREATE
	// must not poison the following notindexed=a content=cc CREATE).
	cleanupOnErr := func(err error) error {
		if err != nil {
			delete(e.ctx.FTSTables(), tableName)
			// Also drop the module's cached FTS3Table: GetOrCreateTable
			// caches by name, so a failed CREATE (e.g. notindexed=d) would
			// otherwise return the poisoned table object to the next CREATE
			// of the same name (fts4noti 1.8 then 1.9).
			ftsMod.DropTable(tableName)
		}
		return err
	}
	// FTS4 content=<table>: when the CREATE declares no explicit columns, the
	// FTS table's columns are derived from the content table's (fts3.c
	// fts3ContentColumns reads the content table schema). The content table
	// must exist; its column names/order become the FTS columns.
	if ct := ftsTable.ContentTable(); ct != "" && len(ftsTable.ColumnNames()) == 0 {
		ctEntry, _, cerr := e.ctx.FindTable(ct)
		if cerr != nil || ctEntry == nil {
			return cleanupOnErr(fmt.Errorf("no such table: main.%s", ct))
		}
		ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
		var names []string
		for _, cd := range ctDefs {
			if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
				continue
			}
			names = append(names, cd.Name)
		}
		ftsTable.SetColumnNames(names)
	}
	// Validate notindexed=<col> against the final column list (a content=
	// table's names were just derived; an unknown name fails the CREATE with
	// "no such column: X" — fts4noti 1.8: notindexed=d with content=cc).
	if bad := ftsTable.ValidateNotindexedColumns(); bad != "" {
		return cleanupOnErr(fmt.Errorf("no such column: %s", bad))
	}
	// SQLite's fts3CreateTables creates the %_content, %_segments, %_segdir
	// backing tables (and %_docsize/%_stat for FTS4) as part of xCreate. The
	// engine mirrors that: the shadow tables exist as real schema entries so
	// CREATE TRIGGER ... ON <name>_content and SELECT count(*) FROM
	// <name>_segdir work (e_fts3 1.2.2.5, fts3aa 10.0). A content=<table>
	// FTS table has NO %_content shadow (fts3.c fts3CreateTables skips it
	// when zContent is set).
	return e.createFTSShadowTables(tableName, ftsTable, moduleName)
}

// createFTSShadowTables creates the FTS backing-store tables for an FTS
// virtual table (fts3.c fts3CreateTables). The content table carries the
// docid plus one column per user column; segments/segdir are the segment
// b-trees. FTS4 additionally creates docsize and stat tables.

