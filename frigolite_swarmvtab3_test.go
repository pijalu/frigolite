package frigolite

// frigolite_swarmvtab3_test.go is the native Go port of SQLite's
// test/swarmvtab3.test (ext/misc/unionvtab.c swarmvtab module). It is the
// authoritative engine-contract test for the swarmvtab file-management
// semantics: missing= / openclose= UDF callbacks, maxopen LRU eviction and
// :param binding in the source query. The transpiled TCL package
// testgen/swarmvtab3 is superseded by this file (see AGENTS.md "Pure-Go
// supersession"); the TCL harness scaffolding (::dbcache mirror, CWD-relative
// file ops, rand()-based ctx names) is reproduced here with plain Go.
//
// Test layout mirrors the TCL file:
//   Section 1: 100 remote files remote_test.db$i; swarm table
//              swarm(id,tbl,minval,maxval); variants maxopen=5/3/1 including
//              the two-parameter :prefix/:suffix source query form.
//   Section 3: context-column form swarm(file,tbl,minval,maxval,ctx) with
//              2-arg missing_db and 3-arg openclose_db.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const swarmvtab3FileCount = 100

// swarmvtab3Fixture holds one test directory with its remote database files
// and the Go-side replica of the TCL ::dbcache observation map: a file
// test.db$i exists in the directory iff dbcache["test.db$i"] > 0.
type swarmvtab3Fixture struct {
	t       *testing.T
	dir     string
	dbcache map[string]int
}

// newSwarmvtab3Fixture creates an isolated fixture directory.
func newSwarmvtab3Fixture(t *testing.T) *swarmvtab3Fixture {
	t.Helper()
	return &swarmvtab3Fixture{
		t:       t,
		dir:     t.TempDir(),
		dbcache: make(map[string]int),
	}
}

// createRemoteFiles writes count databases remote_test.db$i, each holding
// table t1(a INTEGER PRIMARY KEY, b) with the single row (i,i).
func (f *swarmvtab3Fixture) createRemoteFiles(count int) {
	f.t.Helper()
	for i := 0; i < count; i++ {
		f.createRemoteFile(fmt.Sprintf("remote_test.db%d", i), i)
	}
}

// createRemoteFile writes one database name in the fixture directory holding
// table t1(a INTEGER PRIMARY KEY, b) with the single row (val,val).
func (f *swarmvtab3Fixture) createRemoteFile(name string, val int) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	src, err := Open(path)
	if err != nil {
		f.t.Fatalf("open %s: %v", name, err)
	}
	defer src.Close()
	res := src.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b)")
	if res.Error != nil {
		f.t.Fatalf("create t1 in %s: %s", name, res.Error)
	}
	res = src.Exec(fmt.Sprintf("INSERT INTO t1 VALUES(%d,%d)", val, val))
	if res.Error != nil {
		f.t.Fatalf("insert into %s: %s", name, res.Error)
	}
}

// resolve maps a source file name reported by the engine to a disk path.
// Names are absolute in this port (the :prefix option carries the fixture
// directory); relative names are interpreted inside the fixture directory
// for symmetry with the TCL test, which runs with CWD = testdir.
func (f *swarmvtab3Fixture) resolve(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(f.dir, name)
}

// copyIntoDir copies srcBase (a file in the fixture directory) to the source
// file name dst. It is the Go equivalent of TCL `file copy $src $filename`.
func (f *swarmvtab3Fixture) copyIntoDir(srcBase, dst string) error {
	data, err := os.ReadFile(f.resolve(srcBase))
	if err != nil {
		return err
	}
	return os.WriteFile(f.resolve(dst), data, 0o644)
}

// removeFromDir deletes name (TCL forcedelete).
func (f *swarmvtab3Fixture) removeFromDir(name string) {
	_ = os.Remove(f.resolve(name))
}

// registerUDFs installs the missing_db and openclose_db test functions on db.
// hasCtx selects the section-3 forms missing_db(filename,ctx) and
// openclose_db(filename,ctx,bclose).
func (f *swarmvtab3Fixture) registerUDFs(db *DB, hasCtx bool) {
	if hasCtx {
		db.RegisterFunction("missing_db", f.missingWithCtx, 2, 2)
		db.RegisterFunction("openclose_db", f.opencloseWithCtx, 3, 3)
		return
	}
	db.RegisterFunction("missing_db", f.missing, 1, 1)
	db.RegisterFunction("openclose_db", f.openclose, 2, 2)
}

// missing implements TCL `proc missing_db {filename}`: recreate the swarm
// source file by copying its remote_ master into place.
func (f *swarmvtab3Fixture) missing(args []interface{}) (interface{}, error) {
	name := swarmArgString(args[0])
	f.removeFromDir(name)
	master := filepath.Join(f.dir, "remote_"+filepath.Base(name))
	if err := f.copyIntoDir(master, name); err != nil {
		return nil, err
	}
	return nil, nil
}

