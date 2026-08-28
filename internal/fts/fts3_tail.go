package fts

import (
	"sync"

	"github.com/pijalu/frigolite/internal/vtab"
)

func (c *FTS3Cursor) Next() bool                          { return false }
func (c *FTS3Cursor) Column(idx int) (interface{}, error) { return nil, nil }
func (c *FTS3Cursor) Close() error                        { return nil }

// FTS3Module implements vtab.Module for FTS3/4.
// It stores FTS tables in-memory, indexed by table name.
type FTS3Module struct {
	ModuleName string
	Tables     map[string]*FTS3Table
	mu         sync.Mutex
}

// NewFTS3Module creates a new FTS3Module.
func NewFTS3Module(moduleName string) *FTS3Module {
	return &FTS3Module{
		ModuleName: moduleName,
		Tables:     make(map[string]*FTS3Table),
	}
}

// Create implements vtab.Module.Create.
func (m *FTS3Module) Create(args []string) (vtab.VirtualTable, error) {
	return &FTS3VTab{module: m}, nil
}

// Connect implements vtab.Module.Connect.
func (m *FTS3Module) Connect(args []string) (vtab.VirtualTable, error) {
	return &FTS3VTab{module: m}, nil
}

// GetOrCreateTable gets or creates an FTS3Table for the given name.
func (m *FTS3Module) GetOrCreateTable(name, moduleName string, args []string) (*FTS3Table, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if t, ok := m.Tables[name]; ok {
		return t, nil
	}

	t, err := NewFTS3Table(name, moduleName, args)
	if err != nil {
		return nil, err
	}
	m.Tables[name] = t
	return t, nil
}

// GetTable returns an existing FTS3Table by name.
func (m *FTS3Module) GetTable(name string) (*FTS3Table, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.Tables[name]
	return t, ok
}

// DropTable removes an FTS3Table.
func (m *FTS3Module) DropTable(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Tables, name)
}

// RenameTable re-keys the FTS3Table from oldName to newName after an
// ALTER TABLE RENAME (SQLite's fts3RenameMethod). The table keeps its
// inverted index contents; only the lookup key and the internal name
// change.
func (m *FTS3Module) RenameTable(oldName, newName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.Tables[oldName]
	if !ok {
		return
	}
	delete(m.Tables, oldName)
	t.name = newName
	m.Tables[newName] = t
}

// FindFTSModule checks if a module name corresponds to an FTS module.
func FindFTSModule(r *vtab.Registry, moduleName string) *FTS3Module {
	m, ok := r.Find(moduleName)
	if !ok {
		return nil
	}
	// Check if it's an FTS3Module by trying the Tables field
	if ftsMod, isFTS := m.(*FTS3Module); isFTS {
		return ftsMod
	}
	return nil
}
