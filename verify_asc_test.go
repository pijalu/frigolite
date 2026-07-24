package frigolite
import "testing"
func TestVerifyASC(t *testing.T) {
    cases := []string{
        "CREATE TABLE t1(a, b, UNIQUE('b' COLLATE nocase DESC))",
        "CREATE TABLE t1(a, b, UNIQUE(b DESC))",
        "CREATE TABLE t1(a, b, UNIQUE('b' DESC))",
        "CREATE TABLE t1(a, b, UNIQUE(aid ASC, tn DESC))",
        "CREATE TABLE t1(a, b, PRIMARY KEY('a'), UNIQUE('b' COLLATE nocase DESC))",
        "CREATE TABLE t1(a UNIQUE(b DESC))",
    }
    for _, sql := range cases {
        db := setupDB(t)
        res := db.Exec(sql)
        if res.Error != nil {
            t.Errorf("FAIL %-60s → %v", sql, res.Error)
        }
        db.Close()
    }
}
