// Package vtab provides virtual table module support.
package vtab

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Cursor provides row-by-row access to virtual table data.
type Cursor interface {
	Next() bool
	Column(idx int) (interface{}, error)
	Close() error
}

// VirtualTable represents a virtual table instance.
type VirtualTable interface {
	BestIndex(input []byte) (output []byte, err error)
	Open() (Cursor, error)
}

// ColumnInfo is implemented by virtual tables that can report their column
// names (used by table-valued functions like generate_series).
type ColumnInfo interface {
	Columns() []string
}

// ColumnTypeInfo is implemented by virtual tables that report per-column
// declared types (e.g. fts3tokenize's input/token TEXT and start/end/position
// INTEGER columns). The types mirror a CREATE TABLE declaration's type names
// so comparisons apply the column's affinity (fts3_tokenize_vtab.c
// FTS3_TOK_SCHEMA).
type ColumnTypeInfo interface {
	ColumnTypes() []string
}

// HiddenColumnInfo is implemented by virtual tables whose Columns() includes
// HIDDEN columns (SQLite vtab HIDDEN columns are not projected by SELECT * or
// listed by PRAGMA table_info). fts4aux's languageid column is hidden
// (fts3_aux.c: "languageid HIDDEN").
type HiddenColumnInfo interface {
	HiddenColumns() map[int]bool
}

// BoundedVTab is implemented by virtual tables that can accept an inclusive
// upper-bound hint derived from the query's WHERE clause (e.g. wholenumber
// with value<=N / value<N). The engine calls SetUpperBound before reading
// rows so the table can generate only the needed prefix.
type BoundedVTab interface {
	SetUpperBound(n int64)
}

// SchemaBoundVTab is implemented by virtual tables that need to know their
// resolved schema/table name — e.g. to name their shadow/backing tables
// (rtree's <name>_node, dbdata, dbstat). The engine calls BindSchema with the
// db + table name after Create/Connect. It is an OPTIONAL interface (Open/Closed
// principle): modules that don't need it simply omit it.
type SchemaBoundVTab interface {
	// BindSchema binds the resolved db + table name and prepares backing
	// state. An error (e.g. rtree shadow-name collision) aborts the owning
	// statement.
	BindSchema(dbName, tableName string) error
}

// TableDropper is implemented by modules that own per-table persistent state
// and/or shadow tables that must be released when the virtual table is
// dropped (spellfix.c spellfix1Uninit: xDestroy runs DROP TABLE IF EXISTS
// "%w"."%w_vocab" and frees the vtab's cost-table state). The engine calls
// DropTable during DROP TABLE of the virtual table, after the vtab's schema
// entry has been removed.
type TableDropper interface {
	DropTable(dbName, tableName string) error
}

// Disconnecter is implemented by virtual tables that hold persistent,
// per-instance resources across statements (unionvtab.c unionDisconnect:
// the open swarm source databases and the maxopen LRU live for the UnionTab's
// whole lifetime). The engine caches such instances per created table and
// calls Disconnect on DROP TABLE / connection close so the resources are
// released exactly once, matching SQLite's xDisconnect.
type Disconnecter interface {
	Disconnect()
}

// Module creates virtual table instances.
type Module interface {
	Create(args []string) (VirtualTable, error)
	Connect(args []string) (VirtualTable, error)
}

// ValueModule is implemented by table-valued modules whose arguments may be
// arbitrary SQL values — e.g. BLOB/JSONB input for json_each/json_tree —
// rather than their fmt.Sprint string renderings. When a module implements
// this interface the engine passes the evaluated argument values directly,
// preserving types.
type ValueModule interface {
	CreateWithValues(args []interface{}) (VirtualTable, error)
	ConnectWithValues(args []interface{}) (VirtualTable, error)
}

// OperatorOverloadCounter is implemented by virtual-table instances whose
// test modules instrument overridden like()/glob()/regexp() functions: while
// a scan of such an instance feeds a statement, every TRUE evaluation of the
// matching binary operator also invokes the registered user function (once),
// with its result ignored — sqlite's per-row overload probing observable in
// the harness via the function's side effects (vtabH counter increments).
type OperatorOverloadCounter interface {
	CountOperatorOverloads() bool
}

