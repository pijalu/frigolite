package frigolite

import (
	"fmt"
	"strconv"
	"testing"
)

func TestScratchJSONPoison(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	// 1300 loop x100
	for i := 0; i < 100; i++ {
		str := "abcdefuvwxyz"
		db.Query("SELECT json_extract(json_array('" + str + "'),'$[0]')=='" + str + "'")
	}
	// 1401 loop
	js := []string{
		`'{"x":01}'`, `'{"x":-01}'`, `'{"x":0}'`, `'{"x":-0}'`, `'{"x":0.1}'`,
		`'{"x":-0.1}'`, `'{"x":0.0000}'`, `'{"x":-0.0000}'`, `'{"x":01.5}'`,
		`'{"x":-01.5}'`, `'{"x":00}'`, `'{"x":-00}'`, `'{"x":+0}'`, `'{"x":+5}'`, `'{"x":+5.5}'`,
	}
	for _, s := range js {
		db.Query("SELECT json_valid(" + s + "), NOT json_error_position(" + s + ")")
	}
	// 1600 combined
	r := db.Query(`CREATE TABLE t1(id INTEGER PRIMARY KEY, x JSON);
		INSERT INTO t1(id,x) VALUES (6, '{"a":{"b":9}}');
		SELECT id, x->'a' FROM t1 ORDER BY id;`)
	val := "<none>"
	if len(r.Rows) > 0 {
		val = fmt.Sprintf("%v (%T)", r.Rows[0][1], r.Rows[0][1])
	}
	t.Logf("1300+1401+1600 -> %s", val)
	// now just 1300 x100 then combined
	db2, _ := Open(":memory:")
	for i := 0; i < 100; i++ {
		db2.Query("SELECT json_extract(json_array('abcdefuvwxyz'),'$[0]')=='abcdefuvwxyz'")
	}
	r2 := db2.Query(`CREATE TABLE t1(id INTEGER PRIMARY KEY, x JSON);
		INSERT INTO t1(id,x) VALUES (6, '{"a":{"b":9}}');
		SELECT id, x->'a' FROM t1 ORDER BY id;`)
	v2 := "<none>"
	if len(r2.Rows) > 0 {
		v2 = fmt.Sprintf("%v (%T)", r2.Rows[0][1], r2.Rows[0][1])
	}
	t.Logf("1300only+1600 -> %s", v2)
	_ = strconv.Itoa
}
