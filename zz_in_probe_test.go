package frigolite

import "testing"

func TestZZInProbe(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"SELECT 0 WHERE (SELECT 0,0) OR (0 IN (1,2))",
		"SELECT 0 WHERE (SELECT 0,0)",
		"SELECT * FROM (SELECT 1 AS a) WHERE (SELECT 0,0) OR 1",
	} {
		r := db.Query(sql)
		t.Logf("%s -> err=%v rows=%v", sql, r.Error, r.Rows)
	}
}