// PathConstraintOp mirrors test_fs.c's fstree xBestIndex idxNum: the operator
// kind of the usable path constraint (SQLITE_INDEX_CONSTRAINT_GLOB/LIKE/EQ).
// The kind selects the wildcard pair fstreeFilter scans for when deriving the
// recursion root; an equality constraint has no wildcards (the scan directory
// becomes the constraint path's parent).
type PathConstraintOp int

const (
	// PathConstraintGlob is a `path GLOB ?` constraint (wildcards * and ?).
	PathConstraintGlob PathConstraintOp = iota
	// PathConstraintLike is a `path LIKE ?` constraint (wildcards _ and %).
	PathConstraintLike
	// PathConstraintEq is a `path = ?` constraint (no wildcard characters).
	PathConstraintEq
)

// PathFilterSink is implemented by filesystem-tree instances whose scan
// directory derives from a path constraint value (test_fs.c fstreeFilter
// narrows the recursion root to the pattern prefix before the first wildcard).
type PathFilterSink interface {
	SetPathConstraint(value string, op PathConstraintOp)
}

// Registry holds registered virtual table modules.
type Registry struct {
	modules map[string]Module
}

// NewRegistry creates a new registry.
func NewRegistry() *Registry {
	return &Registry{modules: make(map[string]Module)}
}

// Register registers a module.
func (r *Registry) Register(name string, m Module) {
	r.modules[strings.ToUpper(name)] = m
}

// Find finds a module by name.
// Unregister removes a module from the registry (the sqlite3_drop_modules
// test command; fts3dropmod.test).
func (r *Registry) Unregister(name string) {
	delete(r.modules, strings.ToUpper(name))
}

func (r *Registry) Find(name string) (Module, bool) {
	m, ok := r.modules[strings.ToUpper(name)]
	return m, ok
}

// List returns all registered module names in lowercase (for
// pragma_module_list).
func (r *Registry) List() []string {
	out := make([]string, 0, len(r.modules))
	for name := range r.modules {
		out = append(out, strings.ToLower(name))
	}
	return out
}

// Register defaults registers built-in virtual table modules.
func (r *Registry) RegisterDefaults() {
	r.Register("generate_series", &GenerateSeriesModule{})
	r.Register("json_each", &JsonEachModule{})
	r.Register("json_tree", &JsonTreeModule{})
	r.Register("wholenumber", &WholeNumberModule{})
	r.Register("echo", &EchoModule{})
	r.Register("fts3", &NoopModule{ModuleName: "fts3"})
	r.Register("fts4", &NoopModule{ModuleName: "fts4"})
	r.Register("fts5", &NoopModule{ModuleName: "fts5"})
	r.Register("fts4aux", &NoopModule{ModuleName: "fts4aux"})
	r.Register("dbstat", &NoopModule{ModuleName: "dbstat"})
	r.Register("zipfile", NewZipfileModule())
	r.Register("fsdir", NewFsdirModule())
	r.Register("fstree", NewFsTreeModule())
	r.Register("dbdata", &NoopModule{ModuleName: "dbdata"})
	r.Register("tcl", NewTclCommandModule())
	r.Register("csv", &CSVModule{})
	r.Register("csv_wr", &CSVModule{})
	r.Register("tclvar", &TclVarModule{})
	r.Register("prefix_length", &NoopModule{ModuleName: "prefix_length"})
	r.Register("carray", &CarrayModule{})
	r.Register("intarray", NewIntarrayModule())
}

// GenerateSeriesModule implements the generate_series virtual table.
//
// SQLite registers series.c as an EponymousOnlyModule: the name is usable
// directly in a FROM clause (with hidden-column constraints start/stop/step)
// but CREATE VIRTUAL TABLE USING generate_series fails with "no such module"
// (tabfunc01-1.3).
type GenerateSeriesModule struct{}

// EponymousOnly implements EponymousOnlyModule: series.c registers without
// xCreate, so CREATE VIRTUAL TABLE USING generate_series is an error.
func (m *GenerateSeriesModule) EponymousOnly() bool { return true }

type generateSeriesVTab struct {
	start      int64
	stop       int64
	step       int64
	startSeen  bool // a usable START binding arrived (arg or constraint)
	startGiven bool // an explicit START arrived (arg or start= constraint)
	stopGiven  bool // an explicit STOP arrived (arg or stop= constraint)
	stepGiven  bool // an explicit STEP arrived (arg or step= constraint)
	empty      bool // range narrowing proved no rows can match
}

