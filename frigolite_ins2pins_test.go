package frigolite

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pins for PERF.INSERT2 (fleet/perf-insert2): the insert write path now
// reuses pooled split staging, an executor-owned row cell/record/IPK scratch,
// a depth-indexed page-header scratch, and ONE cached b-tree wrapper for the
// row write + the O(log n) duplicate-PK probe (released via Cursor.Close).
// Every buffer-reuse path needs a pin that earlier rows never mutate when
// later rows recycle the scratch, and every split/freeblock shape needs
// integrity_check + file-growth evidence.

func mustExecIns2(t *testing.T, db *DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("exec %q: %v", sql, r.Error)
	}
}

func mustQueryIns2(t *testing.T, db *DB, sql string) [][]interface{} {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %q: %v", sql, r.Error)
	}
	return r.Rows
}

func integrityOKIns2(t *testing.T, db *DB) {
	t.Helper()
	for _, row := range mustQueryIns2(t, db, "PRAGMA integrity_check") {
		if got, _ := row[0].(string); got != "ok" {
			t.Fatalf("integrity_check: %s", got)
		}
	}
}

// TestIns2PinIntegrityAfterSplits drives sequential, random-order, and
// WITHOUT ROWID insert workloads through many leaf/interior splits (with
// mid-transaction delete+reinsert freeblock churn) and requires
// integrity_check to report ok after every shape.
func TestIns2PinIntegrityAfterSplits(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecIns2(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER, s TEXT)")

	// Sequential: every split is a tail split; the tree grows right.
	const n = 20000
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= n; i++ {
		mustExecIns2(t, db, "INSERT INTO t VALUES("+itoaIns2(i)+","+itoaIns2(i*7%1000003)+", 's"+itoaIns2(i)+"')")
	}
	mustExecIns2(t, db, "COMMIT")
	integrityOKIns2(t, db)

	// Mid-transaction delete + reinsert: freeblock reuse and defragmentation
	// on the same pages the split staging walks.
	mustExecIns2(t, db, "BEGIN")
	for i := 3; i <= n; i += 3 {
		mustExecIns2(t, db, "DELETE FROM t WHERE id="+itoaIns2(i))
	}
	for i := 3; i <= n; i += 3 {
		mustExecIns2(t, db, "INSERT INTO t VALUES("+itoaIns2(i)+","+itoaIns2(i*7%1000003)+", 'r"+itoaIns2(i)+"')")
	}
	mustExecIns2(t, db, "COMMIT")
	integrityOKIns2(t, db)
	if got := len(mustQueryIns2(t, db, "SELECT * FROM t")); got != n {
		t.Fatalf("row count after delete+reinsert: %d", got)
	}

	// Random order: interior splits in the middle of the key space.
	mustExecIns2(t, db, "CREATE TABLE r(id INTEGER PRIMARY KEY, c INTEGER)")
	rng := rand.New(rand.NewSource(42))
	perm := rng.Perm(4 * n / 3)
	wantRandom := 0
	mustExecIns2(t, db, "BEGIN")
	for _, k := range perm {
		if k == 0 {
			continue
		}
		wantRandom++
		mustExecIns2(t, db, "INSERT OR IGNORE INTO r VALUES("+itoaIns2(k)+","+itoaIns2(k*13)+")")
	}
	mustExecIns2(t, db, "COMMIT")
	integrityOKIns2(t, db)
	if got := len(mustQueryIns2(t, db, "SELECT * FROM r")); got != wantRandom {
		t.Fatalf("random row count: %d want %d", got, wantRandom)
	}

	// WITHOUT ROWID shapes: index-btree splits with random TEXT keys.
	mustExecIns2(t, db, "CREATE TABLE w(a TEXT PRIMARY KEY, b INTEGER) WITHOUT ROWID")
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= 6000; i++ {
		mustExecIns2(t, db, "INSERT INTO w VALUES('k"+itoaIns2(rng.Intn(100000))+"-"+itoaIns2(i)+"', "+itoaIns2(i)+")")
	}
	mustExecIns2(t, db, "COMMIT")
	integrityOKIns2(t, db)
}

