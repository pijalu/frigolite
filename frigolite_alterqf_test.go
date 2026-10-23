package frigolite

import "testing"

func TestAlterQF_RenameColumnRewritesDQSObjects(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetDQS(true, true)
	if res := db.Exec(`CREATE TABLE x1(one, two, three, PRIMARY KEY(one), CHECK (three!="xyz"), CHECK (two!="one")) WITHOUT ROWID;
CREATE INDEX x1i ON x1(one+"two"+"four") WHERE "five";
CREATE TEMP TRIGGER AFTER INSERT ON x1 BEGIN UPDATE x1 SET two=new.three || "new" WHERE one=new.one||""; END;`); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec(`ALTER TABLE x1 RENAME two TO 'four'`); res.Error != nil {
		t.Fatal(res.Error)
	}
	main := db.Query("SELECT type, sql FROM sqlite_schema WHERE name IN ('x1','x1i') ORDER BY type")
	if main.Error != nil {
		t.Fatal(main.Error)
	}
	if len(main.Rows) != 2 || main.Rows[0][1] != `CREATE INDEX x1i ON x1(one+"four"+'four') WHERE 'five'` || main.Rows[1][1] != `CREATE TABLE x1(one, "four", three, PRIMARY KEY(one), CHECK (three!='xyz'), CHECK ("four"!="one")) WITHOUT ROWID` {
		t.Fatalf("main schema = %#v", main.Rows)
	}
	temp := db.Query("SELECT sql FROM sqlite_temp_schema WHERE type='trigger'")
	if temp.Error != nil {
		t.Fatal(temp.Error)
	}
	if len(temp.Rows) != 1 || temp.Rows[0][0] != `CREATE TRIGGER AFTER INSERT ON x1 BEGIN UPDATE x1 SET "four"=new.three || 'new' WHERE one=new.one||''; END` {
		t.Fatalf("temp trigger = %#v", temp.Rows)
	}
}
