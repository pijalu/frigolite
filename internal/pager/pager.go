// Package pager manages reading and writing of database pages.
//
// File layout (SQLite compatible):
//   Page 1: bytes 0-99 = database header, bytes 100-(pageSize-1) = b-tree content (pageSize total)
//   Pages N>1: bytes 0-(pageSize-1) = b-tree content (pageSize total)
//
// The b-tree layer always sees Data of exactly pageSize bytes.
// For page 1, the first HeaderSize bytes are the database header (unused by b-tree).
// The pager handles the header transparently.
package pager

import (
	"fmt"
	"os"
	"sync"

	"github.com/pijalu/frigolite/internal/storage"
)

const (
	DefaultPageSize = 4096
	DefaultCacheSize = 1000
	HeaderSize      = 100
)

type Pager struct {
	mu       sync.RWMutex
	pageSize uint32
	file     *os.File
	pages    map[uint32]*Page
	dirty    map[uint32]bool
	readOnly bool
	numPages uint32
	header   []byte
}

type Page struct {
	Data    []byte
	PageNum uint32
}

// PagerState is a deep snapshot of a pager's in-memory state, used for
// statement-level rollback (e.g. a failed REPLACE that fired triggers).
type PagerState struct {
	pages    map[uint32]*Page
	dirty    map[uint32]bool
	numPages uint32
	header   []byte
}

// Snapshot captures the pager's current in-memory pages and header so they
// can be restored later with Restore.
func (p *Pager) Snapshot() *PagerState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := &PagerState{
		pages:    make(map[uint32]*Page, len(p.pages)),
		dirty:    make(map[uint32]bool, len(p.dirty)),
		numPages: p.numPages,
	}
	if p.header != nil {
		s.header = append([]byte(nil), p.header...)
	}
	for n, pg := range p.pages {
		cp := &Page{PageNum: pg.PageNum, Data: append([]byte(nil), pg.Data...)}
		s.pages[n] = cp
		if p.dirty[n] {
			s.dirty[n] = true
		}
	}
	return s
}

// Restore replaces the pager's in-memory state with a snapshot taken earlier.
func (p *Pager) Restore(s *PagerState) {
	if s == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = make(map[uint32]*Page, len(s.pages))
	for n, pg := range s.pages {
		cp := &Page{PageNum: pg.PageNum, Data: append([]byte(nil), pg.Data...)}
		p.pages[n] = cp
	}
	p.dirty = make(map[uint32]bool, len(s.dirty))
	for n := range s.dirty {
		p.dirty[n] = true
	}
	p.numPages = s.numPages
	if s.header != nil {
		p.header = append([]byte(nil), s.header...)
	}
}

// Open opens a database file.
func Open(path string, pageSize uint32) (*Pager, error) {
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("pager: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("pager: stat %s: %w", path, err)
	}

	pr := &Pager{
		pageSize: pageSize,
		file:     f,
		pages:    make(map[uint32]*Page),
		dirty:    make(map[uint32]bool),
	}

	if info.Size() > 0 {
		// Read full page 1 into a temporary buffer
		fullPage := make([]byte, pageSize)
		_, err := f.ReadAt(fullPage, 0)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("pager: read page 1: %w", err)
		}
		pr.header = make([]byte, HeaderSize)
		copy(pr.header, fullPage[:HeaderSize])
		pr.numPages = uint32(info.Size() / int64(pageSize))
		if pr.numPages == 0 && info.Size() > 0 {
			pr.numPages = 1
		}
	}

	return pr, nil
}

// OpenInMemory creates an in-memory pager.
func OpenInMemory(pageSize uint32) *Pager {
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	dh := storage.DefaultHeader(pageSize)
	return &Pager{
		pageSize: pageSize,
		file:     nil,
		pages:    make(map[uint32]*Page),
		dirty:    make(map[uint32]bool),
		numPages: 0,
		header:   dh.Encode(),
	}
}

func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.flushAll(); err != nil {
		return err
	}
	if p.file != nil {
		return p.file.Close()
	}
	return nil
}

