package vtab

import (
	"fmt"
	"strings"
)

// This file ports ext/misc/explain.c — the eponymous "explain" table-valued
// function returning one row per opcode of a statement's EXPLAIN program
// (schema "CREATE TABLE x(addr,opcode,p1,p2,p3,p4,p5,comment,sql HIDDEN)").
// xBestIndex hands a usable equality on the hidden `sql` column to xFilter
// (explain.c explainBestIndex: argvIndex=1, omit=1); xFilter wraps the text
// as "EXPLAIN %s", walks the program and stops when no TEXT binding arrived
// (explainFilter's non-TEXT argv[0] → rc=DONE → zero rows). The rowid is the
// instruction address (explainRowid).

// ExplainRowSource produces the EXPLAIN row stream for one SQL text. Each
// row holds the eight EXPLAIN result columns (addr, opcode, p1..p5, comment);
// the module appends the `sql` column value itself (explain.c xColumn returns
// the RAW argv[0] text, not the wrapped form).
type ExplainRowSource interface {
	ExplainRows(sqlText string) ([][]interface{}, error)
}

// ExplainModule provides the eponymous explain() TVF over an ExplainRowSource
// (explain.c registers an eponymous-only module: xCreate is absent, so CREATE
// VIRTUAL TABLE USING explain fails while FROM explain works).
type ExplainModule struct {
	src ExplainRowSource
}

// NewExplainModule builds the module over the engine-backed row source.
func NewExplainModule(src ExplainRowSource) *ExplainModule {
	return &ExplainModule{src: src}
}

// EponymousOnly implements EponymousOnlyModule: explain.c registers without
// xCreate.
func (m *ExplainModule) EponymousOnly() bool { return true }

// Create implements Module (never called for an eponymous-only module; the
// core rejects CREATE VIRTUAL TABLE USING explain).
func (m *ExplainModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

// Connect implements Module (explainConnect parity).
func (m *ExplainModule) Connect(args []string) (VirtualTable, error) {
	v := &explainVTab{mod: m}
	if len(args) > 1 {
		return nil, fmt.Errorf("too many arguments on explain() - max 1")
	}
	if len(args) == 1 {
		v.setSQL(args[0])
	}
	return v, nil
}

// CreateWithValues implements ValueModule: the TVF call site passes typed
// argument VALUES — a non-TEXT argument (integer/blob/null) renders zero rows
// (explainFilter's sqlite3_value_type(argv[0])!=SQLITE_TEXT → rc=DONE).
func (m *ExplainModule) CreateWithValues(args []interface{}) (VirtualTable, error) {
	return m.connectWithValues(args)
}

// ConnectWithValues implements ValueModule (xConnect-side typed arguments).
func (m *ExplainModule) ConnectWithValues(args []interface{}) (VirtualTable, error) {
	return m.connectWithValues(args)
}

func (m *ExplainModule) connectWithValues(args []interface{}) (VirtualTable, error) {
	v := &explainVTab{mod: m}
	if len(args) > 1 {
		return nil, fmt.Errorf("too many arguments on explain() - max 1")
	}
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			v.setSQL(s)
		}
	}
	return v, nil
}

// explainVTab is one explain() instance: the bound sql text ("" with
// sqlBound=false renders zero rows).
type explainVTab struct {
	mod      *ExplainModule
	sql      string
	sqlBound bool
}

// setSQL binds the sql text from a string-rendered argument.
func (v *explainVTab) setSQL(text string) {
	v.sql, v.sqlBound = text, true
}

// SetHiddenConstraint implements HiddenConstraintSetter: the WHERE-clause
// equality on the hidden `sql` column (explainBestIndex's usable-EQ →
// argvIndex=1). A non-TEXT binding renders zero rows.
func (v *explainVTab) SetHiddenConstraint(col string, val interface{}) error {
	if !strings.EqualFold(col, "sql") {
		return fmt.Errorf("no such column: %s", col)
	}
	if s, ok := val.(string); ok {
		v.setSQL(s)
		return nil
	}
	v.sql, v.sqlBound = "", false
	return nil
}

// Columns implements ColumnInfo (explain.c's declare_vtab schema).
func (v *explainVTab) Columns() []string {
	return []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment", "sql"}
}

// HiddenColumns implements HiddenColumnInfo: the `sql` column is HIDDEN.
func (v *explainVTab) HiddenColumns() map[int]bool {
	return map[int]bool{8: true}
}

// BestIndex implements VirtualTable (no index plan; the core re-verifies the
// hidden constraint per row like series.c with
// SQLITE_SERIES_CONSTRAINT_VERIFY=1).
func (v *explainVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Open implements VirtualTable (explainOpen + explainFilter parity): the row
// stream materializes here; a missing or failed binding renders zero rows.
func (v *explainVTab) Open() (Cursor, error) {
	c := &explainCursor{}
	if !v.sqlBound || v.sql == "" {
		return c, nil // explainFilter: non-TEXT argv[0] → rc=DONE
	}
	rows, err := v.mod.src.ExplainRows(v.sql)
	if err != nil {
		return nil, err
	}
	c.rows = make([][]interface{}, 0, len(rows))
	for _, r := range rows {
		row := make([]interface{}, 0, 9)
		row = append(row, r...)
		row = append(row, v.sql)
		c.rows = append(c.rows, row)
	}
	return c, nil
}

// explainCursor walks the materialized program rows (explainNext/explainEof).
type explainCursor struct {
	rows [][]interface{}
	pos  int
}

// Next advances the cursor; it reports false at the end of the stream.
func (c *explainCursor) Next() bool {
	c.pos++
	return c.pos < len(c.rows)
}

// Column returns one column of the current row (xColumn parity; the `sql`
// column returns the raw bound text).
func (c *explainCursor) Column(idx int) (interface{}, error) {
	if c.pos >= len(c.rows) || idx < 0 || idx >= len(c.rows[c.pos]) {
		return nil, fmt.Errorf("explain: no such column: %d", idx)
	}
	return c.rows[c.pos][idx], nil
}

// Close implements Cursor.
func (c *explainCursor) Close() error { return nil }

// Rowid returns the instruction address (explainRowid: column 0).
func (c *explainCursor) Rowid() int64 {
	if c.pos < len(c.rows) {
		if n, ok := c.rows[c.pos][0].(int64); ok {
			return n
		}
	}
	return int64(c.pos)
}
