package exec

// Split out of pragma_state.go for the 1000-line file-size gate.

// CompoundSelectLimit reports the SQLITE_LIMIT_COMPOUND_SELECT setting
// (sqlite3_limit's default of 500 caps UNION/INTERSECT/EXCEPT chains).
func (e *Engine) CompoundSelectLimit() int {
	n := e.settings.compoundSelectLimit
	if n <= 0 {
		n = 500
	}
	return n
}
