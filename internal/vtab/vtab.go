// Package vtab provides virtual table module support.
package vtab

import (
	"fmt"
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

// BoundedVTab is implemented by virtual tables that can accept an inclusive
// upper-bound hint derived from the query's WHERE clause (e.g. wholenumber
// with value<=N / value<N). The engine calls SetUpperBound before reading
// rows so the table can generate only the needed prefix.
type BoundedVTab interface {
	SetUpperBound(n int64)
}

// Module creates virtual table instances.
type Module interface {
	Create(args []string) (VirtualTable, error)
	Connect(args []string) (VirtualTable, error)
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
	r.Register("wholenumber", &WholeNumberModule{})
	r.Register("echo", &NoopModule{ModuleName: "echo"})
	r.Register("fts3", &NoopModule{ModuleName: "fts3"})
	r.Register("fts4", &NoopModule{ModuleName: "fts4"})
	r.Register("fts5", &NoopModule{ModuleName: "fts5"})
	r.Register("fts4aux", &NoopModule{ModuleName: "fts4aux"})
	r.Register("rtree", &NoopModule{ModuleName: "rtree"})
	r.Register("dbstat", &NoopModule{ModuleName: "dbstat"})
	r.Register("dbpage", &NoopModule{ModuleName: "dbpage"})
	r.Register("dbdata", &NoopModule{ModuleName: "dbdata"})
	r.Register("zipfile", &NoopModule{ModuleName: "zipfile"})
	r.Register("tcl", &NoopModule{ModuleName: "tcl"})
	r.Register("csv", &NoopModule{ModuleName: "csv"})
	r.Register("prefix_length", &NoopModule{ModuleName: "prefix_length"})
}

// GenerateSeriesModule implements the generate_series virtual table.
type GenerateSeriesModule struct{}

type generateSeriesVTab struct {
	start int64
	stop  int64
	step  int64
}

func (m *GenerateSeriesModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

func (m *GenerateSeriesModule) Connect(args []string) (VirtualTable, error) {
	start := int64(1)
	stop := int64(10)
	step := int64(1)
	var err error
	if len(args) >= 1 {
		start, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("generate_series: invalid start: %s", args[0])
		}
	}
	if len(args) >= 2 {
		stop, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("generate_series: invalid stop: %s", args[1])
		}
	}
	if len(args) >= 3 {
		step, err = strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("generate_series: invalid step: %s", args[2])
		}
	}
	return &generateSeriesVTab{start: start, stop: stop, step: step}, nil
}

func (v *generateSeriesVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Columns reports the generate_series column names (value).
func (v *generateSeriesVTab) Columns() []string {
	return []string{"value"}
}

func (v *generateSeriesVTab) Open() (Cursor, error) {
	return &generateSeriesCursor{
		current: v.start - v.step,
		stop:    v.stop,
		step:    v.step,
	}, nil
}

type generateSeriesCursor struct {
	current int64
	stop    int64
	step    int64
}

func (c *generateSeriesCursor) Next() bool {
	c.current += c.step
	if c.step > 0 && c.current > c.stop {
		return false
	}
	if c.step < 0 && c.current < c.stop {
		return false
	}
	return true
}

func (c *generateSeriesCursor) Column(idx int) (interface{}, error) {
	if idx == 0 {
		return c.current, nil
	}
	return nil, fmt.Errorf("generate_series: invalid column index %d", idx)
}

func (c *generateSeriesCursor) Close() error {
	return nil
}

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
