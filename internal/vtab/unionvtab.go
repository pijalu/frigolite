package vtab

import (
	"fmt"
	"strings"
)

// DefaultSwarmMaxOpen mirrors SWARMVTAB_MAX_OPEN (unionvtab.c): the default
// swarmvtab limit on concurrently open source database files.
const DefaultSwarmMaxOpen = 9

// UnionSourceDef describes one unionvtab source range: a rowid table in a
// database schema (unionvtab) or a database file (swarmvtab) contributing
// rows with rowid in [Min, Max]. Context carries the optional 5th source
// column (unionConnect's bHasContext rows).
type UnionSourceDef struct {
	Schema  string // schema name (unionvtab; empty = default resolution)
	File    string // database file path (swarmvtab)
	Table   string // source table name
	Min     int64  // inclusive rowid range lower bound
	Max     int64  // inclusive rowid range upper bound
	Context string // 5th column value when present (swarmvtab)
}

// UnionSwarmParam is one bound source-statement parameter (swarmvtab
// ':name=value' aux option, applied in option order).
type UnionSwarmParam struct {
	Name  string // parameter name including the leading ':'
	Value string // text value (C binds with sqlite3_bind_text)
}

// UnionSwarmConfig carries the unionConfigureVtab option results to the
// engine-side source resolver and file-database accessors.
type UnionSwarmConfig struct {
	MaxOpen    int               // maxopen= limit (default DefaultSwarmMaxOpen)
	NotFound   string            // missing= UDF name ("" = none)
	OpenClose  string            // openclose= UDF name ("" = none)
	Params     []UnionSwarmParam // ':name=value' bindings in option order
	HasContext bool              // source statement returns a 5th (context) column
}

// UnionVtabSource is implemented by the engine to resolve unionvtab source
// specifications, validate source tables and read their rows (unionvtab.c
// keeps database handles; this port routes through the engine instead).
type UnionVtabSource interface {
	// UnionResolveSources parses a source specification — SQL text
	// ("VALUES(...)" / a SELECT returning db,tbl,min,max[,context] rows) or
	// a bare source-table name — into definitions ordered by Min (C: the
	// argument statement runs "SELECT * FROM (arg) ORDER BY 3"). Swarm
	// :param bindings from cfg are applied before evaluation and the first
	// source row's File/Context fields are populated for swarm sources.
	UnionResolveSources(query string, cfg *UnionSwarmConfig) ([]UnionSourceDef, error)
	// UnionPrepareSources validates the source statement's SYNTAX without
	// evaluating it (unionConnect prepares "SELECT * FROM (%z) ORDER BY 3"
	// BEFORE unionConfigureVtab parses the aux options, so a statement
	// syntax error surfaces first, wrapped "sql error: %s" by unionPrepare).
	// Unbound :params are accepted (C binds them later, at option parse).
	UnionPrepareSources(query string) error
	// UnionSourceSchema validates one source — it must exist and be an
	// intkey rowid table (unionIsIntkeyTable) — returning its schema
	// fingerprint (unionSourceToStr: pragma_table_info names+types), the
	// declared column names/types and the 0-based index of the declared
	// INTEGER-PK column (pTab->iPK, -1 when none). Swarm file sources open
	// their database on first touch (missing= retry, openclose UDF).
	UnionSourceSchema(def UnionSourceDef, cfg *UnionSwarmConfig) (fp string, cols, types []string, pkCol int, err error)
	// UnionReadRows returns one source's rows in rowid order (declared
	// columns only) with the parallel rowid list (C: SELECT rowid, *).
	UnionReadRows(def UnionSourceDef, cfg *UnionSwarmConfig) (rows [][]interface{}, rowids []int64, err error)
	// UnionReleaseFile drops a swarm file source's open database handle
	// (LRU eviction and scan end; fires the openclose UDF with bClose=1).
	UnionReleaseFile(def UnionSourceDef, cfg *UnionSwarmConfig)
	// UnionFunctionExists reports whether a SQL function of that name is
	// registered — the missing=/openclose= UDF statements are prepared at
	// option-parse time, so an unknown function fails right there
	// ("sql error: no such function: x").
	UnionFunctionExists(name string) bool
}

