package exec

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Swarm file-source database access (unionvtab.c): a swarmvtab source row
// names a database FILE (the db column) instead of a schema. Sources open on
// first touch through unionOpenDatabaseInner — openclose UDF with bClose=0,
// then sqlite3_open_v2(READONLY), then (when the missing= UDF is configured)
// invoke it and retry — and close via UnionReleaseFile (maxopen LRU).

// unionFileDBHandle is one open swarm source database: the engine over its
// pager plus the source definition and swarm config captured at open time,
// so the openclose UDF can still fire when the handle is released without
// its caller-supplied context (engine shutdown).
type unionFileDBHandle struct {
	eng *Engine
	def vtab.UnionSourceDef
	cfg *vtab.UnionSwarmConfig
}

// unionFileKey identifies one swarmvtab source handle by file path. C's
// UnionSrc.db handles live for the UnionTab's whole lifetime (the
// openclose/maxopen LRU is a table-persistent invariant), but frigolite
// re-materializes the vtab instance per statement with a fresh cfg pointer
// — keying by cfg would orphan every handle at the next statement and
// re-open source 0 (unbalanced openclose(0)). Keying by path keeps the
// handle map stable across statements, matching C's table lifetime; the
// per-statement cfg still travels with each call for the UDF arguments.
type unionFileKey struct {
	path string
}

// unionFileDB returns the open engine for one swarm file source, opening it
// on first touch (unionOpenDatabaseInner). On failure the C failure path
// (close + openclose(1)) fires before the error returns.
func (e *Engine) unionFileDB(path string, def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig) (*Engine, error) {
	key := unionFileKey{path: path}
	if h, ok := e.unionFileDBs[key]; ok {
		return h.eng, nil
	}
	if err := e.unionInvokeOpenClose(def, cfg, 0); err != nil {
		e.unionInvokeOpenClose(def, cfg, 1)
		return nil, err
	}
	fe, err := e.openUnionFileEngine(path)
	if err != nil {
		// Retry through the missing= UDF (unionOpenDatabaseInner): its own
		// failure surfaces instead of the open error; otherwise the retry
		// result decides.
		if cfg != nil && cfg.NotFound != "" {
			if uerr := e.unionInvokeNotFound(def, cfg); uerr != nil {
				e.unionInvokeOpenClose(def, cfg, 1)
				return nil, uerr
			}
			fe, err = e.openUnionFileEngine(path)
		}
		if err != nil {
			e.unionInvokeOpenClose(def, cfg, 1)
			return nil, fmt.Errorf("unable to open database file")
		}
	}
	e.unionFileDBs[key] = &unionFileDBHandle{eng: fe, def: def, cfg: cfg}
	return fe, nil
}

// openUnionFileEngine opens one read-only source database file. C opens with
// SQLITE_OPEN_READONLY, so a MISSING file is an immediate error (pager.Open
// would create it; the os.Stat gate preserves that semantics).
func (e *Engine) openUnionFileEngine(path string) (*Engine, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	pg, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		return nil, err
	}
	return NewEngine(pg), nil
}

// unionFileSourceSchema validates one swarm file source (unionSourceToStr on
// an on-demand-opened source database): it must exist and be a rowid table;
// error display uses the BARE table name (C zDb is NULL for swarm sources).
func (e *Engine) unionFileSourceSchema(def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig) (string, []string, []string, int, error) {
	fe, err := e.unionFileDB(def.File, def, cfg)
	if err != nil {
		return "", nil, nil, -1, err
	}
	entry, err := fe.unionSourceEntry("", def.Table)
	if err != nil {
		return "", nil, nil, -1, err
	}
	fp, cols, types, pk := fe.unionFingerprint(entry)
	return fp, cols, types, pk, nil
}

// unionInvokeOpenClose runs the configured openclose UDF as
// SELECT "udf"(file[, context], bClose) and propagates its error
// (unionInvokeOpenClose: a failed statement is the caller's pzErr).
func (e *Engine) unionInvokeOpenClose(def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig, bClose int) error {
	if cfg == nil || cfg.OpenClose == "" {
		return nil
	}
	args := []string{def.File}
	if cfg.HasContext {
		args = append(args, def.Context)
	}
	args = append(args, strconv.Itoa(bClose))
	return e.unionExecUDF(cfg.OpenClose, args)
}

// unionInvokeNotFound runs the configured missing= UDF as
// SELECT "udf"(file[, context]); its error propagates (unionOpenDatabaseInner
// reports sqlite3_errmsg of the main connection after a failed UDF step).
func (e *Engine) unionInvokeNotFound(def vtab.UnionSourceDef, cfg *vtab.UnionSwarmConfig) error {
	if cfg == nil || cfg.NotFound == "" {
		return nil
	}
	args := []string{def.File}
	if cfg.HasContext {
		args = append(args, def.Context)
	}
	return e.unionExecUDF(cfg.NotFound, args)
}

// unionExecUDF evaluates SELECT "name"(args...) on this engine and returns
// the statement error, if any (C steps the prepared UDF statement and reads
// the error via sqlite3_reset/sqlite3_errmsg).
func (e *Engine) unionExecUDF(name string, args []string) error {
	call := `SELECT "` + name + `"(`
	for i, a := range args {
		if i > 0 {
			call += ", "
		}
		call += `'` + strings.ReplaceAll(a, "'", "''") + `'`
	}
	call += `)`
	stmts, err := parse.ParseSQL(call)
	if err != nil || len(stmts) == 0 {
		return fmt.Errorf("sql error: %s", err)
	}
	if res := e.Exec(stmts[0]); res.Error != nil {
		return res.Error
	}
	return nil
}

// closeUnionFileDBs closes every still-open swarm source database handle,
// firing the openclose UDF with bClose=1 first (unionDisconnect parity for
// engine shutdown).
func (e *Engine) closeUnionFileDBs() {
	for _, h := range e.unionFileDBs {
		e.unionInvokeOpenClose(h.def, h.cfg, 1)
		h.eng.Close()
	}
	e.unionFileDBs = make(map[unionFileKey]*unionFileDBHandle)
}
