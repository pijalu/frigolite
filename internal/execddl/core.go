package execddl

import (
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/rename"
	"github.com/pijalu/frigolite/internal/sql"
)

// DDLExecutor executes CREATE/DROP/ALTER/ATTACH/DETACH statements. It owns
// the DDL execution family that previously lived on the Engine and delegates
// engine capability access back through the DDLContext interface.
type DDLExecutor struct {
	ctx DDLContext
}

// NewDDLExecutor builds a DDL executor over the given context.
func NewDDLExecutor(ctx DDLContext) *DDLExecutor {
	return &DDLExecutor{ctx: ctx}
}

// CreateStmt dispatches a CREATE statement to its executor.
func (e *DDLExecutor) CreateStmt(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.CreateTableStmt:
		return e.execCreateTable(s)
	case *sql.CreateIndexStmt:
		return e.execCreateIndex(s)
	case *sql.CreateViewStmt:
		return e.execCreateView(s)
	case *sql.CreateTriggerStmt:
		return e.execCreateTrigger(s)
	case *sql.CreateVirtualTableStmt:
		return e.execCreateVirtualTable(s)
	}
	return &Result{}
}

// DropStmt dispatches a DROP statement to its executor.
func (e *DDLExecutor) DropStmt(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.DropTableStmt:
		return e.execDropTable(s)
	case *sql.DropIndexStmt:
		return e.execDropIndex(s)
	case *sql.DropViewStmt:
		return e.execDropView(s)
	case *sql.DropTriggerStmt:
		return e.execDropTrigger(s)
	}
	return &Result{}
}

// Alter dispatches an ALTER TABLE statement to its executor.
func (e *DDLExecutor) Alter(s *sql.AlterTableStmt) *Result {
	return e.execAlterTable(s)
}

// AttachOrDetach dispatches ATTACH / DETACH to the matching executor.
func (e *DDLExecutor) AttachOrDetach(s *sql.AttachStmt) *Result {
	if s.IsDetach {
		return e.execDetach(s)
	}
	return e.execAttach(s)
}

// Compile-time probes: DDLExecutor implements the DDL execution capability
// interfaces (LSP: the executor is substitutable for each statement-family
// capability it exposes).
var (
	_ createExecutor = (*DDLExecutor)(nil)
	_ dropExecutor   = (*DDLExecutor)(nil)
	_ alterExecutor  = (*DDLExecutor)(nil)
)

// createExecutor is the CREATE-statement execution capability.
type createExecutor interface {
	CreateStmt(stmt sql.Stmt) *Result
}

// dropExecutor is the DROP-statement execution capability.
type dropExecutor interface {
	DropStmt(stmt sql.Stmt) *Result
}

// alterExecutor is the ALTER-statement execution capability.
type alterExecutor interface {
	Alter(s *sql.AlterTableStmt) *Result
}

// Result aliases the shared statement result type.
type Result = execquery.Result

// DatabaseContext aliases the shared per-database state type.
type DatabaseContext = execquery.DatabaseContext

// Row aliases the shared row abstraction.
type Row = execquery.Row

// RowMap aliases the map-backed row abstraction.
type RowMap = execquery.RowMap

// uniqDef aliases the query planner's UNIQUE/PK constraint description.
type uniqDef = execquery.UniqDef

// RenameContext tracks the rename operation state (shared rename utility).
type RenameContext = rename.RenameContext

// RenameRange represents a byte range in the original SQL text to replace.
type RenameRange = rename.RenameRange

// FindRenameTokens parses SQL text and returns all byte ranges that should be
// replaced when renaming a table or column.
var FindRenameTokens = rename.FindRenameTokens

// ApplyRenames applies a set of byte-range replacements to a SQL text.
var ApplyRenames = rename.ApplyRenames
