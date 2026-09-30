package frigolite

import "testing"

// cloneInsertValue's float branch writes FormatFloat(v,'g',-1,64): 8.0 -> "8"
// -> reparsed as INTEGER (oracle: real). reverse (int slot, float value):
// 5.5 -> "5.5" -> real ✓; the coercion is one-directional.
// Also: the default branch "keep original" consumes a value and reuses the
// template's original literal — a blob literal through the INSERT template
// would serve the WRONG bytes for a same-shaped statement.
func TestReviewTemplateBlobParity(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(b BLOB, n INT)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(x'0102', 1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(x'0304', 2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT hex(b), n FROM t ORDER BY n"))
	t.Logf("blob template parity: %s", got)
	if got != "string:0102|int64:1\nstring:0304|int64:2\n" {
		t.Fatalf("template served wrong blob (oracle: 0102/0304): %s", got)
	}
}

func TestReviewTemplateBlobTextMix(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(v, n INT)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Blob first, string second — both normalize to literals.
	if r := db.Exec("INSERT INTO t VALUES(x'aa', 1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES('zz', 2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT typeof(v), v, n FROM t ORDER BY n"))
	t.Logf("blob/text mix: %s", got)
}
