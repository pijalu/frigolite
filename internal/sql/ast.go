package sql

// Stmt is the common interface for all SQL statements.
type Stmt interface {
	stmt()
}

// StmtList is a list of statements.
type StmtList []Stmt

func (StmtList) stmt() {}

// SelectStmt represents a SELECT statement.
type SelectStmt struct {
	Distinct bool
	Columns  []SelectColumn
	From     TableRef
	Joins    []JoinClause   // JOIN clauses
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderByTerm
	Limit    Expr
	Offset   Expr
	Union    *SelectStmt    // UNION (optional)
	UnionAll bool           // UNION ALL vs UNION
}

func (s *SelectStmt) stmt() {}

// JoinClause represents a JOIN clause.
type JoinClause struct {
	Table    TableRef
	JoinType string // "INNER", "LEFT", "RIGHT", "CROSS", ""
	On       Expr   // ON condition
}

// SelectColumn represents a single column in a SELECT list.
type SelectColumn struct {
	Expr Expr
	As   string // optional alias
}

// TableRef represents a table reference (possibly with alias).
type TableRef struct {
	Name string
	As   string
}

// OrderByTerm represents an ORDER BY term.
type OrderByTerm struct {
	Expr Expr
	Desc bool
}

// InsertStmt represents an INSERT statement.
type InsertStmt struct {
	Table   string
	Columns []string
	Values  []Expr
	Select  *SelectStmt // for INSERT ... SELECT
}

func (s *InsertStmt) stmt() {}

// UpdateStmt represents an UPDATE statement.
type UpdateStmt struct {
	Table   string
	Assignments []Assignment
	Where   Expr
}

func (s *UpdateStmt) stmt() {}

// Assignment represents a SET x = y clause.
type Assignment struct {
	Column string
	Value  Expr
}

// DeleteStmt represents a DELETE statement.
type DeleteStmt struct {
	Table string
	Where Expr
}

func (s *DeleteStmt) stmt() {}

// CreateTableStmt represents a CREATE TABLE statement.
type CreateTableStmt struct {
	Name    string
	Columns []ColumnDef
	IfNotExists bool
}

func (s *CreateTableStmt) stmt() {}

// ColumnDef represents a column definition in CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       string
	NotNull    bool
	PrimaryKey bool
	AutoInc    bool
	Default    Expr
}

// CreateIndexStmt represents a CREATE INDEX statement.
type CreateIndexStmt struct {
	Name       string
	Table      string
	Columns    []IndexColumn
	Unique     bool
}

func (s *CreateIndexStmt) stmt() {}

// IndexColumn represents a column in an index definition.
type IndexColumn struct {
	Name string
	Desc bool
}

// DropTableStmt represents a DROP TABLE statement.
type DropTableStmt struct {
	Name string
}

func (s *DropTableStmt) stmt() {}

// CreateViewStmt represents a CREATE VIEW statement.
type CreateViewStmt struct {
	Name   string
	Select *SelectStmt
}

func (s *CreateViewStmt) stmt() {}

// DropViewStmt represents a DROP VIEW statement.
type DropViewStmt struct {
	Name     string
	IfExists bool
}

func (s *DropViewStmt) stmt() {}

// CreateTriggerStmt represents a CREATE TRIGGER statement.
type CreateTriggerStmt struct {
	Name       string
	Table      string
	Event      string // INSERT, UPDATE, DELETE
	Time       string // BEFORE, AFTER, INSTEAD OF
	Statements []Stmt
}

func (s *CreateTriggerStmt) stmt() {}

// CreateVirtualTableStmt represents a CREATE VIRTUAL TABLE statement.
type CreateVirtualTableStmt struct {
	Name   string
	Module string
	Args   []string
}

func (s *CreateVirtualTableStmt) stmt() {}

// DropTriggerStmt represents a DROP TRIGGER statement.
type DropTriggerStmt struct {
	Name     string
	IfExists bool
}

func (s *DropTriggerStmt) stmt() {}

// ExplainStmt wraps another statement with EXPLAIN.
type ExplainStmt struct {
	Statement Stmt
}

func (s *ExplainStmt) stmt() {}

// BeginStmt represents a BEGIN TRANSACTION statement.
type BeginStmt struct{}

func (s *BeginStmt) stmt() {}

// CommitStmt represents a COMMIT statement.
type CommitStmt struct{}

