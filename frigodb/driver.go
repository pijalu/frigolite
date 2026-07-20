// Package frigodb implements database/sql driver for frigolite.
//
// Use it with database/sql:
//
//	import (
//	    "database/sql"
//	    _ "github.com/pijalu/frigolite/frigodb"
//	)
//
//	db, err := sql.Open("frigolite", ":memory:")
//
// The DSN is either ":memory:" for an in-memory database or a file path.
package frigodb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/pijalu/frigolite"
)

func init() {
	sql.Register("frigolite", &frigoliteDriver{})
}

type frigoliteDriver struct{}

// Open returns a new connection to the database.
func (d *frigoliteDriver) Open(dsn string) (driver.Conn, error) {
	var db *frigolite.DB
	var err error
	if dsn == "" || dsn == ":memory:" {
		// Each connection gets its own temp file
		var f *os.File
		f, err = os.CreateTemp("", "frigolite-*.db")
		if err != nil {
			return nil, err
		}
		path := f.Name()
		f.Close()
		db, err = frigolite.Open(path)
		if err != nil {
			os.Remove(path)
			return nil, err
		}
		return &frigoliteConn{db: db, path: path, isMemory: true}, nil
	}
	db, err = frigolite.Open(dsn)
	if err != nil {
		return nil, err
	}
	return &frigoliteConn{db: db, path: dsn, isMemory: false}, nil
}

// frigoliteConn implements driver.Conn.
type frigoliteConn struct {
	db       *frigolite.DB
	isMemory bool
	path     string
	closed   bool
	mu       sync.Mutex
}

func (c *frigoliteConn) Prepare(query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	return &frigoliteStmt{
		conn:  c,
		query: query,
	}, nil
}

func (c *frigoliteConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.db.Close()
	if c.isMemory && c.path != "" {
		os.Remove(c.path)
	}
	return err
}

func (c *frigoliteConn) Begin() (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	res := c.db.Exec("BEGIN")
	if res.Error != nil {
		return nil, res.Error
	}
	return &frigoliteTx{conn: c}, nil
}

// Implement Pinger
func (c *frigoliteConn) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("frigodb: connection closed")
	}
	return nil
}

// Implement ExecerContext for direct exec without prepare
func (c *frigoliteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	q, err := interpolateArgs(query, args)
	if err != nil {
		return nil, err
	}
	res := c.db.Exec(q)
	if res.Error != nil {
		return nil, res.Error
	}
	return &frigoliteResult{changes: res.Changes, lastID: 0}, nil
}

// Implement QueryerContext for direct query without prepare
func (c *frigoliteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	q, err := interpolateArgs(query, args)
	if err != nil {
		return nil, err
	}
	res := c.db.Query(q)
	if res.Error != nil {
		return nil, res.Error
	}
	return &frigoliteRows{columns: res.Columns, rows: res.Rows, idx: 0}, nil
}

// Implement ConnBeginTx for Begin with context and options
func (c *frigoliteConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	// Ignore isolation level (frigolite only supports serializable)
	res := c.db.Exec("BEGIN")
	if res.Error != nil {
		return nil, res.Error
	}
	return &frigoliteTx{conn: c}, nil
}

// Implement SessionResetter
func (c *frigoliteConn) ResetSession(ctx context.Context) error {
	return nil
}

// Implement Validator
func (c *frigoliteConn) IsValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

// frigoliteTx implements driver.Tx.
type frigoliteTx struct {
	conn *frigoliteConn
	done bool
}

func (tx *frigoliteTx) Commit() error {
	if tx.done {
		return fmt.Errorf("frigodb: transaction already done")
	}
	tx.done = true
	res := tx.conn.db.Exec("COMMIT")
	return res.Error
}

func (tx *frigoliteTx) Rollback() error {
	if tx.done {
		return fmt.Errorf("frigodb: transaction already done")
	}
	tx.done = true
	res := tx.conn.db.Exec("ROLLBACK")
	return res.Error
}

// frigoliteStmt implements driver.Stmt.
type frigoliteStmt struct {
	conn  *frigoliteConn
	query string
}

func (s *frigoliteStmt) Close() error {
	return nil
}

func (s *frigoliteStmt) NumInput() int {
	// Check for ? placeholders
	count := strings.Count(s.query, "?")
	if count == 0 {
		return -1 // unknown
	}
	return count
}

func (s *frigoliteStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.execStmt(context.Background(), args)
}

func (s *frigoliteStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.queryStmt(context.Background(), args)
}

// Implement StmtExecContext for direct NamedValue support
func (s *frigoliteStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	return s.execStmt(ctx, vals)
}

// Implement StmtQueryContext for direct NamedValue support
func (s *frigoliteStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	return s.queryStmt(ctx, vals)
}

func (s *frigoliteStmt) execStmt(ctx context.Context, args []driver.Value) (driver.Result, error) {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()
	if s.conn.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	q := interpolateValues(s.query, args)
	res := s.conn.db.Exec(q)
	if res.Error != nil {
		return nil, res.Error
	}
	return &frigoliteResult{changes: res.Changes, lastID: 0}, nil
}

