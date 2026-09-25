package fts5

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/vtab"
)

// This file ports the fts5_structure table-valued function (ext/fts5/
// fts5_index.c fts5struct*, compiled under SQLITE_TEST || SQLITE_FTS5_DEBUG;
// the engine registers it unconditionally like the other test-only fts5
// surface — fts5_decode, fts5_rowid, fts5tokenize). Given a structure record
// blob (typically `SELECT block FROM <fts>_data WHERE id=10`), it emits one
// row per segment:
//
//	CREATE TABLE xyz(
//	  level, segment, merge, segid, leaf1, leaf2, loc1, loc2,
//	  npgtombstone, nentrytombstone, nentry, struct HIDDEN)
//
// The struct=? binding is REQUIRED: C's xBestIndex returns SQLITE_CONSTRAINT
// without it, which the core surfaces as "no query solution".

// StructModule implements vtab.Module for fts5_structure.
type StructModule struct{}

// NewStructModule creates the fts5_structure module.
func NewStructModule() *StructModule { return &StructModule{} }

// structVTab is one fts5_structure table-valued instance (Fts5StructVtab).
type structVTab struct {
	blob    []byte
	hasBlob bool
}

// Create implements vtab.Module.
func (m *StructModule) Create(args []string) (vtab.VirtualTable, error) {
	return &structVTab{}, nil
}

// Connect implements vtab.Module.
func (m *StructModule) Connect(args []string) (vtab.VirtualTable, error) {
	return &structVTab{}, nil
}

// CreateWithValues binds the TVF argument by value (the struct blob).
func (m *StructModule) CreateWithValues(args []interface{}) (vtab.VirtualTable, error) {
	v := &structVTab{}
	if len(args) > 0 {
		v.setBlob(args[0])
	}
	return v, nil
}

// ConnectWithValues binds the TVF argument by value.
func (m *StructModule) ConnectWithValues(args []interface{}) (vtab.VirtualTable, error) {
	return m.CreateWithValues(args)
}

// setBlob absorbs one candidate struct blob binding.
func (v *structVTab) setBlob(arg interface{}) {
	if v.hasBlob {
		return
	}
	if raw, ok := toBytes(arg); ok {
		v.blob = raw
		v.hasBlob = true
	}
}

// BindSchema implements SchemaBoundVTab (no per-name state).
func (v *structVTab) BindSchema(dbName, tableName string) error { return nil }

// BestIndex implements vtab.VirtualTable.
func (v *structVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Columns reports the declared schema (fts5structConnectMethod's declare).
func (v *structVTab) Columns() []string {
	return []string{"level", "segment", "merge", "segid", "leaf1", "leaf2",
		"loc1", "loc2", "npgtombstone", "nentrytombstone", "nentry", "struct"}
}

// HiddenColumns reports the HIDDEN struct column.
func (v *structVTab) HiddenColumns() map[int]bool { return map[int]bool{11: true} }

// SetHiddenConstraint absorbs the `struct = <blob>` binding.
func (v *structVTab) SetHiddenConstraint(col string, val interface{}) error {
	if col != "struct" {
		return fmt.Errorf("no such column: %s", col)
	}
	v.setBlob(val)
	return nil
}

// ValidateInstance implements vtab.InstanceValidator: a scan without the
// required struct blob fails the way C's SQLITE_CONSTRAINT xBestIndex does.
func (v *structVTab) ValidateInstance() error {
	if !v.hasBlob {
		return fmt.Errorf("no query solution")
	}
	return nil
}

// Open decodes the bound structure record (fts5structFilterMethod's
// fts5StructureDecode) and builds the cursor rows.
func (v *structVTab) Open() (vtab.Cursor, error) {
	if !v.hasBlob {
		return nil, fmt.Errorf("no query solution")
	}
	sr, err := decodeStructRec(v.blob)
	if err != nil {
		return nil, err
	}
	c := &structCursor{}
	for lvl, segs := range sr.Levels {
		for iSeg, seg := range segs {
			c.rows = append(c.rows, structRow{
				level:   int64(lvl),
				segment: int64(iSeg),
				merge:   0, // mirror records never persist an in-flight merge
				segid:   seg.Segid,
				leaf1:   seg.PgnoFirst,
				leaf2:   seg.PgnoLast,
				loc1:    int64(seg.Origin1),
				loc2:    int64(seg.Origin2),
				npgtomb: seg.NPgTombstone,
				ntomb:   seg.NEntryTombstone,
				nentry:  seg.NEntry,
			})
		}
	}
	return c, nil
}

// structRow is one output row (fts5structColumnMethod).
type structRow struct {
	level, segment, merge, segid    int64
	leaf1, leaf2, loc1, loc2        int64
	npgtomb, ntomb, nentry          int64
}

// structCursor walks the decoded segments (fts5structNextMethod/EofMethod).
type structCursor struct {
	rows []structRow
	idx  int
}

func (c *structCursor) Next() bool { c.idx++; return c.idx <= len(c.rows) }
func (c *structCursor) Close() error {
	c.rows = nil
	return nil
}

// Column serves one output column.
func (c *structCursor) Column(idx int) (interface{}, error) {
	if c.idx <= 0 || c.idx > len(c.rows) {
		return nil, nil
	}
	r := c.rows[c.idx-1]
	switch idx {
	case 0:
		return r.level, nil
	case 1:
		return r.segment, nil
	case 2:
		return r.merge, nil
	case 3:
		return r.segid, nil
	case 4:
		return r.leaf1, nil
	case 5:
		return r.leaf2, nil
	case 6:
		return r.loc1, nil
	case 7:
		return r.loc2, nil
	case 8:
		return r.npgtomb, nil
	case 9:
		return r.ntomb, nil
	case 10:
		return r.nentry, nil
	}
	return nil, nil // struct HIDDEN
}

var _ vtab.HiddenConstraintSetter = (*structVTab)(nil)
var _ vtab.InstanceValidator = (*structVTab)(nil)
