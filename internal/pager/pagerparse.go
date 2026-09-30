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

	"github.com/pijalu/frigolite/internal/storage"
)

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

// ParsedBTree returns the page's parsed b-tree header for pageSize and
// contentOffset, memoizing the storage.ParsePage result on the page.
//
// The returned *BTreePage is SHARED between callers and owned by the memo:
// callers must treat it as read-only (the btree layer's cursor paths only
// read the header fields; write paths that mutate a parsed header keep their
// own storage.ParsePage result). The fingerprint check makes every access
// validate the current bytes, so a modified page re-parses — and re-validates
// — exactly as a direct storage.ParsePage would. Corruption therefore fails
// at the same points with and without the memo: an unparsable page returns
// the same error on every access and is never memoized.
func (pg *Page) ParsedBTree(pageSize, contentOffset int) (*storage.BTreePage, error) {
	if m := pg.parseMemo.Load(); m != nil && m.pageSize == pageSize && m.coff == contentOffset {
		end := m.coff + len(m.hdrSnap)
		if end <= len(pg.Data) && bytes.Equal(m.hdrSnap, pg.Data[m.coff:end]) {
			return &m.parsed, nil
		}
	}
	// Miss (or first touch): parse fresh — full validation included — then
	// snapshot the parse's source bytes. The memo struct is allocated before
	// the parse so the parsed header lands in it directly (no second copy).
	mp := &pageParseMemo{pageSize: pageSize, coff: contentOffset}
	if _, err := storage.ParsePageInto(pg.Data, pageSize, contentOffset, &mp.parsed); err != nil {
		return nil, err
	}
	span := 8 // leaf header: type, first freeblock, cell count, content start, frag
	if mp.parsed.PageType == storage.PageTypeInteriorIndex || mp.parsed.PageType == storage.PageTypeInteriorTable {
		span = 12 // interior pages add the 4-byte right-most pointer
	}
	end := contentOffset + span + 2*int(mp.parsed.CellCount)
	if end > len(pg.Data) {
		// Synthetic/partial buffers (unit-test headers shorter than a real
		// page): fingerprint the readable span. The parse used only the
		// header fields, so bytes beyond it cannot change the parse result.
		end = len(pg.Data)
	}
	mp.hdrSnap = make([]byte, end-contentOffset)
	copy(mp.hdrSnap, pg.Data[contentOffset:end])
	pg.parseMemo.Store(mp)
	return &mp.parsed, nil
}
