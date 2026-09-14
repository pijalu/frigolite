package frigolite

// Native anchor for the P6.FTS5 fts5savepoint.test adjudication
// (2026-09-14). Section 2.0 (DROP TABLE ft2_idx then writes must fail with
// "database disk image is malformed") is unreachable in frigolite: the
// shadow tables are write-through mirrors of the Go-native single-blob
// storage (internal/fts5/storage.go), so dropping one is not load-bearing
// — that divergence is pinned in frigolite_fts5corrupt_test.go.
//
// This file pins the transactional contracts that ARE engine-visible and
// oracle-verified (python3 sqlite3 3.53.4, 2026-09-14): nested savepoints
// with ROLLBACK TO over an fts5 table commit exactly the surviving rows,
// and the index stays integrity-check-clean across savepoint/commit
// boundaries (fts5savepoint 1.0 and 3.3).

import (
	"testing"
)

// TestFTS5SavepointNestedRollback ports fts5savepoint.test 1.0: two-level
// savepoint nesting where ROLLBACK TO three discards only 'd'; the
// committed table is {a b c} and the integrity-check passes afterwards.
func TestFTS5SavepointNestedRollback(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	stmts := []string{
		"CREATE VIRTUAL TABLE ft USING fts5(c)",
		"BEGIN",
		"SAVEPOINT one",
		"INSERT INTO ft VALUES('a')",
		"SAVEPOINT two",
		"INSERT INTO ft VALUES('b')",
		"RELEASE two",
		"SAVEPOINT four",
		"INSERT INTO ft VALUES('c')",
		"RELEASE four",
		"SAVEPOINT three",
		"INSERT INTO ft VALUES('d')",
		"ROLLBACK TO three",
		"COMMIT",
	}
	for _, s := range stmts {
		checkExecOK(t, db.Exec(s))
	}
	checkQueryResult(t, db.Query("SELECT * FROM ft"), "a b c")
	checkExecOK(t, db.Exec("INSERT INTO ft(ft) VALUES('integrity-check')"))
}

// TestFTS5SavepointCommitIntegrity ports the fts5savepoint.test 3.3 shape:
// an fts5 insert inside a savepoint/COMMIT pair leaves the index
// integrity-check-clean (mixed fts4/fts5 traffic of 3.0-3.2 included by
// driving an fts3/4 table alongside; DROP of the fts4 table stays clean).
func TestFTS5SavepointCommitIntegrity(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE vt0 USING fts5(c0)"))
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE vt1 USING fts4(c0)"))
	checkExecOK(t, db.Exec("INSERT INTO vt1(c0) VALUES(0)"))
	for _, s := range []string{
		"BEGIN",
		"UPDATE vt1 SET c0 = 0",
		"INSERT INTO vt1(c0) VALUES (0), (0)",
		"UPDATE vt0 SET c0 = 0",
		"INSERT INTO vt1(c0) VALUES (0)",
		"INSERT INTO vt1(vt1) VALUES('automerge=1')",
		"COMMIT",
	} {
		checkExecOK(t, db.Exec(s))
	}
	checkExecOK(t, db.Exec("DROP TABLE vt1"))
	checkExecOK(t, db.Exec("SAVEPOINT x"))
	checkExecOK(t, db.Exec("INSERT INTO vt0 VALUES('x')"))
	checkExecOK(t, db.Exec("COMMIT"))
	checkExecOK(t, db.Exec("INSERT INTO vt0(vt0) VALUES('integrity-check')"))
	checkQueryResult(t, db.Query("SELECT * FROM vt0"), "x")
}
