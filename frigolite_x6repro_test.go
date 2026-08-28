package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestX6Repro reproduces the fts4growth x6 sequence (steps 7.1–7.5) directly
// against the engine to debug the end_block suffix / block-sum divergence
// without the tcl2go wrapper.
func TestX6Repro(t *testing.T) {
	dir := t.TempDir()
	genesisPath := ""
	// Anchor the fixture to THIS source file: other tests in the package
	// Chdir away, so repo-relative paths are not stable here.
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		genesisPath = filepath.Join(filepath.Dir(thisFile),
			"internal", "fts", "testdata", "ftsconformance", "genesis_t1.sql")
	}
	genesis, err := os.ReadFile(genesisPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	db, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec(string(genesis)); res != nil && res.Error != nil {
		t.Fatalf("genesis: %v", res.Error)
	}
	// Mirror the tcl2go harness's `first` helper: element 0 of a space-split
	// list (fts4growth registers the same function before step 7.3).
	db.RegisterFunction("first", func(args []interface{}) (interface{}, error) {
		if len(args) < 1 || args[0] == nil {
			return nil, nil
		}
		s := fmt.Sprintf("%v", args[0])
		if i := strings.IndexByte(s, ' '); i >= 0 {
			return s[:i], nil
		}
		return s, nil
	}, 0, -1)

	steps := []struct {
		name string
		sql  string
	}{
		{"7.1", `CREATE VIRTUAL TABLE x6 USING fts4;
			INSERT INTO x6 SELECT words FROM t1;
			INSERT INTO x6 SELECT words FROM t1;
			INSERT INTO x6 SELECT words FROM t1;
			INSERT INTO x6 SELECT words FROM t1;
			INSERT INTO x6 SELECT words FROM t1;
			INSERT INTO x6 SELECT words FROM t1;
			SELECT level, idx, start_block, leaves_end_block, end_block FROM x6_segdir;`},
		{"7.2", `INSERT INTO x6(x6) VALUES('merge=25,4');
			SELECT level, idx, start_block, leaves_end_block, end_block FROM x6_segdir;`},
		{"7.3", `UPDATE x6_segdir SET end_block = first(end_block) WHERE level=1;
			SELECT level, idx, start_block, leaves_end_block, end_block FROM x6_segdir;`},
		{"7.4", `INSERT INTO x6(x6) VALUES('merge=25,4');
			SELECT level, idx, start_block, leaves_end_block, end_block FROM x6_segdir;`},
		{"7.5", `INSERT INTO x6(x6) VALUES('merge=2500,4');
			SELECT level, idx, start_block, leaves_end_block, end_block FROM x6_segdir;
			SELECT sum(length(block)) FROM x6_segments;`},
	}
	for _, s := range steps {
		r := db.Query(s.sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", s.name, r.Error)
		}
		var vals []string
		for _, row := range r.Rows {
			for _, c := range row {
				vals = append(vals, fmtVal(c))
			}
		}
		t.Logf("%s => %s", s.name, strings.Join(vals, " | "))
	}
}
