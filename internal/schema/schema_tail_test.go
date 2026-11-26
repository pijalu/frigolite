package schema

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// TestInteriorTableCellPointersStartAtHeader12 pins the source of a spurious
// "database disk image is malformed" during INSERT ... SELECT growth. An
// interior table page stores its rightmost child at header+8, so its cell
// pointer array starts at header+12 (btree.c checkTreePage's
// cellStart = hdr+12-4*leaf). Reading the array at +8 treated the right
// child's high 16 bits as a cell offset, followed the bytes at that offset,
// and reached the auto-vacuum pointer-map page (page 2) from a healthy tree.
func TestInteriorTableCellPointersStartAtHeader12(t *testing.T) {
	const pageSize = 512
	const rightChild uint32 = 0x00fd0009

	data := make([]byte, pageSize)
	data[0] = storage.PageTypeInteriorTable
	binary.BigEndian.PutUint16(data[3:5], 1)   // one cell
	binary.BigEndian.PutUint16(data[5:7], 250) // content area
	binary.BigEndian.PutUint32(data[8:12], rightChild)
	// The old +8 read saw offset 253 here; those bytes held child page 2.
	binary.BigEndian.PutUint32(data[253:257], 2)
	// The real pointer array begins at +12. Its masked pointer (0x812c & 511)
	// resolves to 300, where the interior cell's left child is page 10.
	binary.BigEndian.PutUint16(data[12:14], 0x812c)
	binary.BigEndian.PutUint32(data[300:304], 10)

	pg := pager.OpenInMemory(pageSize)
	for i := 0; i < 16; i++ {
		pg.AllocatePage()
	}
	m := NewManager(pg)

	wantFree := []uint32{rightChild, 10}
	if got := appendInteriorChildren(nil, data, 0); !reflect.DeepEqual(got, wantFree) {
		t.Fatalf("appendInteriorChildren: got %v want %v", got, wantFree)
	}
	wantChecked := []uint32{rightChild, 10}
	if got := m.appendCheckedInteriorChildren(nil, data, 0); !reflect.DeepEqual(got, wantChecked) {
		t.Fatalf("appendCheckedInteriorChildren: got %v want %v", got, wantChecked)
	}
}
