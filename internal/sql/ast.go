package sql

// TokenInfo records the byte position of a token in the original SQL text.
// Used by ALTER TABLE RENAME to find and replace identifier tokens.
type TokenInfo struct {
	Start int // byte offset of the first character
	End   int // byte offset after the last character (exclusive)
}

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
	Joins    []JoinClause // JOIN clauses
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderByTerm
	Limit    Expr
	Offset   Expr
	Union    *SelectStmt // combined query (optional)
	SetOp    SetOp       // UNION / INTERSECT / EXCEPT
	UnionAll bool        // UNION ALL vs UNION
	CTEs     []CTEDef    // WITH clause CTE definitions
	Windows  []WindowDef // WINDOW clause definitions

	// ValuesChain is set when this SELECT was parsed from a VALUES(...),(...)
	// list (mvalues). It distinguishes INSERT...VALUES from a real
	// INSERT...SELECT that happens to have no FROM clause.
	ValuesChain bool

	// unionTail caches the last element of the Union chain so appending a new
	// member is O(1). It is only maintained by AppendUnion; callers that build
	// chains by assigning Union directly must not rely on it.
	unionTail *SelectStmt

	// RawSQL is the original statement text as written by the caller (used
	// for post-parse fixups such as function-call ORDER BY recovery).
	RawSQL string
}

// AppendUnion appends next to the end of s's compound chain in O(1) amortized
// time and records how the previous tail merges next (op/unionAll semantics).
// Behavior is identical to walking the chain and setting tail.Union/tail.SetOp.
func (s *SelectStmt) AppendUnion(next *SelectStmt, op SetOp, unionAll bool) {
	tail := s.unionTail
	if tail == nil {
		tail = s
		for tail.Union != nil {
			tail = tail.Union
		}
	}
	tail.Union = next
	tail.SetOp = op
	tail.UnionAll = unionAll
	// The new chain tail is next's tail (next may itself be a compound chain,
	// e.g. a multi-row VALUES whose internal UNION ALL chain must be preserved).
	s.unionTail = next
	if next.unionTail != nil {
		s.unionTail = next.unionTail
	}
}

// CTEDef represents a Common Table Expression definition.
type CTEDef struct {
	Name      string
	Columns   []string
	Select    *SelectStmt // body of the CTE (subquery)
	Recursive bool        // defined in a WITH RECURSIVE clause
}

// SetOp represents the type of set operation.
type SetOp int

const (
	SetNone      SetOp = iota
	SetUnion           // UNION
	SetIntersect       // INTERSECT
	SetExcept          // EXCEPT
)

// WindowDef represents a named window definition in a WINDOW clause.
type WindowDef struct {
	Name       string        // window name
	Partitions []Expr        // PARTITION BY expressions
	OrderBy    []OrderByTerm // ORDER BY terms
	FrameSpec  string        // frame spec text (ROWS/RANGE/GROUPS BETWEEN ...)
}

// String returns the SQL representation of the WindowDef.
// For a named window reference (no specs), returns just the name.
// For an empty inline window (OVER ()), returns "()".
// For inline specs, returns "(PARTITION BY ... ORDER BY ...)".
func (w *WindowDef) String() string {
	if w == nil {
		return ""
	}
	// Named window reference (no PARTITION BY/ORDER BY specs)
	if len(w.Partitions) == 0 && len(w.OrderBy) == 0 && w.FrameSpec == "" {
		if w.Name != "" {
			return w.Name
		}
		return "()"
	}
	result := "("
	// PARTITION BY
	if len(w.Partitions) > 0 {
		result += "PARTITION BY "
		for j, p := range w.Partitions {
			if j > 0 {
				result += ", "
			}
			result += ExprString(p)
		}
	}
	// ORDER BY inside window
	if len(w.OrderBy) > 0 {
		if len(w.Partitions) > 0 {
			result += " "
		}
		result += "ORDER BY "
		for j, ob := range w.OrderBy {
			if j > 0 {
				result += ", "
			}
			result += ExprString(ob.Expr)
			if ob.Desc {
				result += " DESC"
			}
		}
	}
	result += ")"
	return result
}

