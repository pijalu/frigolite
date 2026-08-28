package frigolite_test

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSpellfixSmoke exercises the spellfix1 module contract end to end:
// shadow-vocab CRUD, full scans, the rowid plan, MATCH prefix search,
// spellfix1_scriptcode and next_char (spellfix.test 1.x/2.x/3.x shapes).
func TestSpellfixSmoke(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) *frigolite.Result {
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec %q: %v", sql, r.Error)
		}
		return r
	}
	must("CREATE VIRTUAL TABLE t1 USING spellfix1")
	for _, w := range []string{"rabbi", "rabbit", "rabbits", "rasping", "rasped", "rail", "railed"} {
		must("INSERT INTO t1(word) VALUES('" + w + "')")
	}
	q := func(sql string) *frigolite.Result {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("query %q: %v", sql, r.Error)
		}
		return r
	}
	// Full scan sees every word.
	r := q("SELECT count(*) FROM t1")
	if got := r.Rows[0][0]; got != int64(7) {
		t.Fatalf("count = %v, want 7", got)
	}
	// MATCH prefix search (spellfix.test 1.2 shape).
	r = q("SELECT word, matchlen FROM t1 WHERE word MATCH 'ras*' ORDER BY score, word LIMIT 5")
	if len(r.Rows) != 2 {
		t.Fatalf("MATCH ras* rows = %d, want 2", len(r.Rows))
	}
	if r.Rows[0][0] != "rasped" || r.Rows[1][0] != "rasping" {
		t.Fatalf("MATCH ras* = %v/%v, want rasped/rasping", r.Rows[0][0], r.Rows[1][0])
	}
	if r.Rows[0][1] != int64(3) {
		t.Fatalf("matchlen = %v, want 3", r.Rows[0][1])
	}
	// rowid plan.
	r = q("SELECT rowid, word FROM t1 WHERE rowid = 3")
	if len(r.Rows) != 1 || r.Rows[0][1] != "rabbits" {
		t.Fatalf("rowid 3 = %v, want rabbits", r.Rows)
	}
	// scriptcode (spellfix3.test oracle).
	r = q("SELECT spellfix1_scriptcode('Бог сказал')")
	if r.Rows[0][0] != int64(220) {
		t.Fatalf("scriptcode = %v, want 220", r.Rows[0][0])
	}
	// next_char (spellfix.test 1.12 shape).
	must("CREATE TABLE vocab(w TEXT PRIMARY KEY)")
	must("INSERT INTO vocab SELECT word FROM t1")
	r = q("SELECT next_char('ra','vocab','w')")
	if r.Rows[0][0] != "bis" {
		t.Fatalf("next_char = %v, want bis", r.Rows[0][0])
	}
	// DELETE via the shadow.
	must("DELETE FROM t1 WHERE word = 'railed'")
	r = q("SELECT count(*) FROM t1")
	if got := r.Rows[0][0]; got != int64(6) {
		t.Fatalf("count after delete = %v, want 6", got)
	}
}
