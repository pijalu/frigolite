package vtab

import "fmt"

// UniqueConstraintError reports a virtual-table rowid uniqueness violation in
// SQLite's exact wording ("UNIQUE constraint failed: t1.idx"). DML executors
// type-assert it to implement the OR-conflict resolutions (IGNORE / REPLACE /
// FAIL) the way sqlite3 vtab xUpdate handling does.
type UniqueConstraintError struct {
	Table  string
	Column string
	RowID  int64 // offending rowid (0 when not tied to a single rowid)
}

// Error implements error.
func (e *UniqueConstraintError) Error() string {
	return fmt.Sprintf("UNIQUE constraint failed: %s.%s", e.Table, e.Column)
}

// AsUniqueConstraintError extracts a *UniqueConstraintError from err (nil when
// err is nil or a different kind).
func AsUniqueConstraintError(err error) (*UniqueConstraintError, bool) {
	if uce, ok := err.(*UniqueConstraintError); ok {
		return uce, true
	}
	return nil, false
}
