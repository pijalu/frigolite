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
		return nil, fmt.Errorf("pager: page 0 invalid")
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
		return nil, fmt.Errorf("pager: page %d out of range (max %d)", pageNum, p.numPages)
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
		// For page 1, copy the header into the data before writing
		if pageNum == 1 && p.header != nil {
			copy(pg.Data[:HeaderSize], p.header)
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
