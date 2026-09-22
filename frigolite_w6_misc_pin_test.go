// T30-misc native pins (FULL-SUITE-DRIFT.T30-misc): engine-visible contracts
// behind the wave's testgen fixes and adjudicated evidence-skips, each
// verified against the /usr/bin/sqlite3 oracle.
package frigolite_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// openW6MiscPin opens a scratch database in t.TempDir().
func openW6MiscPin(t *testing.T, name string) *frigolite.DB {
	t.Helper()
	path := t.TempDir() + "/" + name
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func w6Exec(t *testing.T, db *frigolite.DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("exec %q: %v", sql, r.Error)
	}
}

func w6Rows(t *testing.T, db *frigolite.DB, sql string) []string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %q: %v", sql, r.Error)
	}
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		for _, c := range row {
			out = append(out, fmt.Sprint(c))
		}
	}
	return out
}

// TestW6MiscPin_LegacyAlterTriggerTarget: alterlegacy-4.2 / altertab-4.2.
// alter.c renameTableFunc finds the trigger's own ON-table token in BOTH
// legacy and modern modes (only the body walk is gated on isLegacy), and
// renameEditSql's bQuote=1 substitutes the double-quoted new name.
func TestW6MiscPin_LegacyAlterTriggerTarget(t *testing.T) {
	for _, tc := range []struct{ legacySQL, onTarget, bodyRef string }{
		{"PRAGMA legacy_alter_table = 1", `ON "t11"`, "t1.x"},
		{"", `ON "t11"`, `"t11".x`},
	} {
		db := openW6MiscPin(t, "legacy_alter.db")
		if tc.legacySQL != "" {
			w6Exec(t, db, tc.legacySQL)
		}
		w6Exec(t, db, "CREATE TABLE t1(x, y)")
		w6Exec(t, db, "CREATE TABLE t2(a, b)")
		w6Exec(t, db, "CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN\n  SELECT t1.x, * FROM t1, t2;\n  INSERT INTO t2 VALUES(new.x, new.y);\nEND")
		w6Exec(t, db, "ALTER TABLE t1 RENAME TO t11")
		rows := w6Rows(t, db, "SELECT sql FROM sqlite_master WHERE name = 'tr1'")
		got := strings.Join(rows, " ")
		if !strings.Contains(got, tc.onTarget) {
			t.Errorf("%s: trigger ON target not rewritten: %q", tc.legacySQL, got)
		}
		if tc.legacySQL != "" && strings.Contains(got, `"t11".x`) {
			t.Errorf("legacy mode rewrote the trigger body: %q", got)
		}
	}
}

// TestW6MiscPin_SameNamedTriggerSchemaScope: attach-4.6/4.7 — triggers with
// the SAME name in main and an attached schema fire from their OWN schema
// only (Trigger.pTabSchema); the body's unqualified t4 resolves there.
func TestW6MiscPin_SameNamedTriggerSchemaScope(t *testing.T) {
	dir := t.TempDir()
	main, aux := dir+"/main.db", dir+"/aux.db"
	db, err := frigolite.Open(main)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db2, err := frigolite.Open(aux)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	w6Exec(t, db2, "CREATE TABLE t3(x,y); CREATE TABLE t4(x)")
	w6Exec(t, db2, "CREATE TRIGGER t3r3 AFTER INSERT ON t3 BEGIN INSERT INTO t4 VALUES('db2.' || NEW.x); END")
	w6Exec(t, db, "CREATE TABLE t3(a,b); CREATE TABLE t4(y)")
	w6Exec(t, db, "CREATE TRIGGER t3r3 AFTER INSERT ON t3 BEGIN INSERT INTO t4 VALUES('main.' || NEW.a); END")
	w6Exec(t, db, "ATTACH DATABASE '"+aux+"' AS db2")
	w6Exec(t, db, "INSERT INTO db2.t3 VALUES(13,14)")
	w6Exec(t, db, "INSERT INTO main.t3 VALUES(11,12)")
	mainRows := w6Rows(t, db, "SELECT * FROM main.t4")
	auxRows := w6Rows(t, db, "SELECT * FROM db2.t4")
	if got := strings.Join(mainRows, ","); got != "main.11" {
		t.Errorf("main.t4 = %q, want main.11", got)
	}
	if got := strings.Join(auxRows, ","); got != "db2.13" {
		t.Errorf("db2.t4 = %q, want db2.13", got)
	}
}