func (s *SelectStmt) stmt() {}

// JoinClause represents a JOIN clause.
type JoinClause struct {
	Table     TableRef
	JoinType  string   // "INNER", "LEFT", "RIGHT", "CROSS", ""
	On        Expr     // ON condition
	CommaJoin bool     // true if this is a comma-join (FROM t1, t2) not explicit CROSS JOIN
	Using     []string // USING(column-list) columns, merged by the join
}

// SelectColumn represents a single column in a SELECT list.
type SelectColumn struct {
	Expr Expr
	As   string // optional alias
}

// TableRef represents a table reference (possibly with alias).
type TableRef struct {
	Name     string
	As       string
	NameTok  TokenInfo   // byte position of the Name token in original SQL
	Subquery *SelectStmt // subquery in FROM clause (optional)
	Args     []Expr      // table-valued function arguments: FROM tablename(args)
	// IndexedBy is the index name from an "INDEXED BY name" clause ("NOT
	// INDEXED" is represented as the sentinel ""). The engine validates
	// that the named index can serve the query.
	IndexedBy string
}

// OrderByTerm represents an ORDER BY term.
type OrderByTerm struct {
	Expr Expr
	Desc bool
	// NullsFirst forces NULLs to sort first regardless of direction;
	// NullsLast forces them last. Both false means SQLite's default (NULLs
	// first for ASC, last for DESC).
	NullsFirst bool
	NullsLast  bool
}

// InsertStmt represents an INSERT statement.
type InsertStmt struct {
	Table        string
	Columns      []string
	Values       [][]Expr    // list of value tuples; empty means DEFAULT VALUES
	Select       *SelectStmt // for INSERT ... SELECT
	CTEs         []CTEDef    // WITH clause CTE definitions
	OnConflict   *OnConflictClause
	IsReplace    bool // true for REPLACE INTO or INSERT OR REPLACE
	OrIgnore     bool // true for INSERT OR IGNORE
	OrFail       bool // true for INSERT OR FAIL (a failed row aborts the statement but earlier rows survive)
	OrConflict   string // statement-level OR conflict action: "", "IGNORE", "REPLACE", "FAIL", "ABORT", "ROLLBACK" (overrides per-constraint ON CONFLICT)
	Returning    SelectColumn
	HasReturning bool
}

func (s *InsertStmt) stmt() {}

// OnConflictClause represents an ON CONFLICT ... DO ... clause.
type OnConflictClause struct {
	ConflictColumn string // optional conflict target column
	Action         ConflictAction
	Assignments    []Assignment // for DO UPDATE SET
	Where          Expr         // optional WHERE for DO UPDATE
}

// ConflictAction is the action taken on conflict.
type ConflictAction int

const (
	ConflictDoNothing ConflictAction = iota
	ConflictDoUpdate
)

// UpdateStmt represents an UPDATE statement.
type UpdateStmt struct {
	Table           string
	OnConflict      string // "REPLACE", "IGNORE", etc. from UPDATE OR <action>
	Assignments     []Assignment
	SetParenColumns []string // when set, indicates SET (col1,col2)=(val1,val2) format
	From            TableRef // UPDATE ... FROM <tables> (SQLite 3.33+)
	FromJoins       []TableRef
	Where           Expr
	OrderBy         []OrderByTerm
	Limit           Expr
	Offset          Expr
	CTEs            []CTEDef // WITH clause CTE definitions
	Returning       SelectColumn
	HasReturning    bool
}

func (s *UpdateStmt) stmt() {}

// Assignment represents a SET x = y clause.
type Assignment struct {
	Column string
	Value  Expr
}

