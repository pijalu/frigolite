package frigolite

import (
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// s66BlobPair is one deterministic 100-byte (a,b) payload pair, hex encoded
// for direct use in SQL blob literals.
type s66BlobPair struct {
	a, b string
}

// s66BlobPairs generates the S66 fixture with an LCG so the same keys are
// inserted into both engines. math/rand is deliberately avoided: its stream
// is not reproducible across runs, which made earlier page-count comparisons
// meaningless (see portplan/PLAN_2026-11-26_S66_INDEX_DENSITY.md).
func s66BlobPairs() []s66BlobPair {
	var state uint64 = 88172645463325252
	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state
	}
	pairs := make([]s66BlobPair, 500)
	for i := range pairs {
		a, b := make([]byte, 100), make([]byte, 100)
		for j := range a {
			a[j], b[j] = byte(next()), byte(next())
		}
		pairs[i] = s66BlobPair{hex.EncodeToString(a), hex.EncodeToString(b)}
	}
	return pairs
}

// TestS66SQLiteFileCompatibility proves the accepted engine-only index
// density variance does not affect the SQLite file-format contract. Both
// engines write the same 500-row two-column blob index and then open the
// other engine's file, checking integrity_check and the full row set in
// both directions.
func TestS66SQLiteFileCompatibility(t *testing.T) {
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 oracle unavailable")
	}
	pairs := s66BlobPairs()
	want := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		want[p.a+":"+p.b] = true
	}

	var insert strings.Builder
	insert.WriteString("BEGIN;")
	for _, p := range pairs {
		fmt.Fprintf(&insert, "INSERT INTO t2 VALUES(X'%s',X'%s');", p.a, p.b)
	}
	insert.WriteString("COMMIT;")

	dir := t.TempDir()
	frigPath, sqlitePath := filepath.Join(dir, "frigolite.db"), filepath.Join(dir, "sqlite.db")

	// Frigolite-written file.
	fdb, err := Open(frigPath)
	if err != nil {
		t.Fatal(err)
	}
	if r := fdb.Exec("PRAGMA page_size=1024; CREATE TABLE t2(a,b); CREATE INDEX i2 ON t2(a,b);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := fdb.Exec(insert.String()); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := fdb.Close(); err != nil {
		t.Fatal(err)
	}

	// sqlite3-written file, same schema and rows.
	var script strings.Builder
	script.WriteString("PRAGMA page_size=1024;\nCREATE TABLE t2(a,b);\nCREATE INDEX i2 ON t2(a,b);\n")
	script.WriteString(insert.String())
	script.WriteByte('\n')
	cmd := exec.Command(sqlite3, sqlitePath)
	cmd.Stdin = strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 write: %v (%s)", err, out)
	}

	// Direction 1: sqlite3 opens the Frigolite file.
	cmd = exec.Command(sqlite3, "-noheader", frigPath,
		"PRAGMA integrity_check;",
		"SELECT count(*) FROM t2;",
		"SELECT hex(a)||':'||hex(b) FROM t2;")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 read Frigolite file: %v (%s)", err, out)
	}
	assertS66SQLiteReport(t, string(out), "Frigolite-written", want)

	// Direction 2: Frigolite opens the sqlite3 file.
	sdb, err := Open(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	if r := sdb.Query("PRAGMA integrity_check;"); r.Error != nil {
		t.Fatal(r.Error)
	} else if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("Frigolite integrity_check on sqlite3 file = %#v, want ok", r.Rows)
	}
	if r := sdb.Query("SELECT count(*) FROM t2;"); r.Error != nil {
		t.Fatal(r.Error)
	} else if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != int64(500) {
		t.Fatalf("Frigolite SELECT count(*) on sqlite3 file = %#v, want 500", r.Rows)
	}
	if r := sdb.Query("SELECT hex(a)||':'||hex(b) FROM t2;"); r.Error != nil {
		t.Fatal(r.Error)
	} else {
		got := make(map[string]bool, len(r.Rows))
		for _, row := range r.Rows {
			if len(row) != 1 {
				t.Fatalf("row shape = %#v, want one column", row)
			}
			s, ok := row[0].(string)
			if !ok {
				t.Fatalf("row value type = %T, want string", row[0])
			}
			got[strings.ToLower(s)] = true
		}
		assertS66RowSet(t, "Frigolite read of sqlite3 file", got, want)
	}
}

// assertS66SQLiteReport parses the -noheader sqlite3 report: integrity_check,
// row count, then one hex key per row.
func assertS66SQLiteReport(t *testing.T, out, label string, want map[string]bool) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("%s: sqlite3 report = %q, want integrity + count lines", label, out)
	}
	if got := strings.TrimSpace(lines[0]); got != "ok" {
		t.Errorf("%s: sqlite3 PRAGMA integrity_check = %q, want ok", label, got)
	}
	if got := strings.TrimSpace(lines[1]); got != "500" {
		t.Errorf("%s: sqlite3 SELECT count(*) = %q, want 500", label, got)
	}
	got := make(map[string]bool, len(lines)-2)
	for _, line := range lines[2:] {
		if s := strings.TrimSpace(line); s != "" {
			got[strings.ToLower(s)] = true
		}
	}
	assertS66RowSet(t, "sqlite3 read of "+label+" file", got, want)
}

// assertS66RowSet compares the complete 500-row payload set, not just counts.
func assertS66RowSet(t *testing.T, label string, got, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d distinct rows, want %d", label, len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s: missing row %s", label, k)
			break
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s: unexpected row %s", label, k)
			break
		}
	}
}