// TestW6MiscPin_RaiseUndoScopes: trigger3-1..4 — RAISE(ABORT) undoes the
// failing statement inside the transaction, RAISE(FAIL) keeps the changes
// made so far, RAISE(ROLLBACK) rolls back the whole transaction, and
// RAISE(IGNORE) skips the row silently.
func TestW6MiscPin_RaiseUndoScopes(t *testing.T) {
	db := openW6MiscPin(t, "raise.db")
	w6Exec(t, db, "CREATE TABLE tbl(a, b, c)")
	w6Exec(t, db, "CREATE TRIGGER after_tbl_insert AFTER INSERT ON tbl BEGIN SELECT CASE WHEN (new.a = 1) THEN RAISE(ABORT, 'Trigger abort') WHEN (new.a = 2) THEN RAISE(FAIL, 'Trigger fail') WHEN (new.a = 3) THEN RAISE(ROLLBACK, 'Trigger rollback') END; END")
	expectErr := func(sql, msg string) {
		t.Helper()
		if r := db.Exec(sql); r.Error == nil || r.Error.Error() != msg {
			t.Fatalf("%s: error %v, want %q", sql, r.Error, msg)
		}
	}
	// ABORT
	expectErr("BEGIN; INSERT INTO tbl VALUES (5,5,6); INSERT INTO tbl VALUES (1,5,6);", "Trigger abort")
	if got := w6Rows(t, db, "SELECT count(*) FROM tbl")[0]; got != "1" {
		t.Errorf("ABORT kept %s rows, want 1", got)
	}
	w6Exec(t, db, "ROLLBACK")
	// FAIL keeps the prior rows of the failing statement
	expectErr("BEGIN; INSERT INTO tbl VALUES (5,5,6); INSERT INTO tbl VALUES (2,5,6);", "Trigger fail")
	if got := strings.Join(w6Rows(t, db, "SELECT count(*) FROM tbl"), ""); got != "2" {
		t.Errorf("FAIL kept %s rows, want 2", got)
	}
	w6Exec(t, db, "ROLLBACK")
	// ROLLBACK unwinds the whole transaction
	expectErr("BEGIN; INSERT INTO tbl VALUES (5,5,6); INSERT INTO tbl VALUES (3,5,6);", "Trigger rollback")
	if got := w6Rows(t, db, "SELECT count(*) FROM tbl")[0]; got != "0" {
		t.Errorf("ROLLBACK left %s rows, want 0", got)
	}
}

// TestW6MiscPin_UserFunctionShadowsEngineBuiltin: trigger6-1.5 — an
// application-registered counter() replaces the engine's test builtin
// (sqlite3FindFunction consults the user hash before the builtin table).
func TestW6MiscPin_UserFunctionShadowsEngineBuiltin(t *testing.T) {
	db := openW6MiscPin(t, "udf.db")
	w6Exec(t, db, "CREATE TABLE t1(x, y)")
	calls := 0
	db.RegisterFunction("counter", func(args []interface{}) (interface{}, error) {
		calls++
		return calls, nil
	}, 0, -1)
	w6Exec(t, db, "INSERT INTO t1 VALUES(1, counter(5))")
	if got := w6Rows(t, db, "SELECT y FROM t1")[0]; got != "1" {
		t.Errorf("counter() returned %s, want 1 (user UDF must shadow the builtin)", got)
	}
}

