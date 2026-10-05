package frigolite

// PERF.ARENA nesting pins: every statement-scoped scratch slot introduced by
// the arena work (affinity collectors, the point-fetch decode buffer and
// StructRow, the seek-plan pair, the DML result/plan/name-set slots, the SET
// column buffer, and the statement-journal list) must survive a nested
// statement executing while an enclosing statement's slot is live. The
// nested statement (a correlated subquery, a trigger body, eval()) always
// takes a deeper slot; these pins fail if a slot is reset while an outer
// statement still reads it.

import (
	"fmt"
	"strings"
	"testing"
)

// newArenaPinDB opens a memory database with the two-table fixture the pins
// share: t (IPK id, integer c, NOCASE text tag) and u (IPK id, integer v).
func newArenaPinDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExecPin(db, `CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER, tag TEXT COLLATE NOCASE)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO t VALUES(1, 10, 'alpha'), (2, 20, 'beta'), (3, 30, 'gamma')`,
		`INSERT INTO u VALUES(1, 100), (2, 200), (3, 300)`)
	return db
}

func mustExecPin(db *DB, stmts ...string) {
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			panic("exec " + s + ": " + r.Error.Error())
		}
	}
}

func queryRowsPin(t *testing.T, db *DB, sql string) []string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %s: %v", sql, r.Error)
	}
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = strings.TrimSpace(sprintfValue(v))
		}
		out = append(out, strings.Join(parts, ","))
	}
	return out
}

func sprintfValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// TestArenaPin_AffinitySlotNesting re-enters scanTableAffinityCols at a
// deeper selectDepth while the outer scan's affinity set is live: the outer
// statement's WHERE has a NOCASE-collated comparison (its affinity set holds
// "tag") and a correlated subquery over u whose own WHERE collects u's
// columns into the DEEPER slot. The outer set must survive the inner
// statement intact (tag stays affinity-wrapped or the NOCASE match breaks).
func TestArenaPin_AffinitySlotNesting(t *testing.T) {
	db := newArenaPinDB(t)
	// 20 rows so the correlated subquery runs repeatedly against the outer
	// scan's live affinity set.
	mustExecPin(db, `INSERT INTO u VALUES(10, 10), (20, 20), (30, 30)`)
	rows := queryRowsPin(t, db, `SELECT tag FROM t WHERE tag = 'BETA' AND EXISTS(SELECT 1 FROM u WHERE u.id = t.id AND u.v >= 20) ORDER BY tag`)
	if len(rows) != 1 || rows[0] != "beta" {
		t.Fatalf("nocase outer/inner affinity nesting: got %v, want [beta]", rows)
	}
	// The reverse nesting: inner statement over the NOCASE table.
	rows = queryRowsPin(t, db, `SELECT v FROM u WHERE EXISTS(SELECT 1 FROM t WHERE t.id = u.id AND t.tag = 'GAMMA') ORDER BY v`)
	if len(rows) != 1 || rows[0] != "300" {
		t.Fatalf("inner nocase affinity: got %v, want [300]", rows)
	}
}

// TestArenaPin_SeekRowSlotNesting keeps the outer point fetch's decode
// buffer and StructRow live across a nested statement: the uncovered WHERE
// forces per-candidate re-evaluation (RowPassesWhere against the slotted
// row) and the IN-subquery executes a full nested SELECT per candidate. A
// clobbered seekRowScratch slot shows up as a wrong or NULL c value.
func TestArenaPin_SeekRowSlotNesting(t *testing.T) {
	db := newArenaPinDB(t)
	rows := queryRowsPin(t, db, `SELECT c FROM t WHERE id = 2 AND id IN (SELECT id FROM u WHERE v > 50)`)
	if len(rows) != 1 || rows[0] != "20" {
		t.Fatalf("seek row slot nesting: got %v, want [20]", rows)
	}
	// Two consecutive point selects must not see each other's scratch
	// (stale-value leak through the re-nilled decode buffer).
	rows = queryRowsPin(t, db, `SELECT tag FROM t WHERE id = 3`)
	if len(rows) != 1 || rows[0] != "gamma" {
		t.Fatalf("seek row slot reuse: got %v, want [gamma]", rows)
	}
}

// TestArenaPin_SeekPlanSlotNesting nests a trigger-body point UPDATE inside
// an outer point UPDATE: both statements run planDMLSeek through the SAME
// DMLExecutor, the trigger body at a deeper execDepth. A clobbered
// seekPlanScratch / resultScratch / setColsBuf slot corrupts one side's row
// count or SET list.
func TestArenaPin_SeekPlanSlotNesting(t *testing.T) {
	db := newArenaPinDB(t)
	mustExecPin(db,
		`CREATE TABLE log(n INTEGER)`,
		`CREATE TRIGGER t_up AFTER UPDATE OF c ON t BEGIN UPDATE log SET n = n + 1; END`,
		`INSERT INTO log VALUES(0)`)
	if r := db.Exec(`UPDATE t SET c = 21 WHERE id = 2`); r.Error != nil {
		t.Fatalf("outer update: %v", r.Error)
	}
	if r := db.Exec(`UPDATE t SET c = 31 WHERE id = 3`); r.Error != nil {
		t.Fatalf("outer update 2: %v", r.Error)
	}
	rows := queryRowsPin(t, db, `SELECT c FROM t WHERE id = 2`)
	if len(rows) != 1 || rows[0] != "21" {
		t.Fatalf("nested update outer row: got %v, want [21]", rows)
	}
	rows = queryRowsPin(t, db, `SELECT n FROM log`)
	if len(rows) != 1 || rows[0] != "2" {
		t.Fatalf("nested update trigger fired per statement: got %v, want [2]", rows)
	}
	// UPDATE OF gate: updating a column OUTSIDE the SET list must not fire.
	mustExecPin(db, `UPDATE t SET tag = 'beta2' WHERE id = 2`)
	rows = queryRowsPin(t, db, `SELECT n FROM log`)
	if len(rows) != 1 || rows[0] != "2" {
		t.Fatalf("UPDATE OF c gate after nested push/restore: got %v, want [2]", rows)
	}
}

