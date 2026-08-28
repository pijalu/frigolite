package frigolite

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/sql"
)

// Stmt is a reusable prepared SQL statement. It provides parameter binding and
// row-at-a-time execution while retaining Frigolite's Result-based API.
type Stmt struct {
	db       *DB
	sql      string
	args     map[int]interface{}
	named    map[string]interface{}
	result   *Result
	row      int
	closed   bool
	readLock bool
	readStmt bool
	// paramNames holds one entry per parameter slot (index i is parameter
	// i+1; "" for unnamed), assigned per resolve.c sqlite3ExprAssignVarNumber.
	paramNames []string
	// vmState mirrors the VDBE run-state needed by sqlite3_step/sqlite3_bind
	// misuse detection: stepping past DONE, or using a statement whose step
	// failed, without an intervening reset returns SQLITE_MISUSE (vdbeapi.c).
	vmState vmState
	// lastErr retains the error of the most recent failed Step, mirroring
	// the VDBE's rc: sqlite3_finalize of a statement whose step failed
	// re-reports that error as the connection's last error.
	lastErr error
}

// vmState models the observable subset of the VDBE run-state:
//   - vmReady: freshly prepared or reset; bind/step allowed.
//   - vmDone: stepped to completion; further steps return MISUSE
//     ("no more rows available") until Reset.
//   - vmPoisoned: last execution errored; bind/step return MISUSE
//     ("bad parameter or other API misuse") until Reset.
type vmState int

const (
	vmReady vmState = iota
	vmDone
	vmPoisoned
)

// Misuse errors mirror vdbeapi.c / main.c SQLITE_MISUSE messages observed on
// the oracle C API transcript (testdata/stmtbindconformance).
var (
	errNoMoreRows = fmt.Errorf("no more rows available")
	errBadParam   = fmt.Errorf("bad parameter or other API misuse")
	errOutOfRange = fmt.Errorf("column index out of range")
)

// Prepare compiles SQL for repeated execution. Syntax and schema errors are
// reported immediately, matching sqlite3_prepare_v2's prepare-time behavior.
func (db *DB) Prepare(sqlText string) (*Stmt, error) {
	if db == nil || db.engine == nil {
		return nil, fmt.Errorf("frigolite: database not initialized")
	}
	parsed, err := db.engine.Prepare(sqlText)
	if err != nil {
		return nil, err
	}
	stmt := &Stmt{db: db, sql: sqlText, args: make(map[int]interface{}), named: make(map[string]interface{})}
	if len(parsed) == 1 {
		_, stmt.readStmt = parsed[0].(*sql.SelectStmt)
	}
	// CollectParameterNames already validated the statement inside
	// Engine.Prepare; a failure here mirrors that prepare error.
	names, err := exec.CollectParameterNames(sqlText)
	if err != nil {
		return nil, err
	}
	stmt.paramNames = names
	db.registerStmt()
	return stmt, nil
}

// BindParameterCount returns the number of SQL parameters (sqlite3_bind_parameter_count).
func (s *Stmt) BindParameterCount() int {
	if s == nil || s.closed {
		return 0
	}
	return len(s.paramNames)
}

// BindParameterName returns the name of parameter idx (1-based) exactly as
// written in the SQL, including its leading ":"/"@"/"$"/"?" punctuation
// (vdbeapi.c returns aVar[idx-1] verbatim); unnamed and out-of-range indexes
// return "".
func (s *Stmt) BindParameterName(idx int) string {
	if s == nil || s.closed || idx < 1 || idx > len(s.paramNames) {
		return ""
	}
	return s.paramNames[idx-1]
}

// BindParameterIndex returns the index of the named parameter, matching its
// full token text (":abc", "@a", "$v", "?4") case-insensitively as recorded
// at prepare time; unnamed or unknown names return 0
// (sqlite3_bind_parameter_index).
func (s *Stmt) BindParameterIndex(name string) int {
	if s == nil || s.closed || name == "" {
		return 0
	}
	for i, candidate := range s.paramNames {
		if candidate != "" && strings.EqualFold(candidate, name) {
			return i + 1
		}
	}
	return 0
}

// Bind binds 1-based positional parameter index.
func (s *Stmt) Bind(index int, value interface{}) error {
	if s == nil || s.closed {
		return fmt.Errorf("statement is closed")
	}
	// vdbeapi.c sqlite3Bind: binding on a statement whose program ran to
	// completion (or errored) without a reset is SQLITE_MISUSE.
	if s.vmState != vmReady {
		return errBadParam
	}
	// Out-of-range indexes are SQLITE_RANGE ("column index out of range",
	// main.c sqlite3StrAccum text used by vdbeapi.c).
	if index < 1 || index > len(s.paramNames) {
		return errOutOfRange
	}
	s.args[index] = value
	return nil
}

