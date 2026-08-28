package frigolite_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestP6EXTBaseXX(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT base64(x'000102030405')`, "AAECAwQF\n"},
		{`SELECT base64(x'0001020304')`, "AAECAwQ=\n"},
		{`SELECT base64(x'00010203')`, "AAECAw==\n"},
		{`SELECT hex(base64('AAECAwQF'))`, "000102030405"},
		{`SELECT hex(base64('~AAEC~AwQF~'))`, "000102030405"},
		{`SELECT hex(base64(' AAECAwQF '))`, "000102030405"},
		{`SELECT base85(x'000102030405')`, "##/2,#2/\n"},
		{`SELECT base85(x'0001020304')`, "##/2,#*\n"},
		{`SELECT base85(x'00010203')`, "##/2,\n"},
		{`SELECT hex(base85(' ##/2,#2/ '))`, "000102030405"},
		{`SELECT hex(base85('~##/2,#2/~'))`, "000102030405"},
		{`SELECT is_base85(' '||base85(x'123456')||char(10))`, "1"},
		{`SELECT is_base85('!')`, "0"},
		{`SELECT is_base85(NULL) IS NULL`, "1"},
	}
	for _, c := range cases {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Errorf("%s: error %v", c.sql, r.Error)
			continue
		}
		got := ""
		if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
			got = toStringP6(r.Rows[0][0])
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
	// error cases
	r := db.Query(`SELECT base64(1)`)
	if r.Error == nil {
		t.Errorf("base64(1): expected error, got nil")
	}
	r = db.Query(`SELECT is_base85(1)`)
	if r.Error == nil {
		t.Errorf("is_base85(1): expected error, got nil")
	}
}

func TestP6EXTIeee754(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT ieee754(1.0)`, "ieee754(1,0)"},
		{`SELECT ieee754(2.0)`, "ieee754(2,0)"},
		{`SELECT ieee754(0.5)`, "ieee754(1,-1)"},
		{`SELECT ieee754(1.5)`, "ieee754(3,-1)"},
		{`SELECT ieee754(0.0)`, "ieee754(0,-1075)"},
		{`SELECT ieee754(1,0)==1.0`, "1"},
		{`SELECT ieee754(181,-2)`, "45.25"},
		{`SELECT ieee754(4503599627370495,973) is null`, "1"},
		{`SELECT ieee754_mantissa(45.25)`, "181"},
		{`SELECT ieee754_exponent(45.25)`, "-2"},
		{`SELECT hex(ieee754_to_blob(1.0))`, "3FF0000000000000"},
		{`SELECT ieee754_from_blob(x'3ff0000000000000')`, "1.0"},
	}
	for _, c := range cases {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Errorf("%s: error %v", c.sql, r.Error)
			continue
		}
		got := ""
		if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
			got = toStringP6(r.Rows[0][0])
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
}

func TestP6EXTFileio(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`SELECT writefile('/tmp/p6ext_wf.txt', 'A second test line')`)
	if r.Error != nil {
		t.Fatalf("writefile: %v", r.Error)
	}
	got := toStringP6(r.Rows[0][0])
	if got != "18" {
		t.Errorf("writefile: got %q want 18", got)
	}
	r = db.Query(`SELECT writefile('/tmp/p6ext_wf.txt', NULL)`)
	if r.Error != nil {
		t.Fatalf("writefile null: %v", r.Error)
	}
	if toStringP6(r.Rows[0][0]) != "0" {
		t.Errorf("writefile null: got %q want 0", toStringP6(r.Rows[0][0]))
	}
	r = db.Query(`SELECT readfile('/tmp/p6ext_wf.txt'), length(readfile('/tmp/p6ext_wf.txt'))`)
	if r.Error != nil {
		t.Fatalf("readfile: %v", r.Error)
	}
	if toStringP6(r.Rows[0][1]) != "0" {
		t.Errorf("readfile after truncate: got %q want 0", toStringP6(r.Rows[0][1]))
	}
	r = db.Query(`SELECT readfile('/tmp/does-not-exist-p6ext')`)
	if r.Error != nil {
		t.Fatalf("readfile missing: %v", r.Error)
	}
	if toStringP6(r.Rows[0][0]) != "" {
		t.Errorf("readfile missing: got %q want empty", toStringP6(r.Rows[0][0]))
	}
}

