package fts

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

// FTS4AuxModule implements the fts4aux virtual table module (ext/fts3/
// fts3_aux.c). A table declared `CREATE VIRTUAL TABLE terms USING
// fts4aux(fts_table)` exposes one row per (term, column) with the documents
// and occurrences counts from the FTS index, plus a per-term "*" row
// aggregating across all columns. The schema is
// `(term, col, documents, occurrences, languageid HIDDEN)`.
type FTS4AuxModule struct {
	// modules maps module name ("fts3", "fts4", "fts5") to the FTS module
	// whose Tables hold the in-memory indexes the aux table reads.
	modules map[string]*FTS3Module
}

// NewFTS4AuxModule creates an fts4aux module backed by the given FTS modules.
func NewFTS4AuxModule(modules map[string]*FTS3Module) *FTS4AuxModule {
	return &FTS4AuxModule{modules: modules}
}

type fts4auxVTab struct {
	module *FTS4AuxModule
	table  *FTS3Table
}

func (m *FTS4AuxModule) Create(args []string) (vtab.VirtualTable, error) {
	return m.Connect(args)
}

func (m *FTS4AuxModule) Connect(args []string) (vtab.VirtualTable, error) {
	// args: [0] is the module name placeholder; the FTS table name is the
	// last argument (fts3_aux.c: argv[3], or argv[4] with a schema prefix).
	// A nil/empty arg list happens when the query layer probes the module's
	// column names (vtabModuleColDefs); return an inert instance.
	if len(args) < 1 || strings.TrimSpace(args[len(args)-1]) == "" {
		return &fts4auxVTab{module: m}, nil
	}
	name := strings.TrimSpace(args[len(args)-1])
	name = strings.Trim(name, "'\"`")
	// The FTS table may live in any of the FTS modules.
	for _, mod := range m.modules {
		if mod == nil {
			continue
		}
		if t, ok := mod.GetTable(name); ok {
			return &fts4auxVTab{module: m, table: t}, nil
		}
	}
	return nil, fmt.Errorf("no such table: %s", name)
}

func (v *fts4auxVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Columns reports the fts4aux column names.
func (v *fts4auxVTab) Columns() []string {
	return []string{"term", "col", "documents", "occurrences", "languageid"}
}

// HiddenColumns reports that languageid is a HIDDEN vtab column (fts3_aux.c
// FTS4_AUX_SCHEMA: "term, col, documents, occurrences, languageid HIDDEN").
func (v *fts4auxVTab) HiddenColumns() map[int]bool {
	return map[int]bool{4: true}
}

func (v *fts4auxVTab) Open() (vtab.Cursor, error) {
	if v.table == nil {
		return &fts4auxCursor{}, nil
	}
	// fts3_aux.c reads EVERY term's postings via a full segment merge, so a
	// corrupt term (out-of-range column marker, unreadable doclist) fails
	// with FTS_CORRUPT_VTAB (fts3corrupt7 4.4).
	if v.table.HasOutOfRangePostings() || v.table.HasCorruptTerms() {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	return &fts4auxCursor{rows: v.table.AuxTerms()}, nil
}

type fts4auxCursor struct {
	rows []AuxTerm
	idx  int
}

func (c *fts4auxCursor) Next() bool {
	c.idx++
	return c.idx <= len(c.rows)
}

func (c *fts4auxCursor) Column(idx int) (interface{}, error) {
	if c.idx <= 0 || c.idx > len(c.rows) {
		return nil, nil
	}
	row := c.rows[c.idx-1]
	switch idx {
	case 0:
		return row.Term, nil
	case 1:
		// fts3_aux.c emits col as the 0-based column INDEX (integer); the
		// per-term aggregate row ("*") reports NULL.
		if row.Column == "*" {
			return nil, nil
		}
		n, err := strconv.Atoi(row.Column)
		if err != nil {
			return nil, fmt.Errorf("fts4aux: bad column %q", row.Column)
		}
		return int64(n), nil
	case 2:
		return row.Documents, nil
	case 3:
		return row.Occurrences, nil
	case 4:
		return int64(0), nil // languageid
	}
	return nil, fmt.Errorf("fts4aux: invalid column index %d", idx)
}

func (c *fts4auxCursor) Close() error { return nil }
