// Temporary S29 probe — deleted before commit.
package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestS29Probe(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	probes := []string{
		// A6 truthiness
		"SELECT CASE WHEN 'engligh' THEN 'true' ELSE 'false' END, iif('engligh','true','false')",
		"SELECT CASE WHEN '0' THEN 'true' ELSE 'false' END, iif('0','true','false')",
		"SELECT CASE WHEN '1english' THEN 'true' ELSE 'false' END, iif('1engl','true','false')",
		// A5 NUMERIC keeps REAL
		"SELECT typeof(CAST('9.223372036e15' AS NUMERIC))",
		"SELECT typeof(CAST('-9.223372036e15' AS NUMERIC))",
		// A3 substring affinity
		"SELECT typeof(CAST('def' AS shobblob_x)), typeof(CAST('ghi' AS abbLOb10)), typeof(CAST('x' AS shint_x)), typeof(CAST('x' AS clob))",
		// A4 NUL text: hex keeps bytes; the RENDERER must stop at NUL
		"SELECT hex(CAST(X'670068006900' AS text)), typeof(CAST(X'670068006900' AS text))",
		// A7 variable limit
		"SELECT ?32766, 22",
	}
	for _, p := range probes {
		res := db.Query(p)
		if res.Error != nil {
			fmt.Printf("PROBE %s\n  ERROR: %v\n", p, res.Error)
			continue
		}
		for _, row := range res.Rows {
			var vals []interface{}
			for _, v := range row {
				if cv, ok := v.(interface{ Unwrap() interface{} }); ok {
					vals = append(vals, cv)
				} else {
					vals = append(vals, v)
				}
			}
			fmt.Printf("PROBE %s\n  ROW: %v\n", p, vals)
		}
	}
}
