// Package pager — memoized b-tree page-header parsing.
//
// Every cursor operation touches pages through ReadPage + storage.ParsePage,
// and a fresh BTree (built per statement over the shared pager) re-parses the
// same root/leaf images millions of times across a workload. This file caches
// the parsed header alongside the cached page so repeated cursor touches of
// an unmodified page skip the re-parse.
//
// Staleness is handled by VALIDATION, not by chasing invalidation points:
// the memo stores a snapshot of exactly the byte range a parse reads (page
// header, right-most pointer, cell pointer array), and every access re-checks
// those bytes against the page's current Data. A page written, dirtied,
// defragmented, split, restored by rollback, or re-read after an external
// change has different bytes in that range (or arrives as a fresh Page object
// after the cache drop), so the memo misses and a full parse — including
// validatePageHeader's corruption checks — runs again. A page whose payload
// bytes change while its header and pointer array stay identical parses to
// the identical BTreePage, so serving the memo is correct there too.
package pager

import (
	"bytes"
	"encoding/binary"
	"sync/atomic"

	"github.com/pijalu/frigolite/internal/storage"
)

// Parse-memo hit/miss counters. Production code never reads them; the
// memo-refresh pins use them to prove a post-mutation access is SERVED from
// the refreshed memo instead of re-parsed.
var (
	parseMemoHits   atomic.Int64
	parseMemoMisses atomic.Int64
)

// ParseMemoCountersForTest returns the memo hit/miss counters (white-box
// tests).
func ParseMemoCountersForTest() (hits, misses int64) {
	return parseMemoHits.Load(), parseMemoMisses.Load()
}

// ResetParseMemoCountersForTest zeroes the memo counters (white-box tests).
func ResetParseMemoCountersForTest() {
	parseMemoHits.Store(0)
	parseMemoMisses.Store(0)
}

// pageParseMemo is one generation of a page's parsed b-tree header. hdrSnap
// mirrors Data[coff : coff+span+2*CellCount] at parse time — the full byte
// span the parse (and validatePageHeader) reads; parsed is derived only from
// those bytes, so a byte-identical span implies a byte-identical parse.
type pageParseMemo struct {
	hdrSnap  []byte
	parsed   storage.BTreePage
	pageSize int
	coff     int
}

// snapshotContentStart re-derives CellContent from a snapshot with
// storage.ParsePage's 64KiB wrap normalization (a 0 field on a 65536-byte
// page parses as the unwrapped full-page content start).
func snapshotContentStart(b []byte, pageSize int) int {
	cc := int(binary.BigEndian.Uint16(b[5:7]))
	if cc == 0 && pageSize == 65536 {
		return 65536
	}
	return cc
}

// parsedMatchesSnapshot re-derives the parsed header fields from hdrSnap and
// reports whether they still match. storage.ParsePage is a pure function of
// the snapshotted span, so a mismatch means the shared parsed struct was
// mutated by a consumer (the byte check cannot see Go-side writes to the
// struct) or the snapshot was captured from different bytes than the parse
// read (a torn capture across a page rewrite). Either way this memo
// generation must not be served: the canary turns a would-be poisoned memo
// into a one-generation miss (the fields re-parse from the settled bytes),
// instead of a permanently stale CellCount feeding cell-pointer arithmetic —
// the slice-bounds shape the memo tranche was blamed for.
func (m *pageParseMemo) parsedMatchesSnapshot() bool {
	b := m.hdrSnap
	switch {
	case len(b) < 8:
		return false
	case m.parsed.PageType != b[0],
		m.parsed.FirstFree != binary.BigEndian.Uint16(b[1:3]),
		m.parsed.CellCount != binary.BigEndian.Uint16(b[3:5]),
		m.parsed.FragFree != b[7],
		m.parsed.CellContent != snapshotContentStart(b, m.pageSize):
		return false
	case m.parsed.PageType == storage.PageTypeInteriorIndex,
		m.parsed.PageType == storage.PageTypeInteriorTable:
		return len(b) >= 12 && m.parsed.RightmostPtr == binary.BigEndian.Uint32(b[8:12])
	default:
		return true
	}
}

// parseMemoSpan returns the byte span a parse of pageType/cellCount reads:
// the page header (8 bytes leaf, 12 interior) plus the cell pointer array.
func parseMemoSpan(pageType byte, cellCount int) int {
	span := 8 // leaf header: type, first freeblock, cell count, content start, frag
	if pageType == storage.PageTypeInteriorIndex || pageType == storage.PageTypeInteriorTable {
		span = 12 // interior pages add the 4-byte right-most pointer
	}
	return span + 2*cellCount
}