func (s *frigoliteStmt) queryStmt(ctx context.Context, args []driver.Value) (driver.Rows, error) {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()
	if s.conn.closed {
		return nil, fmt.Errorf("frigodb: connection closed")
	}
	q := interpolateValues(s.query, args)
	res := s.conn.db.Query(q)
	if res.Error != nil {
		return nil, res.Error
	}
	return &frigoliteRows{columns: res.Columns, rows: res.Rows, idx: 0}, nil
}

// frigoliteResult implements driver.Result.
type frigoliteResult struct {
	changes int64
	lastID  int64
}

func (r *frigoliteResult) LastInsertId() (int64, error) {
	return r.lastID, nil
}

func (r *frigoliteResult) RowsAffected() (int64, error) {
	return r.changes, nil
}

// frigoliteRows implements driver.Rows.
type frigoliteRows struct {
	columns []string
	rows    [][]interface{}
	idx     int
}

func (r *frigoliteRows) Columns() []string {
	return r.columns
}

func (r *frigoliteRows) Close() error {
	r.rows = nil
	return nil
}

func (r *frigoliteRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	// If dest is empty (no columns), use it as-is
	// Otherwise, fill dest from row values
	for i := 0; i < len(dest) && i < len(row); i++ {
		val := row[i]
		if val == nil {
			dest[i] = nil
			continue
		}
		switch v := val.(type) {
		case int64:
			dest[i] = v
		case float64:
			dest[i] = v
		case string:
			dest[i] = v
		case []byte:
			dest[i] = v
		case bool:
			dest[i] = v
		default:
			dest[i] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}

// interpolateArgs replaces ? and $N placeholders with named values.
func interpolateArgs(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	// Map named args by ordinal
	byOrdinal := make(map[int]driver.Value)
	for _, a := range args {
		byOrdinal[a.Ordinal] = a.Value
	}

	var b strings.Builder
	i := 0
	argIdx := 1
	for i < len(query) {
		if query[i] == '?' {
			val, ok := byOrdinal[argIdx]
			if !ok {
				return "", fmt.Errorf("frigodb: missing argument for placeholder %d", argIdx)
			}
			b.WriteString(escapeSQL(val))
			argIdx++
			i++
		} else if n, j, ok := tryParseDollarN(query, i); ok {
			val, ok := byOrdinal[n]
			if !ok {
				return "", fmt.Errorf("frigodb: missing argument for placeholder $%d", n)
			}
			b.WriteString(escapeSQL(val))
			i = j
		} else {
			b.WriteByte(query[i])
			i++
		}
	}
	return b.String(), nil
}

// tryParseDollarN attempts to parse a $N placeholder at position i.
// Returns the placeholder number, end index, and whether parsing succeeded.
func tryParseDollarN(query string, i int) (int, int, bool) {
	if query[i] != '$' || i+1 >= len(query) || query[i+1] < '0' || query[i+1] > '9' {
		return 0, 0, false
	}
	j := i + 1
	for j < len(query) && query[j] >= '0' && query[j] <= '9' {
		j++
	}
	n := 0
	fmt.Sscanf(query[i+1:j], "%d", &n)
	return n, j, true
}

// interpolateValues replaces ? placeholders with values.
func interpolateValues(query string, args []driver.Value) string {
	if len(args) == 0 {
		return query
	}
	var b strings.Builder
	argIdx := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' && argIdx < len(args) {
			b.WriteString(escapeSQL(args[argIdx]))
			argIdx++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

func escapeSQL(val interface{}) string {
	if val == nil {
		return "NULL"
	}
	switch v := val.(type) {
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%f", v)
	case bool:
		if v {
			return "1"
		}
		return "0"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return "X'" + fmt.Sprintf("%x", v) + "'"
	default:
		return fmt.Sprintf("'%v'", v)
	}
}

// Compile-time interface checks
var (
	_ driver.Driver          = (*frigoliteDriver)(nil)
	_ driver.Conn            = (*frigoliteConn)(nil)
	_ driver.Pinger          = (*frigoliteConn)(nil)
	_ driver.ExecerContext   = (*frigoliteConn)(nil)
	_ driver.QueryerContext  = (*frigoliteConn)(nil)
	_ driver.ConnBeginTx     = (*frigoliteConn)(nil)
	_ driver.SessionResetter = (*frigoliteConn)(nil)
	_ driver.Validator       = (*frigoliteConn)(nil)
	_ driver.Stmt            = (*frigoliteStmt)(nil)
	_ driver.StmtExecContext = (*frigoliteStmt)(nil)
	_ driver.StmtQueryContext = (*frigoliteStmt)(nil)
	_ driver.Result          = (*frigoliteResult)(nil)
	_ driver.Rows            = (*frigoliteRows)(nil)
	_ driver.Tx              = (*frigoliteTx)(nil)
)