package frigolite

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/storage"
)

func TestAnalyzeAuthorizationDeniesAnalyze(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE analyze_auth_test(a)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.SetAuthorizer(auth.NewActionFilterAuthorizer(auth.ActionAnalyze))
	if res := db.Exec("ANALYZE"); res.Error == nil || res.Error.Error() != "not authorized" {
		t.Fatalf("ANALYZE error = %v, want not authorized", res.Error)
	}
}

func TestAnalyzeStat4StoresIndexSampleRecords(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE stat4_sample_test(a TEXT)",
		"INSERT INTO stat4_sample_test VALUES('alpha'),('bravo'),('charlie'),('delta')",
		"CREATE INDEX stat4_sample_test_a ON stat4_sample_test(a)",
		"ANALYZE stat4_sample_test",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT nEq, nLt, nDLt, sample FROM sqlite_stat4 WHERE idx='stat4_sample_test_a' ORDER BY rowid")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	wantKeys := []string{"alpha", "bravo", "charlie", "delta"}
	if len(res.Rows) != len(wantKeys) {
		t.Fatalf("sqlite_stat4 sample rows = %d, want %d: %#v", len(res.Rows), len(wantKeys), res.Rows)
	}
	for i, row := range res.Rows {
		if len(row) != 4 || row[3] == nil {
			t.Fatalf("sqlite_stat4 sample row = %#v, want statistics and non-NULL sample record", row)
		}
		sample, ok := row[3].([]byte)
		if !ok {
			t.Fatalf("sample type = %T, want []byte", row[3])
		}
		decoded, err := storage.DecodeRecord(sample)
		if err != nil {
			t.Fatalf("decode sample %d: %v", i, err)
		}
		if len(decoded.Values) != 2 || decoded.Values[0] != wantKeys[i] || decoded.Values[1] != int64(i+1) {
			t.Fatalf("sample %d decoded = %#v", i, decoded.Values)
		}
		wantStats := []interface{}{"1 1", fmt.Sprintf("%d %d", i, i), fmt.Sprintf("%d %d", i, i)}
		if !reflect.DeepEqual(row[:3], wantStats) {
			t.Fatalf("sample %d stats = %#v, want %#v", i, row[:3], wantStats)
		}
	}
	if res := db.Exec("ANALYZE stat4_sample_test"); res.Error != nil {
		t.Fatal(res.Error)
	}
	recount := db.Query("SELECT count(*) FROM sqlite_stat4 WHERE idx='stat4_sample_test_a'")
	if recount.Error != nil || len(recount.Rows) != 1 || recount.Rows[0][0] != int64(4) {
		t.Fatalf("reanalysis sample count=%#v error=%v", recount.Rows, recount.Error)
	}
}

func TestAnalyzeStat4RetainsDistinctPrefixSamples(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"CREATE TABLE stat4_distinct(a TEXT)", "CREATE INDEX stat4_distinct_a ON stat4_distinct(a)"} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	for i := 0; i < 1000; i++ {
		value := []string{"alpha", "bravo", "charlie", "delta"}[i/250]
		if res := db.Exec(fmt.Sprintf("INSERT INTO stat4_distinct VALUES('%s')", value)); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	if res := db.Exec("ANALYZE stat4_distinct"); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := db.Query("SELECT nEq,nLt,nDLt,sample FROM sqlite_stat4 WHERE idx='stat4_distinct_a' ORDER BY rowid")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 8 {
		t.Fatalf("STAT4 sample count=%d want SQLite periodic count 8", len(res.Rows))
	}
	want := []struct {
		key                        string
		rowid                      int64
		prefixLt, prefixDLt, rowLt int
	}{
		{"alpha", 112, 0, 0, 111}, {"alpha", 224, 0, 0, 223},
		{"bravo", 336, 250, 1, 335}, {"bravo", 448, 250, 1, 447},
		{"charlie", 560, 500, 2, 559}, {"charlie", 672, 500, 2, 671},
		{"delta", 784, 750, 3, 783}, {"delta", 896, 750, 3, 895},
	}
	for i, row := range res.Rows {
		stats := []interface{}{"250 1", fmt.Sprintf("%d %d", want[i].prefixLt, want[i].rowLt), fmt.Sprintf("%d %d", want[i].prefixDLt, want[i].rowLt)}
		if !reflect.DeepEqual(row[:3], stats) {
			t.Fatalf("sample %d stats=%#v want %#v", i, row[:3], stats)
		}
		sample, ok := row[3].([]byte)
		if !ok {
			t.Fatalf("sample type %T", row[3])
		}
		rec, err := storage.DecodeRecord(sample)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rec.Values, []interface{}{want[i].key, want[i].rowid}) {
			t.Fatalf("sample %d key=%#v", i, rec.Values)
		}
	}
}