// UnionVtabModule implements the unionvtab contract: a TEMP-only virtual
// table that unions the contents of several rowid tables with disjoint
// rowid ranges. The same C module backs "unionvtab" (schema-qualified
// sources) and "swarmvtab" (file-backed sources plus aux options); the
// instance name is carried so error texts use the connected name
// (unionConnect's zVtab).
type UnionVtabModule struct {
	src   UnionVtabSource
	name  string
	swarm bool
}

// NewUnionVtabModule binds an engine-side source resolver for unionvtab.
func NewUnionVtabModule(src UnionVtabSource) *UnionVtabModule {
	return &UnionVtabModule{src: src, name: "unionvtab"}
}

// NewSwarmVtabModule binds an engine-side source resolver for swarmvtab
// (accepts aux options and file-backed sources).
func NewSwarmVtabModule(src UnionVtabSource) *UnionVtabModule {
	return &UnionVtabModule{src: src, name: "swarmvtab", swarm: true}
}

// ModuleName reports the connected module name ("unionvtab" or "swarmvtab")
// for the TEMP-schema error wording.
func (m *UnionVtabModule) ModuleName() string { return m.name }

// Create implements Module (unionConnect).
func (m *UnionVtabModule) Create(args []string) (VirtualTable, error) {
	// unionConnect arity: unionvtab takes exactly one module argument;
	// swarmvtab takes one plus any aux options.
	if m.src == nil || len(args) < 1 || (len(args) != 1 && !m.swarm) {
		return nil, fmt.Errorf("wrong number of arguments for %s", m.name)
	}
	cfg := &UnionSwarmConfig{MaxOpen: DefaultSwarmMaxOpen}
	// unionConnect PREPARES the source statement before parsing the aux
	// options (unionPreparePrintf, then unionConfigureVtab), so a statement
	// syntax error surfaces first — even when an option names an unknown
	// function (swarmvtab.test 2.4/2.5).
	if err := m.src.UnionPrepareSources(unquoteVtabArg(strings.TrimSpace(args[0]))); err != nil {
		return nil, err
	}
	if m.swarm {
		if err := m.configureSwarm(args[1:], cfg); err != nil {
			return nil, err
		}
	}
	defs, err := m.src.UnionResolveSources(unquoteVtabArg(strings.TrimSpace(args[0])), cfg)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("no source tables configured")
	}
	// Range checks while stepping the (Min-sorted) rows: a source with
	// Max<Min, or overlapping its predecessor, is a configuration error.
	for i := range defs {
		if defs[i].Max < defs[i].Min || (i > 0 && defs[i].Min <= defs[i-1].Max) {
			return nil, fmt.Errorf("rowid range mismatch error")
		}
	}
	if m.swarm {
		// Swarm sources carry the FILE name in column 0 (C: pSrc->zFile);
		// zDb stays NULL, so all later schema lookups and error displays use
		// the bare table name against the opened file's main database.
		for i := range defs {
			defs[i].File = defs[i].Schema
			defs[i].Schema = ""
		}
		return m.createSwarm(defs, cfg)
	}
	return m.createUnion(defs, cfg)
}

// createUnion validates every source table exists with an identical schema
// (unionSourceCheck) and declares the vtab from source 0.
func (m *UnionVtabModule) createUnion(defs []UnionSourceDef, cfg *UnionSwarmConfig) (VirtualTable, error) {
	fp0, cols, types, pk, err := m.src.UnionSourceSchema(defs[0], cfg)
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(defs); i++ {
		fp, _, _, _, err := m.src.UnionSourceSchema(defs[i], cfg)
		if err != nil {
			return nil, err
		}
		// sqlite3_stricmp comparison of the pragma_table_info fingerprints.
		if !strings.EqualFold(fp, fp0) {
			return nil, fmt.Errorf("source table schema mismatch")
		}
	}
	v := &unionVTab{module: m, cols: cols, types: types, pkCol: pk, sources: defs, selected: allSourceIndices(len(defs)), cfg: cfg}
	v.opened = make([]bool, len(defs))
	v.nUser = make([]int, len(defs))
	return v, nil
}