// missingWithCtx implements TCL section-3 `proc missing_db {filename ctx}`:
// copy the per-context master file $ctx over $filename.
func (f *swarmvtab3Fixture) missingWithCtx(args []interface{}) (interface{}, error) {
	name := swarmArgString(args[0])
	ctx := swarmArgString(args[1])
	if err := f.copyIntoDir(ctx, name); err != nil {
		return nil, err
	}
	return nil, nil
}

// openclose implements TCL `proc openclose_db {filename bClose}`: maintain
// the dbcache reference count; when the count returns to zero the source
// file is deleted so checkDbcache can observe the LRU state on disk.
func (f *swarmvtab3Fixture) openclose(args []interface{}) (interface{}, error) {
	name := swarmArgString(args[0])
	return nil, f.bump(name, args[1])
}

// opencloseWithCtx is the section-3 form openclose_db(filename,ctx,bClose).
func (f *swarmvtab3Fixture) opencloseWithCtx(args []interface{}) (interface{}, error) {
	name := swarmArgString(args[0])
	return nil, f.bump(name, args[2])
}

// bump applies one openclose reference-count step for name.
func (f *swarmvtab3Fixture) bump(name string, bCloseArg interface{}) error {
	if swarmArgInt(bCloseArg) == 0 {
		f.dbcache[name]++
		return nil
	}
	f.dbcache[name]--
	if f.dbcache[name] <= 0 {
		delete(f.dbcache, name)
		f.removeFromDir(name)
	}
	return nil
}

// checkDbcache is TCL `proc check_dbcache {nTest nMaxOpen}`: the set of
// existing test.db* files must equal the set of positive dbcache entries,
// and their number must equal nMaxOpen (the LRU holds exactly maxopen
// sources open after each full query).
func (f *swarmvtab3Fixture) checkDbcache(maxopen int) {
	f.t.Helper()
	existing := 0
	for name := range f.dbcache {
		if _, err := os.Stat(f.resolve(name)); err != nil {
			f.t.Errorf("dbcache entry %s has no file on disk", name)
			continue
		}
		existing++
	}
	files := swarmListFiles(f.dir, "test.db")
	if len(files) != existing {
		f.t.Errorf("dbcache/file inconsistency: %d files on disk, %d dbcache entries",
			len(files), existing)
	}
	if existing != maxopen {
		f.t.Errorf("open files = %d, want maxopen %d (files: %v)", existing, maxopen, files)
	}
}

// swarmArgString renders a UDF argument the way TCL string comparison would.
func swarmArgString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// swarmArgInt converts a UDF numeric argument to int.
func swarmArgInt(v interface{}) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		var n int
		fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

// swarmListFiles returns the names of files in dir whose base name starts
// with prefix (TCL `glob test.db*`).
func swarmListFiles(dir, prefix string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	return names
}

// swarmQueryStrings runs q on db and returns the single-column result as
// strings, in row order (do_execsql_test compares ordered lists).
func swarmQueryStrings(t *testing.T, db *DB, q string) []string {
	t.Helper()
	res := db.Query(q)
	if res.Error != nil {
		t.Fatalf("query %q: %s", q, res.Error)
	}
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) == 0 {
			t.Fatalf("query %q: empty row", q)
		}
		out = append(out, swarmArgString(row[0]))
	}
	return out
}

// swarmExpectRows asserts the query result equals want, element by element.
func swarmExpectRows(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %s, want %s (full result %v)", i, got[i], want[i], got)
		}
	}
}

// swarmRangeStrings builds {"0","1",...,"n-1"} — the expected result of
// `SELECT b FROM s WHERE a < n`.
func swarmRangeStrings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%d", i)
	}
	return out
}

// swarmDecadeStrings builds {"0","10",...,"90"} — the expected result of
// `SELECT b FROM s WHERE (b%10)=0`.
func swarmDecadeStrings() []string {
	out := make([]string, 0, 10)
	for i := 0; i < swarmvtab3FileCount; i += 10 {
		out = append(out, fmt.Sprintf("%d", i))
	}
	return out
}

