package frigolite_test

// R13-L6 pin: the typed SET lane's literal arms (update_setlane.go
// compileLaneOperandRef) must mirror the generic evaluator's literal
// semantics exactly — including SQLITE_LIMIT_LENGTH, which EvalExpr applies
// through evalBoundedLiteral for OP_String8 and OP_Blob literals
// (expression.go:62, sqllimits1-5.17.1). The lane bails on an over-limit
// literal so the generic path raises "string or blob too big"; a lane that
// silently took the store would lose the error.

import (
	"strings"
	"testing"

	frigo "github.com/pijalu/frigolite"
)

// TestUpdateSetLaneLiteralLengthLimit pins that an over-limit text literal in
// a point-UPDATE SET stores nothing and reports the TOOBIG error, and that a
// literal at the limit still stores.
func TestUpdateSetLaneLiteralLengthLimit(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prev := db.SetLimit("SQLITE_LIMIT_LENGTH", 100)
	defer db.SetLimit("SQLITE_LIMIT_LENGTH", prev)
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES(1, 'seed')")

	// Exactly at the limit: stored.
	atLimit := strings.Repeat("a", 100)
	if res := db.Exec("UPDATE t SET c='" + atLimit + "' WHERE id=1"); res.Error != nil {
		t.Fatalf("at-limit literal: %v", res.Error)
	}
	if got := db.Query("SELECT length(c) FROM t WHERE id=1"); got.Error != nil {
		t.Fatal(got.Error)
	} else if got.Rows[0][0] != int64(100) {
		t.Fatalf("at-limit store length = %v, want 100", got.Rows[0][0])
	}

	// One byte over: SQLITE_TOOBIG, and the row keeps its previous value.
	overLimit := strings.Repeat("b", 101)
	res := db.Exec("UPDATE t SET c='" + overLimit + "' WHERE id=1")
	if res.Error == nil {
		t.Fatalf("over-limit literal stored without error")
	}
	if !strings.Contains(res.Error.Error(), "string or blob too big") {
		t.Fatalf("over-limit error = %q, want \"string or blob too big\"", res.Error)
	}
	if got := db.Query("SELECT length(c) FROM t WHERE id=1"); got.Error != nil {
		t.Fatal(got.Error)
	} else if got.Rows[0][0] != int64(100) {
		t.Fatalf("failed statement changed the row: length = %v, want 100", got.Rows[0][0])
	}

	// The same limits apply to a blob literal store.
	blob := "x'" + strings.Repeat("cd", 101) + "'" // 202 bytes
	blobRes := db.Exec("UPDATE t SET c=" + blob + " WHERE id=1")
	if blobRes.Error == nil || !strings.Contains(blobRes.Error.Error(), "string or blob too big") {
		t.Fatalf("over-limit blob error = %v, want \"string or blob too big\"", blobRes.Error)
	}
}