// BindInt binds an integer parameter.
func (s *Stmt) BindInt(index, value int) error { return s.Bind(index, value) }

// BindInt64 binds an int64 parameter.
func (s *Stmt) BindInt64(index int, value int64) error { return s.Bind(index, value) }

// BindFloat binds a floating-point parameter.
func (s *Stmt) BindFloat(index int, value float64) error { return s.Bind(index, value) }

// BindText binds a string parameter.
func (s *Stmt) BindText(index int, value string) error { return s.Bind(index, value) }

// BindBlob binds a byte slice parameter.
func (s *Stmt) BindBlob(index int, value []byte) error { return s.Bind(index, value) }

// BindNull binds SQL NULL.
func (s *Stmt) BindNull(index int) error { return s.Bind(index, nil) }

// BindNamed binds a named parameter, with or without its leading punctuation.
// The value lands on the parameter's resolve.c slot so positional and named
// binds share one storage map.
func (s *Stmt) BindNamed(name string, value interface{}) error {
	if s == nil || s.closed {
		return fmt.Errorf("statement is closed")
	}
	key := strings.TrimLeft(name, ":@$?")
	slot := 0
	for i, candidate := range s.paramNames {
		if candidate != "" && strings.EqualFold(strings.TrimLeft(strings.TrimPrefix(candidate, "?"), ":@$?"), key) {
			slot = i + 1
			break
		}
	}
	if slot == 0 {
		return fmt.Errorf("unknown parameter: %s", name)
	}
	s.args[slot] = value
	return nil
}

// Step executes statement on first call and advances one result row per call.
// It returns true while a row is available and false after completion.
func (s *Stmt) Step() (bool, error) {
	if s == nil || s.closed {
		return false, fmt.Errorf("statement is closed")
	}
	// vdbeapi.c sqlite3Step: stepping a statement that already ran to
	// completion (or errored) without an intervening reset is SQLITE_MISUSE.
	// These misuse errors do NOT become the connection's last error (the
	// oracle transcript shows finalize after them reporting SQLITE_OK).
	if s.vmState == vmDone {
		return false, errNoMoreRows
	}
	if s.vmState == vmPoisoned {
		return false, errBadParam
	}
	if s.result == nil {
		if s.readStmt && !s.readLock {
			s.db.engine.SetPreparedReadLock("main", true)
			s.readLock = true
		}
		r := s.db.Query(s.renderBoundSQL())
		if r.Error != nil {
			s.releaseReadLock()
			s.lastErr = r.Error
			s.vmState = vmPoisoned
			// Classify the error message so sqlite3_errcode emulation
			// reports the proper code (e.g. malformed → SQLITE_CORRUPT).
			s.db.engine.SetLastErr(r.Error.Error(), s.db.ErrorCodeFor(r.Error))
			return false, r.Error
		}
		s.lastErr = nil
		s.result = r
	}
	if s.row >= len(s.result.Rows) {
		s.releaseReadLock()
		s.vmState = vmDone
		return false, nil
	}
	s.row++
	return true, nil
}

// Row returns current row after a successful Step.
func (s *Stmt) Row() []interface{} {
	if s == nil || s.result == nil || s.row == 0 || s.row > len(s.result.Rows) {
		return nil
	}
	return s.result.Rows[s.row-1]
}

// Columns returns result column names.
func (s *Stmt) Columns() []string {
	if s == nil || s.result == nil {
		return nil
	}
	return s.result.Columns
}

// Exec executes statement and returns its complete result. Unlike Step it
// re-runs the statement from the start on every call (the transpiler helpers
// use it for side-effect-only steps).
func (s *Stmt) Exec() *Result {
	if s == nil || s.closed {
		return &Result{Error: fmt.Errorf("statement is closed")}
	}
	s.vmState = vmReady
	r := s.db.Query(s.renderBoundSQL())
	s.result = r
	s.row = len(r.Rows)
	if r.Error != nil {
		s.lastErr = r.Error
		s.vmState = vmPoisoned
		s.db.engine.SetLastErr(r.Error.Error(), s.db.ErrorCodeFor(r.Error))
	} else {
		s.lastErr = nil
		s.vmState = vmDone
	}
	return r
}

// Reset allows statement execution again, retaining bindings.
func (s *Stmt) Reset() error {
	if s == nil || s.closed {
		return fmt.Errorf("statement is closed")
	}
	s.releaseReadLock()
	s.result = nil
	s.row = 0
	s.vmState = vmReady
	return nil
}