func TestAnalyze5Stat4SampleContracts(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE analyze5_stat4(t REAL,u,v TEXT COLLATE NOCASE,w,x,y,z)",
		"CREATE INDEX t1t ON analyze5_stat4(t)",
		"CREATE INDEX t1u ON analyze5_stat4(u)",
		"CREATE INDEX t1v ON analyze5_stat4(v)",
		"CREATE INDEX t1w ON analyze5_stat4(w)",
		"CREATE INDEX t1x ON analyze5_stat4(x)",
		"CREATE INDEX t1y ON analyze5_stat4(y)",
		"CREATE INDEX t1z ON analyze5_stat4(z)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	for i := 0; i < 1000; i++ {
		group := 0
		if i >= 400 {
			group++
		}
		if i >= 700 {
			group++
		}
		if i >= 875 {
			group++
		}
		names := []string{"alpha", "bravo", "charlie", "delta"}
		v := names[group]
		if i%2 == 1 {
			v = strings.ToUpper(v)
		}
		x, w := interface{}(group), interface{}(group)
		if group == 0 {
			x = nil
		}
		if group == 3 {
			w = nil
		}
		y := 0
		if i >= 25 && i <= 50 {
			y = 1
		}
		stmt := fmt.Sprintf("INSERT INTO analyze5_stat4 VALUES(%g,'%s','%s',%v,%v,%d,%d)", float64(group)+0.5, names[group], v, stat4SQLLiteral(w), stat4SQLLiteral(x), y, group)
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("insert %d: %v", i, res.Error)
		}
	}
	if res := db.Exec("ANALYZE analyze5_stat4"); res.Error != nil {
		t.Fatal(res.Error)
	}
	for _, index := range []string{"t1u", "t1v"} {
		res := db.Query("SELECT sample FROM sqlite_stat4 WHERE idx='" + index + "' ORDER BY nLt")
		if res.Error != nil {
			t.Fatal(res.Error)
		}
		seen := map[string]bool{}
		for _, row := range res.Rows {
			blob, ok := row[0].([]byte)
			if !ok {
				continue
			}
			rec, err := storage.DecodeRecord(blob)
			if err != nil {
				t.Fatal(err)
			}
			if len(rec.Values) > 0 {
				if s, ok := rec.Values[0].(string); ok {
					seen[strings.ToLower(s)] = true
				}
			}
		}
		if len(seen) != 4 {
			t.Fatalf("%s STAT4 labels=%v want alpha/bravo/charlie/delta", index, seen)
		}
	}
	counts := db.Query("SELECT idx,count(*) FROM sqlite_stat4 GROUP BY idx")
	if counts.Error != nil {
		t.Fatal(counts.Error)
	}
	wantCounts := map[string]int{"t1t": 8, "t1u": 8, "t1v": 8, "t1w": 8, "t1x": 8, "t1y": 9, "t1z": 8}
	for _, row := range counts.Rows {
		name, _ := row[0].(string)
		got, _ := row[1].(int64)
		if int(got) != wantCounts[name] {
			t.Errorf("%s sample rows=%d want %d", name, got, wantCounts[name])
		}
		delete(wantCounts, name)
	}
	if len(wantCounts) != 0 {
		t.Fatalf("missing STAT4 indexes: %v", wantCounts)
	}
	ySample := db.Query("SELECT nEq,nLt,nDLt,sample FROM sqlite_stat4 WHERE idx='t1y' ORDER BY rowid DESC LIMIT 1")
	if ySample.Error != nil || len(ySample.Rows) != 1 {
		t.Fatalf("t1y extra-prefix sample=%#v error=%v", ySample.Rows, ySample.Error)
	}
	if !reflect.DeepEqual(ySample.Rows[0][:3], []interface{}{"26 1", "974 975", "1 975"}) {
		t.Fatalf("t1y extra sample stats=%#v", ySample.Rows[0][:3])
	}
	yRecord, err := storage.DecodeRecord(ySample.Rows[0][3].([]byte))
	if err != nil || !reflect.DeepEqual(yRecord.Values, []interface{}{int64(1), int64(27)}) {
		t.Fatalf("t1y extra sample record=%#v err=%v", yRecord, err)
	}
}

