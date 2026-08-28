package frigolite

import (
	"fmt"
	"testing"
)

func TestP6JSONConstructors(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct{ sql, want string }{
		{"SELECT json('[1, 2, {\"x\":true}]')", `[1,2,{"x":true}]`},
		{"SELECT json_array(1, 'x', NULL)", `[1,"x",null]`},
		{"SELECT json_object('a', 1, 'b', 'x')", `{"a":1,"b":"x"}`},
		{"SELECT json_extract('{\"a\": {\"b\": 2}}', '$.a.b')", `2`},
	}
	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", tc.sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Errorf("%s: got %#v", tc.sql, r.Rows)
			continue
		}
		if got := fmt.Sprint(r.Rows[0][0]); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.sql, got, tc.want)
		}
	}
}
