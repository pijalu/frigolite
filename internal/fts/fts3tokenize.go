package fts

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

// FTS3TokenizeModule implements the fts3tokenize virtual table module
// (ext/fts3/fts3_tokenize_vtab.c). A table declared
// `CREATE VIRTUAL TABLE t USING fts3tokenize(tokenizer[, args...])` exposes
// the schema (input, token, start, end, position): querying with
// `WHERE input = <string>` tokenizes the string with the configured tokenizer
// and returns one row per token.
type FTS3TokenizeModule struct{}

// NewFTS3TokenizeModule creates an fts3tokenize module.
func NewFTS3TokenizeModule() *FTS3TokenizeModule {
	return &FTS3TokenizeModule{}
}

type fts3tokenizeVTab struct {
	tokenizer Tokenizer
	input     string
	hasInput  bool
}

// InputConstrainedVTab is implemented by virtual tables that need a literal
// equality constraint on their first column (fts3tokenize's `input = <string>`)
// before rows can be produced. The engine extracts the constant RHS and passes
// it via SetInputConstraint before Open.
type InputConstrainedVTab interface {
	SetInputConstraint(value string)
}

func (m *FTS3TokenizeModule) Create(args []string) (vtab.VirtualTable, error) {
	return m.Connect(args)
}

func (m *FTS3TokenizeModule) Connect(args []string) (vtab.VirtualTable, error) {
	// args are the module arguments after the module name: fts3tokenize() →
	// simple; fts3tokenize(simple) → simple; fts3tokenize(name, a, b) → the
	// named tokenizer with extra args (fts3_tokenize_vtab.c
	// fts3tokConnectMethod: azDequote[0] is the tokenizer name, the rest are
	// passed to its xCreate).
	spec := "simple"
	var extra []string
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		spec = strings.Trim(args[0], "'\"`")
		extra = args[1:]
	}
	// The args may come from parseVTabSQL (re-parsing the stored CREATE SQL),
	// which preserves the SQL quoting ('', 'xyz '); dequote each arg so the
	// delimiter string is the literal text (fts3_tokenize_vtab.c
	// fts3tokDequoteArray).
	for i := range extra {
		extra[i] = strings.Trim(strings.TrimSpace(extra[i]), "'\"`")
	}
	tok, err := NewTokenizerFromSpec(spec, extra)
	if err != nil {
		return nil, err
	}
	return &fts3tokenizeVTab{tokenizer: tok}, nil
}

func (v *fts3tokenizeVTab) SetInputConstraint(value string) {
	v.input = value
	v.hasInput = true
}

func (v *fts3tokenizeVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Columns reports the fts3tokenize column names
// (fts3_tokenize_vtab.c FTS3_TOK_SCHEMA).
func (v *fts3tokenizeVTab) Columns() []string {
	return []string{"input", "token", "start", "end", "position"}
}

// ColumnTypes reports the declared types of the fts3tokenize columns
// (fts3_tokenize_vtab.c FTS3_TOK_SCHEMA "CREATE TABLE x(input, token, start,
// end, position)"): input/token are TEXT, the offsets are INTEGER. The types
// give the columns TEXT/INTEGER affinity so `input = 123` compares numerically
// (fts3tok1 1.9).
func (v *fts3tokenizeVTab) ColumnTypes() []string {
	return []string{"TEXT", "TEXT", "INTEGER", "INTEGER", "INTEGER"}
}

func (v *fts3tokenizeVTab) Open() (vtab.Cursor, error) {
	if !v.hasInput {
		// Querying the fts3tokenize vtab without an `input = <string>`
		// constraint is an error (fts3_tokenize_vtab.c fts3tokFilterMethod:
		// SQLITE_ERROR when idxNum!=1 — "SQL logic error"; fts3tok1 2.1).
		return nil, fmt.Errorf("SQL logic error")
	}
	tokens := v.tokenizer.TokenizeOffsets(v.input)
	rows := make([]tokenizeRow, 0, len(tokens))
	for i, tk := range tokens {
		rows = append(rows, tokenizeRow{
			input: v.input, token: tk.Term, start: tk.Start, end: tk.End, pos: i,
		})
	}
	return &fts3tokenizeCursor{rows: rows}, nil
}

// tokenizeRow is one output row of the fts3tokenize virtual table.
type tokenizeRow struct {
	input string
	token string
	start int
	end   int
	pos   int
}

type fts3tokenizeCursor struct {
	rows []tokenizeRow
	idx  int
}

func (c *fts3tokenizeCursor) Next() bool {
	c.idx++
	return c.idx <= len(c.rows)
}

func (c *fts3tokenizeCursor) Column(idx int) (interface{}, error) {
	if c.idx <= 0 || c.idx > len(c.rows) {
		return nil, nil
	}
	row := c.rows[c.idx-1]
	switch idx {
	case 0:
		return row.input, nil
	case 1:
		return row.token, nil
	case 2:
		return int64(row.start), nil
	case 3:
		return int64(row.end), nil
	case 4:
		return int64(row.pos), nil
	}
	return nil, fmt.Errorf("fts3tokenize: invalid column index %d", idx)
}

func (c *fts3tokenizeCursor) Close() error { return nil }
