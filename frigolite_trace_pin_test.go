package frigolite_test

// Pin test for the statement-trace contract exercised by trace.test and
// trace3.test (FULL-SUITE-DRIFT pairs-pager cluster).
//
// C semantics (vdbeapi.c / tclsqlite.c):
//   - sqlite3_trace fires when a statement first begins running with the
//     statement text as prepared, INCLUDING its trailing semicolon
//     (trace-1.4: three entries "CREATE TABLE t1(a,b);" etc.);
//   - the text starts at the statement's first token (leading whitespace of
//     the script is not part of the statement);
//   - sqlite3_profile fires when the statement finishes with elapsed
//     nanoseconds (trace-3.4);
//   - sqlite3_trace_v2 masks: statement=1, profile=2, row=4, close=8 —
//     ROW events fire per produced row (trace3-5.x: 16 rows), PROFILE after
//     the rows (trace3-4.x), CLOSE on connection close (trace3-11.x).

import (
	"strconv"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestTraceHookContract(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}

	var traced []string
	db.SetTraceHook(func(sqlText string) {
		traced = append(traced, sqlText)
	})
	r := db.Query("\n    CREATE TABLE t1(a,b);\n    INSERT INTO t1 VALUES(1,2);\n    SELECT * FROM t1;\n  ")
	if r.Error != nil {
		t.Fatalf("script: %v", r.Error)
	}
	want := []string{"CREATE TABLE t1(a,b);", "INSERT INTO t1 VALUES(1,2);", "SELECT * FROM t1;"}
	if len(traced) != len(want) {
		t.Fatalf("traced %v, want %v", traced, want)
	}
	for i := range want {
		if traced[i] != want[i] {
			t.Fatalf("traced[%d]=%q, want %q", i, traced[i], want[i])
		}
	}

	// trace_v2: statement + row events; profile fires after the rows.
	var events []string
	db.SetTraceV2Hook(func(event int, id int64, text string) {
		events = append(events, strconv.Itoa(event)+":"+strconv.FormatInt(id, 10))
	}, frigolite.TraceStmt|frigolite.TraceProfile|frigolite.TraceRow)
	r = db.Query("SELECT * FROM t1")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	// stmt event, then one ROW event per row (1 row), then the PROFILE.
	if len(events) != 3 || events[0][0] != '1' {
		t.Fatalf("trace_v2 events: %v", events)
	}
	if events[1][0] != '4' {
		t.Fatalf("missing ROW event: %v", events)
	}
	if events[2][0] != '2' {
		t.Fatalf("missing PROFILE event: %v", events)
	}

	// CLOSE fires on connection close (trace3-11.x).
	closeFired := false
	db.SetTraceV2Hook(func(event int, id int64, text string) {
		if event == frigolite.TraceClose {
			closeFired = true
		}
	}, frigolite.TraceClose)
	db.Close()
	if !closeFired {
		t.Fatalf("CLOSE event did not fire")
	}
}
