//go:build scratch
// +build scratch

package frigolite_test

import (
	"os"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestGrowth22(t *testing.T) {
	os.Remove("/tmp/e2.db")
	db, _ := frigolite.Open("/tmp/e2.db")
	defer db.Close()
	sqlBytes, _ := os.ReadFile("/tmp/growth22.sql")
	for _, stmt := range strings.Split(string(sqlBytes), "\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatalf("stmt failed: %v", r.Error)
		}
	}
	r := db.Query("SELECT level, idx, start_block, leaves_end_block, end_block, length(root) FROM x2_segdir ORDER BY level, idx")
	for _, row := range r.Rows {
		var sb strings.Builder
		for _, v := range row {
			sb.WriteString(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(toString2(v), "\n", " "), "  ", " ")))
			sb.WriteString("|")
		}
		t.Log(sb.String())
	}
}
func toString2(v interface{}) string {
	switch x := v.(type) {
	case int64:
		return fmtInt(x)
	case string:
		return x
	}
	return "blob"
}
func fmtInt(i int64) string { return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(fmtInt64(i), "\n", " "), "  ", " ")) }
func fmtInt64(i int64) string { return strconvI(i) }
func strconvI(i int64) string { b := []byte{}; if i == 0 { return "0" }; neg := i < 0; if neg { i = -i }; for i > 0 { b = append([]byte{byte('0' + i%10)}, b...); i /= 10 }; if neg { return "-" + string(b) }; return string(b) }
