package frigolite_test

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestZZ50(t *testing.T) {
	data, _ := os.ReadFile("/tmp/zzall_" + "x" + ".db")
	_ = data
	f, _ := frigolite.Open(":memory:")
	f.Exec("CREATE VIRTUAL TABLE t1 USING fts3(a,b,c)")
	// The 50.1 DB has t1_segdir with PRIMARY KEY. Create the same and insert.
	res := f.Exec("SELECT NULL FROM t1 WHERE t1 MATCH '\"^enable\"'")
	t.Logf("50.1: err=%v", res.Error)
	f.Close()
}
