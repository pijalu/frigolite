package exec

import "fmt"

// Multi-statement script support (engine side).
//
// A script submitted as one text (N statements joined by ';') is prepared and
// run statement by statement by the connection layer, sqlite3_exec's loop over
// sqlite3_prepare (see the root package's execScript/runScript). That loop
// needs exactly one thing from the engine: the SQL length limit has to be
// enforced on the WHOLE script text before it is split, because the limit
// counts the text a single prepare call receives (tokenize.c
// sqlite3RunParser's mxSqlLen against db->aLimit[SQLITE_LIMIT_SQL_LENGTH]) and
// a per-statement split must not let an over-long script through
// (sqllimits1-6.1).

// CheckScriptLength enforces SQLITE_LIMIT_SQL_LENGTH on a script text before
// the caller splits it into statements. The whole-script parser reports the
// same error for the same text, so an over-long script behaves identically on
// both paths.
func (e *Engine) CheckScriptLength(sqlStr string) error {
	if e.settings.sqlLengthLimit != 0 && len(sqlStr) > e.settings.sqlLengthLimit {
		return fmt.Errorf("string or blob too big")
	}
	return nil
}
