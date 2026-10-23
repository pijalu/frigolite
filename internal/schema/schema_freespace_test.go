package schema

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func TestFreeSpaceWalkStopsAtCyclicInteriorPage(t *testing.T) {
	pg, err := pager.Open(filepath.Join(t.TempDir(), "cycle.db"), pager.DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	if pg.AllocatePage() == nil {
		t.Fatal("allocate page 1")
	}
	page := pg.AllocatePage()
	if page == nil || page.PageNum == 1 {
		t.Fatal("allocate interior page")
	}
	page.Data[0] = storage.PageTypeInteriorTable
	binary.BigEndian.PutUint32(page.Data[8:12], page.PageNum)
	m := NewManager(pg)
	finished := make(chan struct{})
	go func() {
		m.freeSpaceWalk(page.PageNum, map[uint32]bool{})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("free-space traversal did not stop at cyclic child page")
	}
}
