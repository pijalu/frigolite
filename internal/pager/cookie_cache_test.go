// SchemaCookie atomic fast-path cache pins: the lock-free read must track
// every path that can move the header cookie — BumpSchemaCookie (DDL), a
// whole-header SetHeader (PRAGMA schema_version), an AmendHeader in-place
// mutation, a Snapshot/Restore cycle (DDL transaction rollback), and a
// statement-journal rollback that reinstates the begin header. A stale
// cached value would keep cookie-keyed derived caches (the schema manager's
// entry cache, the engine's fingerprint memos) from rebuilding.
package pager

import (
	"encoding/binary"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// cookieOfLocked reads the cookie straight from the header image. Caller
// holds p.mu (RLock is enough).
func cookieOfLocked(p *Pager) uint32 {
	return binary.BigEndian.Uint32(p.header[40:44])
}

func TestSchemaCookieCacheTracksMutations(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	// Prime the cache, then re-read: the fast path must serve the same
	// value.
	c0 := p.SchemaCookie()
	if got := p.SchemaCookie(); got != c0 {
		t.Fatalf("primed read moved: %d then %d", c0, got)
	}

	// DDL: BumpSchemaCookie must be visible on the next lock-free read.
	p.BumpSchemaCookie()
	if got := p.SchemaCookie(); got != c0+1 {
		t.Fatalf("after BumpSchemaCookie: got %d want %d", got, c0+1)
	}

	// PRAGMA schema_version=N path: a whole-header SetHeader must be
	// visible immediately.
	hdr := make([]byte, HeaderSize)
	p.mu.RLock()
	copy(hdr, p.header)
	p.mu.RUnlock()
	binary.BigEndian.PutUint32(hdr[40:44], 7777)
	p.SetHeader(hdr)
	if got := p.SchemaCookie(); got != 7777 {
		t.Fatalf("after SetHeader: got %d want 7777", got)
	}

	// AmendHeader's in-place mutation must be visible immediately.
	if !p.AmendHeader(func(h []byte) {
		binary.BigEndian.PutUint32(h[40:44], 8888)
	}) {
		t.Fatal("AmendHeader declined a valid header")
	}
	if got := p.SchemaCookie(); got != 8888 {
		t.Fatalf("after AmendHeader: got %d want 8888", got)
	}

	// Snapshot / Restore (a rolled-back DDL transaction) reverts the
	// cookie: the cache must follow.
	snap := p.Snapshot()
	p.BumpSchemaCookie()
	if got := p.SchemaCookie(); got != 8889 {
		t.Fatalf("after post-snapshot bump: got %d want 8889", got)
	}
	p.Restore(snap)
	if got := p.SchemaCookie(); got != 8888 {
		t.Fatalf("after Restore: got %d want 8888", got)
	}

	// A statement rollback reinstates the begin header: the scope's header
	// copy is restored over p.header (restoreFileImageLocked's drop + copy
	// for a memory pager), so a cookie bumped INSIDE the scope must revert
	// and the cache must follow.
	stmt := p.BeginStatement() // begin header: 8888
	p.BumpSchemaCookie()       // 8889, inside the statement scope
	if got := p.SchemaCookie(); got != 8889 {
		t.Fatalf("inside scope: got %d want 8889", got)
	}
	p.RollbackStatement(stmt)
	if got := p.SchemaCookie(); got != 8888 {
		t.Fatalf("after statement rollback: got %d want 8888", got)
	}
}

// TestSchemaCookieCacheFilePager drives the file-backed paths that the
// in-memory test cannot reach: the page-1 (re)materialization on read
// (which must not serve a pre-materialization cached value) and the
// external-change reload.
func TestSchemaCookieCacheFilePager(t *testing.T) {
	path := t.TempDir() + "/cookie.db"
	p, err := Open(path, 1024)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	// No header image yet (fresh file): cookie reads as 0 and must stay 0
	// across repeated reads.
	if got := p.SchemaCookie(); got != 0 {
		t.Fatalf("fresh file cookie: got %d want 0", got)
	}
	if got := p.SchemaCookie(); got != 0 {
		t.Fatalf("repeat fresh read: got %d want 0", got)
	}

	// Materialize the header through the page-1 read path; the cache must
	// not hold the pre-materialization answer once a header exists.
	p.mu.Lock()
	p.header = storage.DefaultHeader(p.pageSize).Encode()
	binary.BigEndian.PutUint32(p.header[40:44], 42)
	p.invalidateCookieCacheLocked()
	p.mu.Unlock()
	if got := p.SchemaCookie(); got != 42 {
		t.Fatalf("after header materialization: got %d want 42", got)
	}
}