// ParsedBTree returns the page's parsed b-tree header for pageSize and
// contentOffset, memoizing the storage.ParsePage result on the page.
//
// The returned *BTreePage is SHARED between callers and owned by the memo:
// callers must treat it as read-only (the btree layer's cursor paths only
// read the header fields; write paths that mutate a parsed header keep their
// own storage.ParsePage result). Two validations make every access safe:
// the fingerprint check re-validates the current bytes (a modified page
// re-parses — and re-validates — exactly as a direct storage.ParsePage
// would), and the parsedMatchesSnapshot canary re-derives the struct fields
// from the snapshot, so a consumer that mutates the shared struct anyway is
// detected on the NEXT access and the memo self-heals by re-parsing instead
// of serving the mutant. Corruption therefore fails at the same points with
// and without the memo: an unparsable page returns the same error on every
// access and is never memoized.
func (pg *Page) ParsedBTree(pageSize, contentOffset int) (*storage.BTreePage, error) {
	if m := pg.parseMemo.Load(); m != nil && m.pageSize == pageSize && m.coff == contentOffset {
		end := m.coff + len(m.hdrSnap)
		if end <= len(pg.Data) && bytes.Equal(m.hdrSnap, pg.Data[m.coff:end]) && m.parsedMatchesSnapshot() {
			parseMemoHits.Add(1)
			return &m.parsed, nil
		}
	}
	parseMemoMisses.Add(1)
	// Miss (or first touch): parse fresh — full validation included — then
	// snapshot the parse's source bytes. The memo struct is allocated before
	// the parse so the parsed header lands in it directly (no second copy).
	mp := &pageParseMemo{pageSize: pageSize, coff: contentOffset}
	if _, err := storage.ParsePageInto(pg.Data, pageSize, contentOffset, &mp.parsed); err != nil {
		return nil, err
	}
	span := parseMemoSpan(mp.parsed.PageType, int(mp.parsed.CellCount))
	end := contentOffset + span
	if end > len(pg.Data) {
		// Synthetic/partial buffers (unit-test headers shorter than a real
		// page): fingerprint the readable span. The parse used only the
		// header fields, so bytes beyond it cannot change the parse result.
		end = len(pg.Data)
	}
	mp.hdrSnap = make([]byte, end-contentOffset)
	copy(mp.hdrSnap, pg.Data[contentOffset:end])
	if !mp.parsedMatchesSnapshot() {
		// Torn capture: the page bytes changed between the parse and the
		// snapshot, so the parsed header and hdrSnap disagree (impossible
		// under the engine's single-threaded-per-pager contract; cheap
		// insurance if that contract ever widens). Serve the current parse
		// UNMEMOIZED — the next access re-runs this path against the
		// settled bytes instead of pinning the mixed generation forever.
		return storage.ParsePage(pg.Data, pageSize, contentOffset)
	}
	pg.parseMemo.Store(mp)
	return &mp.parsed, nil
}

// parsedMatchesData reports whether parsed is exactly the header the bytes at
// data[contentOffset:] describe — the live-image canary for the refresh path
// (parsedMatchesSnapshot's counterpart, reading the page instead of the
// snapshot). storage.ParsePage is a pure function of these bytes, so a match
// means the caller's struct can serve as this byte generation's parse.
func parsedMatchesData(parsed *storage.BTreePage, data []byte, pageSize, contentOffset int) bool {
	if len(data) < contentOffset+8 {
		return false
	}
	b := data[contentOffset:]
	switch {
	case parsed.PageType != b[0],
		parsed.FirstFree != binary.BigEndian.Uint16(b[1:3]),
		parsed.CellCount != binary.BigEndian.Uint16(b[3:5]),
		parsed.FragFree != b[7],
		parsed.CellContent != snapshotContentStart(b, pageSize):
		return false
	case parsed.PageType == storage.PageTypeInteriorIndex,
		parsed.PageType == storage.PageTypeInteriorTable:
		return len(b) >= 12 && parsed.RightmostPtr == binary.BigEndian.Uint32(b[8:12])
	default:
		return true
	}
}

// RefreshParsedBTree re-arms the page's parse memo from a mutating b-tree
// write path's own parsed header (btree.c keeps a live MemPage per cached
// page and updates it incrementally in insertCell/dropCell/freeSpace —
// sqlite NEVER re-parses a cached page after mutation; this is the Go
// equivalent at memo granularity). After a page mutation the memo's
// fingerprint no longer matches, so the next ParsedBTree access would run a
// full parse+validate+snapshot; a write path whose parsed struct stayed in
// step with its byte writes hands that struct here instead, and the next
// access is a memo HIT.
//
// The refresh is defensive by construction: it proceeds only when the
// caller's struct matches the page's CURRENT bytes field-for-field (the
// live-image canary) and passes the same validation a fresh parse would run
// — a stale or unsynchronized struct (or a page this path did not write) is
// declined, leaving the memo to the next access's normal validating parse.
// Pages loaded FROM DISK never come through here, so their memos always
// carry the validating parse. The snapshot buffer and memo struct are reused
// in place (the engine holds one goroutine per pager; the atomic pointer
// keeps the memo metadata race-free regardless), so a steady-state
// mutate-then-seek workload allocates nothing.
func (pg *Page) RefreshParsedBTree(pageSize, contentOffset int, parsed *storage.BTreePage) {
	if parsed == nil {
		return
	}
	m := pg.parseMemo.Load()
	if m == nil || m.pageSize != pageSize || m.coff != contentOffset {
		// No memo generation for this (pageSize, contentOffset) yet: the
		// next access would parse and memoize anyway — nothing to refresh.
		return
	}
	if !parsedMatchesData(parsed, pg.Data, pageSize, contentOffset) {
		return
	}
	if err := storage.ValidatePageHeader(parsed, pg.Data, pageSize, contentOffset); err != nil {
		return
	}
	end := contentOffset + parseMemoSpan(parsed.PageType, int(parsed.CellCount))
	if end > len(pg.Data) {
		// Synthetic/partial buffers: fingerprint the readable span (same
		// rule as the miss path).
		end = len(pg.Data)
	}
	if n := end - contentOffset; cap(m.hdrSnap) >= n {
		m.hdrSnap = m.hdrSnap[:n]
	} else {
		m.hdrSnap = make([]byte, n)
	}
	copy(m.hdrSnap, pg.Data[contentOffset:end])
	m.parsed = *parsed
}