func (s *CommitStmt) stmt() {}

// RollbackStmt represents a ROLLBACK statement.
type RollbackStmt struct{}

func (s *RollbackStmt) stmt() {}

// PragmaStmt represents a PRAGMA statement.
type PragmaStmt struct {
	Name  string
	Value string // optional value
}

func (s *PragmaStmt) stmt() {}

// AlterTableStmt represents an ALTER TABLE statement.
type AlterTableStmt struct {
	Table   string
	Action  string // "RENAME", "ADD", "DROP"
	NewName string // for RENAME
	Column  string // for ADD/DROP columns
	ColDef  ColumnDef // for ADD
}

func (s *AlterTableStmt) stmt() {}

// AttachStmt represents an ATTACH DATABASE statement.
type AttachStmt struct {
	Path   string
	Schema string
}

func (s *AttachStmt) stmt() {}

// VacuumStmt represents a VACUUM statement.
type VacuumStmt struct{}

func (s *VacuumStmt) stmt() {}

// AnalyzeStmt represents an ANALYZE statement.
type AnalyzeStmt struct {
	Name string // optional table/index name
}

func (s *AnalyzeStmt) stmt() {}

// ReindexStmt represents a REINDEX statement.
type ReindexStmt struct{}

func (s *ReindexStmt) stmt() {}

// SavepointStmt represents a SAVEPOINT/RELEASE/ROLLBACK TO statement.
type SavepointStmt struct {
	Name string
	Type string // "SAVEPOINT", "RELEASE", "ROLLBACK"
}

func (s *SavepointStmt) stmt() {}

// Expr is the common interface for all expressions.
type Expr interface {
	expr()
}

// BinaryOp represents a binary operation.
type BinaryOp struct {
	Left     Expr
	Right    Expr
	Operator string // =, <, >, <=, >=, <>, +, -, *, /, AND, OR, LIKE, etc.
}

func (e *BinaryOp) expr() {}

// UnaryOp represents a unary operation.
type UnaryOp struct {
	Operand  Expr
	Operator string // NOT, -
}

func (e *UnaryOp) expr() {}

// ColumnRef represents a reference to a column.
type ColumnRef struct {
	Table string
	Name  string
}

func (e *ColumnRef) expr() {}

// NumericLit represents a numeric literal.
type NumericLit struct {
	Value string
}

func (e *NumericLit) expr() {}

// StringLit represents a string literal.
type StringLit struct {
	Value string
}

func (e *StringLit) expr() {}

// NullLit represents a NULL literal.
type NullLit struct{}

func (e *NullLit) expr() {}

// FuncCall represents a function call.
type FuncCall struct {
	Name string
	Args []Expr
}

func (e *FuncCall) expr() {}

// IsNull represents an IS NULL expression.
type IsNull struct {
	Operand Expr
}

func (e *IsNull) expr() {}

// IsNotNull represents an IS NOT NULL expression.
type IsNotNull struct {
	Operand Expr
}

func (e *IsNotNull) expr() {}

// Between represents a BETWEEN expression.
type Between struct {
	Operand Expr
	Low     Expr
	High    Expr
	Negated bool
}

func (e *Between) expr() {}

// InList represents an IN (list) expression.
type InList struct {
	Operand  Expr
	List     []Expr
	Negated  bool
}

func (e *InList) expr() {}

// Subquery represents a subquery in an expression (SELECT ...).
type Subquery struct {
	Select *SelectStmt
}

func (e *Subquery) expr() {}

// ExistsExpr represents an EXISTS(subquery) or NOT EXISTS(subquery).
type ExistsExpr struct {
	Select  *SelectStmt
	Negated bool
}

func (e *ExistsExpr) expr() {}

// CaseExpr represents a CASE WHEN THEN ELSE expression.
type CaseExpr struct {
	Operand Expr      // CASE x WHEN ... (optional)
	Whens   []WhenClause
	Else    Expr       // ELSE expression (optional)
}

func (e *CaseExpr) expr() {}

// WhenClause is a single WHEN ... THEN ... in a CASE expression.
type WhenClause struct {
	When Expr
	Then Expr
}

// CastExpr represents a CAST(x AS type) expression.
type CastExpr struct {
	Operand Expr
	AsType  string
}

func (e *CastExpr) expr() {}
