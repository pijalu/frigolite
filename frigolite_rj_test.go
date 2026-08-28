package frigolite

import "testing"

func TestP6JSON106Debug(t *testing.T) {
	db, _ := Open(":memory:")
	r := db.Query("SELECT 'a\\'b'")
	t.Logf("r=%v err=%v", r.Rows, r.Error)
}