// DeleteStmt represents a DELETE statement.
type DeleteStmt struct {
	Table        string
	Where        Expr
	OrderBy      []OrderByTerm
	Limit        Expr
	Offset       Expr
	CTEs         []CTEDef // WITH clause CTE definitions
	Returning    SelectColumn
	HasReturning bool
}

func (s *DeleteStmt) stmt() {}

// CreateTableStmt represents a CREATE TABLE statement.
// ConstraintType represents the type of a table constraint.
type ConstraintType string

const (
	ConstraintPrimaryKey ConstraintType = "PRIMARY KEY"
	ConstraintUnique     ConstraintType = "UNIQUE"
	ConstraintCheck      ConstraintType = "CHECK"
	ConstraintForeignKey ConstraintType = "FOREIGN KEY"
)

// IndexedColumn represents a column in a table constraint (PRIMARY KEY, UNIQUE, etc.).
type IndexedColumn struct {
	Name    string // column name
	Collate string // optional COLLATE collation name
	Desc    bool   // DESC (default is ASC)
}

// ParenExpr represents a parenthesized expression: (expr).
type ParenExpr struct {
	Expr Expr
}

func (e *ParenExpr) expr() {}

// TableConstraint represents a table-level constraint in CREATE TABLE.
type TableConstraint struct {
	Type    ConstraintType  // PRIMARY KEY, UNIQUE, CHECK, FOREIGN KEY
	Name    string          // optional constraint name
	Expr    Expr            // for CHECK: the check expression
	Columns []IndexedColumn // for PRIMARY KEY/UNIQUE: indexed columns with options
	// OnConflict is the optional ON CONFLICT resolution for the constraint
	// (e.g. "IGNORE", "REPLACE", "ABORT", "FAIL", "ROLLBACK").
	OnConflict string

	// FOREIGN KEY reference info (Type == ConstraintForeignKey):
	RefTable  string   // referenced (parent) table name
	RefCols   []string // referenced parent columns (empty = parent's PK)
	RefAction string   // "ON DELETE X ON UPDATE Y" action text
	Deferred  bool     // DEFERRABLE INITIALLY DEFERRED
}

type CreateTableStmt struct {
	Name         string
	NameTok      TokenInfo // byte position of the table name in original SQL
	Columns      []ColumnDef
	IfNotExists  bool
	AsSelect     *SelectStmt       // CREATE TABLE ... AS SELECT
	Constraints  []TableConstraint // table-level constraints
	WithoutRowid bool              // WITHOUT ROWID option
	Strict       bool              // STRICT tables (type-enforced)
	Temporary    bool              // CREATE TEMP TABLE

	// RawSQL is the original CREATE TABLE statement text as written by the
	// user. It is stored verbatim in sqlite_schema (matching SQLite) so that
	// column constraints (PRIMARY KEY, UNIQUE, AS(...) generated columns,
	// DEFAULT, NOT NULL, ...) survive round-tripping through the parser.
	RawSQL string
}

func (s *CreateTableStmt) stmt() {}

// ColumnDef represents a column definition in CREATE TABLE.
type ColumnDef struct {
	Name           string
	Type           string
	NotNull        bool
	PrimaryKey     bool
	PKDesc         bool // PRIMARY KEY DESC (INTEGER PRIMARY KEY DESC is NOT a rowid alias)
	AutoInc        bool
	Unique         bool
	OnConflict     string // optional: REPLACE, ABORT, FAIL, ROLLBACK, IGNORE
	Collate        string
	ConstraintName string // optional: CONSTRAINT name before constraint clause
	References     string
	Default        Expr
	Check          Expr
	Generated      Expr // generated column expression (b AS(expr)); nil for normal columns
	Dropped        bool // column has been dropped via ALTER TABLE DROP COLUMN
	Hidden         bool // HIDDEN column in a virtual table declaration
}

