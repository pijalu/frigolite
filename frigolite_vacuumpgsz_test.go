// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
package frigolite

import (
	"path/filepath"
	"testing"
)

// TestVacuumPendingPageSizeAfterInsert pins vacuum-11.2/11.3: a pending page
// size (PRAGMA page_size=N) must be adopted by the next VACUUM even when the
// connection has already INSERTed rows at the old page size. The insert write
// path's cached b-tree wrapper snapshots the page geometry at build time, so
// the rebuild's copy-back (which re-inserts every row through the same
// executor) must observe the layout hook's invalidation — a stale wrapper
// writes the old geometry into the new layout, the copy-back fails
// "database disk image is malformed", and the rebuild's restore path silently
// reverts the file to the old page size.
func TestVacuumPendingPageSizeAfterInsert(t *testing.T) {
	for _, tc := range []struct {
		name string
		ps   string
	}{
		{"2048", "2048"},
		{"4096", "4096"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "vacps.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if r := db.Exec("CREATE TABLE t8(a, b)"); r.Error != nil {
				t.Fatal(r.Error)
			}
			if r := db.Exec("INSERT INTO t8 VALUES (1, 'x'), (2, 'y'), (3, 'z')"); r.Error != nil {
				t.Fatal(r.Error)
			}
			// First vacuum keeps/adopts the explicit base size, then the
			// pending size change must be adopted by the second VACUUM.
			if r := db.Exec("PRAGMA page_size=1024"); r.Error != nil {
				t.Fatal(r.Error)
			}
			if r := db.Exec("VACUUM"); r.Error != nil {
				t.Fatal(r.Error)
			}
			if r := db.Exec("PRAGMA page_size=" + tc.ps); r.Error != nil {
				t.Fatal(r.Error)
			}
			if r := db.Exec("VACUUM"); r.Error != nil {
				t.Fatal(r.Error)
			}
			res := db.Query("PRAGMA page_size")
			if res.Error != nil {
				t.Fatal(res.Error)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("PRAGMA page_size: got %d rows, want 1", len(res.Rows))
			}
			got, _ := res.Rows[0][0].(int64)
			if want := psForName(t, tc.ps); got != want {
				t.Fatalf("PRAGMA page_size after VACUUM: got %d, want %d", got, want)
			}
		})
	}
}

// psForName parses the pin table's page-size literal.
func psForName(t *testing.T, s string) int64 {
	t.Helper()
	switch s {
	case "2048":
		return 2048
	case "4096":
		return 4096
	}
	t.Fatalf("unknown page size %q", s)
	return 0
}
