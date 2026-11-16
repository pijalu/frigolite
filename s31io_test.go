package frigolite

import (
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

func TestProbeIOERRCommit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "t.db")
	db, _ := Open(dbPath)
	db.Exec("PRAGMA journal_mode = DELETE")
	db.Exec("CREATE TABLE zz(top PRIMARY KEY)")
	db.Exec("INSERT INTO zz VALUES('x')")
	for i := 0; i < 5; i++ {
		if r := db.Exec("INSERT INTO zz SELECT printf('%s-%d', top, (SELECT count(*) FROM zz)) FROM zz"); r.Error != nil {
			t.Fatalf("iter %d: %v", i, r.Error)
		}
	}
	t.Logf("after growth count=%v", db.Query("SELECT count(*) FROM zz").Rows)
	db.Exec("BEGIN")
	db.Exec("UPDATE zz SET top = top || '!!'")
	db.engine.InstallVFSHook(func(op, path string, details []string) error {
		if op == "xWrite" || op == "xTruncate" || op == "xSync" {
			return &pager.VFSHookError{Code: "SQLITE_IOERR"}
		}
		return nil
	}, -1)
	r := db.Exec("COMMIT")
	t.Logf("COMMIT err=%v (want disk I/O error)", r.Error)
	// 11.3: a second connection plays back and switches to TRUNCATE
	db2, _ := Open(dbPath)
	r2 := db2.Exec("PRAGMA journal_mode = TRUNCATE")
	t.Logf("db2 jm err=%v res=%v", r2.Error, r2.Rows)
	q2 := db2.Query("PRAGMA integrity_check")
	t.Logf("db2 integrity err=%v rows=%v", q2.Error, q2.Rows)
	db2.Close()
	q := db.Query("SELECT count(*) FROM zz")
	t.Logf("count err=%v rows=%v (want 32)", q.Error, q.Rows)
}