// createSwarm opens the first source database now (unionOpenDatabase(0));
// its schema fixes the fingerprint every later open must match. Remaining
// sources open on demand during scans. Failure after a successful open
// closes the handle again and fires openclose(1) (unionOpenDatabase's else
// branch).
func (m *UnionVtabModule) createSwarm(defs []UnionSourceDef, cfg *UnionSwarmConfig) (VirtualTable, error) {
	fp, cols, types, pk, err := m.src.UnionSourceSchema(defs[0], cfg)
	if err != nil {
		m.src.UnionReleaseFile(defs[0], cfg)
		return nil, err
	}
	v := &unionVTab{
		module: m, cols: cols, types: types, pkCol: pk, sources: defs,
		selected: allSourceIndices(len(defs)),
		cfg:      cfg, sourceStr: fp, nOpen: 1, closable: []int{0},
		opened: make([]bool, len(defs)),
		nUser:  make([]int, len(defs)),
	}
	v.opened[0] = true
	return v, nil
}

// Connect implements Module.
func (m *UnionVtabModule) Connect(args []string) (VirtualTable, error) {
	return m.Create(args)
}

// TempSchemaOnly marks the module as creatable only in the TEMP schema
// (unionvtab.c rejects other schemas).
func (m *UnionVtabModule) TempSchemaOnly() bool { return true }

// unionVTab is one bound instance over resolved source tables.
type unionVTab struct {
	module  *UnionVtabModule
	cols    []string
	types   []string
	pkCol   int // declared INTEGER-PK column (pTab->iPK, -1 = none)
	sources []UnionSourceDef
	// selected holds SOURCES-ARRAY indices in scan order (unionOpenDatabase
	// takes iSrc as an aSrc[] index; the swarm LRU closable list speaks the
	// same index space — a filtered copy of the defs would desync the two).
	selected []int
	lo, hi   *int64 // effective inclusive interval from WHERE (nil = open)
	cfg      *UnionSwarmConfig
	// Swarm open-file LRU state (unionvtab.c). opened[i] mirrors pSrc->db!=0;
	// nUser[i] mirrors the per-source scan refcount. closable mirrors
	// pClosable: it holds ONLY idle (nUser==0) sources, most recent FIRST
	// (C prepends on open/finalize and evicts from the tail). sourceStr is
	// the first-ever schema fingerprint (zSourceStr).
	sourceStr string
	nOpen     int
	closable  []int
	opened    []bool
	nUser     []int
}