func stat4SQLLiteral(v interface{}) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}

func TestAnalyzeStat4InterleavedMultiColumnOracleSamples(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"CREATE TABLE stat4_interleaved(a INT,b INT)", "CREATE INDEX stat4_interleaved_ab ON stat4_interleaved(a,b)"} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	for a := 0; a < 10; a++ {
		for b := 0; b < 100; b++ {
			if res := db.Exec(fmt.Sprintf("INSERT INTO stat4_interleaved VALUES(%d,%d)", a, b%5)); res.Error != nil {
				t.Fatal(res.Error)
			}
		}
	}
	if res := db.Exec("ANALYZE stat4_interleaved"); res.Error != nil {
		t.Fatal(res.Error)
	}
	ordered := db.Query("SELECT a,b,rowid FROM stat4_interleaved INDEXED BY stat4_interleaved_ab ORDER BY a,b,rowid")
	if ordered.Error != nil {
		t.Fatal(ordered.Error)
	}
	if len(ordered.Rows) != 1000 {
		t.Fatalf("index cursor rows=%d", len(ordered.Rows))
	}
	for pos, row := range ordered.Rows {
		if row[0] != int64(pos/100) || row[1] != int64((pos/20)%5) {
			t.Fatalf("index order at %d: %#v", pos, row)
		}
		if pos > 0 && row[0] == ordered.Rows[pos-1][0] && row[1] == ordered.Rows[pos-1][1] && row[2].(int64) <= ordered.Rows[pos-1][2].(int64) {
			t.Fatalf("rowid suffix not ascending at %d: %#v after %#v", pos, row, ordered.Rows[pos-1])
		}
	}
	rows := db.Query("SELECT nEq,nLt,nDLt,sample FROM sqlite_stat4 WHERE idx='stat4_interleaved_ab' ORDER BY rowid")
	if rows.Error != nil {
		t.Fatal(rows.Error)
	}
	if len(rows.Rows) != 24 {
		t.Fatalf("sample count=%d want SQLite max 24", len(rows.Rows))
	}
	want := []string{
		"100 20 1|0 40 52|0 2 52|04080101023F", "100 20 1|0 60 71|0 3 71|04080101033B", "100 20 1|0 80 94|0 4 94|04080101044B",
		"100 20 1|100 100 111|1 5 111|04090802009C", "100 20 1|100 140 141|1 7 141|04090101026C", "100 20 1|200 200 207|2 10 207|040108020200EC",
		"100 20 1|200 220 223|2 11 223|040109020200D9", "100 20 1|200 280 288|2 14 288|04010102020400F5", "100 20 1|300 300 302|3 15 302|04010802030137",
		"100 20 1|300 320 335|3 16 335|04010902030179", "100 20 1|300 340 357|3 17 357|0401010203020184", "100 20 1|300 360 361|3 18 361|0401010203030135",
		"100 20 1|300 380 396|3 19 396|0401010203040181", "100 20 1|400 400 419|4 20 419|040108020401F0", "100 20 1|400 440 447|4 22 447|04010102040201B6",
		"100 20 1|500 500 513|5 25 513|04010802050236", "100 20 1|500 520 523|5 26 523|04010902050205", "100 20 1|500 540 559|5 27 559|0401010205020256",
		"100 20 1|600 660 671|6 33 671|0401010206030293", "100 20 1|700 700 713|7 35 713|040108020702FE", "100 20 1|700 760 771|7 38 771|04010102070302F7",
		"100 20 1|700 780 783|7 39 783|04010102070402D0", "100 20 1|800 880 895|8 44 895|0401010208040370", "100 20 1|900 980 999|9 49 999|04010102090403E8",
	}
	for i, row := range rows.Rows {
		blob, ok := row[3].([]byte)
		if !ok {
			t.Fatalf("sample %d type=%T", i, row[3])
		}
		got := fmt.Sprintf("%v|%v|%v|%s", row[0], row[1], row[2], strings.ToUpper(hex.EncodeToString(blob)))
		if got != want[i] {
			rec, _ := storage.DecodeRecord(blob)
			t.Errorf("sample %d=%s decoded=%#v want %s", i, got, rec.Values, want[i])
		}
	}
}

