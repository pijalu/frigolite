package fts5

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

// This file ports the fts5vocab virtual table module (ext/fts5/fts5_vocab.c):
// direct read access to an existing fts5 table's index through three table
// shapes —
//
//	col:      CREATE TABLE vocab(term, col, doc, cnt)
//	row:      CREATE TABLE vocab(term, doc, cnt)
//	instance: CREATE TABLE vocab(term, doc, col, offset)
//
// The module resolves the named fts5 table through the registered fts5
// module's live instances (C walks a MATCH '*id' probe and a cursor-id
// lookup), so uncommitted in-session index state is visible, and a target
// that exists but is not an fts5 table reports C's generic failure. Zero
// counts render as SQL NULL (fts5VocabColumnMethod leaves the result unset
// for iVal==0), as does the 'col' name of a detail=none table and the
// 'offset' of anything coarser than detail=full.

// VocabResolver resolves the named table in the named database for the
// vocab module: it returns the live fts5 table, the module name of the
// schema entry ("" when the name resolves to nothing), and whether any
// schema entry exists.
type VocabResolver func(dbName, tbl string) (t *Table, module string, found bool)

// VocabModule implements vtab.Module for fts5vocab.
type VocabModule struct {
	resolve VocabResolver
}

// NewVocabModule creates the fts5vocab module bound to a table resolver.
func NewVocabModule(resolve VocabResolver) *VocabModule {
	return &VocabModule{resolve: resolve}
}

// vocab type codes (FTS5_VOCAB_COL/ROW/INSTANCE).
type vocabType int

const (
	vocabTypeCol vocabType = iota
	vocabTypeRow
	vocabTypeInstance
)

// vocabSchemas are the declared shapes (fts5_vocab.c azSchema).
var vocabSchemas = map[vocabType][]string{
	vocabTypeCol:      {"term", "col", "doc", "cnt"},
	vocabTypeRow:      {"term", "doc", "cnt"},
	vocabTypeInstance: {"term", "doc", "col", "offset"},
}

// vocabTable is one fts5vocab virtual-table instance
// (fts5_vocab.c Fts5VocabTable).
type vocabTable struct {
	mod      *VocabModule
	zDb      string
	zTbl     string
	eType    vocabType
	hasDbArg bool // the three-argument (db, table, type) form
	dbBound  bool
}

// dequoteVocabArg removes one level of SQL quoting ('...', "...", `...`,
// [...] with doubled-quote escapes — fts5_test_tok.c fts5tokDequote's rule).
func dequoteVocabArg(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	qc := q
	switch q {
	case '\'', '"', '`':
	case '[':
		qc = ']'
	default:
		return s
	}
	if s[len(s)-1] != qc {
		return s
	}
	inner := s[1 : len(s)-1]
	return strings.ReplaceAll(inner, string(q)+string(q), string(q))
}

func (m *VocabModule) Create(args []string) (vtab.VirtualTable, error) {
	return m.Connect(args)
}

// Connect parses the (fts5-table, type) or (db, fts5-table, type) argument
// forms (fts5VocabInitVtab). Argument-count and type validation run at
// connect time like C's; the target fts5 table itself resolves at scan time.
func (m *VocabModule) Connect(args []string) (vtab.VirtualTable, error) {
	if len(args) != 2 && len(args) != 3 {
		return nil, fmt.Errorf("wrong number of vtable arguments")
	}
	zDb := ""
	hasDbArg := false
	if len(args) == 3 {
		zDb = dequoteVocabArg(args[0])
		hasDbArg = true
	}
	zTbl := dequoteVocabArg(args[len(args)-2])
	typeArg := dequoteVocabArg(args[len(args)-1])
	var eType vocabType
	switch strings.ToLower(typeArg) {
	case "col":
		eType = vocabTypeCol
	case "row":
		eType = vocabTypeRow
	case "instance":
		eType = vocabTypeInstance
	default:
		return nil, fmt.Errorf("fts5vocab: unknown table type: '%s'", typeArg)
	}
	return &vocabTable{mod: m, zDb: zDb, zTbl: zTbl, eType: eType, hasDbArg: hasDbArg}, nil
}

// BindSchema captures the schema the vocab table lives in: the two-argument
// form reads the fts5 table from the same schema (fts5VocabInitVtab's
// argv[1]); the three-argument form is the temp-schema shape and fails
// anywhere else (bDb requires argv[1]=="temp").
func (v *vocabTable) BindSchema(dbName, tableName string) error {
	if v.hasDbArg {
		if !strings.EqualFold(v.zDb, "temp") || !strings.EqualFold(dbName, "temp") {
			return fmt.Errorf("wrong number of vtable arguments")
		}
	} else {
		v.zDb = dbName
	}
	v.dbBound = true
	return nil
}

func (v *vocabTable) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Columns reports the declared shape.
func (v *vocabTable) Columns() []string { return vocabSchemas[v.eType] }