// TestSwarmvtab3_Section1 ports swarmvtab3.test section 1: three swarmvtab
// variants over the same 100 remote files, one per maxopen value 5, 3 and 1.
// Variant 3 additionally exercises the two-parameter source query form
// (:prefix||'.'||:suffix||id with bare-word option values).
func TestSwarmvtab3_Section1(t *testing.T) {
	f := newSwarmvtab3Fixture(t)
	f.createRemoteFiles(swarmvtab3FileCount)

	db, err := Open(filepath.Join(f.dir, "main.db"))
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	defer db.Close()
	f.registerUDFs(db, false)

	res := db.Exec("CREATE TEMP TABLE swarm(id, tbl, minval, maxval)")
	if res.Error != nil {
		t.Fatalf("create swarm: %s", res.Error)
	}
	for i := 0; i < swarmvtab3FileCount; i++ {
		res = db.Exec(fmt.Sprintf("INSERT INTO swarm VALUES(%d,'t1',%d,%d)", i, i, i))
		if res.Error != nil {
			t.Fatalf("insert swarm row %d: %s", i, res.Error)
		}
	}

	// TCL uses relative file names (CWD = testdir); this port passes the
	// fixture directory through the :prefix option so names are absolute.
	prefix := filepath.Join(f.dir, "test.db")
	variants := []struct {
		sel  string
		opts string
		open int
	}{
		// TCL $tn=1: maxopen=5, single :prefix parameter.
		{"SELECT :prefix||id, tbl, minval, minval FROM swarm",
			fmt.Sprintf(":prefix='%s', maxopen=5", prefix), 5},
		// TCL $tn=2: maxopen=3.
		{"SELECT :prefix||id, tbl, minval, minval FROM swarm",
			fmt.Sprintf(":prefix='%s', maxopen=3", prefix), 3},
		// TCL $tn=3: maxopen=1, two parameters joined by a dot; the suffix
		// stays a bare word to keep the unquoted-value parse path covered.
		{"SELECT :prefix||'.'||:suffix||id, tbl, minval, minval FROM swarm",
			fmt.Sprintf(":prefix='%s', :suffix=db, maxopen=1", filepath.Join(f.dir, "test")), 1},
	}
	for vn, v := range variants {
		stmt := fmt.Sprintf(
			"CREATE VIRTUAL TABLE temp.s USING swarmvtab('%s', %s, missing=missing_db, openclose=openclose_db)",
			v.sel, v.opts)
		res = db.Exec(stmt)
		if res.Error != nil {
			t.Fatalf("variant %d create vtab: %s", vn+1, res.Error)
		}
		f.runSwarmQueries(db, v.open)
		res = db.Exec("DROP TABLE s")
		if res.Error != nil {
			t.Fatalf("variant %d drop s: %s", vn+1, res.Error)
		}
		// TCL resets ::dbcache between variants; the dropped table already
		// released its sources through openclose(bClose=1), so the map is
		// empty here. Any leftover would be caught by checkDbcache.
		if len(f.dbcache) != 0 {
			t.Fatalf("variant %d: dbcache not empty after DROP TABLE: %v", vn+1, f.dbcache)
		}
	}
}

// TestSwarmvtab3_Section3 ports swarmvtab3.test section 3: the context-column
// form where each swarm row carries its own master-file name (ctx column) and
// the missing/openclose UDFs receive it as an extra argument.
func TestSwarmvtab3_Section3(t *testing.T) {
	f := newSwarmvtab3Fixture(t)

	db, err := Open(filepath.Join(f.dir, "main.db"))
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	defer db.Close()
	f.registerUDFs(db, true)

	res := db.Exec("CREATE TEMP TABLE swarm(file, tbl, minval, maxval, ctx)")
	if res.Error != nil {
		t.Fatalf("create swarm: %s", res.Error)
	}
	// Deterministic distinct ctx names replace TCL's random ctx selection;
	// the engine contract only requires a unique master file per source.
	for i := 0; i < swarmvtab3FileCount; i++ {
		ctx := fmt.Sprintf("test_remote.db%d", 100000+i)
		f.createRemoteFile(ctx, i)
		file := filepath.Join(f.dir, fmt.Sprintf("test.db%d", i))
		res = db.Exec(fmt.Sprintf(
			"INSERT INTO swarm VALUES('%s','t1',%d,%d,'%s')", file, i, i, ctx))
		if res.Error != nil {
			t.Fatalf("insert swarm row %d: %s", i, res.Error)
		}
	}

	res = db.Exec("CREATE VIRTUAL TABLE temp.s USING swarmvtab(" +
		"'SELECT file, tbl, minval, minval, ctx FROM swarm', " +
		"missing=missing_db, openclose=openclose_db, maxopen=5)")
	if res.Error != nil {
		t.Fatalf("create vtab: %s", res.Error)
	}
	f.runSwarmQueries(db, 5)
}

// runSwarmQueries performs the two section-1/3 query assertions on virtual
// table s and checks the open-file LRU state after each (TCL 1.2/1.4, 3.2/3.4).
func (f *swarmvtab3Fixture) runSwarmQueries(db *DB, maxopen int) {
	f.t.Helper()
	got := swarmQueryStrings(f.t, db, "SELECT b FROM s WHERE a < 10")
	swarmExpectRows(f.t, got, swarmRangeStrings(10))
	f.checkDbcache(maxopen)
	got = swarmQueryStrings(f.t, db, "SELECT b FROM s WHERE (b%10) = 0")
	swarmExpectRows(f.t, got, swarmDecadeStrings())
	f.checkDbcache(maxopen)
}