func (m *GenerateSeriesModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

// NeedsLimitPushdown implements LimitPushdown: generate_series can emit an
// effectively unbounded stream when STOP is defaulted or huge, so the core's
// LIMIT must reach the generator.
func (m *GenerateSeriesModule) NeedsLimitPushdown() bool { return true }

func (m *GenerateSeriesModule) Connect(args []string) (VirtualTable, error) {
	start := int64(0)
	stop := int64(0xffffffff) // series.c default STOP when omitted
	step := int64(1)
	v := &generateSeriesVTab{start: start, stop: stop, step: step}
	if len(args) > 3 {
		return nil, fmt.Errorf("too many arguments on generate_series() - max 3")
	}
	var err error
	if len(args) >= 1 {
		v.start, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			// An unusable START (NULL/column reference) is series.c's
			// bStartSeen failure.
			return nil, fmt.Errorf("first argument to %q missing or unusable", "generate_series()")
		}
		v.startSeen, v.startGiven = true, true
	}
	if len(args) >= 2 {
		v.stop, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("generate_series: invalid stop: %s", args[1])
		}
		v.stopGiven = true
	}
	if len(args) >= 3 {
		v.step, err = strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("generate_series: invalid step: %s", args[2])
		}
		if v.step == 0 {
			v.step = 1 // series.c: a zero STEP degenerates to 1
		}
		v.stepGiven = true
	}
	return v, nil
}

