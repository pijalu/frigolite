// Package pager manages reading and writing of database pages.
//
// This file implements sqlite3_serialize / sqlite3_deserialize (memdb.c +
// tclsqlite.c DB_DESERIALIZE): contiguous image dump and restore.
package pager

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/storage"
)

// Serialize returns a contiguous copy of the database image: numPages ×
// pageSize bytes, page 1 carrying the live 100-byte header (memdb.c
// sqlite3_serialize: sz = page_count × pageSize; each page copied from the
// pager cache, missing pages zero-filled). The caller owns the slice.
func (p *Pager) Serialize() []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := p.numPages
	if n == 0 {
		n = 1
	}
	out := make([]byte, int64(n)*int64(p.pageSize))
	for pgno := uint32(1); pgno <= n; pgno++ {
		dst := out[int64(pgno-1)*int64(p.pageSize) : int64(pgno)*int64(p.pageSize)]
		if pg, ok := p.pages[pgno]; ok && pg != nil && len(pg.Data) > 0 {
			copy(dst, pg.Data)
			if pgno == 1 && len(p.header) >= HeaderSize && len(dst) >= HeaderSize {
				copy(dst[:HeaderSize], p.header)
			}
		} else if pgno == 1 && len(p.header) >= HeaderSize && len(dst) >= HeaderSize {
			copy(dst[:HeaderSize], p.header)
		}
	}
	return out
}

// Deserialize replaces the pager's in-memory image with img (memdb.c
// sqlite3_deserialize + tclsqlite.c DB_DESERIALIZE): img must hold a whole
// number of pageSize pages; the header is parsed like Open (bad magic →
// deferred headerCorrupt, surfaced as "file is not a database" on the
// first schema read); maxSize caps growth (SQLITE_FCNTL_SIZE_LIMIT);
// readOnly marks the image read-only (writes fail SQLITE_READONLY).
// An empty image resets to an empty database (memdb1.test 400).
func (p *Pager) Deserialize(img []byte, maxSize int64, readOnly bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(img) == 0 {
		p.pages = make(map[uint32]*Page)
		p.dirty = make(map[uint32]bool)
		p.numPages = 0
		p.headerCorrupt = false
		p.readOnly = readOnly
		if maxSize > 0 {
			p.maxPageCount = uint32(maxSize / int64(p.pageSize))
		}
		return nil
	}
	// A WAL-mode image truncated mid-file (memdb1.test 800s: first 20KiB
	// of a 24KiB file) cuts a page short: pad the tail with zeros so the
	// page map stays whole (memdb.c memcpy loop zero-fills missing pages;
	// SQLite then reports the corruption at query time, not deserialize).
	if r := int64(len(img)) % int64(p.pageSize); r != 0 {
		pad := make([]byte, int64(p.pageSize)-r)
		img = append(append([]byte(nil), img...), pad...)
	}
	if maxSize > 0 && int64(len(img)) > maxSize {
		return fmt.Errorf("database or disk is full")
	}
	n := uint32(len(img) / int(p.pageSize))
	hdr := make([]byte, HeaderSize)
	copy(hdr, img[:HeaderSize])
	if _, perr := storage.ParseHeader(hdr); perr != nil {
		p.pages = make(map[uint32]*Page)
		p.dirty = make(map[uint32]bool)
		p.numPages = n
		p.header = append([]byte(nil), hdr...)
		p.headerCorrupt = true
		p.readOnly = readOnly
		// A corrupt image must fail the NEXT schema read, not the
		// deserializing call itself (memdb1.test 500: deserialize a
		// non-database returns ok, integrity_check reports NOTADB).
		return nil
	}
	p.pages = make(map[uint32]*Page, n)
	for pgno := uint32(1); pgno <= n; pgno++ {
		src := img[int64(pgno-1)*int64(p.pageSize) : int64(pgno)*int64(p.pageSize)]
		pg := &Page{PageNum: pgno, Data: append([]byte(nil), src...)}
		p.pages[pgno] = pg
	}
	p.dirty = make(map[uint32]bool)
	p.numPages = n
	p.header = append([]byte(nil), hdr...)
	if len(hdr) > 20 {
		p.reserved = uint32(hdr[20])
	}
	p.headerCorrupt = false
	p.readOnly = readOnly
	if maxSize > 0 {
		p.maxPageCount = uint32(maxSize / int64(p.pageSize))
	}
	if p.file != nil {
		p.fileSize = int64(len(img))
	}
	return nil
}