// TestArenaPin_SeekPlanSlotDelete nests a point DELETE inside a trigger
// firing from an outer point DELETE (the delete path shares the seek-plan
// slot).
func TestArenaPin_SeekPlanSlotDelete(t *testing.T) {
	db := newArenaPinDB(t)
	mustExecPin(db,
		`CREATE TABLE u2(id INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u2 VALUES(1, 1), (2, 2)`,
		`CREATE TRIGGER u_del AFTER DELETE ON u BEGIN DELETE FROM u2 WHERE id = OLD.id; END`)
	if r := db.Exec(`DELETE FROM u WHERE id = 1`); r.Error != nil {
		t.Fatalf("outer delete: %v", r.Error)
	}
	rows := queryRowsPin(t, db, `SELECT COUNT(*) FROM u`)
	if len(rows) != 1 || rows[0] != "2" {
		t.Fatalf("outer delete rowcount: got %v, want [2]", rows)
	}
	rows = queryRowsPin(t, db, `SELECT COUNT(*) FROM u2`)
	if len(rows) != 1 || rows[0] != "1" {
		t.Fatalf("nested cascade delete via trigger: got %v, want [1]", rows)
	}
}

// TestArenaPin_SnapshotSlotNesting runs a failing nested statement (trigger
// body raising via a constraint) inside an outer DML statement, so both
// exec depths open statement journals through snapBufs; the outer statement
// must roll back its own writes when the outer statement fails after the
// nested one succeeded.
func TestArenaPin_SnapshotSlotNesting(t *testing.T) {
	db := newArenaPinDB(t)
	mustExecPin(db,
		`CREATE TABLE z(k INTEGER UNIQUE)`,
		`CREATE TRIGGER z_ins AFTER INSERT ON u BEGIN INSERT INTO z VALUES(7); END`,
		`INSERT INTO z VALUES(7)`)
	// The trigger fires on the outer INSERT and its own write violates z's
	// UNIQUE constraint (7 already exists), failing the whole statement
	// (sqlite3 oracle: "UNIQUE constraint failed: z.k"): the outer scope's
	// journal must roll the outer write back along with the trigger's write
	// (one statement scope per execDepth, both slotted).
	if r := db.Exec(`INSERT INTO u VALUES(9, 900)`); r.Error == nil {
		t.Fatalf("expected UNIQUE failure from trigger insert, got none")
	}
	rows := queryRowsPin(t, db, `SELECT COUNT(*) FROM u`)
	if len(rows) != 1 || rows[0] != "3" {
		t.Fatalf("failed statement rolled back: got %v, want [3]", rows)
	}
	rows = queryRowsPin(t, db, `SELECT COUNT(*) FROM z`)
	if len(rows) != 1 || rows[0] != "1" {
		t.Fatalf("trigger write rolled back with statement: got %v, want [1]", rows)
	}
}

// TestArenaPin_SeekAnalysisSlotNesting runs an EQP render (the shared
// analyzeRowidSeek consumer) interleaved with executing point selects, then
// a range seek, so the analysis struct + conjunct slot cycle through
// different shapes; results must stay shape-accurate.
func TestArenaPin_SeekAnalysisSlotNesting(t *testing.T) {
	db := newArenaPinDB(t)
	for i := 0; i < 3; i++ {
		rows := queryRowsPin(t, db, `SELECT c FROM t WHERE id = 1`)
		if len(rows) != 1 || rows[0] != "10" {
			t.Fatalf("point select %d: got %v, want [10]", i, rows)
		}
		rows = queryRowsPin(t, db, `SELECT c FROM t WHERE id BETWEEN 1 AND 2 ORDER BY c`)
		if len(rows) != 2 || rows[0] != "10" || rows[1] != "20" {
			t.Fatalf("range select %d: got %v, want [10 20]", i, rows)
		}
	}
	r := db.Query("EXPLAIN QUERY PLAN SELECT c FROM t WHERE id = 2")
	if r.Error != nil {
		t.Fatalf("eqp: %v", r.Error)
	}
	joined := strings.Join(queryRowsPin(t, db, `SELECT c FROM t WHERE id = 2`), "|")
	if joined != "20" {
		t.Fatalf("point select after eqp: got %q, want 20", joined)
	}
}