// TestIns2PinNoAliasingAcrossRows requires rows written EARLIER to hold
// their exact bytes after thousands of later rows reused the cell, record,
// IPK-alias, split-staging, and page-header scratch. Any stale-buffer bug
// (a later row's encode overwriting an earlier row's page bytes, divider
// payload, or overflow chain) shows up as a corrupted read-back.
func TestIns2PinNoAliasingAcrossRows(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecIns2(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER, s TEXT, b BLOB, r REAL)")
	const n = 30000
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= n; i++ {
		mustExecIns2(t, db, "INSERT INTO t VALUES(" + itoaIns2(i) + "," + itoaIns2(i*7%1000003) +
			",'row-" + itoaIns2(i) + "-end', x'" + hexRepIns2(i, 24) + "'," + rtoaIns2(i) + ")")
	}
	mustExecIns2(t, db, "COMMIT")

	rows := mustQueryIns2(t, db, "SELECT id, c, s, b, r FROM t ORDER BY id")
	if len(rows) != n {
		t.Fatalf("row count: %d", len(rows))
	}
	for i, row := range rows {
		id := i + 1
		if got := row[0].(int64); got != int64(id) {
			t.Fatalf("row %d: id=%v", id, got)
		}
		if got := row[1].(int64); got != int64(id*7%1000003) {
			t.Fatalf("row %d: c=%v", id, got)
		}
		if got := row[2].(string); got != "row-"+itoaIns2(id)+"-end" {
			t.Fatalf("row %d: s=%q", id, got)
		}
		if got := string(row[3].([]byte)); got != hexDecIns2(id, 24) {
			t.Fatalf("row %d: b=%q", id, got)
		}
		if got := row[4].(float64); got != float64(id)/8 {
			t.Fatalf("row %d: r=%v", id, got)
		}
	}

	// WITHOUT ROWID: storage-order re-encode + PK probe on the cached tree.
	mustExecIns2(t, db, "CREATE TABLE w(k TEXT PRIMARY KEY, v INTEGER, x REAL) WITHOUT ROWID")
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= n; i++ {
		mustExecIns2(t, db, "INSERT INTO w VALUES('key"+itoaIns2(i)+"',"+itoaIns2(i*3)+","+rtoaIns2(i)+")")
	}
	mustExecIns2(t, db, "COMMIT")
	rows = mustQueryIns2(t, db, "SELECT k, v, x FROM w ORDER BY v")
	if len(rows) != n {
		t.Fatalf("wr row count: %d", len(rows))
	}
	for i, row := range rows {
		id := i + 1
		if got := row[0].(string); got != "key"+itoaIns2(id) {
			t.Fatalf("wr row %d: k=%q", id, got)
		}
		if got := row[1].(int64); got != int64(id*3) {
			t.Fatalf("wr row %d: v=%v", id, got)
		}
	}
}

// TestIns2PinDuplicatePKProbeOnCachedTree drives the O(log n) duplicate-PK
// probe against the SAME cached wrapper the row write uses: every duplicate
// must raise the exact UNIQUE text, every non-duplicate must land, and the
// table must stay intact (the probe's cursor is released via Cursor.Close so
// the cached tree is cursor-free for the next write).
func TestIns2PinDuplicatePKProbeOnCachedTree(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecIns2(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c TEXT)")
	for i := 1; i <= 2000; i++ {
		mustExecIns2(t, db, "INSERT INTO t VALUES("+itoaIns2(i)+",'v"+itoaIns2(i)+"')")
	}
	// Duplicates at the head, middle, and tail of the key space — the probe
	// seek lands on different leaves of the split-grown tree.
	for _, dup := range []int{1, 2, 1000, 1999, 2000} {
		r := db.Exec("INSERT INTO t VALUES(" + itoaIns2(dup) + ",'dup')")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "UNIQUE constraint failed: t.id") {
			t.Fatalf("dup %d: want UNIQUE error, got %v", dup, r.Error)
		}
	}
	if got := len(mustQueryIns2(t, db, "SELECT * FROM t")); got != 2000 {
		t.Fatalf("row count after dup probes: %d", got)
	}
	integrityOKIns2(t, db)

	// OR IGNORE dups skip; the cached wrapper survives every probe.
	for i := 1; i <= 200; i++ {
		mustExecIns2(t, db, "INSERT OR IGNORE INTO t VALUES("+itoaIns2(i)+",'ignored')")
	}
	if got := len(mustQueryIns2(t, db, "SELECT * FROM t")); got != 2000 {
		t.Fatalf("row count after OR IGNORE dups: %d", got)
	}
}