// resolveTarget resolves the fts5 table at scan time (fts5VocabOpenMethod's
// cursor-id lookup): a missing or non-fts5 target fails like C.
func (v *vocabTable) resolveTarget() (*Table, error) {
	t, module, found := v.mod.resolve(v.zDb, v.zTbl)
	if found && module == "fts5vocab" {
		// A recursive definition (fts5vocab over an fts5vocab table): C's
		// cursor-id probe resolves to a non-fts5 vtab and fails generically.
		return nil, fmt.Errorf("SQL logic error")
	}
	if t == nil {
		return nil, fmt.Errorf("no such fts5 table: %s.%s", v.zDb, v.zTbl)
	}
	return t, nil
}

// Open builds the cursor: the terms are served from the live inverted index.
func (v *vocabTable) Open() (vtab.Cursor, error) {
	t, err := v.resolveTarget()
	if err != nil {
		return nil, err
	}
	// Structure integrity check (fts5VocabNextMethod's
	// sqlite3Fts5StructureTest): the engine's structure record is the fixed
	// seven-byte empty-structure seed; any other content is uninterpretable.
	rows, err := t.db.ExecSQL(fmt.Sprintf(
		"SELECT block FROM %s WHERE id=10", qual(t.dbName, t.cfg.Name+"_data")))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	if raw, ok := toBytes(rows[0][0]); !ok || len(raw) != 7 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	return newVocabCursor(v, t), nil
}

// vocabRow is one output row of the vocab scan.
type vocabRow struct {
	term   string
	name   interface{} // 'col' column (column name or NULL)
	doc    interface{} // 'doc'/'rowid' column
	offset interface{} // 'offset' column
}

// vocabCursor walks the index in C's output order: term ascending; row mode
// emits one row per term; col mode one row per (term, column); instance mode
// one row per (term, rowid[, column[, offset]]).
type vocabCursor struct {
	tab  *vocabTable
	t    *Table
	rows []vocabRow
	idx  int
}

// newVocabCursor materializes the whole scan (fts5VocabFilterMethod +
// fts5VocabNextMethod walking the term iterator).
func newVocabCursor(v *vocabTable, t *Table) *vocabCursor {
	c := &vocabCursor{tab: v, t: t}
	for _, term := range sortedIndexTerms(t) {
		c.addTermRows(term, t.ix.postings[term])
	}
	return c
}