// ClearBindings removes all parameter values.
func (s *Stmt) ClearBindings() error {
	if s == nil || s.closed {
		return fmt.Errorf("statement is closed")
	}
	s.args = make(map[int]interface{})
	s.named = make(map[string]interface{})
	return nil
}

// Close finalizes statement.
func (s *Stmt) Close() error {
	if s == nil {
		return nil
	}
	if s.closed {
		return nil
	}
	s.releaseReadLock()
	s.closed = true
	s.result = nil
	s.db.unregisterStmt()
	return nil
}

// Finalize closes the statement with sqlite3_finalize's error semantics: the
// return value (and the connection's last-error state) reflects the error of
// the statement's most recent Step — finalizing a statement whose step failed
// re-reports that error as the connection's last error (vdbeapi.c
// sqlite3VdbeFinalize), while finalizing after success clears it.
func (s *Stmt) Finalize() error {
	if s == nil {
		return nil
	}
	err := s.lastErr
	if cerr := s.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if s.db != nil && s.db.engine != nil {
		if err != nil {
			s.db.engine.SetLastErr(err.Error(), s.db.ErrorCodeFor(err))
		} else {
			s.db.engine.SetLastErr("", "")
		}
	}
	return err
}

func (s *Stmt) releaseReadLock() {
	if s.readLock {
		s.db.engine.SetPreparedReadLock("main", false)
		s.readLock = false
	}
}

//lint:ignore U1000 retained for prepared-read compatibility
func isPreparedRead(sqlText string) bool {
	trimmed := strings.TrimSpace(strings.ToUpper(sqlText))
	return strings.HasPrefix(trimmed, "SELECT") || strings.HasPrefix(trimmed, "WITH") || strings.HasPrefix(trimmed, "PRAGMA")
}

// renderBoundSQL renders the statement's SQL with bound values substituted as
// literals so execution goes through the regular query path. Parameter tokens
// resolve against paramNames (the resolve.c slot assignment): "?NNN"/":NNN"
// take slot NNN directly, bare "?" continues from the highest assigned slot,
// and named tokens match their slot case-insensitively.
func (s *Stmt) renderBoundSQL() string {
	t := sql.NewTokenizer(s.sql)
	var b strings.Builder
	positional := 0 // highest parameter index assigned so far (resolve.c nVar)
	prev := 0
	for {
		tok := t.Next()
		if tok.Pos > prev {
			b.WriteString(s.sql[prev:tok.Pos]) // preserve original spacing
		}
		prev = tok.Pos + len(tok.Value)
		if tok.Type != sql.TokenParam {
			if tok.Type == sql.TokenEOF {
				return b.String()
			}
			b.WriteString(tok.Value)
			continue
		}
		s.writeParamLiteral(&b, tok.Value, &positional)
	}
}

// writeParamLiteral appends the SQL literal for one parameter token,
// advancing *positional past any slot it consumes.
func (s *Stmt) writeParamLiteral(b *strings.Builder, token string, positional *int) {
	name := token[1:]
	numbered := token[0] == '?' && name != "" ||
		token[0] == ':' && isAllDigits(name)
	switch {
	case numbered:
		idx := 0
		for k := 0; k < len(name); k++ {
			idx = idx*10 + int(name[k]-'0')
		}
		if idx > *positional {
			*positional = idx
		}
		b.WriteString(stmtSQLLiteral(s.args[idx]))
	case token[0] == '?':
		*positional++
		b.WriteString(stmtSQLLiteral(s.args[*positional]))
	default:
		idx := s.namedSlot(token)
		b.WriteString(stmtSQLLiteral(s.args[idx]))
	}
}

// namedSlot resolves a named parameter token ("@a", "$v", ":p", including TCL
// forms like "$::ns:x" / "$x(y)") to its assigned parameter index by matching
// the recorded paramNames case-insensitively; 0 means unbound (NULL).
func (s *Stmt) namedSlot(token string) int {
	for i, candidate := range s.paramNames {
		if candidate != "" && strings.EqualFold(candidate, token) {
			return i + 1
		}
	}
	return 0
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
func stmtSQLLiteral(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + fmt.Sprintf("%X", x) + "'"
	case bool:
		if x {
			return "1"
		}
		return "0"
	case float64:
		// Keep REAL type through the literal: SQLite parses an integral
		// text like "42" as INTEGER, so whole floats need a trailing ".0"
		// (and NaN is stored as NULL per SQLite semantics).
		if math.IsNaN(x) {
			return "NULL"
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	default:
		return fmt.Sprint(x)
	}
}
