package frigolite

import "fmt"

// backupInitError records sqlite3_backup_init failures on destination handle.
func backupInitError(dst *DB, format string, args ...interface{}) error {
	err := fmt.Errorf(format, args...)
	if dst != nil && dst.engine != nil {
		dst.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
	}
	return err
}
