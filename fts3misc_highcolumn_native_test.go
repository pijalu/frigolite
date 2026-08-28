package frigolite

import (
	"strings"
	"testing"
)

// TestFTS3MiscHighColumnPhraseNative pins the fts3misc 2.x engine gap:
// phrase queries against columns with index >= 128 in a many-column FTS3
// table return no rows even though the phrase occurs (oracle: rowids
// 7,15,23,... for MATCH '"a b c"' after inserting v1(i)/v2(i) into
// columns c198/c199 of a 200-column table).
//
// Expected values come from /usr/bin/sqlite3 3.51.0 (UCL rule U1):
//   SELECT rowid FROM t2 WHERE t2 MATCH '"a b c"'
//     -> 7 15 23 31 ... 199   (i & 7 == 7)
func TestFTS3MiscHighColumnPhraseNative(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if r := db.Exec("PRAGMA page_size = 4096"); r.Error != nil {
		t.Fatalf("page_size: %v", r.Error)
	}
	var cols []string
	for i := 0; i < 200; i++ {
		cols = append(cols, "c"+itoa(i))
	}
	if r := db.Exec("CREATE VIRTUAL TABLE t2 USING fts3(" + strings.Join(cols, ",") + ")"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	// v1(i): vector a..h entries whose bit is set; v2(i): d..k.
	reg := func(name string, vec [8]string) {
		vecCopy := vec
		db.RegisterFunction(name, func(args []interface{}) (interface{}, error) {
			var iv int64
			switch x := args[0].(type) {
			case int64:
				iv = x
			case string:
				var n int64
				for _, c := range x {
					n = n*10 + int64(c-'0')
				}
				iv = n
			}
			var out []string
			for i := 0; i < 8; i++ {
				if iv&(1<<uint(i)) != 0 {
					out = append(out, vecCopy[i])
				}
			}
			return strings.Join(out, " "), nil
		}, 1, 1)
	}
	reg("v1", [8]string{"a", "b", "c", "d", "e", "f", "g", "h"})
	reg("v2", [8]string{"d", "e", "f", "g", "h", "i", "j", "k"})

	if r := db.Exec(`WITH data(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM data WHERE i<200)
INSERT INTO t2(c198, c199) SELECT v1(i), v2(i) FROM data`); r.Error != nil {
		t.Fatalf("cte insert: %v", r.Error)
	}

	r := db.Query(`SELECT rowid FROM t2 WHERE t2 MATCH '"a b c"' ORDER BY rowid`)
	if r.Error != nil {
		t.Fatalf("phrase query: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatal("phrase query over high-column-index FTS3 returned no rows — ENGINE GAP (fts3misc 2.1); oracle expects 25 rows starting at rowid 7")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
