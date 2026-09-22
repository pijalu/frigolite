package frigolite

import (
	"fmt"
	"testing"
)

func TestW5DebugMisc817(t *testing.T) {
	db := w5Open(t)
	// register eval like the testgen does
	db.RegisterFunction("eval", func(args []interface{}) (interface{}, error) {
		if len(args) == 0 {
			return nil, nil
		}
		s, err := db.EvalExecSQL(toStringW5(args[0]), " ")
		if err != nil {
			return nil, err
		}
		return s, nil
	}, 1, 2)
	// State A: like pre-skip (1.6 deleted all rows, so 1.7 inserts 3)
	if r := db.Exec("CREATE TABLE t1(a,b,c);INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,null,9);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	resA, errA := db.EvalExecSQL("BEGIN; CREATE TABLE t2(x); SELECT a, coalesce(b, eval('ROLLBACK; SELECT ''bam''')), c FROM t1 ORDER BY rowid", " ")
	fmt.Println("stateA:", resA, "err:", errA)
	// State B: like post-skip (1.6 skipped, t1 has 4 rows before 1.7 insert)
	db2, _ := Open(t.TempDir() + "/b.db")
	db2.RegisterFunction("eval", func(args []interface{}) (interface{}, error) {
		if len(args) == 0 {
			return nil, nil
		}
		s, err := db2.EvalExecSQL(toStringW5(args[0]), " ")
		if err != nil {
			return nil, err
		}
		return s, nil
	}, 1, 2)
	db2.Exec("CREATE TABLE t1(a,b,c);INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,null,9),(10,11,12);")
	resB, errB := db2.EvalExecSQL("INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,null,9); BEGIN; CREATE TABLE t2(x); SELECT a, coalesce(b, eval('ROLLBACK; SELECT ''bam''')), c FROM t1 ORDER BY rowid", " ")
	fmt.Println("stateB:", resB, "err:", errB)
}

func toStringW5(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}