// sortedIndexTerms returns the index's terms in ascending order (the vocab
// scan's sorted term iterator).
func sortedIndexTerms(t *Table) []string {
	terms := make([]string, 0, len(t.ix.postings))
	for term := range t.ix.postings {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	return terms
}

// addTermRows appends the vocab rows for one term and its postings
// (fts5VocabNextMethod's per-term emission, one shape per table type).
func (c *vocabCursor) addTermRows(term string, docs map[int64]*docPostings) {
	switch c.tab.eType {
	case vocabTypeRow:
		c.rows = append(c.rows, vocabTermRows(term, docs, c.t.cfg.Detail)...)
	case vocabTypeCol:
		c.rows = append(c.rows, vocabColRows(c.t, term, docs)...)
	case vocabTypeInstance:
		c.rows = append(c.rows, vocabInstanceRows(c.t, term, docs)...)
	}
}

// postingRowids returns a posting map's rowids in ascending order.
func postingRowids(docs map[int64]*docPostings) []int64 {
	rowids := make([]int64, 0, len(docs))
	for rowid := range docs {
		rowids = append(rowids, rowid)
	}
	sortRowids(rowids)
	return rowids
}

// vocabTermRows builds the row-mode rows for one term: doc counts rows, cnt
// totals positions under detail=full (fts5VocabNextMethod's row branch).
func vocabTermRows(term string, docs map[int64]*docPostings, detail DetailMode) []vocabRow {
	nDoc := int64(0)
	nCnt := int64(0)
	for _, rowid := range postingRowids(docs) {
		nDoc++
		if detail == DetailFull {
			for _, pos := range docs[rowid].cols {
				nCnt += int64(len(pos))
			}
		}
	}
	return []vocabRow{{term: term, doc: nDoc, offset: nCnt}}
}

// vocabColRows builds the col-mode rows for one term (fts5VocabNextMethod's
// col branch): one row per (term, column) with per-column doc/instance
// counts; detail=none collapses to a single row with the row count.
func vocabColRows(t *Table, term string, docs map[int64]*docPostings) []vocabRow {
	rowids := postingRowids(docs)
	if t.cfg.Detail == DetailNone {
		// detail=none counts rows per term in the first column slot
		// only, and the col name renders as NULL.
		return []vocabRow{{term: term, doc: int64(len(rowids))}}
	}
	aDoc, aCnt := colPostingsCounts(docs, rowids, t.cfg.Detail)
	var rows []vocabRow
	for _, col := range sortedKeys(aDoc) {
		rows = append(rows, vocabRow{
			term: term, name: columnName(t, col), doc: aDoc[col], offset: aCnt[col],
		})
	}
	return rows
}

// colPostingsCounts accumulates per-column doc and instance counts over a
// term's rowids; empty position lists are skipped (fts5VocabNextMethod).
func colPostingsCounts(docs map[int64]*docPostings, rowids []int64, detail DetailMode) (map[int]int64, map[int]int64) {
	aDoc := make(map[int]int64)
	aCnt := make(map[int]int64)
	for _, rowid := range rowids {
		for col, pos := range docs[rowid].cols {
			if len(pos) == 0 {
				continue
			}
			aDoc[col]++
			if detail == DetailFull {
				aCnt[col] += int64(len(pos))
			} else {
				aCnt[col] = 0
			}
		}
	}
	return aDoc, aCnt
}

// vocabInstanceRows builds the instance-mode rows for one term
// (fts5VocabNextMethod's instance branch): one row per (term, rowid) under
// detail=none, per (term, rowid, column) under detail=columns, and per
// (term, rowid, column, offset) under detail=full.
func vocabInstanceRows(t *Table, term string, docs map[int64]*docPostings) []vocabRow {
	var rows []vocabRow
	for _, rowid := range postingRowids(docs) {
		if t.cfg.Detail == DetailNone {
			rows = append(rows, vocabRow{term: term, doc: rowid})
			continue
		}
		for _, col := range sortedIntKeys(docs[rowid].cols) {
			rows = append(rows, vocabInstanceColRows(t, term, rowid, col, docs[rowid].cols[col])...)
		}
	}
	return rows
}

// vocabInstanceColRows builds one column's instance rows for (term, rowid).
func vocabInstanceColRows(t *Table, term string, rowid int64, col int, pos []int) []vocabRow {
	if t.cfg.Detail == DetailColumns {
		return []vocabRow{{term: term, doc: rowid, name: columnName(t, col)}}
	}
	rows := make([]vocabRow, 0, len(pos))
	for _, off := range pos {
		rows = append(rows, vocabRow{term: term, doc: rowid, name: columnName(t, col), offset: int64(off)})
	}
	return rows
}

// columnName renders the 'col' value: the column's name, or NULL beyond the
// declared columns.
func columnName(t *Table, col int) interface{} {
	if col < len(t.cfg.Columns) {
		return t.cfg.Columns[col]
	}
	return nil
}

// sortedKeys returns a count map's keys in ascending order.
func sortedKeys(m map[int]int64) []int {
	return sortedIntKeys(m)
}

// sortedIntKeys returns an int-keyed map's keys in ascending order.
func sortedIntKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// normalize zeroes stored zero counts into NULLs (fts5VocabColumnMethod's
// "if(iVal>0)" result rule).
func (r *vocabRow) normalize() {
	if n, ok := r.doc.(int64); ok && n == 0 {
		r.doc = nil
	}
	if n, ok := r.offset.(int64); ok && n == 0 {
		r.offset = nil
	}
}

func (c *vocabCursor) Next() bool {
	c.idx++
	return c.idx <= len(c.rows)
}

func (c *vocabCursor) Column(idx int) (interface{}, error) {
	if c.idx <= 0 || c.idx > len(c.rows) {
		return nil, fmt.Errorf("fts5vocab: no current row")
	}
	row := c.rows[c.idx-1]
	// Zero counts render as NULL (fts5VocabColumnMethod's "if(iVal>0)" rule).
	// The instance mode renders doc/offset directly (an offset of 0 is a
	// value, not an unset count).
	if c.tab.eType != vocabTypeInstance {
		row.normalize()
	}
	fields := vocabRowFields[c.tab.eType]
	if idx >= len(fields) {
		return nil, fmt.Errorf("fts5vocab: invalid column index %d", idx)
	}
	return vocabRowField(&row, fields[idx]), nil
}

// vocabRowField reads one field of a row by its slot letter.
func vocabRowField(row *vocabRow, field byte) interface{} {
	switch field {
	case 't':
		return row.term
	case 'n':
		return row.name
	case 'd':
		return row.doc
	default:
		return row.offset
	}
}

// vocabRowFields maps each table type's columns to row slots: t=term, n=col
// name, d=doc, o=offset (fts5_vocab.c's aCol declarations).
var vocabRowFields = map[vocabType][]byte{
	vocabTypeRow:      {'t', 'd', 'o'},
	vocabTypeCol:      {'t', 'n', 'd', 'o'},
	vocabTypeInstance: {'t', 'd', 'n', 'o'},
}

// Rowid implements vtab.RowidCursor: a 1-based row counter
// (fts5VocabRowidMethod's pCsr->rowid).
func (c *vocabCursor) Rowid() int64 { return int64(c.idx) }

func (c *vocabCursor) Close() error { return nil }
