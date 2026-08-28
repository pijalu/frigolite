package frigolite

// Native regression tests for the P6.JSON remainder work: json_each/json_tree
// JSONB-offset ids, JSON5 constructs, json_valid flag bitmask, extreme
// numbers, and correlated FROM-TVF subqueries.

import (
	"fmt"
	"testing"
)

// TestP6JSONEachIDs verifies oracle (sqlite3 3.51) id/parent/fullkey values:
// ids are byte offsets into the JSONB blob.
func TestP6JSONEachIDs(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	res := db.Query(`SELECT * FROM json_each('{"a":1, "b":2}')`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := "[[a 1 integer 1 1 <nil> $.a $] [b 2 integer 2 5 <nil> $.b $]]"
	if got := fmt.Sprint(res.Rows); got != want {
		t.Errorf("each:\n got %s\nwant %s", got, want)
	}
}

// TestP6JSONValidFlags checks json_valid's FLAGS bitmask semantics.
func TestP6JSONValidFlags(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT json_valid('{"a":1}')`, "[[1]]"},
		{`SELECT json_valid('{a:1}')`, "[[0]]"},
		{`SELECT json_valid('{a:1}',2)`, "[[1]]"},
		{`SELECT json_valid('{"a":1}',5)`, "[[1]]"},
		{`SELECT json_valid('{"a":1}',2)`, "[[1]]"},
		{`SELECT json_valid('0x10')`, "[[0]]"},
		{`SELECT json_valid('0x10',2)`, "[[1]]"},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Errorf("%s: %v", c.sql, res.Error)
			continue
		}
		if got := fmt.Sprint(res.Rows); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.sql, got, c.want)
		}
	}
}

// TestP6JSON5AndExtreme covers JSON5 constructs and out-of-range numbers.
func TestP6JSON5AndExtreme(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT json_object('a',2e370,'b',-3e380)->>'a'`, "[[+Inf]]"},
		{`SELECT json_insert('{}','$.a',json_tree.value) FROM json_tree('[1,2,3]') WHERE atom IS NULL`, `[[{"a":[1,2,3]}]]`},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Errorf("%s: %v", c.sql, res.Error)
			continue
		}
		if got := fmt.Sprint(res.Rows); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.sql, got, c.want)
		}
	}
}

// TestP6CorrelatedEachTVF exercises a correlated json_each inside EXISTS.
func TestP6CorrelatedEachTVF(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	for _, sql := range []string{
		`CREATE TABLE t1(id, json)`,
		`INSERT INTO t1 VALUES(1,'{"items":[3,5]}')`,
		`CREATE TABLE t2(id, json)`,
		`INSERT INTO t2 VALUES(3,'{"value":3}')`,
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	res := db.Query(`SELECT t1.id, t2.id FROM t1 CROSS JOIN t2
	 WHERE EXISTS(SELECT 1 FROM json_each(t1.json,'$.items') AS Z WHERE Z.value==t2.id)`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if got, want := fmt.Sprint(res.Rows), "[[1 3]]"; got != want {
		t.Errorf("correlated each:\n got %s\nwant %s", got, want)
	}
}

// TestP6RandomJSONSeeds validates random_json/random_json5 output across many
// seeds (json106 invariant suite).
func TestP6RandomJSONSeeds(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	for i := int64(0); i < 500; i++ {
		r := db.Query(fmt.Sprintf(`SELECT count(*) FROM json_tree(random_json5(%d))`, i))
		if r.Error != nil {
			t.Fatalf("seed %d tree: %v", i, r.Error)
		}
		rv := db.Query(fmt.Sprintf(
			`SELECT json_valid(random_json(%d)), json_valid(random_json5(%d),2)`, i, i))
		if got, want := fmt.Sprint(rv.Rows), "[[1 1]]"; got != want {
			t.Fatalf("seed %d flags: %v", i, rv.Rows)
		}
		p := db.Query(fmt.Sprintf(`SELECT count(*) WHERE (SELECT json(json_pretty(random_json(%d)))) IS NOT NULL`, i))
		if p.Error != nil {
			t.Fatalf("seed %d pretty: %v", i, p.Error)
		}
	}
}