func (p *Pager) PageSize() uint32 { return p.pageSize }
func (p *Pager) NumPages() uint32 { p.mu.RLock(); defer p.mu.RUnlock(); return p.numPages }
func (p *Pager) Header() []byte   { p.mu.RLock(); defer p.mu.RUnlock(); return p.header }
func (p *Pager) SetHeader(h []byte) { p.mu.Lock(); defer p.mu.Unlock(); p.header = append([]byte(nil), h...) }

// AllocatePage creates a new page. Data is always pageSize bytes.
// For page 1, the first HeaderSize bytes are reserved for the database header.
func (p *Pager) AllocatePage() *Page {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.numPages++
	pg := &Page{
		Data:    make([]byte, p.pageSize),
		PageNum: p.numPages,
	}
	// For page 1, pre-fill with header
	if p.numPages == 1 && p.header != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	p.pages[pg.PageNum] = pg
	p.dirty[pg.PageNum] = true
	return pg
}

// ReadPage reads a page. Data is always pageSize bytes.
func (p *Pager) ReadPage(pageNum uint32) (*Page, error) {
	if pageNum == 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	p.mu.RLock()
	if pg, ok := p.pages[pageNum]; ok {
		p.mu.RUnlock()
		return pg, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if pg, ok := p.pages[pageNum]; ok {
		return pg, nil
	}
	if pageNum > p.numPages {
		return nil, fmt.Errorf("database disk image is malformed")
	}

	pg := &Page{
		Data:    make([]byte, p.pageSize),
		PageNum: pageNum,
	}
	if p.file != nil {
		off := int64(pageNum-1) * int64(p.pageSize)
		_, err := p.file.ReadAt(pg.Data, off)
		if err != nil {
			return nil, fmt.Errorf("pager: read page %d: %w", pageNum, err)
		}
		// For page 1, extract the header from the full page data
		if pageNum == 1 && p.header == nil {
			p.header = make([]byte, HeaderSize)
			copy(p.header, pg.Data[:HeaderSize])
		}
	}
	p.pages[pageNum] = pg
	return pg, nil
}

// FileInfo returns the underlying file's info (nil, false for in-memory
// pagers). Used to detect external modification of attached database files.
func (p *Pager) FileInfo() (os.FileInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.file == nil {
		return nil, false
	}
	info, err := p.file.Stat()
	if err != nil {
		return nil, false
	}
	return info, true
}

// InvalidateCache drops the in-memory page cache and page-count so the next
// read re-reads the file. Used when an external connection may have modified
// the database file (schema reload after an ATTACHed file changes).
func (p *Pager) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = make(map[uint32]*Page)
	p.header = nil
	if p.file != nil {
		if info, err := p.file.Stat(); err == nil {
			p.numPages = uint32(info.Size() / int64(p.pageSize))
			if p.numPages == 0 && info.Size() > 0 {
				p.numPages = 1
			}
		}
	}
}

// WritePage marks a page as dirty.
func (p *Pager) WritePage(pg *Page) error {
	if p.readOnly {
		return fmt.Errorf("pager: read-only")
	}
	// For page 1, ensure the header is preserved in Data[0:HeaderSize]
	if pg.PageNum == 1 && p.header != nil {
		copy(pg.Data[:HeaderSize], p.header)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages[pg.PageNum] = pg
	p.dirty[pg.PageNum] = true
	return nil
}

func (p *Pager) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flushAll()
}

func (p *Pager) flushAll() error {
	if p.file == nil {
		return nil
	}
	for pageNum := range p.dirty {
		pg, ok := p.pages[pageNum]
		if !ok {
			continue
		}
		off := int64(pageNum-1) * int64(p.pageSize)
		fileEnd := int64(pageNum) * int64(p.pageSize)
		if info, err := p.file.Stat(); err == nil && info.Size() < fileEnd {
			if err := p.file.Truncate(fileEnd); err != nil {
				return fmt.Errorf("pager: truncate: %w", err)
			}
		}
		if _, err := p.file.WriteAt(pg.Data, off); err != nil {
			return fmt.Errorf("pager: write page %d: %w", pageNum, err)
		}
	}
	p.dirty = make(map[uint32]bool)
	return nil
}

func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file != nil {
		return p.file.Sync()
	}
	return nil
}