// CreateIndexStmt represents a CREATE INDEX statement.
type CreateIndexStmt struct {
	Name     string
	Table    string
	TableTok TokenInfo // byte position of the Table name in original SQL
	Columns  []IndexColumn
	// Terms are the full index key expressions (sortlist), kept alongside the
	// flattened Columns so DDL DQS validation and ALTER TABLE DROP COLUMN
	// dependency checks can inspect expression index keys (e.g. z||"abc").
	Terms  []OrderByTerm
	Unique bool
	Where  Expr // optional WHERE clause for partial indexes
	// IfNotExists is set for "CREATE INDEX IF NOT EXISTS ..." (SQLite
	// silently ignores the statement when the index already exists).
	IfNotExists bool

	// RawSQL is the original CREATE INDEX statement text as written by the
	// user. It is stored verbatim in sqlite_schema (matching SQLite) so the
	// stored SQL preserves expression index keys and original quoting.
	RawSQL string
}

func (s *CreateIndexStmt) stmt() {}

// IndexColumn represents a column in an index definition.
type IndexColumn struct {
	Name string
	Desc bool
}

// DropTableStmt represents a DROP TABLE statement.
type DropTableStmt struct {
	Name     string
	IfExists bool
}

func (s *DropTableStmt) stmt() {}

// DropIndexStmt represents a DROP INDEX statement.
type DropIndexStmt struct {
	Name     string
	IfExists bool
}

func (s *DropIndexStmt) stmt() {}

// CreateViewStmt represents a CREATE VIEW statement.
type CreateViewStmt struct {
	Name    string
	NameTok TokenInfo // byte position of the view name in original SQL
	Columns []string  // optional declared column list: CREATE VIEW v(c0, c1) AS ...
	Select  *SelectStmt
	RawSQL  string // verbatim CREATE VIEW text (preserves CTEs)

	// Temporary is set for CREATE TEMP VIEW (or CREATE TEMPORARY VIEW).
	Temporary bool
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
	Name        string
	Table       string
	TableTok    TokenInfo // byte position of the Table name in original SQL
	Event       string    // INSERT, UPDATE, DELETE
	Time        string    // BEFORE, AFTER, INSTEAD OF
	When        Expr      // WHEN clause (optional)
	Statements  []Stmt
	IfNotExists bool
	RawSQL      string // original CREATE TRIGGER text (verbatim storage)
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
	QueryPlan bool // true for EXPLAIN QUERY PLAN
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
	// Schema is the optional schema qualifier (PRAGMA main.foreign_key_check
	// sets Schema="main", Name="foreign_key_check").
	Schema string
}

func (s *PragmaStmt) stmt() {}

// AlterTableStmt represents an ALTER TABLE statement.
type AlterTableStmt struct {
	Table          string
	TableTok       TokenInfo // byte position of the Table name in original SQL
	Action         string    // "RENAME", "ADD", "DROP", "ALTER"
	NewName        string    // for RENAME
	Column         string    // for ADD/DROP columns
	ColumnTok      TokenInfo // byte position of the Column name in original SQL (for RENAME COLUMN)
	ColDef         ColumnDef // for ADD
	AlterColAction string    // "SET NOT NULL" or "DROP NOT NULL" for ALTER COLUMN

	// NewConstraint carries the table-level constraint added by
	// ALTER TABLE ... ADD [CONSTRAINT nm] CHECK(expr).
	NewConstraint *TableConstraint
}

func (s *AlterTableStmt) stmt() {}

