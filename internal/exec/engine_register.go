package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execddl"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Engine construction helpers: NewEngine delegates the database-context
// setup and the module/function registration blocks to these methods so the
// constructor stays a linear composition (SOLID: one concern per method).

// newEngineContexts builds the main context over pg and the separate
// in-memory temp context. SQLite's temp database shadows main for
// unqualified name resolution (temp-first), so a temp view/table with the
// same name as a main object takes precedence for unqualified references.
func newEngineContexts(pg *pager.Pager) (mainCtx, tempCtx *DatabaseContext) {
	mainCtx = &DatabaseContext{
		Name:     "main",
		Pager:    pg,
		Schema:   schema.NewManager(pg),
		FilePath: "",
		IsMemory: pg.IsMemory(),
		IsTemp:   false,
	}
	if tempPg := pager.OpenInMemory(pager.DefaultPageSize); tempPg != nil {
		tc := &DatabaseContext{
			Name:     "temp",
			Pager:    tempPg,
			Schema:   schema.NewManager(tempPg),
			FilePath: "",
			IsMemory: true,
			IsTemp:   true,
		}
		if err := tc.Schema.Init(); err == nil {
			tempCtx = tc
		}
	}
	return mainCtx, tempCtx
}

// registerVTabModules registers every built-in virtual table module and its
// companion SQL functions (SQLite registers these on connection open via
// extension auto-load; frigolite registers them eagerly).
func (e *Engine) registerVTabModules() {
	e.vtabs.RegisterDefaults()
	// sqlite_dbpage (src/dbpage.c): raw page access through the pagers of
	// every attached database; registered here so instances can resolve the
	// schema argument against this connection's databases.
	e.vtabs.Register("sqlite_dbpage", vtab.NewDBPageModule(enginePageSources{e: e}))
	// rtree / rtree_i32 (ext/rtree/rtree.c): R-tree B+tree over shadow tables.
	// Bound to this connection's Database handle so the module can create/read its
	// shadow tables and register its SQL functions (rtreenode/rtreedepth/...).
	e.vtabs.Register("rtree", vtab.NewRtreeModule[float32](e.Database()))
	e.vtabs.Register("rtree_i32", vtab.NewRtreeModule[int32](e.Database()))
	// ext/rtree global SQL functions (rtreenode/rtreedepth/rtreecheck).
	// SQLite reports arity errors with the per-function wording.
	vtab.RegisterRTreeSQLFunctions(e.Database())
	for _, fn := range []string{"rtreenode", "rtreedepth", "rtreecheck"} {
		if f, ok := e.funcs.Find(fn); ok {
			f.WrongArgMsg = true
		}
	}
	// transitive_closure (ext/misc/closure.c): closure of a tree/DAG base
	// table; the source resolves edges through this connection's tables.
	e.vtabs.Register("transitive_closure", vtab.NewTransitiveClosureModule(closureEdgeSource{e: e}))
	// unionvtab (ext/misc/unionvtab.c): union of rowid tables with disjoint
	// ranges; sources resolve through this engine.
	e.RegisterUnionVtab()
	// approximate_match (ext/misc/amatch.c): weighted edit distance over a
	// vocabulary table/column with a cost matrix.
	e.vtabs.Register("approximate_match", vtab.NewApproximateMatchModule(engineVocabSource{e: e}))
	// spellfix1 (ext/misc/spellfix.c): fuzzy search over a vocabulary shadow
	// table (transliteration k1 + phonetic hash k2); bound to this
	// connection so the module can run its shadow DDL/DML and register the
	// spellfix SQL functions (editdist, spellfix1_translit, ...).
	e.vtabs.Register("spellfix1", vtab.NewSpellfixModule(e.Database()))
	vtab.RegisterSpellfixSQLFunctions(e.Database())
	e.registerFTSModules()
}

// registerFTSModules registers the fts3/4/5 modules plus their aux/term/
// tokenize companions (which resolve the live module instances).
func (e *Engine) registerFTSModules() {
	// Register FTS modules (overrides NoopModule defaults)
	ftsMod := fts.NewFTS3Module("fts3")
	e.vtabs.Register("fts3", ftsMod)
	e.vtabs.Register("fts4", fts.NewFTS3Module("fts4"))
	e.vtabs.Register("fts5", fts.NewFTS3Module("fts5"))
	// fts4aux reads the FTS3/4/5 in-memory indexes (fts3_aux.c). Register it
	// after the FTS modules so it can resolve the target table.
	fts4Mod, _ := e.vtabs.Find("fts4")
	fts5Mod, _ := e.vtabs.Find("fts5")
	byName := map[string]*fts.FTS3Module{
		"fts3": ftsMod,
		"fts4": fts4Mod.(*fts.FTS3Module),
		"fts5": fts5Mod.(*fts.FTS3Module),
	}
	e.vtabs.Register("fts4aux", fts.NewFTS4AuxModule(byName))
	// fts4term exposes the raw terms of each FTS index (fts3_term.c), a
	// test-only module that fts3prefix.test uses to verify prefix indexes.
	e.vtabs.Register("fts4term", fts.NewFTS4TermModule(byName))
	// fts3tokenize exposes a tokenizer as a virtual table (fts3_tokenize_vtab.c):
	// querying WHERE input = <text> returns one row per token.
	e.vtabs.Register("fts3tokenize", fts.NewFTS3TokenizeModule())
}

