package frigolite_test

// Pin test for the tkt3992-2.3 engine contract: inside a BEFORE UPDATE
// trigger, NEW.c for an untouched REAL-affinity column retains its stored
// REAL type — typeof(new.c) == 'real' and new.c renders as 3.0 (C vdbe:
// OP_Column reads the old record; unchanged columns are not re-affined).
// Oracle (sqlite3 3.51.0): raise(FAIL, typeof(new.c)||' '||new.c) fires
// "real 3.0".

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestTkt3992TriggerNewUntouchedRealColumn(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	setup := `
    CREATE TABLE t2(a REAL, b REAL, c REAL);
    INSERT INTO t2 VALUES(1, 2, 3);
  `
	if r := db.Exec(setup); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}

	var seen []string
	db.RegisterFunction("rec", func(args []interface{}) (interface{}, error) {
		if len(args) > 0 && args[0] != nil {
			seen = append(seen, strings.TrimSpace(args[0].(string)))
		}
		return nil, nil
	}, 1, 1)

	if r := db.Exec(`
    CREATE TRIGGER tr2 BEFORE UPDATE ON t2 BEGIN
      SELECT rec(typeof(new.a) || ' ' || typeof(new.c) || ' ' || new.c);
    END;
  `); r.Error != nil {
		t.Fatalf("create trigger: %v", r.Error)
	}
	if r := db.Exec("UPDATE t2 SET a = 'I';"); r.Error != nil {
		t.Fatalf("update: %v", r.Error)
	}
	if len(seen) != 1 {
		t.Fatalf("trigger fired %d times, want 1 (seen=%v)", len(seen), seen)
	}
	// Oracle: typeof(new.a)='text' (SET a='I'), typeof(new.c)='real',
	// new.c renders "3.0" (REAL affinity forces float text rendering).
	if seen[0] != "text real 3.0" {
		t.Errorf("trigger NEW row: got %q, want %q", seen[0], "text real 3.0")
	}

	// Post-update stored row: typeof(c)='real', text renders 3.0, and the
	// Go value is float64 (REAL storage) — fmt.Sprint(float64(3)) is "3" in
	// Go, so text fidelity is asserted via the engine's own CAST rendering.
	r := db.Query("SELECT typeof(a), typeof(c), CAST(c AS TEXT) FROM t2")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	got := fmt.Sprint(r.Rows[0][0]) + " " + fmt.Sprint(r.Rows[0][1]) + " " + fmt.Sprint(r.Rows[0][2])
	if got != "text real 3.0" {
		t.Errorf("stored row: got %q, want %q", got, "text real 3.0")
	}
	r2 := db.Query("SELECT c FROM t2")
	if f, ok := r2.Rows[0][0].(float64); !ok || f != 3.0 {
		t.Errorf("stored c: got %T %v, want float64 3", r2.Rows[0][0], r2.Rows[0][0])
	}
}