// BestIndex accepts the default full-scan plan.
func (v *unionVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open concatenates every selected source table's rows in source order (the
// ranges are disjoint and Min-sorted at configure time, matching C scan
// order). Swarm sources open their database files on demand and release
// them per the maxopen LRU.
func (v *unionVTab) Open() (Cursor, error) {
	if v.module.swarm {
		return v.openSwarm()
	}
	var all [][]interface{}
	var rowids []int64
	for _, i := range v.selected {
		s := v.sources[i]
		rows, ids, err := v.module.src.UnionReadRows(s, v.cfg)
		if err != nil {
			return nil, err
		}
		// unionvtab.c trusts each source's DECLARED range: a source fully
		// inside the constraint interval is scanned whole; a partially
		// covered one contributes only the rows inside the interval
		// (unionvtab.test 3.4.x/3.5 counts).
		fully := (v.lo == nil || s.Min >= *v.lo) && (v.hi == nil || s.Max <= *v.hi)
		if fully {
			all = append(all, rows...)
			rowids = append(rowids, ids...)
			continue
		}
		for i, id := range ids {
			if v.lo != nil && id < *v.lo {
				continue
			}
			if v.hi != nil && id > *v.hi {
				continue
			}
			all = append(all, rows[i])
			rowids = append(rowids, id)
		}
	}
	return &unionRowidCursor{sliceCursor: &sliceCursor{rows: all}, rowids: rowids}, nil
}

// WithoutRowidVTab: union tables behave as ordinary rowid tables (rowid ==
// the source INTEGER PRIMARY KEY, or the source's implicit rowid).
func (v *unionVTab) WithoutRowid() bool { return false }

// Disconnect implements Disconnecter (unionDisconnect): DROP TABLE / engine
// shutdown closes every still-open swarm source database, firing the
// openclose UDF with bClose=1 for each, then resets the LRU state so a
// recreated table of the same name starts fresh. It is a no-op for the
// non-file unionvtab form (no per-source file handles).
func (v *unionVTab) Disconnect() {
	if !v.module.swarm {
		return
	}
	for i := range v.sources {
		if v.opened[i] {
			v.module.src.UnionReleaseFile(v.sources[i], v.cfg)
			v.opened[i] = false
		}
	}
	v.nOpen = 0
	v.closable = nil
	v.nUser = make([]int, len(v.sources))
}

// unionRowidCursor exposes each row's source-table rowid (unionRowid reads
// column 0 of the per-source "SELECT rowid, *" statement) while Column(i)
// returns declared column i (unionColumn reads column i+1).
type unionRowidCursor struct {
	*sliceCursor
	rowids []int64
}

// Rowid implements RowidCursor: the source table's rowid.
func (c *unionRowidCursor) Rowid() int64 {
	if c.idx < 0 || c.idx >= len(c.rowids) {
		return 0
	}
	return c.rowids[c.idx]
}

// Column returns declared column idx. UnionReadRows already stripped the
// internal rowid column (it lives in the parallel rowids list), so no index
// shift is needed here.
func (c *unionRowidCursor) Column(idx int) (interface{}, error) {
	rows := c.rows
	if c.idx < 0 || c.idx >= len(rows) || idx >= len(rows[c.idx]) {
		return nil, fmt.Errorf("no row")
	}
	return rows[c.idx][idx], nil
}

// Columns implements ColumnInfo for the created-instance defs path.
func (v *unionVTab) Columns() []string { return v.cols }

// RowidColumn implements RowidColumner: the declared INTEGER-PK column acts
// as the rowid (pTab->iPK); -1 when the sources have no explicit IPK.
func (v *unionVTab) RowidColumn() int { return v.pkCol }

// ColumnTypes implements ColumnTypeInfo: declared source-table types.
func (v *unionVTab) ColumnTypes() []string { return v.types }

// TempSchemaOnlyMarker marks modules creatable only in the TEMP schema
// (checked by the DDL executor before source resolution).
type TempSchemaOnly interface {
	TempSchemaOnly() bool
}

// ModuleNamer is implemented by modules whose error texts name the module
// (unionConnect's zVtab is "unionvtab" or "swarmvtab").
type ModuleNamer interface {
	ModuleName() string
}

// RowidColumner is implemented by virtual tables whose rowid is ALSO a
// declared column: unionvtab keeps the source's INTEGER-PK column index
// (pTab->iPK) and xBestIndex treats constraints on that column exactly like
// rowid constraints. RowidColumn returns the 0-based declared-column index
// or -1 when the rowid is not a declared column.
type RowidColumner interface {
	RowidColumn() int
}

// RowidRangeConsumer is implemented by unionVTab so the engine can hand in
// the WHERE-clause rowid interval before rows are generated. unionvtab.c
// uses the interval to pick WHICH source tables to scan (each chosen table
// is then fully scanned and the core re-applies the constraint as a filter).
type RowidRangeConsumer interface {
	ConsumeRowidRange(lo *int64, loIncl bool, hi *int64, hiIncl bool)
}

// ConsumeRowidRange implements RowidRangeConsumer: record the interval and
// select the sources whose [min,max] intersects it.
func (v *unionVTab) ConsumeRowidRange(lo *int64, loIncl bool, hi *int64, hiIncl bool) {
	v.lo, v.hi = lo, hi
	v.selected = v.selectSources(lo, loIncl, hi, hiIncl)
}

// allSourceIndices returns 0..n-1 (an unconstrained scan covers every source).
func allSourceIndices(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// selectSources returns the SOURCES-ARRAY indices whose [min,max] interval
// intersects the consumed rowid range, in Min-sorted scan order (sources are
// Min-sorted at configure time via ORDER BY 3).
func (v *unionVTab) selectSources(lo *int64, loIncl bool, hi *int64, hiIncl bool) []int {
	selected := allSourceIndices(len(v.sources))
	if lo != nil || hi != nil {
		out := make([]int, 0, len(v.sources))
		for i, s := range v.sources {
			if lo != nil {
				if loIncl && s.Max < *lo {
					continue
				}
				if !loIncl && s.Max <= *lo {
					continue
				}
			}
			if hi != nil {
				if hiIncl && s.Min > *hi {
					continue
				}
				if !hiIncl && s.Min >= *hi {
					continue
				}
			}
			out = append(out, i)
		}
		selected = out
	}
	return selected
}

// swarmEnsureOpen opens source i's file when it is not already open
// (unionOpenDatabase): evict idle LRU victims down to MaxOpen-1 first
// (unionCloseSources(nMaxOpen-1)), then open, then push the source at the
// HEAD of the closable list. An already-open source is a no-op, exactly
// like C (pSrc->db!=0 skips the whole body — no list churn).
func (v *unionVTab) swarmEnsureOpen(i int) error {
	if v.opened[i] {
		return nil
	}
	v.swarmCloseLRU(v.cfg.MaxOpen - 1)
	def := v.sources[i]
	fp, _, _, _, err := v.module.src.UnionSourceSchema(def, v.cfg)
	if err != nil {
		// Opened-but-invalid (wrong schema): close + openclose(1), like the
		// else branch of unionOpenDatabase. A failed OPEN releases nothing
		// (UnionReleaseFile is a no-op then).
		v.module.src.UnionReleaseFile(def, v.cfg)
		return err
	}
	if v.sourceStr == "" {
		v.sourceStr = fp
	} else if !strings.EqualFold(fp, v.sourceStr) {
		return fmt.Errorf("source table schema mismatch")
	}
	v.opened[i] = true
	v.nOpen++
	v.closable = append([]int{i}, v.closable...)
	return nil
}

// swarmIncrRef mirrors unionIncrRefcount: a source about to be scanned is
// unlinked from the closable list when its refcount was zero (it is no
// longer idle), then its refcount is bumped.
func (v *unionVTab) swarmIncrRef(i int) {
	if v.nUser[i] == 0 {
		for j, idx := range v.closable {
			if idx == i {
				v.closable = append(v.closable[:j], v.closable[j+1:]...)
				break
			}
		}
	}
	v.nUser[i]++
}

// swarmDecrRef mirrors the swarm half of unionFinalizeCsrStmt: when the scan
// of a source ends its refcount drops; on reaching zero the source becomes
// idle again and returns to the HEAD of the closable list, after which idle
// files beyond MaxOpen are closed (unionCloseSources(nMaxOpen)).
func (v *unionVTab) swarmDecrRef(i int) {
	v.nUser[i]--
	if v.nUser[i] == 0 {
		v.closable = append([]int{i}, v.closable...)
		v.swarmCloseLRU(v.cfg.MaxOpen)
	}
}

// swarmCloseLRU closes idle open files until at most nMax remain
// (unionCloseSources): eviction pops the TAIL of the closable list (the
// least recently used idle source) and fires openclose(bClose=1) through
// UnionReleaseFile. In-use sources (nUser>0) are never on the list, so a
// source can never be closed out from under its own scan.
func (v *unionVTab) swarmCloseLRU(nMax int) {
	for len(v.closable) > 0 && v.nOpen > nMax {
		oldest := v.closable[len(v.closable)-1]
		v.closable = v.closable[:len(v.closable)-1]
		v.opened[oldest] = false
		v.nOpen--
		v.module.src.UnionReleaseFile(v.sources[oldest], v.cfg)
	}
}

// openSwarm scans the selected sources in order, opening each file on
// demand (first matching source first) and releasing per the maxopen LRU
// when a source is exhausted (doUnionNext → unionFinalizeCsrStmt).
func (v *unionVTab) openSwarm() (Cursor, error) {
	var all [][]interface{}
	var rowids []int64
	for _, i := range v.selected {
		s := v.sources[i]
		if v.lo != nil && s.Max < *v.lo {
			continue
		}
		if v.hi != nil && s.Min > *v.hi {
			continue
		}
		if err := v.swarmEnsureOpen(i); err != nil {
			return nil, err
		}
		// Scan in progress: hold a reference so the LRU cannot evict this
		// source from under its own read (unionIncrRefcount).
		v.swarmIncrRef(i)
		rows, ids, err := v.module.src.UnionReadRows(s, v.cfg)
		if err != nil {
			v.swarmDecrRef(i)
			return nil, err
		}
		for j, id := range ids {
			if v.lo != nil && id < *v.lo {
				continue
			}
			if v.hi != nil && id > *v.hi {
				continue
			}
			all = append(all, rows[j])
			rowids = append(rowids, id)
		}
		// Source exhausted: drop the reference; it returns to the idle
		// (closable) pool and the maxopen LRU closes overflow
		// (unionFinalizeCsrStmt → unionCloseSources(nMaxOpen)).
		v.swarmDecrRef(i)
	}
	return &unionRowidCursor{sliceCursor: &sliceCursor{rows: all}, rowids: rowids}, nil
}
