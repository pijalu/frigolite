package fts

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

// FTS4TermModule implements the fts4term virtual table module (ext/fts3/
// fts3_term.c, a test-only module that provides direct access to the full-text
// index of an FTS table). A table declared
// `CREATE VIRTUAL TABLE p USING fts4term(fts_table[, iIndex])` exposes one row
// per (term, docid, col, pos) posting of the FTS table's iIndex-th index
// (0 = the main index; iIndex >= 1 = prefix index with length
// prefixLengths[iIndex-1]). The schema is `(term, docid, col, pos)`.
type FTS4TermModule struct {
	// modules maps module name ("fts3", "fts4", "fts5") to the FTS module
	// whose Tables hold the in-memory indexes the term table reads.
	modules map[string]*FTS3Module
}

// NewFTS4TermModule creates an fts4term module backed by the given FTS modules.
func NewFTS4TermModule(modules map[string]*FTS3Module) *FTS4TermModule {
	return &FTS4TermModule{modules: modules}
}

type fts4termVTab struct {
	module *FTS4TermModule
	table  *FTS3Table
	iIndex int
}

func (m *FTS4TermModule) Create(args []string) (vtab.VirtualTable, error) {
	return m.Connect(args)
}

func (m *FTS4TermModule) Connect(args []string) (vtab.VirtualTable, error) {
	// args are the module arguments: fts4term(tbl) or fts4term(tbl, iIndex).
	// A nil/empty arg list happens when the query layer probes the module's
	// column names (vtabModuleColDefs); return an inert instance.
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return &fts4termVTab{module: m}, nil
	}
	name := strings.Trim(args[0], "'\"`")
	iIndex := 0
	if len(args) >= 2 {
		// fts3_term.c: atoi(argv[4]) when argc==5 (an index argument is
		// present). A non-numeric value yields 0, matching atoi.
		iIndex, _ = strconv.Atoi(strings.TrimSpace(args[1]))
	}
	var table *FTS3Table
	for _, mod := range m.modules {
		if mod == nil {
			continue
		}
		if t, ok := mod.GetTable(name); ok {
			table = t
			break
		}
	}
	if table == nil {
		return nil, fmt.Errorf("no such table: %s", name)
	}
	return &fts4termVTab{module: m, table: table, iIndex: iIndex}, nil
}

func (v *fts4termVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Columns reports the fts4term column names (fts3_term.c FTS3_TERMS_SCHEMA).
func (v *fts4termVTab) Columns() []string {
	return []string{"term", "docid", "col", "pos"}
}

func (v *fts4termVTab) Open() (vtab.Cursor, error) {
	if v.table == nil {
		return &fts4termCursor{}, nil
	}
	return &fts4termCursor{rows: v.table.IndexPostings(v.iIndex)}, nil
}

type fts4termCursor struct {
	rows []IndexPosting
	idx  int
}

func (c *fts4termCursor) Next() bool {
	c.idx++
	return c.idx <= len(c.rows)
}

func (c *fts4termCursor) Column(idx int) (interface{}, error) {
	if c.idx <= 0 || c.idx > len(c.rows) {
		return nil, nil
	}
	row := c.rows[c.idx-1]
	switch idx {
	case 0:
		return row.Term, nil
	case 1:
		return row.DocID, nil
	case 2:
		return int64(row.Col), nil
	case 3:
		return int64(row.Pos), nil
	}
	return nil, fmt.Errorf("fts4term: invalid column index %d", idx)
}

func (c *fts4termCursor) Close() error { return nil }