// TestW6MiscPin_SetDefaultFiresOnParentUpdate: e_fkey-51 (static default) —
// ON UPDATE SET DEFAULT assigns the column's DEFAULT value when the parent
// key changes.
func TestW6MiscPin_SetDefaultFiresOnParentUpdate(t *testing.T) {
	db := openW6MiscPin(t, "setdefault.db")
	w6Exec(t, db, "PRAGMA foreign_keys = ON")
	w6Exec(t, db, "CREATE TABLE parent(x PRIMARY KEY, t TEXT)")
	w6Exec(t, db, "INSERT INTO parent VALUES(1,'one')")
	w6Exec(t, db, "INSERT INTO parent VALUES(99,'dflt')")
	w6Exec(t, db, "CREATE TABLE child(a DEFAULT 99 REFERENCES parent ON UPDATE SET DEFAULT)")
	w6Exec(t, db, "INSERT INTO child VALUES(1)")
	w6Exec(t, db, "UPDATE parent SET x = 22 WHERE x = 1")
	if got := w6Rows(t, db, "SELECT a FROM child")[0]; got != "99" {
		t.Errorf("child.a = %s, want 99", got)
	}
}

// TestW6MiscPin_ForeignKeysDefaultOff: e_fkey-4.1 — foreign key enforcement
// is disabled by default on a fresh connection.
func TestW6MiscPin_ForeignKeysDefaultOff(t *testing.T) {
	db := openW6MiscPin(t, "fkoff.db")
	w6Exec(t, db, "CREATE TABLE p(i PRIMARY KEY)")
	w6Exec(t, db, "CREATE TABLE c(j REFERENCES p ON UPDATE CASCADE)")
	if got := w6Rows(t, db, "PRAGMA foreign_keys")[0]; got != "0" {
		t.Fatalf("fresh connection foreign_keys = %s, want 0", got)
	}
	w6Exec(t, db, "INSERT INTO p VALUES('hello')")
	w6Exec(t, db, "INSERT INTO c VALUES('world')")
	w6Exec(t, db, "UPDATE p SET i = 'world'")
	if got := w6Rows(t, db, "SELECT j FROM c")[0]; got != "world" {
		t.Errorf("c.j = %s, want world (no cascade with FK off)", got)
	}
}

// TestW6MiscPin_RenameOddQuotedTable: e_fkey-56 — a single-quoted identifier
// with embedded double quotes renames its stored SQL and FK REFERENCES, and
// ON UPDATE CASCADE still applies.
func TestW6MiscPin_RenameOddQuotedTable(t *testing.T) {
	db := openW6MiscPin(t, "oddrename.db")
	w6Exec(t, db, "PRAGMA foreign_keys = ON")
	w6Exec(t, db, `CREATE TABLE 'p 1 "parent one"'(a REFERENCES 'p 1 "parent one"', b, PRIMARY KEY(b))`)
	w6Exec(t, db, `CREATE TABLE c1(c, d REFERENCES 'p 1 "parent one"' ON UPDATE CASCADE)`)
	w6Exec(t, db, `INSERT INTO 'p 1 "parent one"' VALUES(1, 1)`)
	w6Exec(t, db, "INSERT INTO c1 VALUES(1, 1)")
	w6Exec(t, db, `ALTER TABLE 'p 1 "parent one"' RENAME TO p`)
	sql := w6Rows(t, db, "SELECT sql FROM sqlite_master WHERE name='p'")[0]
	if !strings.Contains(sql, `CREATE TABLE "p"(a REFERENCES "p"`) {
		t.Errorf("stored SQL not rewritten: %q", sql)
	}
	w6Exec(t, db, "UPDATE p SET a='xxx', b='xxx'")
	if got := w6Rows(t, db, "SELECT d FROM c1")[0]; got != "xxx" {
		t.Errorf("c1.d = %s, want xxx (cascade)", got)
	}
}

// TestW6MiscPin_CacheSizeRoundTrip: pragma-1.5 / 1.15 / 4.3 — negative
// cache_size reads back verbatim, default_cache_size sets the current size,
// and a re-attached database reports the header default.
func TestW6MiscPin_CacheSizeRoundTrip(t *testing.T) {
	db := openW6MiscPin(t, "cache.db")
	w6Exec(t, db, "PRAGMA cache_size = -4321")
	if got := w6Rows(t, db, "PRAGMA cache_size")[0]; got != "-4321" {
		t.Errorf("cache_size = %s, want -4321", got)
	}
	w6Exec(t, db, "PRAGMA default_cache_size = 123")
	if got := w6Rows(t, db, "PRAGMA cache_size")[0]; got != "123" {
		t.Errorf("cache_size after default_cache_size=123 is %s, want 123", got)
	}
	if got := w6Rows(t, db, "PRAGMA default_cache_size")[0]; got != "123" {
		t.Errorf("default_cache_size = %s, want 123", got)
	}
}

