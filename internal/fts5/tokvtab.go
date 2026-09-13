package fts5

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/vtab"
)

// This file ports the fts5tokenize virtual table module (ext/fts5/
// fts5_test_tok.c, compiled under SQLITE_TEST only; the engine registers it
// unconditionally — a documented deviation). A table declared
//
//	CREATE VIRTUAL TABLE t USING fts5tokenize(tokenizer[, args...])
//
// exposes CREATE TABLE x(input HIDDEN, token, start, end, position): a query
// with `input = <text>` (or the table-valued form FROM t(<text>)) tokenizes
// the text through the configured fts5 tokenizer and returns one row per
// token. With no tokenizer argument the default (unicode61) applies. An
// unknown tokenizer fails the CREATE with the generic constructor text
// (fts5tokConnectMethod's message-free SQLITE_ERROR), and a scan without an
// input binding fails with C's generic SQLITE_ERROR text.

// TokenizeModule implements vtab.Module for fts5tokenize.
type TokenizeModule struct{}

// NewTokenizeModule creates the fts5tokenize module.
func NewTokenizeModule() *TokenizeModule { return &TokenizeModule{} }

// tokenizeVTab is the fts5tokenize virtual-table instance
// (fts5_test_tok.c Fts5tokTable).
type tokenizeVTab struct {
	tok        Tokenizer
	createErr  error // deferred xCreate failure, surfaced at BindSchema
	input      string
	hasInput   bool
	inputGiven bool // an input binding arrived (even an empty one)
}

func (m *TokenizeModule) Create(args []string) (vtab.VirtualTable, error) {
	return m.Connect(args)
}

// Connect resolves the tokenizer (fts5tokConnectMethod): the first argument
// names it, the rest go to its constructor. With no argument C's tokenizer
// lookup on an empty name falls back to the default (unicode61).
func (m *TokenizeModule) Connect(args []string) (vtab.VirtualTable, error) {
	v := &tokenizeVTab{}
	var spec []string
	if len(args) > 0 {
		name := dequoteVocabArg(strings.TrimSpace(args[0]))
		if name != "" {
			spec = append(spec, name)
			for _, a := range args[1:] {
				spec = append(spec, dequoteVocabArg(strings.TrimSpace(a)))
			}
		}
	}
	if len(spec) > 0 {
		tok, err := NewTokenizer(spec)
		if err != nil {
			// C's xFindTokenizer/xCreate failure carries no message; the
			// core surfaces it as "vtable constructor failed: <name>".
			v.createErr = fmt.Errorf("vtable constructor failed")
			return v, nil
		}
		v.tok = tok
		return v, nil
	}
	tok, err := NewTokenizer([]string{"unicode61"})
	if err != nil {
		v.createErr = fmt.Errorf("vtable constructor failed")
		return v, nil
	}
	v.tok = tok
	return v, nil
}

// BindSchema surfaces the deferred constructor failure with the resolved
// table name (sqlite3's vtab constructor wrapping).
func (v *tokenizeVTab) BindSchema(dbName, tableName string) error {
	if v.createErr != nil {
		return fmt.Errorf("vtable constructor failed: %s", tableName)
	}
	return nil
}

// SetInputConstraint absorbs the `input = <text>` binding
// (fts3tokenize's InputConstrainedVTab pattern).
func (v *tokenizeVTab) SetInputConstraint(value string) {
	v.input = value
	v.hasInput = true
	v.inputGiven = true
}

// ValidateInstance implements vtab.InstanceValidator: a scan without an
// input binding fails (fts5tokFilterMethod's idxNum==0 SQLITE_ERROR).
func (v *tokenizeVTab) ValidateInstance() error {
	if !v.hasInput {
		return fmt.Errorf("SQL logic error")
	}
	if v.createErr != nil {
		return v.createErr
	}
	return nil
}

func (v *tokenizeVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Columns reports the declared schema (fts5_test_tok.c FTS3_TOK_SCHEMA).
func (v *tokenizeVTab) Columns() []string {
	return []string{"input", "token", "start", "end", "position"}
}

// ColumnTypes reports the column affinities (input/token TEXT, offsets
// INTEGER) so `input = 123` compares numerically (fts3tok1 1.9 parity).
func (v *tokenizeVTab) ColumnTypes() []string {
	return []string{"TEXT", "TEXT", "INTEGER", "INTEGER", "INTEGER"}
}

// HiddenColumns reports the HIDDEN input column.
func (v *tokenizeVTab) HiddenColumns() map[int]bool { return map[int]bool{0: true} }

func (v *tokenizeVTab) Open() (vtab.Cursor, error) {
	if !v.hasInput {
		return nil, fmt.Errorf("SQL logic error")
	}
	tokens := v.tok.Tokenize(v.input)
	c := &tokenizeCursor{input: v.input}
	for i, tk := range tokens {
		c.rows = append(c.rows, tokenizeRow{
			input: v.input, token: tk.Term, start: tk.Start, end: tk.End, pos: i,
		})
	}
	return c, nil
}

// tokenizeRow is one output row (Fts5tokRow).
type tokenizeRow struct {
	input string
	token string
	start int
	end   int
	pos   int
}

// tokenizeCursor serves the rows (fts5tokNextMethod/fts5tokColumnMethod).
type tokenizeCursor struct {
	input string
	rows  []tokenizeRow
	idx   int
}

func (c *tokenizeCursor) Next() bool {
	c.idx++
	return c.idx <= len(c.rows)
}

func (c *tokenizeCursor) Column(idx int) (interface{}, error) {
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
	return nil, fmt.Errorf("fts5tokenize: invalid column index %d", idx)
}

// Rowid implements vtab.RowidCursor (fts5tokRowidMethod: the 1-based row
// counter).
func (c *tokenizeCursor) Rowid() int64 { return int64(c.idx) }

func (c *tokenizeCursor) Close() error { return nil }
