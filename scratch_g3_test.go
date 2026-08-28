//go:build scratch
// +build scratch

package frigolite_test

import (
	"os"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestGrowth23(t *testing.T) {
	os.Remove("/tmp/e2.db")
	db, _ := frigolite.Open("/tmp/e2.db")
	defer db.Close()
	sqlBytes, _ := os.ReadFile("/tmp/growth22.sql")
	for _, stmt := range strings.Split(string(sqlBytes), "\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		db.Exec(stmt)
	}
	r := db.Query("SELECT hex(substr(block,1,60)) FROM x2_segments WHERE blockid=788")
	t.Logf("block788=%v", r.Rows)
	o := db.Query("SELECT hex(root) FROM x2_segdir WHERE level=1 AND idx=0")
	if len(o.Rows) == 1 && len(o.Rows[0]) > 0 {
		s, _ := o.Rows[0][0].(string)
		n := len(s)
		if n > 120 {
			n = 120
		}
		t.Logf("engineL1root=%s", s[:n])
	}
}
