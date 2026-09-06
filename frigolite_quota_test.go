package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/quota"
)

// The quota-layer contract pinned here mirrors ext/misc/test_quota.c and is
// oracle-checked against the SQLite TCL suite (test/quota.test, test/quota2.test):
//
//   - a database file opened while the layer is initialized joins the group
//     matching its path; growth past the group limit invokes the callback
//     (which may raise or zero the limit) and is refused with SQLITE_FULL
//     ("database or disk is full") otherwise;
//   - a zero limit set by the callback DISABLES enforcement for that write
//     (test_quota.c quotaWrite's second check is gated on iLimit>0);
//   - shutdown refuses with SQLITE_MISUSE while any connection is open.

func TestNativeQuota_DenyGrowthPastLimit(t *testing.T) {
	dir := t.TempDir()
	if code := quota.Initialize("", true); code != quota.OK {
		t.Fatalf("initialize: %d", code)
	}
	t.Cleanup(func() { quota.Shutdown() })
	type cbCall struct {
		name    string
		limit   int64
		size    int64
		extends bool
	}
	var calls []cbCall
	requestOK := false
	if code := quota.Set("*"+filepath.Base(dir)+"*/qtest.db", 4096, func(name string, limit *int64, size int64) {
		calls = append(calls, cbCall{name: name, limit: *limit, size: size, extends: requestOK})
		if requestOK {
			*limit = size
		}
	}); code != quota.OK {
		t.Fatalf("set: %d", code)
	}
	// The pattern must match the temp-dir path.
	db, err := openQuotaDB(t, dir, "qtest.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{"PRAGMA page_size=1024", "CREATE TABLE t1(a, b)"} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	// Fill to exactly the limit: page 4 is allowed (szNew == limit is not
	// over-limit in test_quota.c quotaWrite's `szNew > iLimit` check).
	for i := 0; i < 2; i++ {
		if r := db.Exec("INSERT INTO t1 VALUES(1, randomblob(900))"); r.Error != nil {
			t.Fatalf("setup insert %d: %v", i, r.Error)
		}
	}
	// The growth past 4096 must be refused and must invoke the callback.
	res := db.Exec("INSERT INTO t1 VALUES(2, randomblob(900))")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "database or disk is full") {
		t.Fatalf("insert past quota: want 'database or disk is full', got %v", res.Error)
	}
	if len(calls) == 0 {
		t.Fatal("callback never invoked on over-limit write")
	}
	if calls[0].extends {
		t.Fatal("callback extended the limit while requestOK was false")
	}
}

func TestNativeQuota_CallbackExtensionAllowsGrowth(t *testing.T) {
	dir := t.TempDir()
	if code := quota.Initialize("", true); code != quota.OK {
		t.Fatalf("initialize: %d", code)
	}
	t.Cleanup(func() { quota.Shutdown() })
	requestOK := false
	if code := quota.Set("*"+filepath.Base(dir)+"*/qtest2.db", 4096, func(name string, limit *int64, size int64) {
		if requestOK {
			*limit = size
		}
	}); code != quota.OK {
		t.Fatalf("set: %d", code)
	}
	db, err := openQuotaDB(t, dir, "qtest2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{"PRAGMA page_size=1024", "CREATE TABLE t1(a, b)"} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	requestOK = true
	if r := db.Exec("INSERT INTO t1 VALUES(1, randomblob(1500))"); r.Error != nil {
		t.Fatalf("insert with callback extension: %v", r.Error)
	}
	// A zero limit set by the callback disables enforcement entirely
	// (quota-3.3.1): subsequent growth succeeds unbounded.
	requestOK = true
	for i := 0; i < 3; i++ {
		if r := db.Exec("INSERT INTO t1 VALUES(2, randomblob(1500))"); r.Error != nil {
			t.Fatalf("insert %d with zeroed limit: %v", i, r.Error)
		}
	}
}

func TestNativeQuota_ShutdownMisuseWhileOpen(t *testing.T) {
	if code := quota.Initialize("", true); code != quota.OK {
		t.Fatalf("initialize: %d", code)
	}
	defer quota.Shutdown()
	// A connection on a file in a quota group blocks shutdown
	// (test_quota.c quotaShutdown refuses while quota files are open).
	dir := t.TempDir()
	if code := quota.Set("*"+filepath.Base(dir)+"*/qtest3.db", 4096, nil); code != quota.OK {
		t.Fatalf("set: %d", code)
	}
	db, err := openQuotaDB(t, dir, "qtest3.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if code := quota.Shutdown(); code != quota.MISUSE {
		t.Fatalf("shutdown with open connection: want MISUSE (%d), got %d", quota.MISUSE, code)
	}
}

func TestNativeQuota_Strglob(t *testing.T) {
	cases := []struct {
		pattern, text string
		want          bool
	}{
		{"abcdefg", "abcdefg", true},
		{"abcdefG", "abcdefg", false},
		{"abcdef?", "abcdefg", true},
		{"abc/def", "abc\\def", true},  // quota-glob-10.2: '/' matches '\'
		{"*/abc/*", "x\\abc\\y", true}, // quota-glob-12.2
		{"abc[][*?]efg", "abc]efg", true},
		{"abc[][*?]efg", "abc[efg", true},
		{"abc[^][*?]efg", "abcdefg", true},
		{"*[xyz]efg", "abcxefg", true}, // quota-glob-53
		{"*[xyz]efg", "abcwefg", false},
		{"{abc", "{abc", true},
	}
	for _, tc := range cases {
		if got := quota.Strglob(tc.pattern, tc.text); got != tc.want {
			t.Errorf("Strglob(%q, %q) = %v, want %v", tc.pattern, tc.text, got, tc.want)
		}
	}
}

// openQuotaDB opens dir/name while the quota layer is initialized so the
// file registers with its matching quota group.
func openQuotaDB(t *testing.T, dir, name string) (*frigolite.DB, error) {
	return frigolite.Open(filepath.Join(dir, name))
}
