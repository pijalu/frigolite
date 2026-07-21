// Frigolite is a pure-Go SQL database engine compatible with the SQLite file format.
//
// Basic usage:
//
//	db, err := frigolite.Open(":memory:")
//	if err != nil { ... }
//	defer db.Close()
//
//	res := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")
//	res = db.Exec("INSERT INTO users VALUES (1, 'Alice')")
//	res = db.Query("SELECT * FROM users")
//	for _, row := range res.Rows {
//	    fmt.Println(row)
//	}
package frigolite

import (
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// DB is an open database connection.
type DB struct {
	pager  *pager.Pager
	schema *schema.Manager
	engine *exec.Engine
	path   string
}

// Result holds query results.
type Result struct {
	Columns []string
	Rows    [][]interface{}
	Changes int64
	Error   error
}

// Open opens a database file. Use ":memory:" for an in-memory database.
func Open(path string) (*DB, error) {
	var pg *pager.Pager
	var err error

	if path == "" || path == ":memory:" {
		pg = pager.OpenInMemory(pager.DefaultPageSize)
	} else {
		pg, err = pager.Open(path, pager.DefaultPageSize)
		if err != nil {
			return nil, fmt.Errorf("frigolite: open: %w", err)
		}
	}

	db := &DB{
		pager:  pg,
		engine: exec.NewEngine(pg),
		path:   path,
	}
	db.schema = schema.NewManager(pg)

	// Initialize schema if needed
	if err := db.schema.Init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("frigolite: init schema: %w", err)
	}

	return db, nil
}

// Close closes the database.
func (db *DB) Close() error {
	if db.pager != nil {
		return db.pager.Close()
	}
	return nil
}

// execResult converts an exec.Result to a public Result.
func execResult(er *exec.Result) *Result {
	if er == nil {
		return nil
	}
	return &Result{
		Columns: er.Columns,
		Rows:    er.Rows,
		Changes: er.Changes,
		Error:   er.Error,
	}
}

// Exec executes a SQL statement that does not return rows.
func (db *DB) Exec(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized")}
	}
	parser := sql.NewParser(sqlStr)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", parser.Err())}
	}

	var lastResult *exec.Result
	for _, stmt := range stmts {
		res := db.engine.Exec(stmt)
		if res.Error != nil {
			return execResult(res)
		}
		lastResult = res
	}

	if lastResult == nil {
		return &Result{}
	}
	return execResult(lastResult)
}

// Query executes a SQL query and returns rows.
func (db *DB) Query(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized")}
	}
	parser := sql.NewParser(sqlStr)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", parser.Err())}
	}

	if len(stmts) == 0 {
		return &Result{}
	}

	return execResult(db.engine.Exec(stmts[0]))
}

// DumpAll logs all schema entries and table contents (debug helper).
func (db *DB) DumpAll() {
	entries, err := db.schema.GetEntries("")
	if err != nil {
		fmt.Printf("dump error: %v\n", err)
		return
	}
	fmt.Printf("=== Schema (%d entries) ===\n", len(entries))
	for _, e := range entries {
		fmt.Printf("  type=%s name=%s tbl_name=%s root=%d\n", e.Type, e.Name, e.TblName, e.RootPage)
	}

	// Dump table contents
	for _, e := range entries {
		if e.Type == schema.TypeTable {
			res := db.Query("SELECT rowid, * FROM " + e.Name)
			if res.Error != nil {
				fmt.Printf("  dump %s: %v\n", e.Name, res.Error)
				continue
			}
			fmt.Printf("\n=== %s (%d rows) ===\n", e.Name, len(res.Rows))
			fmt.Printf("  columns: %v\n", res.Columns)
			for _, row := range res.Rows {
				fmt.Printf("  %v\n", row)
			}
		}
	}
}

// Save persists an in-memory database to a file.
func (db *DB) Save(path string) error {
	if db.pager == nil {
		return fmt.Errorf("frigolite: database not open")
	}
	return db.pager.Flush()
}

// Path returns the database path.
func (db *DB) Path() string {
	return db.path
}

// FileExists checks if a database file exists.
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
