package frigolite

// frigolite_swarmvtab2_test.go is the native Go port of SQLite's
// test/swarmvtab2.test (ext/misc/unionvtab.c swarmvtab module). It covers
// the positional missing-UDF form — swarmvtab('SELECT * FROM t1',
// 'create_database') — where each source names a not-yet-existing database
// file that the missing= UDF creates lazily on first touch, and observes
// the maxopen LRU through the set of files existing on disk (TCL `glob`
// after each query). The transpiled TCL package testgen/swarmvtab2 is
// superseded by this file (see AGENTS.md "Pure-Go supersession"): the TCL
// version's CWD-relative file creation and glob observation are reproduced
// here against an isolated fixture directory with absolute file names.
//
// Test layout mirrors the TCL file:
//   100: create t1 + v1 over 99 not-yet-existing source files
//   110/120: point lookup a=3875 creates exactly test001.db + test003.db
//   130/140: range BETWEEN 3999 AND 4000 adds test004.db
//   150/160: a>=99998 adds test099.db (dictionary-sorted glob)

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// swarmvtab2Dir is the fixture directory holding the lazily created source
// databases test001.db … test099.db plus the main test.db.
type swarmvtab2Dir struct {
	t   *testing.T
	dir string
}

func newSwarmvtab2Dir(t *testing.T) *swarmvtab2Dir {
	t.Helper()
	return &swarmvtab2Dir{t: t, dir: t.TempDir()}
}

// createDatabase implements TCL `proc create_database {filename}`: build the
// source database for one swarm source — t2(a INTEGER PRIMARY KEY, b) with
// rows num*1000 … num*1000+999 where num is the digit run in the file name
// (regsub [^0-9]+ then trimleft 0), b = printf('**%05d**', a).
func (f *swarmvtab2Dir) createDatabase(args []interface{}) (interface{}, error) {
	name := swarmArgString(args[0])
	num, err := swarmvtab2FileNum(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	start := num * 1000
	src, err := Open(f.resolve(name))
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if res := src.Exec("CREATE TABLE t2(a INTEGER PRIMARY KEY,b)"); res.Error != nil {
		return nil, res.Error
	}
	// The TCL test fills 1000 rows via a recursive CTE; a Go loop with one
	// multi-row INSERT per chunk keeps the fixture fast without changing
	// the resulting file contents.
	const chunk = 100
	for base := start; base < start+1000; base += chunk {
		var sb strings.Builder
		sb.WriteString("INSERT INTO t2(a,b) VALUES ")
		for i := 0; i < chunk; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(%d,'**%05d**')", base+i, base+i)
		}
		if res := src.Exec(sb.String()); res.Error != nil {
			return nil, res.Error
		}
	}
	return nil, nil
}

// swarmvtab2FileNum extracts the integer formed by the digit run of base
// (TCL regsub -all {[^0-9]+} $filename {} then string trimleft 0).
func swarmvtab2FileNum(base string) (int, error) {
	var digits strings.Builder
	for _, r := range base {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	s := strings.TrimLeft(digits.String(), "0")
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// resolve maps a source file name to a disk path inside the fixture
// directory (this port passes absolute names through the source query, but
// relative names — as the TCL CWD=testdir run would produce — resolve
// inside the directory too).
func (f *swarmvtab2Dir) resolve(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(f.dir, name)
}

// globDbs returns the sorted base names of existing test???.db source files
// (TCL `glob -nocomplain test?*.db`; the main test.db is excluded because it
// has no digit run after "test").
func (f *swarmvtab2Dir) globDbs() []string {
	matches, _ := filepath.Glob(filepath.Join(f.dir, "test?*.db"))
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	sort.Strings(names)
	return names
}

// expectGlob asserts the on-disk source file set (TCL do_test lsort [glob]).
func (f *swarmvtab2Dir) expectGlob(want ...string) {
	f.t.Helper()
	got := f.globDbs()
	if len(got) != len(want) {
		f.t.Fatalf("glob = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			f.t.Fatalf("glob = %v, want %v", got, want)
		}
	}
}

// TestSwarmvtab2_PositionalMissingUDF ports swarmvtab2.test in full: the
// single-positional-argument missing= UDF form with lazy source creation and
// the default maxopen LRU observed through the file system.
func TestSwarmvtab2_PositionalMissingUDF(t *testing.T) {
	f := newSwarmvtab2Dir(t)

	db, err := Open(filepath.Join(f.dir, "test.db"))
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	defer db.Close()
	db.RegisterFunction("create_database", f.createDatabase, 1, 1)

	// TCL 100: t1(filename,tablename,istart,iend) with 99 rows naming
	// test001.db…test099.db (absolute here), then the swarm vtab.
	res := db.Exec("CREATE TABLE t1(filename, tablename, istart, iend)")
	if res.Error != nil {
		t.Fatalf("create t1: %s", res.Error)
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO t1 VALUES ")
	for x := 1; x < 100; x++ {
		if x > 1 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "('%s','t2',%d,%d)",
			filepath.Join(f.dir, fmt.Sprintf("test%03d.db", x)), x*1000, x*1000+999)
	}
	if res = db.Exec(sb.String()); res.Error != nil {
		t.Fatalf("insert t1: %s", res.Error)
	}
	res = db.Exec("CREATE VIRTUAL TABLE temp.v1 USING swarmvtab(" +
		"'SELECT * FROM t1', 'create_database')")
	if res.Error != nil {
		t.Fatalf("create v1: %s", res.Error)
	}

	// TCL 110/120: point lookup inside source 3; only the sources actually
	// touched exist afterwards (source 1 was opened at CREATE time, source 3
	// was created by the missing= UDF).
	got := swarmQueryStrings(t, db, "SELECT b FROM v1 WHERE a=3875")
	swarmExpectRows(t, got, []string{"**03875**"})
	f.expectGlob("test001.db", "test003.db")

	// TCL 130/140: a range spanning sources 3 and 4 adds test004.db.
	got = swarmQueryStrings(t, db, "SELECT b FROM v1 WHERE a BETWEEN 3999 AND 4000 ORDER BY a")
	swarmExpectRows(t, got, []string{"**03999**", "**04000**"})
	f.expectGlob("test001.db", "test003.db", "test004.db")

	// TCL 150/160: the tail range adds test099.db.
	got = swarmQueryStrings(t, db, "SELECT b FROM v1 WHERE a>=99998")
	swarmExpectRows(t, got, []string{"**99998**", "**99999**"})
	f.expectGlob("test001.db", "test003.db", "test004.db", "test099.db")
}