// registerEngineFuncs registers the built-in and test-harness scalar and
// aggregate SQL functions this engine provides.
func (e *Engine) registerEngineFuncs() {
	// SQLite-internal functions used by ALTER TABLE machinery.
	e.funcs.Register("sqlite_rename_quotefix", execddl.FnSQLiteRenameQuoteFix, 2, 2)
	// The rank() test-harness function (src/test_func.c): reads an FTS
	// matchinfo blob plus one weight per column and returns a relevance score.
	// The SQLite test build registers it via install_fts3_rank_function; the
	// engine provides it for the fts3rank testgen package.
	e.funcs.Register("rank", function.FtsRankFunc, 1, -1)
	// The inttoptr() test-harness function (tabfunc01/carray tests): turns an
	// 'intarray_addr ...' text into an opaque carray pointer handle.
	e.funcs.Register("inttoptr", vtab.IntToptrFunc, 1, 1)
	// The remember(X, PTR) test-harness function (ext/misc/remember.c analog):
	// stores X into the first element of the addressed carray and returns X.
	e.funcs.Register("remember", vtab.RememberFunc, 2, 2)
	e.registerFileIOFuncs()
	e.registerZipFuncs()
	// Built-in decimal collation (ext/misc/decimal.c creates it via
	// sqlite3_create_collation on extension load).
	e.collations["DECIMAL"] = function.DecimalCollation
}

// registerFileIOFuncs registers the fileio.c scalars (readfile/writefile) —
// used by zipfile.test 2.5.x and the fsdir verification loops.
func (e *Engine) registerFileIOFuncs() {
	e.funcs.Register("readfile", func(args []interface{}) (interface{}, error) {
		path, _ := args[0].(string)
		return vtab.ReadFileFunc(path)
	}, 1, 1)
	e.funcs.Register("writefile", func(args []interface{}) (interface{}, error) {
		name, _ := args[0].(string)
		var data []byte
		if s, ok := args[1].(string); ok {
			data = []byte(s)
		}
		return vtab.WriteFileFunc(name, data, writeFileMode(args), writeFileMtime(args))
	}, 2, 4)
}

// writeFileMode decodes the optional writefile() mode argument.
func writeFileMode(args []interface{}) *uint32 {
	if len(args) < 3 || args[2] == nil {
		return nil
	}
	if n, ok := vtab.AsVtabInt64(args[2]); ok {
		u := uint32(n)
		return &u
	}
	return nil
}

// writeFileMtime decodes the optional writefile() mtime argument.
func writeFileMtime(args []interface{}) *int64 {
	if len(args) < 4 || args[3] == nil {
		return nil
	}
	if n, ok := vtab.AsVtabInt64(args[3]); ok {
		return &n
	}
	return nil
}

// registerZipFuncs registers the zipfile() aggregate and its zipfile_cds()
// xFindFunction overload (zipfile.c zipStep/xFinal + zipfileFindFunction).
func (e *Engine) registerZipFuncs() {
	// zipfile(name[,mode,mtime],data[,method]) aggregate (zipfile.c
	// zipStep/xFinal): each row contributes one member; Final emits ONE
	// combined archive blob per group.
	e.funcs.RegisterAggregate("zipfile", 1, 5, func() function.Aggregator {
		return &vtab.ZipAgg{}
	})
	// zipfile_cds(z): central-directory JSON for one archive member. A
	// non-sentinel argument yields NULL, matching the C behaviour when the
	// argument is not a z column value.
	e.funcs.Register("zipfile_cds", zipCdsScalar, 0, -1)
}

// zipCdsScalar implements the zipfile_cds() overload body.
func zipCdsScalar(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments to function zipfile_cds()")
	}
	s, ok := args[0].(string)
	if !ok || !strings.HasPrefix(s, vtab.ZipCdsSentinelPrefix) {
		return nil, nil
	}
	rest := strings.TrimPrefix(s, vtab.ZipCdsSentinelPrefix)
	i := strings.LastIndex(rest, "\x1f")
	if i < 0 {
		return nil, nil
	}
	idx, cerr := strconv.Atoi(rest[i+1:])
	if cerr != nil {
		return nil, nil
	}
	return vtab.ZipCdsJSON(rest[:i], idx), nil
}