func TestP6EXTDecimal(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT decimal(1)`, "1"},
		{`SELECT decimal('+0')`, "0"},
		{`SELECT decimal('-0')`, "0"},
		{`SELECT decimal('1.0')`, "1.0"},
		{`SELECT decimal('0001.0')`, "1.0"},
		{`SELECT decimal('+0001.0')`, "1.0"},
		{`SELECT decimal('-0001.0')`, "-1.0"},
		{`SELECT decimal('-0000.0')`, "0.0"},
		{`SELECT decimal('1.0e72')`, "1000000000000000000000000000000000000000000000000000000000000000000000000"},
		{`SELECT decimal('1.0e-72')`, "0.0000000000000000000000000000000000000000000000000000000000000000000000010"},
		{`SELECT decimal('-123e-4')`, "-0.0123"},
		{`SELECT decimal('+123e+4')`, "1230000"},
		{`SELECT decimal_exp('+123e+4')`, "+1.23e+06"},
		{`SELECT decimal_cmp('-9999e99','-9998.000e+99')`, "-1"},
		{`SELECT decimal_add('1.5','2.25')`, "3.75"},
		{`SELECT decimal_mul('1234.00','2.00')`, "2468.00"},
		{`SELECT decimal_mul('1234.00','2.0000')`, "2468.00"},
		{`SELECT decimal_mul('1234.0000','2.000')`, "2468.000"},
		{`SELECT decimal_mul('1234.0000','2')`, "2468"},
		{`SELECT decimal_mul('1.23','4.56')`, "5.6088"},
		{`SELECT decimal_sub('5.6','1.234')`, "4.366"},
		{`SELECT decimal('999999999999999',1)`, "1000000000000000"},
		{`SELECT decimal('999999999999999',15)`, "999999999999999"},
		{`SELECT decimal('899999999999999',14)`, "900000000000000"},
		{`SELECT decimal('989999999999999',14)`, "990000000000000"},
		{`SELECT decimal('998999999999999',14)`, "999000000000000"},
		{`SELECT decimal('999.999',3)`, "1000.000"},
		{`SELECT decimal_sum(val) FROM (SELECT 1 AS val UNION ALL SELECT 2 UNION ALL SELECT 3)`, "6"},
		{`SELECT hex(ieee754_to_blob(1.0))`, "3FF0000000000000"},
	}
	for _, c := range cases {
		r := db.Query(c.sql)
		if r.Error != nil {
			t.Errorf("%s: error %v", c.sql, r.Error)
			continue
		}
		got := ""
		if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
			got = toStringP6(r.Rows[0][0])
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.sql, got, c.want)
		}
	}
	// collation
	r := db.Query(`WITH t(x) AS (VALUES('999.999'),('-1'),('0.5'),('10')) SELECT group_concat(x,' ') FROM (SELECT x FROM t ORDER BY x COLLATE decimal)`)
	if r.Error != nil {
		t.Fatalf("collate: %v", r.Error)
	}
	got := toStringP6(r.Rows[0][0])
	if got != "-1 0.5 10 999.999" {
		t.Errorf("collate: got %q", got)
	}
}

func toStringP6(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if f, ok := v.(float64); ok {
		// SQLite renders integral doubles with a trailing ".0".
		if f == float64(int64(f)) && !math.IsInf(f, 0) {
			return fmt.Sprintf("%.1f", f)
		}
		return fmt.Sprintf("%v", f)
	}
	return fmt.Sprint(v)
}
