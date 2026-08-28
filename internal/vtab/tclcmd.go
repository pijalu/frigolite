package vtab

import (
	"fmt"
	"os"
	"strings"
)

// TclCommandModule implements the "tcl" test module (test_tclvar.c /
// tclsqlite.c register_tcl_module parity): the module argument names a TCL
// proc whose xConnect callback returns the DECLARE schema text. The Go port
// resolves that name through the shared variable registry — the transpiled
// `set ::create_table_sql $cts` populates it before CREATE runs.
//
// declare_vtab emulation (sqlite3DeclareVTab):
//   - the schema must be a CREATE TABLE statement → otherwise
//     "declare_vtab: syntax error";
//   - CREATE TABLE ... AS SELECT parses but is rejected →
//     "declare_vtab: SQL logic error";
//   - valid schemas expose their declared columns.
type TclCommandModule struct{}

// NewTclCommandModule builds the tcl module.
func NewTclCommandModule() *TclCommandModule { return &TclCommandModule{} }

// Eponymous implements EponymousModule: the tcl module supports both direct
// FROM use and explicit CREATE VIRTUAL TABLE.
func (m *TclCommandModule) Eponymous() bool { return true }

// tclCmdVTab is one instance bound to a validated schema.
type tclCmdVTab struct {
	columns []string
}

// Create implements Module.
func (m *TclCommandModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module.
func (m *TclCommandModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

func (m *TclCommandModule) connect(args []string) (VirtualTable, error) {
	procName := ""
	if len(args) > 0 {
		procName = strings.TrimSpace(args[0])
	}
	schema := TclVarGet(procName, "")
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "TCLCMD proc=%q schema=%q\n", procName, schema)
	}
	if schema == "" && len(args) > 0 {
		// Fallback: the transpiler may register the variable under its bare
		// namespace-free name while the module arg carries a different
		// proc name; try the conventional create_table_sql slot.
		schema = TclVarGet("create_table_sql", "")
	}
	cols, err := classifyDeclareSchema(schema)
	if err != nil {
		return nil, err
	}
	return &tclCmdVTab{columns: cols}, nil
}

// Columns implements ColumnInfo.
func (v *tclCmdVTab) Columns() []string { return v.columns }

// BestIndex accepts the default full-scan plan.
func (v *tclCmdVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open yields no rows: the tcl module has no data source of its own; scans
// over it exist to exercise declare-time validation.
func (v *tclCmdVTab) Open() (Cursor, error) {
	return &emptyCursor{}, nil
}

// emptyCursor is a cursor over zero rows.
type emptyCursor struct{}

// Next implements Cursor.
func (c *emptyCursor) Next() bool { return false }

// Column implements Cursor.
func (c *emptyCursor) Column(idx int) (interface{}, error) {
	return nil, fmt.Errorf("no row")
}

// Close implements Cursor.
func (c *emptyCursor) Close() error { return nil }

// classifyDeclareSchema emulates sqlite3DeclareVTab's acceptance rules and
// extracts the declared column names:
//
//   - the text must begin with CREATE TABLE (optionally IF NOT EXISTS);
//     anything else fails with "syntax error";
//   - CREATE TABLE ... AS SELECT parses grammatically but is rejected with
//     "SQL logic error";
//   - valid definitions yield their column names (table-level constraints
//     skipped), mirroring csv.c's generated-schema handling.
func classifyDeclareSchema(schema string) ([]string, error) {
	syntaxErr := fmt.Errorf("declare_vtab: syntax error")
	logicErr := fmt.Errorf("declare_vtab: SQL logic error")

	s := strings.TrimSpace(schema)
	if s == "" {
		return nil, syntaxErr
	}
	up := " " + strings.ToUpper(s) + " "
	if !strings.Contains(up, " CREATE ") {
		return nil, syntaxErr
	}
	if !strings.Contains(up, " TABLE ") {
		return nil, syntaxErr
	}
	// Strip a leading IF NOT EXISTS after CREATE TABLE for name scanning.
	body := s
	if idx := strings.Index(up, " TABLE "); idx >= 0 {
		rest := strings.TrimSpace(s[idx+len(" TABLE "):])
		if restUpper := strings.ToUpper(rest); strings.HasPrefix(restUpper, "IF NOT EXISTS ") {
			body = strings.TrimSpace(rest[len("IF NOT EXISTS "):])
		} else {
			body = rest
		}
	}
	// CTAS: "AS SELECT" appears before the column-list parenthesis.
	paren := strings.IndexByte(body, '(')
	asIdx := indexOutsideParens(body, " AS ")
	hasParen := paren >= 0
	if !hasParen {
		// No column list: CREATE TABLE xyz AS SELECT ... is grammatical but
		// rejected by declare_vtab with SQLITE_ERROR ("SQL logic error"),
		// while a bare/malformed tail fails as a syntax error.
		if strings.Contains(up, " AS SELECT ") {
			return nil, logicErr
		}
		return nil, syntaxErr
	}
	if asIdx >= 0 && asIdx < paren {
		// "AS" before "(" — e.g. CREATE TABLE x AS SELECT ...: CTAS.
		return nil, logicErr
	}
	closeIdx := strings.LastIndexByte(body, ')')
	if closeIdx < paren {
		return nil, syntaxErr
	}
	colBody := body[paren+1 : closeIdx]
	names, err := columnNamesFromSchema("(" + colBody + ")")
	if err != nil {
		return nil, syntaxErr
	}
	if len(names) == 0 {
		return nil, syntaxErr
	}
	return names, nil
}

// indexOutsideParens finds needle in s at a position not nested inside any
// parenthesis (-1 when absent or always nested).
func indexOutsideParens(s, needle string) int {
	depth := 0
	for i := 0; i+len(needle) <= len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && strings.HasPrefix(s[i:], needle) {
			return i
		}
	}
	return -1
}