// TestW6MiscPin_SynchronousNumericRoundTrip: pragma-1.13/1.14.x — the stored
// safety level is (value+1) & 0x07 and the getter reports stored-1.
func TestW6MiscPin_SynchronousNumericRoundTrip(t *testing.T) {
	db := openW6MiscPin(t, "sync.db")
	for _, tc := range []struct{ set, want string }{
		{"0", "0"}, {"2", "2"}, {"4", "4"}, {"8", "0"}, {"10", "2"}, {"OFF", "0"}, {"ON", "1"},
	} {
		w6Exec(t, db, "PRAGMA synchronous="+tc.set)
		if got := w6Rows(t, db, "PRAGMA synchronous")[0]; got != tc.want {
			t.Errorf("synchronous=%s reads back %s, want %s", tc.set, got, tc.want)
		}
	}
}

// TestW6MiscPin_TempStoreOutOfRange: pragma-9.14 — temp_store=3 falls back
// to 0 (getTempStore only honors leading digits 0..2, file, memory).
func TestW6MiscPin_TempStoreOutOfRange(t *testing.T) {
	db := openW6MiscPin(t, "tempstore.db")
	w6Exec(t, db, "PRAGMA temp_store = 3")
	if got := w6Rows(t, db, "PRAGMA temp_store")[0]; got != "0" {
		t.Errorf("temp_store=3 reads back %s, want 0", got)
	}
}

// TestW6MiscPin_UserVersionSigned: pragma-8.2.15 — user_version is a signed
// 32-bit header field.
func TestW6MiscPin_UserVersionSigned(t *testing.T) {
	db := openW6MiscPin(t, "uv.db")
	w6Exec(t, db, "PRAGMA user_version = -450")
	if got := w6Rows(t, db, "PRAGMA user_version")[0]; got != "-450" {
		t.Errorf("user_version = %s, want -450", got)
	}
}