// AttachStmt represents an ATTACH DATABASE statement.
type AttachStmt struct {
	Path     string
	PathExpr Expr // expression for path (evaluated at runtime if Path is empty)
	Schema   string
	IsDetach bool // true for DETACH, false for ATTACH
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
	Escape   string // escape character for LIKE (optional)
	// HasEscape is true when an explicit ESCAPE clause was present on the
	// LIKE/GLOB operator (even with an empty string). The query planner uses
	// it to refuse the LIKE optimization for ESCAPE '' (SQLite: the optimizer
	// only applies when the ESCAPE is a single character).
	HasEscape bool
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
	Table    string
	Name     string
	TableTok TokenInfo // byte position of the Table qualifier (if present)
	NameTok  TokenInfo // byte position of the Name token

	// Quoted is true when the identifier came from a double-quoted token
	// ("name"). SQLite's DQS (double-quoted string) behavior resolves such
	// identifiers as column references first, then falls back to a string
	// literal when no column matches and DQS is enabled for the context.
	Quoted bool
}

func (e *ColumnRef) expr() {}

// NumericLit represents a numeric literal.
type NumericLit struct {
	Value  string
	cached interface{} // cached parsed value (int64 or float64), avoids re-parsing
}

func (e *NumericLit) expr() {}

// Cached returns the pre-parsed cached value, or nil if not yet cached.
func (e *NumericLit) Cached() interface{} { return e.cached }

// SetCached stores a pre-parsed value for future use.
func (e *NumericLit) SetCached(v interface{}) { e.cached = v }

// StringLit represents a string literal.
type StringLit struct {
	Value string
}

func (e *StringLit) expr() {}

// NullLit represents a NULL literal.
type NullLit struct{}

func (e *NullLit) expr() {}

// ParameterExpr represents a bound-parameter placeholder (?NNN, ?name, $name,
// :name or @name). Frigolite does not support runtime binding, so it evaluates
// to NULL; it exists separately from NullLit so CREATE TABLE can reject the
// non-constant DEFAULT expressions that contain it.
type ParameterExpr struct {
	Name string
}

func (e *ParameterExpr) expr() {}

// BlobLit represents a hex blob literal (x'00' or X'AB').
type BlobLit struct {
	Value []byte
}

func (e *BlobLit) expr() {}

// FuncCall represents a function call.
type FuncCall struct {
	Name     string
	Args     []Expr
	Distinct bool          // DISTINCT keyword inside function, e.g. COUNT(DISTINCT x)
	OrderBy  []OrderByTerm // ORDER BY inside aggregate function call
	Filter   Expr          // FILTER (WHERE condition) — nil if no FILTER
	Over     *WindowDef    // OVER clause for window functions (nil if not a window function)
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

// IsDistinctFrom represents an IS DISTINCT FROM expression.
type IsDistinctFrom struct {
	Left  Expr
	Right Expr
}

func (e *IsDistinctFrom) expr() {}

// IsNotDistinctFrom represents an IS NOT DISTINCT FROM expression.
type IsNotDistinctFrom struct {
	Left  Expr
	Right Expr
}

func (e *IsNotDistinctFrom) expr() {}

// IsTrue represents an IS TRUE or IS NOT TRUE expression.
type IsTrue struct {
	Operand Expr
	Negated bool // true for IS NOT TRUE
}

func (e *IsTrue) expr() {}

// IsFalse represents an IS FALSE or IS NOT FALSE expression.
type IsFalse struct {
	Operand Expr
	Negated bool // true for IS NOT FALSE
}

func (e *IsFalse) expr() {}

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
	Operand Expr
	List    []Expr
	Negated bool
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
	Operand Expr // CASE x WHEN ... (optional)
	Whens   []WhenClause
	Else    Expr // ELSE expression (optional)
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

// RaiseExpr represents the RAISE() special function, which is only valid
// inside a trigger program. Kind is one of "IGNORE", "ROLLBACK", "ABORT" or
// "FAIL". Message is the (optional) error message expression for the
// non-IGNORE kinds.
type RaiseExpr struct {
	Kind    string
	Message Expr
}

func (r *RaiseExpr) expr() {}

// RowValue represents a parenthesized list of expressions (row value / tuple).
// (a, b, c) is a row value used in comparisons: (a,b) = (1,2)
type RowValue struct {
	Values []Expr
}

func (r *RowValue) expr() {}
