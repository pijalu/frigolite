package fts

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pijalu/frigolite/internal/vtab"
)

// FTS3Table is an in-memory FTS3/4 virtual table data store.
type FTS3Table struct {
	mu          sync.Mutex
	name        string
	moduleName  string
	columnNames []string
	tokenizer   Tokenizer
	index       *InvertedIndex
}

// NewFTS3Table creates a new FTS3 table with the given configuration.
// hasFTSOptionPrefix reports whether an FTS module argument names an option
// (content=, prefix=, tokendata=, notindexed=, notreatas=, tokenize=) rather
// than a column. The parser may join option tokens with spaces, so match the
// name prefix followed by whitespace or '='.
func hasFTSOptionPrefix(upper string) bool {
	for _, name := range []string{"CONTENT", "PREFIX", "TOKENDATA", "NOTINDEXED", "NOTREATAS", "TOKENIZE", "TOKENIZER"} {
		if strings.HasPrefix(upper, name) {
			rest := strings.TrimSpace(upper[len(name):])
			if rest == "" || strings.HasPrefix(rest, "=") {
				return true
			}
		}
	}
	return false
}

func NewFTS3Table(name, moduleName string, args []string) (*FTS3Table, error) {
	t := &FTS3Table{
		name:       name,
		moduleName: moduleName,
		tokenizer:  &SimpleTokenizer{},
		index:      NewInvertedIndex(),
	}

	// Parse args to extract column names and tokenizer option
	var cols []string
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		upper := strings.ToUpper(arg)

		// Check for tokenizer option
		if strings.HasPrefix(upper, "TOKENIZE=") || strings.HasPrefix(upper, "TOKENIZER=") {
			tokenizerName := ""
			eqIdx := strings.Index(arg, "=")
			if eqIdx >= 0 {
				tokenizerName = strings.TrimSpace(arg[eqIdx+1:])
			}
			t.tokenizer = NewTokenizer(tokenizerName)
			continue
		}

		// Skip content=, prefix=, tokendata=, notindexed= etc options. The
		// parser joins vtabarg tokens with spaces (content='' becomes
		// "content = ''" with the empty literal trimmed), so match the option
		// name prefix rather than requiring "name=" exactly.
		if hasFTSOptionPrefix(upper) {
			continue
		}

		if arg == "" {
			continue
		}

		cols = append(cols, arg)
	}

	if len(cols) == 0 {
		cols = []string{"content"}
	}

	t.columnNames = cols
	return t, nil
}

// ColumnNames returns the table's column names.
func (t *FTS3Table) ColumnNames() []string {
	return t.columnNames
}

// Insert inserts a row and returns the rowid.
func (t *FTS3Table) Insert(values []interface{}) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	cols := make([]string, len(t.columnNames))
	for i := range t.columnNames {
		if i < len(values) && values[i] != nil {
			cols[i] = fmt.Sprintf("%v", values[i])
		}
	}
	return t.index.Insert(cols, t.tokenizer)
}

// InsertWithID inserts a row with a specific rowid.
func (t *FTS3Table) InsertWithID(rowid int64, values []interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cols := make([]string, len(t.columnNames))
	for i := range t.columnNames {
		if i < len(values) && values[i] != nil {
			cols[i] = fmt.Sprintf("%v", values[i])
		}
	}
	t.index.InsertWithID(rowid, cols, t.tokenizer)
}

// Delete removes a row.
func (t *FTS3Table) Delete(rowid int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.Delete(rowid)
}

// Update updates a row's content.
func (t *FTS3Table) Update(rowid int64, values []interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cols := make([]string, len(t.columnNames))
	for i := range t.columnNames {
		if i < len(values) && values[i] != nil {
			cols[i] = fmt.Sprintf("%v", values[i])
		}
	}
	t.index.Update(rowid, cols, t.tokenizer)
}

// AllRows returns all rows.
func (t *FTS3Table) AllRows() [][]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	docIDs := t.index.AllDocIDs()
	rows := make([][]interface{}, len(docIDs))
	for i, docID := range docIDs {
		doc := t.index.GetDoc(docID)
		if doc == nil {
			continue
		}
		row := make([]interface{}, len(t.columnNames))
		for j, col := range doc.Columns {
			row[j] = col
		}
		rows[i] = row
	}
	return rows
}

// AllRowsMap returns all rows with rowid as first element.
func (t *FTS3Table) AllRowsMap() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.AllDocIDs()
}

// GetDoc returns a document by docid.
func (t *FTS3Table) GetDoc(docID int64) *Document {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.GetDoc(docID)
}

// MatchDocIDs returns docids matching a MATCH query.
func (t *FTS3Table) MatchDocIDs(query string) ([]int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := ParseMatchQuery(query)
	if err != nil {
		return nil, err
	}

	node = ResolveQuery(node, t.columnNames)
	allIDs := t.index.AllDocIDs()
	var matched []int64
	for _, docID := range allIDs {
		if node.MatchDoc(t.index, docID) {
			matched = append(matched, docID)
		}
	}
	return matched, nil
}

// MatchQuery checks if a specific document matches the given MATCH query.
func (t *FTS3Table) MatchQuery(docID int64, query string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, err := ParseMatchQuery(query)
	if err != nil {
		return false, err
	}

	node = ResolveQuery(node, t.columnNames)
	return node.MatchDoc(t.index, docID), nil
}

// --- vtab interface implementation ---

// FTS3VTab implements vtab.VirtualTable for FTS3/4.
type FTS3VTab struct {
	module *FTS3Module
}

func (v *FTS3VTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

func (v *FTS3VTab) Open() (vtab.Cursor, error) {
	return &FTS3Cursor{}, nil
}

// FTS3Cursor implements vtab.Cursor (stateless placeholder).
type FTS3Cursor struct{}

func (c *FTS3Cursor) Next() bool                                 { return false }
func (c *FTS3Cursor) Column(idx int) (interface{}, error)       { return nil, nil }
func (c *FTS3Cursor) Close() error                               { return nil }

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