// TestIns2PinTriggerNestedInsertNotTainted inserts through a table whose
// trigger writes another table: the nested statement's insert must not
// corrupt the outer row's values (which flow through the executor's
// scratch) and both trees must stay consistent across the cache identity
// switch (outer table <-> trigger target alternate every row).
func TestIns2PinTriggerNestedInsertNotTainted(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecIns2(t, db, "CREATE TABLE a(id INTEGER PRIMARY KEY, s TEXT)")
	mustExecIns2(t, db, "CREATE TABLE b(id INTEGER PRIMARY KEY, s TEXT)")
	mustExecIns2(t, db, "CREATE TRIGGER ai AFTER INSERT ON a BEGIN INSERT INTO b VALUES(new.id, 'mirror-' || new.s); END")
	const n = 5000
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= n; i++ {
		mustExecIns2(t, db, "INSERT INTO a VALUES("+itoaIns2(i)+",'s"+itoaIns2(i)+"')")
	}
	mustExecIns2(t, db, "COMMIT")
	for _, row := range mustQueryIns2(t, db, "SELECT id, s FROM a ORDER BY id") {
		id := row[0].(int64)
		if got := row[1].(string); got != "s"+itoaIns2(int(id)) {
			t.Fatalf("a row %d: s=%q", id, got)
		}
	}
	for _, row := range mustQueryIns2(t, db, "SELECT id, s FROM b ORDER BY id") {
		id := row[0].(int64)
		if got := row[1].(string); got != "mirror-s"+itoaIns2(int(id)) {
			t.Fatalf("b row %d: s=%q", id, got)
		}
	}
	integrityOKIns2(t, db)
}

// TestIns2PinPageReuseAfterDelete requires freed page space to be recycled:
// after deleting the tail of a table in a file database, re-inserting rows
// into the DELETED key range must absorb them through freeblock reuse and
// defragmentation without growing the file (the freed bytes live inside the
// surviving leaves; a tail-range insert reuses exactly that space).
func TestIns2PinPageReuseAfterDelete(t *testing.T) {
	fpath := filepath.Join(t.TempDir(), "reuse.db")
	db, err := Open(fpath)
	if err != nil {
		t.Fatal(err)
	}
	mustExecIns2(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT)")
	const n = 20000
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= n; i++ {
		mustExecIns2(t, db, "INSERT INTO t VALUES("+itoaIns2(i)+",'pad"+itoaIns2(i)+"-"+strings.Repeat("x", 40)+"')")
	}
	mustExecIns2(t, db, "COMMIT")
	sizeAfterInsert := fileSizeIns2(t, fpath)

	// Delete the tail half, then re-insert half of THAT range (same key
	// span, smaller volume — it must fit the freed bytes with no growth).
	mustExecIns2(t, db, "DELETE FROM t WHERE id > "+itoaIns2(n/2))
	mustExecIns2(t, db, "BEGIN")
	for i := 1; i <= n/4; i++ {
		mustExecIns2(t, db, "INSERT INTO t VALUES("+itoaIns2(n/2+i)+",'pad"+itoaIns2(i)+"-"+strings.Repeat("y", 40)+"')")
	}
	mustExecIns2(t, db, "COMMIT")
	sizeAfterReinsert := fileSizeIns2(t, fpath)
	if sizeAfterReinsert > sizeAfterInsert {
		t.Fatalf("file grew after free-space reinsert: %d > %d", sizeAfterReinsert, sizeAfterInsert)
	}
	if got := len(mustQueryIns2(t, db, "SELECT * FROM t")); got != n-n/2+n/4 {
		t.Fatalf("row count: %d", got)
	}
	integrityOKIns2(t, db)
	db.Close()
}

func fileSizeIns2(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func itoaIns2(v int) string {
	return fmtIntIns2(int64(v))
}

func fmtIntIns2(v int64) string {
	// Small local formatter: avoids fmt in the hot pin loops.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func rtoaIns2(i int) string {
	// i/8 as a decimal REAL literal (exact in binary: eighths terminate).
	return fmtIntIns2(int64(i/8)) + "." + fmtIntIns2(int64(i%8)*125)
}

func hexRepIns2(i, n int) string {
	const hex = "0123456789abcdef"
	var sb strings.Builder
	seed := i
	for j := 0; j < n; j++ {
		seed = seed*1103515245 + 12345
		sb.WriteByte(hex[(seed>>16)&0xF])
	}
	return sb.String()
}

// hexDecIns2 is hexRepIns2 decoded to raw bytes (what a x'..' literal stores
// and a BLOB read-back returns).
func hexDecIns2(i, n int) string {
	rep := hexRepIns2(i, n)
	b := make([]byte, n/2)
	for j := 0; j < n/2; j++ {
		hi := strings.IndexByte("0123456789abcdef", rep[2*j])
		lo := strings.IndexByte("0123456789abcdef", rep[2*j+1])
		b[j] = byte(hi<<4 | lo)
	}
	return string(b)
}