func TestAnalyzeStat4EmptyIndexHasNoSamples(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"CREATE TABLE stat4_empty(a TEXT)", "CREATE INDEX stat4_empty_a ON stat4_empty(a) WHERE a IS NOT NULL", "ANALYZE stat4_empty"} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT count(*) FROM sqlite_stat4 WHERE idx='stat4_empty_a'")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(0) {
		t.Fatalf("empty index STAT4 rows=%#v", res.Rows)
	}
}

func TestAnalyzeStat4DuplicateMultiColumnSamples(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE stat4_dup(a TEXT,b INT)",
		"INSERT INTO stat4_dup VALUES('a',1),('a',2),('a',3),('b',1),('b',2),('c',1)",
		"CREATE INDEX stat4_dup_ab ON stat4_dup(a,b)",
		"ANALYZE stat4_dup",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT nEq,nLt,nDLt,sample FROM sqlite_stat4 WHERE idx='stat4_dup_ab' ORDER BY rowid")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := [][]string{{"3 1 1", "0 0 0", "0 0 0"}, {"3 1 1", "0 1 1", "0 1 1"}, {"3 1 1", "0 2 2", "0 2 2"}, {"2 1 1", "3 3 3", "1 3 3"}, {"2 1 1", "3 4 4", "1 4 4"}, {"1 1 1", "5 5 5", "2 5 5"}}
	if len(res.Rows) != len(want) {
		t.Fatalf("samples=%d want %d: %#v", len(res.Rows), len(want), res.Rows)
	}
	wantKeys := [][]interface{}{{"a", int64(1), int64(1)}, {"a", int64(2), int64(2)}, {"a", int64(3), int64(3)}, {"b", int64(1), int64(4)}, {"b", int64(2), int64(5)}, {"c", int64(1), int64(6)}}
	for i, row := range res.Rows {
		for j := 0; j < 3; j++ {
			if row[j] != want[i][j] {
				t.Errorf("sample %d stat %d=%v want %s", i, j, row[j], want[i][j])
			}
		}
		sample, ok := row[3].([]byte)
		if !ok {
			t.Fatalf("sample %d type=%T", i, row[3])
		}
		rec, err := storage.DecodeRecord(sample)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rec.Values, wantKeys[i]) {
			t.Fatalf("sample %d record=%#v want %#v", i, rec.Values, wantKeys[i])
		}
	}
}
