package frigolite

import (
	"testing"
)

func TestDefensiveFTSShadowGuards(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE VIRTUAL TABLE y1 USING fts3"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.SetDefensive(true)
	for _, tc := range []struct{ sql, want string }{
		{"INSERT INTO y1_segments VALUES(1, X'1234567890')", "table y1_segments may not be modified"},
		{"UPDATE y1_segments SET block=X'00'", "table y1_segments may not be modified"},
		{"DELETE FROM y1_segments", "table y1_segments may not be modified"},
		{"DROP TABLE y1_segments", "table y1_segments may not be dropped"},
		{"ALTER TABLE y1_segments RENAME TO abc", "table y1_segments may not be altered"},
		{"CREATE TABLE y1_segments_extra(x)", ""},
	} {
		got := db.Exec(tc.sql).Error
		if tc.want == "" {
			if got != nil {
				t.Errorf("lookalike: unexpected error: %v", got)
			}
		} else if got == nil || got.Error() != tc.want {
			t.Errorf("%s: want error %q, got %v", tc.sql, tc.want, got)
		}
	}
	if got := db.Exec("CREATE TABLE y1_segdir(x)").Error; got == nil || got.Error() != "object name reserved for internal use: y1_segdir" {
		t.Errorf("shadow CREATE: want reserved-name error, got %v", got)
	}
	db.SetDefensive(false)
	if got := db.Exec("INSERT INTO y1_segments VALUES(1, X'1234567890')").Error; got != nil {
		t.Errorf("defensive disabled INSERT: %v", got)
	}
	if got := db.Exec("ALTER TABLE y1_segments RENAME TO abc").Error; got != nil {
		t.Errorf("defensive disabled ALTER: %v", got)
	}
	if got := db.Exec("DROP TABLE abc").Error; got != nil {
		t.Errorf("defensive disabled DROP: %v", got)
	}
}