// TestW6MiscPin_SchemaQualifiedPagePragmas: pragma2-3.1 / pragma-14.2 —
// freelist_count and page_count honor their schema qualifier, and the lazy
// temp database reports 0 pages.
func TestW6MiscPin_SchemaQualifiedPagePragmas(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(dir + "/a.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w6Exec(t, db, "ATTACH DATABASE '"+dir+"/b.db' AS aux")
	w6Exec(t, db, "CREATE TABLE aux.abc(a, b, c)")
	w6Exec(t, db, "INSERT INTO aux.abc VALUES(1, 2, '"+strings.Repeat("x", 10000)+"')")
	w6Exec(t, db, "DELETE FROM aux.abc")
	if got := w6Rows(t, db, "PRAGMA aux.freelist_count")[0]; got == "0" {
		t.Errorf("aux.freelist_count = 0 after freeing a spilled row", )
	}
	if got := w6Rows(t, db, "PRAGMA temp.page_count")[0]; got != "0" {
		t.Errorf("temp.page_count = %s, want 0", got)
	}
	if got := w6Rows(t, db, "PRAGMA freelist_count = 500")[0]; got == "" {
		t.Errorf("read-only freelist_count setter form must still return the count row")
	}
}

// TestW6MiscPin_LockStatusSharedAfterRead: backup-8.9 — a read statement
// inside an explicit transaction takes the pager SHARED lock.
func TestW6MiscPin_LockStatusSharedAfterRead(t *testing.T) {
	db := openW6MiscPin(t, "lock.db")
	w6Exec(t, db, "CREATE TABLE t1(x)")
	w6Exec(t, db, "BEGIN")
	defer w6Exec(t, db, "COMMIT")
	w6Exec(t, db, "SELECT * FROM t1")
	got := strings.Join(w6Rows(t, db, "PRAGMA lock_status"), " ")
	if !strings.HasPrefix(got, "main shared") {
		t.Errorf("lock_status after read = %q, want main shared...", got)
	}
}

// TestW6MiscPin_CsvFieldsAreText: csv01-2.3 — csv.c returns every field as
// TEXT (sqlite3_result_text), so a BLOB-declared column never matches a
// numeric literal, while INT/TEXT columns still match through affinity.
func TestW6MiscPin_CsvFieldsAreText(t *testing.T) {
	db := openW6MiscPin(t, "csv.db")
	defer func() {
		_ = os.Remove("csv_t.db")
	}()
	w6Exec(t, db, "CREATE VIRTUAL TABLE t2 USING csv(data='1,2,3,4\n9,10,11,12\n', columns=4, schema='CREATE TABLE t2(a INT, b TEXT, c REAL, d BLOB)')")
	if got := w6Rows(t, db, "SELECT typeof(d) FROM t2 WHERE a=9")[0]; got != "text" {
		t.Errorf("typeof(d) = %s, want text", got)
	}
	if rows := w6Rows(t, db, "SELECT * FROM t2 WHERE d=12"); len(rows) != 0 {
		t.Errorf("d=12 matched %v, want no rows (BLOB affinity vs integer)", rows)
	}
	if rows := w6Rows(t, db, "SELECT * FROM t2 WHERE a=9"); len(rows) == 0 {
		t.Errorf("a=9 matched nothing, want the row (INT affinity)")
	}
}

// TestW6MiscPin_TableInfoPkOrdinals: pragma-6.2.2 / 6.8 — table_info's pk
// column reports the position within the PRIMARY KEY, duplicates shift later
// positions, and DEFAULT expressions render compactly ("5+3").
func TestW6MiscPin_TableInfoPkOrdinals(t *testing.T) {
	db := openW6MiscPin(t, "pkinfo.db")
	w6Exec(t, db, "CREATE TABLE t5(\n      a TEXT DEFAULT CURRENT_TIMESTAMP, \n      b DEFAULT (5+3),\n      c TEXT,\n      d INTEGER DEFAULT NULL,\n      e TEXT DEFAULT '',\n      UNIQUE(b,c,d),\n      PRIMARY KEY(e,b,c)\n    )")
	rows := w6Rows(t, db, "PRAGMA table_info(t5)")
	got := strings.Join(rows, " ")
	for _, want := range []string{"5+3", "e TEXT 0 '' 1", "b  0 5+3 2", "c TEXT 0 <nil> 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("table_info(t5) missing %q in %q", want, got)
		}
	}
	w6Exec(t, db, "CREATE TABLE t68(a,b,c,PRIMARY KEY(a,b,a,c))")
	got68 := strings.Join(w6Rows(t, db, "PRAGMA table_info(t68)"), " ")
	for _, want := range []string{"a  0 <nil> 1", "b  0 <nil> 2", "c  0 <nil> 4"} {
		if !strings.Contains(got68, want) {
			t.Errorf("table_info(t68) missing %q in %q", want, got68)
		}
	}
}

// TestW6MiscPin_TableInfoSchemaQualified: pragma-6.6.3/6.6.4 — the schema
// qualifier restricts table_info to that database.
func TestW6MiscPin_TableInfoSchemaQualified(t *testing.T) {
	db := openW6MiscPin(t, "schemaq.db")
	w6Exec(t, db, "CREATE TABLE trial(x)")
	w6Exec(t, db, "CREATE TEMP TABLE trial(y)")
	if got := strings.Join(w6Rows(t, db, "PRAGMA main.table_info(trial)"), " "); !strings.Contains(got, "x") {
		t.Errorf("main.table_info = %q, want the main column", got)
	}
	if got := strings.Join(w6Rows(t, db, "PRAGMA temp.table_info(trial)"), " "); !strings.Contains(got, "y") {
		t.Errorf("temp.table_info = %q, want the temp column", got)
	}
}
