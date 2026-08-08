package frigolite

import (
	"testing"
)

// TestP4Printf_Standard covers SQLite's printf standard conversions
// (%d %s %f %x %X %o %c %u) and width/precision/flags.
func TestP4Printf_Standard(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql, want string
	}{
		{`SELECT printf('%d,%d,%d',55,-11,3421)`, `55,-11,3421`},
		{`SELECT format('%d,%d,%d,%d',55,'-11',3421)`, `55,-11,3421,0`},
		{`SELECT printf('%.2f',3.141592653)`, `3.14`},
		{`SELECT format('%.*f',2,3.141592653)`, `3.14`},
		{`SELECT printf('%*.*f',5,2,3.141592653)`, ` 3.14`},
		{`SELECT format('%d',314159.2653)`, `314159`},
		{`SELECT printf('%x %X %o',255,255,8)`, `ff FF 10`},
		{`SELECT printf('%c',65)`, `A`},
		{`SELECT printf('%u',7)`, `7`},
		{`SELECT printf('%05d',42)`, `00042`},
		{`SELECT printf('%+d % d',5,5)`, `+5  5`},
		{`SELECT printf('%-5d|',42)`, `42   |`},
		{`SELECT printf('%#x %#o',255,8)`, `0xff 010`},
		{`SELECT printf('%s','hello')`, `hello`},
		{`SELECT printf('%s',NULL)`, ``},
		{`SELECT printf('%c','abcdefghijklmnop')`, `a`},
		{`SELECT printf('%p',-1)`, `FFFFFFFFFFFFFFFF`},
	}
	for _, c := range cases {
		q := db.Query(c.sql)
		if q.Error != nil {
			t.Errorf("%s: unexpected error %v", c.sql, q.Error)
			continue
		}
		got := flattenResult(q)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}

// TestP4Printf_Bang covers the SQLite-specific '!' flag (altform2):
// %!.20g forces the full 20 significant digits and a decimal point.
func TestP4Printf_Bang(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql, want string
	}{
		{`SELECT format('%!.20g', 13.0)`, `13.0`},
		{`SELECT format('%!.0e',-1e100)`, `-1.0e+100`},
	}
	for _, c := range cases {
		q := db.Query(c.sql)
		if q.Error != nil {
			t.Errorf("%s: unexpected error %v", c.sql, q.Error)
			continue
		}
		got := flattenResult(q)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}

// TestP4Printf_Comma covers the ',' thousands-separator flag.
func TestP4Printf_Comma(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql, want string
	}{
		{`SELECT printf('%,d',1234567890)`, `1,234,567,890`},
		{`SELECT printf('%,d',-1234567890)`, `-1,234,567,890`},
		{`SELECT format('%,.0f',12345e+10)`, `123,450,000,000,000`},
	}
	for _, c := range cases {
		q := db.Query(c.sql)
		if q.Error != nil {
			t.Errorf("%s: unexpected error %v", c.sql, q.Error)
			continue
		}
		got := flattenResult(q)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}

// TestP4Printf_Quote covers %q / %Q / %w escaping conversions.
func TestP4Printf_Quote(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql, want string
	}{
		{`SELECT printf('%q',"it's")`, `it''s`},
		{`SELECT printf('%Q',"it's")`, `'it''s'`},
		{`SELECT printf('%Q',NULL)`, `NULL`},
		{`SELECT printf('%q',NULL)`, `(NULL)`},
		{`SELECT printf('%w','a"b')`, `a""b`},
	}
	for _, c := range cases {
		q := db.Query(c.sql)
		if q.Error != nil {
			t.Errorf("%s: unexpected error %v", c.sql, q.Error)
			continue
		}
		got := flattenResult(q)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}

// TestP4Printf_FormatAlias covers FORMAT() as an alias and NULL/empty handling.
func TestP4Printf_FormatAlias(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql, want string
	}{
		{`SELECT quote(format()), quote(format(NULL,1,2,3))`, `NULL NULL`},
		{`SELECT printf('hello')`, `hello`},
		{`SELECT printf('%n',0)`, ``},
	}
	for _, c := range cases {
		q := db.Query(c.sql)
		if q.Error != nil {
			t.Errorf("%s: unexpected error %v", c.sql, q.Error)
			continue
		}
		got := flattenResult(q)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}

// TestP4Printf_Precision covers huge-precision clamping and %g/%e/%f forms.
func TestP4Printf_Precision(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql, want string
	}{
		{`SELECT printf('%.*g',2147483647,0.01)`, `0.01`},
		{`SELECT format('%.3e', 199990000.0)`, `2.000e+08`},
		{`SELECT format('%.3f', 199990000.0)`, `199990000.000`},
		{`SELECT format('%.30f',1.0000000000000000076e-50)`, `0.000000000000000000000000000000`},
		{`SELECT format('%0.0f %#0.0f',0.0, 0.0)`, `0 0.`},
		{`SELECT length( format('%,.249f', -5.0e-300) )`, `252`},
	}
	for _, c := range cases {
		q := db.Query(c.sql)
		if q.Error != nil {
			t.Errorf("%s: unexpected error %v", c.sql, q.Error)
			continue
		}
		got := flattenResult(q)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}
