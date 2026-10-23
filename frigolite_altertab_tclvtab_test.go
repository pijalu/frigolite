package frigolite

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
)

type altertabTCLModule struct{}
type altertabTCLTable struct{}
type altertabTCLCursor struct{ pos int }

func (altertabTCLModule) Create([]string) (vtab.VirtualTable, error)  { return altertabTCLTable{}, nil }
func (altertabTCLModule) Connect([]string) (vtab.VirtualTable, error) { return altertabTCLTable{}, nil }
func (altertabTCLTable) BestIndex([]byte) ([]byte, error)             { return nil, nil }
func (altertabTCLTable) Columns() []string                            { return []string{"a", "b", "c"} }
func (altertabTCLTable) Open() (vtab.Cursor, error)                   { return &altertabTCLCursor{}, nil }
func (c *altertabTCLCursor) Next() bool                               { c.pos++; return false }
func (*altertabTCLCursor) Column(int) (interface{}, error)            { return nil, nil }
func (*altertabTCLCursor) Close() error                               { return nil }

func TestAltertabTCLVtabCreateRename(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.RegisterVtabModule("tcl", altertabTCLModule{})
	for _, sql := range []string{
		"CREATE VIRTUAL TABLE x1 USING tcl(tcl_command)",
		"ALTER TABLE x1 RENAME TO x2",
	} {
		if got := db.Exec(sql); got.Error != nil {
			t.Fatalf("%s: %v", sql, got.Error)
		}
	}
	got := db.Query("SELECT sql FROM sqlite_master WHERE name='x2'")
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	want := `CREATE VIRTUAL TABLE "x2" USING tcl(tcl_command)`
	if len(got.Rows) != 1 || len(got.Rows[0]) != 1 || got.Rows[0][0] != want {
		t.Fatalf("sqlite_master SQL = %#v, want %q (rows=%s)", got.Rows, want, fmt.Sprint(got.Rows))
	}
}