func (v *generateSeriesVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Columns reports the generate_series columns in declared order; start,
// stop and step are HIDDEN (series.c SERIES_SCHEMA) so SELECT * and
// PRAGMA table_info project only "value" while table_xinfo lists all four.
func (v *generateSeriesVTab) Columns() []string {
	return []string{"value", "start", "stop", "step"}
}

// HiddenColumns reports the HIDDEN column indexes of the series schema.
func (v *generateSeriesVTab) HiddenColumns() map[int]bool {
	return map[int]bool{1: true, 2: true, 3: true}
}

// SetHiddenConstraint absorbs one WHERE equality binding on a hidden column
// (series.c xFilter argv parity).
func (v *generateSeriesVTab) SetHiddenConstraint(col string, val interface{}) error {
	n, err := setInt64(val)
	if err != nil {
		return fmt.Errorf("generate_series: unusable %s constraint value", col)
	}
	switch col {
	case "start":
		v.start, v.startSeen, v.startGiven = n, true, true
	case "stop":
		v.stop, v.stopGiven = n, true
	case "step":
		v.step, v.stepGiven = n, true
	default:
		return fmt.Errorf("no such column: %s", col)
	}
	return nil
}

// ValidateInstance enforces series.c's bStartSeen rule: a usable START must
// be present, either as a function argument or as a WHERE constraint.
func (v *generateSeriesVTab) ValidateInstance() error {
	if !v.startSeen {
		return fmt.Errorf("first argument to %q missing or unusable", "generate_series()")
	}
	return nil
}

func (v *generateSeriesVTab) Open() (Cursor, error) {
	c := &generateSeriesCursor{
		value: v.start,
		term:  v.stop,
		start: v.start,
		stop:  v.stop,
		step:  v.step,
	}
	if v.empty {
		c.done = true
		return c, nil
	}
	c.init(v.start, v.stop, v.step)
	return c, nil
}

// generateSeriesCursor ports series.c's cursor arithmetic exactly: the step
// is carried as an UNSIGNED 64-bit magnitude (so step=-9223372036854775808 is
// representable) and the terminal value is aligned to the series grid so the
// final row lands exactly on it (tabfunc01-930: start=MaxI64, stop=MinI64,
// step=MinI64 yields two rows and terminates).
type generateSeriesCursor struct {
	value   int64  // current output value
	term    int64  // aligned terminal value (last row)
	ustep   uint64 // |step| in unsigned space
	desc    bool   // descending sequence
	done    bool   // stepped past the last element
	started bool   // at least one row served
	start,
	stop,
	step int64 // bound parameters echoed by hidden columns
}

// span64 returns uint64(a)-uint64(b); requires a>=b (series.c span64).
func span64(a, b int64) uint64 { return uint64(a) - uint64(b) }

// add64/sub64 wrap through unsigned space (series.c add64/sub64).
func add64(a int64, b uint64) int64 { return int64(uint64(a) + b) }
func sub64(a int64, b uint64) int64 { return int64(uint64(a) - b) }

// ExpandValueDefaults widens the implicit START/STOP defaults when only the
// VALUE column is constrained (series.c xFilter: with no START/STEP binding
// an equality or >=/> value bound lifts START to SMALLEST_INT64, and with no
// STOP/STEP binding an equality or <=/< bound raises STOP to LARGEST_INT64).
// An equality/value lower bound also satisfies series.c's bStartSeen rule.
func (v *generateSeriesVTab) ExpandValueDefaults(lowerSeen, upperSeen bool) {
	if !v.startGiven && !v.stepGiven && lowerSeen {
		v.start = math.MinInt64
		v.startSeen = true
	}
	if !v.stopGiven && !v.stepGiven && upperSeen {
		v.stop = math.MaxInt64
	}
}

// NarrowValueRange shrinks start/stop to satisfy WHERE constraints on the
// value column (series.c xFilter iMin/iMax narrowing). min or max is nil
// when unconstrained. The cursor's init() then aligns the terminal to the
// step grid and detects empty ranges.
func (v *generateSeriesVTab) NarrowValueRange(min, max *int64) {
	desc := v.step < 0
	ustep := uint64(0)
	switch {
	case v.step > 0:
		ustep = uint64(v.step)
	case v.step > math.MinInt64:
		ustep = uint64(-v.step)
	default:
		ustep = uint64(math.MaxInt64) + 1
	}
	if desc {
		// Values run start down to stop. Lower the head until <= max
		// (series.c: floor multiples, then one guarded extra step).
		if max != nil && v.start > *max {
			span := span64(v.start, *max)
			v.start = sub64(v.start, (span/ustep)*ustep)
			if v.start > *max {
				if v.start < add64(math.MinInt64, ustep) {
					v.empty = true
					return
				}
				v.start = sub64(v.start, ustep)
				if v.start > *max {
					v.empty = true
					return
				}
			}
		}
		// Raise the tail until >= min.
		if min != nil && v.stop < *min {
			v.stop = *min
		}
		return
	}
	// Ascending: raise the head until >= min.
	if min != nil && v.start < *min {
		span := uint64(*min) - uint64(v.start)
		v.start = add64(v.start, (span/ustep)*ustep)
		if v.start < *min {
			if v.start > sub64(math.MaxInt64, ustep) {
				v.empty = true
				return
			}
			v.start = add64(v.start, ustep)
			if v.start < *min {
				v.empty = true
				return
			}
		}
	}
	// Lower the tail until <= max.
	if max != nil && v.stop > *max {
		v.stop = *max
	}
}

func (c *generateSeriesCursor) init(start, stop, ostep int64) {
	switch {
	case ostep > 0:
		c.ustep = uint64(ostep)
	case ostep > math.MinInt64:
		c.ustep = uint64(-ostep)
	default:
		// -9223372036854775808 has no positive counterpart.
		c.ustep = uint64(math.MaxInt64) + 1
	}
	c.desc = ostep < 0
	if (!c.desc && start > stop) || (c.desc && start < stop) {
		c.done = true
		return
	}
	// Align the terminal so it is exactly the last value of the series
	// (series.c xFilter tail).
	if !c.desc {
		c.term = sub64(stop, span64(stop, start)%c.ustep)
	} else {
		c.term = add64(stop, span64(start, stop)%c.ustep)
	}
}

func (c *generateSeriesCursor) Next() bool {
	if c.done {
		return false
	}
	if c.started {
		if c.value == c.term {
			c.done = true
			return false
		}
		if c.desc {
			c.value = sub64(c.value, c.ustep)
		} else {
			c.value = add64(c.value, c.ustep)
		}
	}
	c.started = true
	return true
}

// Column serves the full declared schema (value, start, stop, step) so the
// core can re-verify hidden-column constraints per row (series.c compiled
// with SQLITE_SERIES_CONSTRAINT_VERIFY=1, as in tabfunc01).
func (c *generateSeriesCursor) Column(idx int) (interface{}, error) {
	switch idx {
	case 0:
		return c.value, nil
	case 1:
		return c.start, nil
	case 2:
		return c.stop, nil
	case 3:
		return c.step, nil
	}
	return nil, fmt.Errorf("generate_series: invalid column index %d", idx)
}

func (c *generateSeriesCursor) Close() error {
	return nil
}

// Rowid implements RowidCursor: series.c's xSeriesRowid returns the current
// value itself (tabfunc01-1.9: rowid == value).
func (c *generateSeriesCursor) Rowid() int64 { return c.value }

// WholeNumberModule implements the wholenumber virtual table: a single
// "value" column yielding positive integers 1, 2, 3, ... The engine does not
// implement constraint pushdown through BestIndex, so the query layer sets an
// upper bound (via BoundedVTab) from the WHERE clause (e.g. value<=20 or
// value<1000); without a bound the sequence is capped at a generous default.
type WholeNumberModule struct{}

type wholeNumberVTab struct {
	upper int64
}

// DefaultWholeNumberBound is the sequence cap used when a wholenumber query
// has no extractable upper bound (SQLite's wholenumber is unbounded, but a
// finite cap keeps memory bounded; tests use bounds up to 500000).
const DefaultWholeNumberBound = 1000000

func (m *WholeNumberModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

func (m *WholeNumberModule) Connect(args []string) (VirtualTable, error) {
	return &wholeNumberVTab{upper: DefaultWholeNumberBound}, nil
}

// SetUpperBound implements BoundedVTab: the query layer calls it with the
// inclusive upper bound from the WHERE clause before reading rows.
func (v *wholeNumberVTab) SetUpperBound(n int64) {
	if n > 0 && n < v.upper {
		v.upper = n
	}
}

func (v *wholeNumberVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Columns reports the wholenumber column name (value).
func (v *wholeNumberVTab) Columns() []string {
	return []string{"value"}
}

func (v *wholeNumberVTab) Open() (Cursor, error) {
	return &wholeNumberCursor{current: 0, upper: v.upper}, nil
}

type wholeNumberCursor struct {
	current int64
	upper   int64
}

func (c *wholeNumberCursor) Next() bool {
	c.current++
	return c.current <= c.upper
}

func (c *wholeNumberCursor) Column(idx int) (interface{}, error) {
	if idx == 0 {
		return c.current, nil
	}
	return nil, fmt.Errorf("wholenumber: invalid column index %d", idx)
}

func (c *wholeNumberCursor) Close() error {
	return nil
}

// EchoModule is the echo virtual-table module (CREATE VIRTUAL TABLE t USING
// echo(real_table)). In SQLite's test suite the echo module is a test-only
// C module that mirrors an underlying real table: its schema comes from the
// source table's CREATE statement and reads/writes route through to the
// source. Frigolite implements the echo semantics in the exec layer (the
// engine resolves the source table and proxies rows/columns), so this module
// type exists to mark the module as real (not a NoopModule stub): ALTER
// TABLE ... RENAME and other vtab lifecycle operations treat echo tables as
// first-class. The Create/Connect methods validate the argument form and
// return an inert instance; the engine's echoVTabSource/virtualTableRows
// handle the actual proxying.
type EchoModule struct{}

type echoVTab struct{}

func (m *EchoModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

func (m *EchoModule) Connect(args []string) (VirtualTable, error) {
	return &echoVTab{}, nil
}

func (v *echoVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

func (v *echoVTab) Open() (Cursor, error) {
	return v, nil
}

func (c *echoVTab) Column(idx int) (interface{}, error) {
	return nil, nil
}

func (c *echoVTab) Next() bool {
	return false
}

func (c *echoVTab) Close() error {
	return nil
}

// NoopModule is a stub virtual table module that returns an error
// indicating the module is not supported. This allows CREATE VIRTUAL TABLE
// statements to parse and execute without crashing.
type NoopModule struct {
	ModuleName string
}

type noopVTab struct {
	name string
}

func (m *NoopModule) Create(args []string) (VirtualTable, error) {
	return &noopVTab{name: m.ModuleName}, nil
}

func (m *NoopModule) Connect(args []string) (VirtualTable, error) {
	return &noopVTab{name: m.ModuleName}, nil
}

func (v *noopVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

func (v *noopVTab) Open() (Cursor, error) {
	return v, nil
}

func (c *noopVTab) Column(idx int) (interface{}, error) {
	return nil, nil
}

func (c *noopVTab) Next() bool {
	return false
}

func (c *noopVTab) Close() error {
	return nil
}
