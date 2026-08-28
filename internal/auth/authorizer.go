// Package auth provides a Go-style authorization interface for database operations.
//
// The Authorizer interface replaces the C-style sqlite3_set_authorizer callback
// with a clean Go interface. Use AllowAllAuthorizer for the default behavior
// where all operations are permitted.
package auth

// Action represents a database operation being authorized.
type Action int

const (
	ActionCreateTable   Action = iota // CREATE TABLE
	ActionCreateIndex                 // CREATE INDEX
	ActionCreateView                  // CREATE VIEW
	ActionCreateTrigger               // CREATE TRIGGER
	ActionDropTable                   // DROP TABLE
	ActionDropIndex                   // DROP INDEX
	ActionDropView                    // DROP VIEW
	ActionDropTrigger                 // DROP TRIGGER
	ActionInsert                      // INSERT
	ActionUpdate                      // UPDATE
	ActionDelete                      // DELETE
	ActionSelect                      // SELECT (top-level)
	ActionRead                        // table read in SELECT
	ActionAlterTable                  // ALTER TABLE
	ActionAttach                      // ATTACH DATABASE
	ActionDetach                      // DETACH DATABASE
	ActionFunction                    // function call
	ActionPragma                      // PRAGMA
)

// String returns a human-readable name for the action.
// actionNames maps each Action to its SQLite authorizer string.
var actionNames = map[Action]string{
	ActionCreateTable:   "SQLITE_CREATE_TABLE",
	ActionCreateIndex:   "SQLITE_CREATE_INDEX",
	ActionCreateView:    "SQLITE_CREATE_VIEW",
	ActionCreateTrigger: "SQLITE_CREATE_TRIGGER",
	ActionDropTable:     "SQLITE_DROP_TABLE",
	ActionDropIndex:     "SQLITE_DROP_INDEX",
	ActionDropView:      "SQLITE_DROP_VIEW",
	ActionDropTrigger:   "SQLITE_DROP_TRIGGER",
	ActionInsert:        "SQLITE_INSERT",
	ActionUpdate:        "SQLITE_UPDATE",
	ActionDelete:        "SQLITE_DELETE",
	ActionSelect:        "SQLITE_SELECT",
	ActionRead:          "SQLITE_READ",
	ActionAlterTable:    "SQLITE_ALTER_TABLE",
	ActionAttach:        "SQLITE_ATTACH",
	ActionDetach:        "SQLITE_DETACH",
	ActionFunction:      "SQLITE_FUNCTION",
	ActionPragma:        "SQLITE_PRAGMA",
}

func (a Action) String() string {
	if name, ok := actionNames[a]; ok {
		return name
	}
	return "SQLITE_UNKNOWN"
}

// Result of an authorization check.
type Result int

const (
	ResultOK     Result = iota // allow the operation
	ResultDeny                 // deny with "not authorized" error
	ResultIgnore               // treat as NULL (for column reads)
)

// String returns a human-readable name for the result.
func (r Result) String() string {
	switch r {
	case ResultOK:
		return "SQLITE_OK"
	case ResultDeny:
		return "SQLITE_DENY"
	case ResultIgnore:
		return "SQLITE_IGNORE"
	default:
		return "SQLITE_UNKNOWN"
	}
}

// Authorizer is the interface that wraps the Authorize method.
//
// Authorize is called before each database operation to check whether
// the operation should be allowed. The arguments provide context about
// the operation:
//   - arg1: first argument (depends on action, e.g. table name)
//   - arg2: second argument (depends on action, e.g. column or object name)
//   - arg3: third argument (depends on action, e.g. schema name)
//   - arg4: fourth argument (depends on action, e.g. trigger name)
//
// Return ResultOK to allow, ResultDeny to reject with an error, or
// ResultIgnore to treat the column read as NULL.
type Authorizer interface {
	Authorize(action Action, arg1, arg2, arg3, arg4 string) Result
}

// AllowAllAuthorizer allows all operations (default behavior when no
// authorizer is set).
type AllowAllAuthorizer struct{}

// Authorize always returns ResultOK, allowing all operations.
func (a *AllowAllAuthorizer) Authorize(action Action, arg1, arg2, arg3, arg4 string) Result {
	return ResultOK
}

// DenyAllAuthorizer denies all operations.
type DenyAllAuthorizer struct{}

// Authorize always returns ResultDeny, denying all operations.
func (a *DenyAllAuthorizer) Authorize(action Action, arg1, arg2, arg3, arg4 string) Result {
	return ResultDeny
}

// ActionFilterAuthorizer allows or denies operations based on a set of actions.
// Actions not in the set are allowed by default.
type ActionFilterAuthorizer struct {
	DeniedActions map[Action]bool
}

// NewActionFilterAuthorizer creates an authorizer that denies the specified actions.
func NewActionFilterAuthorizer(denied ...Action) *ActionFilterAuthorizer {
	m := make(map[Action]bool, len(denied))
	for _, a := range denied {
		m[a] = true
	}
	return &ActionFilterAuthorizer{DeniedActions: m}
}

// Authorize denies the action if it is in the denied set, otherwise allows it.
func (a *ActionFilterAuthorizer) Authorize(action Action, arg1, arg2, arg3, arg4 string) Result {
	if a.DeniedActions[action] {
		return ResultDeny
	}
	return ResultOK
}
