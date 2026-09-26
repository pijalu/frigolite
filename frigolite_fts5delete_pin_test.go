package frigolite

import "testing"

// Native pins for the special-'delete' corruption contract (T33r-fts,
// 2026-09-26; testgen/fts5delete 2.1-2.4). Every expectation was validated
// against the sqlite3 CLI oracle 3.54.0 before being pinned:
//
//   - a special 'delete' whose supplied tokens underflow a column total
//     (empty table) fails immediately with "database disk image is
//     malformed" (2.1);
//   - a second 'delete' of a (term, rowid) pair the index no longer holds
//     fails the same way (2.2);
//   - a redundant 'delete' of the same (term, rowid) while OTHER documents
//     remain is silent, but C's flushed doclist would hold that rowid
//     twice — a structural violation (doclist rowids are strictly
//     increasing) that every later index READ reports as "database disk
//     image is malformed" while 'integrity-check' still passes (2.3/2.4,
//     and the oracle keeps MATCH 'one' working because its own iterator
//     never reaches the violating entry — the mirror flags all index
//     reads, a documented superset);
//   - a 'delete' of a never-seen (term, rowid) stays a silent no-op
//     (fts5secure4 1.1);
//   - re-inserting a deleted document consumes its markers, so a later
//     'delete' of the same pair writes a fresh marker instead of a
//     duplicate (C's merge semantics).
func TestFTS5SpecialDeleteCorruptPin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE test (id INTEGER PRIMARY KEY, name TEXT, value TEXT);
CREATE VIRTUAL TABLE ti USING fts5(name, content=test, content_rowid=id);`); res.Error != nil {
		t.Fatal(res.Error)
	}
	exec := func(sql string) error { return db.Exec(sql).Error }
	query := func(sql string) error { return db.Query(sql).Error }
	corrupt := "database disk image is malformed"

	// 2.1 — column-total underflow on an empty index.
	if err := exec(`INSERT INTO ti(ti, rowid, name) VALUES('delete', 1, 'quick');`); err == nil || err.Error() != corrupt {
		t.Errorf("2.1: want %q, got %v", corrupt, err)
	}

	// 2.2 — the second delete of a removed pair fails immediately.
	if err := exec(`INSERT INTO ti(rowid, name) VALUES(123, 'one one one');`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO ti(ti, rowid, name) VALUES('delete', 123, 'one');`); err != nil {
		t.Fatalf("2.2 first delete: %v", err)
	}
	if err := exec(`INSERT INTO ti(ti, rowid, name) VALUES('delete', 123, 'one');`); err == nil || err.Error() != corrupt {
		t.Errorf("2.2: want %q, got %v", corrupt, err)
	}

	// 2.3/2.4 — the redundant triple delete is silent, later MATCH reads
	// are corrupt, 'integrity-check' still passes.
	if err := exec(`DROP TABLE ti;
CREATE VIRTUAL TABLE ti USING fts5(name, content=test, content_rowid=id);
INSERT INTO ti(rowid, name) VALUES(123, 'one one one');
INSERT INTO ti(rowid, name) VALUES(124, 'two two two');
INSERT INTO ti(rowid, name) VALUES(125, 'two two two');
INSERT INTO ti(ti, rowid, name) VALUES('delete', 123, 'one');
INSERT INTO ti(ti, rowid, name) VALUES('delete', 123, 'one');
INSERT INTO ti(ti, rowid, name) VALUES('delete', 123, 'one');`); err != nil {
		t.Fatalf("2.3: %v", err)
	}
	if err := query(`SELECT rowid FROM ti WHERE ti MATCH 'two';`); err == nil || err.Error() != corrupt {
		t.Errorf("2.4: want %q, got %v", corrupt, err)
	}
	if err := query(`SELECT rowid FROM ti WHERE ti MATCH 'two' ORDER BY rank;`); err == nil || err.Error() != corrupt {
		t.Errorf("2.4 rank: want %q, got %v", corrupt, err)
	}
	if err := exec(`INSERT INTO ti(ti) VALUES('integrity-check');`); err != nil {
		t.Errorf("2.4 integrity-check: want success, got %v", err)
	}

	// fts5secure4 1.1 — a never-seen (term, rowid) pair is a silent no-op.
	if err := exec(`DROP TABLE ti;
CREATE VIRTUAL TABLE ti USING fts5(name, content=test, content_rowid=id);
INSERT INTO ti(rowid, name) VALUES(9, 'crunch cronch');`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO ti(ti, rowid, name) VALUES('delete', 4, 'nosuchtoken');`); err != nil {
		t.Errorf("secure4: want success, got %v", err)
	}

	// Re-insert consumes the markers — the second delete pair is fresh.
	if err := exec(`DROP TABLE ti;
CREATE VIRTUAL TABLE ti USING fts5(name, content=test, content_rowid=id);
INSERT INTO ti(rowid, name) VALUES(9, 'crunch');
INSERT INTO ti(ti, rowid, name) VALUES('delete', 9, 'crunch');
INSERT INTO ti(rowid, name) VALUES(9, 'crunch');
INSERT INTO ti(ti, rowid, name) VALUES('delete', 9, 'crunch');`); err != nil {
		t.Fatalf("reinsert cycle: %v", err)
	}
	if err := query(`SELECT rowid FROM ti WHERE ti MATCH 'crunch';`); err != nil {
		t.Errorf("after reinsert cycle: want success, got %v", err)
	}
}
