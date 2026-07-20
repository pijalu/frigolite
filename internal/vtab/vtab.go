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

// RegisterDefaults registers built-in virtual table modules.
func (r *Registry) RegisterDefaults() {
	r.Register("generate_series", &GenerateSeriesModule{})
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
	started bool
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
