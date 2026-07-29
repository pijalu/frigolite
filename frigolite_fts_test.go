package frigolite

import (
	"testing"
)

// TestFTSBasic validates basic FTS3 table operations.
func TestFTSBasic(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// Create an FTS3 table
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content)"))

	// Insert data
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello world')"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('goodbye world')"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello there')"))

	// Query with MATCH
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'hello'"), "1 3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'world'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'goodbye'"), "2")

	// Multiple terms (implicit AND)
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'hello world'"), "1")

	// OR
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'hello OR goodbye'"), "1 2 3")

	// NOT
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'hello -world'"), "3")

	// Table-level MATCH
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'hello'"), "1 3")

	// docid
	checkQueryResult(t, db.Query("SELECT docid FROM t1 WHERE t1 MATCH 'hello'"), "1 3")

	// SELECT content
	checkQueryResult(t, db.Query("SELECT content FROM t1 WHERE t1 MATCH 'goodbye'"), "goodbye world")
}

// TestFTS4 validates that FTS4 tables work (same as FTS3).
func TestFTS4(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts4(content)"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello world')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'hello'"), "1")
}

// TestFTSMultiColumn validates multi-column FTS tables.
func TestFTSMultiColumn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(title, body)"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello title', 'hello body')"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('other title', 'other body')"))

	// Column-specific MATCH
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE title MATCH 'hello'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE body MATCH 'hello'"), "1")

	// Column prefix in query
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'title:hello'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'body:hello'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'title:other'"), "2")
}

// TestFTSPhrase validates phrase matching.
func TestFTSPhrase(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content)"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello world foo')"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello foo world')"))

	// Phrase should only match consecutive terms
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH '\"hello world\"'"), "1")

	// Non-matching phrase
	r := db.Query("SELECT rowid FROM t1 WHERE content MATCH '\"world hello\"'")
	if r.Error != nil {
		t.Errorf("query error: %v", r.Error)
	}
	if len(r.Rows) != 0 {
		t.Errorf("expected 0 rows for non-matching phrase, got %d", len(r.Rows))
	}
}

// TestFTSUpdateDelete validates update and delete on FTS tables.
func TestFTSUpdateDelete(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content)"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello world')"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('goodbye world')"))

	// Both match 'world'
	r := db.Query("SELECT count(*) FROM t1 WHERE content MATCH 'world'")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Errorf("count before delete = %v, want [[2]]", r.Rows)
	}

	// Delete by rowid
	checkExecOK(t, db.Exec("DELETE FROM t1 WHERE rowid = 1"))

	// Only one remains
	r = db.Query("SELECT rowid FROM t1 WHERE content MATCH 'world'")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Errorf("after delete, rows = %v, want [[2]]", r.Rows)
	}
}

// TestFTSDefaultColumn validates FTS3 with no explicit columns.
func TestFTSDefaultColumn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts3"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello world')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE content MATCH 'hello'"), "1")
}

// TestFTSAuxFunctions validates snippet() and offsets() exist (non-functional).
func TestFTSAuxFunctions(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// These should return errors rather than crash
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content)"))
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('hello world')"))

	// snippet() - should return an error (not implemented) not crash
	r := db.Query("SELECT snippet(t1) FROM t1 WHERE t1 MATCH 'hello'")
	if r.Error == nil {
		t.Logf("snippet() succeeded unexpectedly (may change)")
	}

	// offsets() - should return an error (not implemented) not crash
	r = db.Query("SELECT offsets(t1) FROM t1 WHERE t1 MATCH 'hello'")
	if r.Error == nil {
		t.Logf("offsets() succeeded unexpectedly (may change)")
	}
}
