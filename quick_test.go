package frigolite

import "testing"

func TestRemainingBugs(t *testing.T) {
    db := setupDB(t)
    
    tests := []string{
        "CREATE TABLE t1(a, b, c, d, e, PRIMARY KEY('a'), UNIQUE('b' COLLATE nocase DESC))",
        "CREATE INDEX t1c ON t1('c')",
        "CREATE INDEX t1d ON t1('d' COLLATE binary ASC)",
        "SELECT * FROM t1 WHERE 0xda-0xda",
    }
    for _, sql := range tests {
        res := db.Exec(sql)
        status := "OK"
        if res.Error != nil {
            status = res.Error.Error()
        }
        t.Logf("%-55s → %s", sql[:min(len(sql),55)], status)
    }
}
func min(a, b int) int { if a < b { return a }; return b }
