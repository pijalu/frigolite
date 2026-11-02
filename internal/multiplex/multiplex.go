// Package multiplex implements SQLite's multiplexor VFS shim (a port of
// src/test_multiplex.c — ext/misc/multiplex.c is not in-tree): a database
// file that may exceed filesystem or interface size limits is split across
// several chunk files. Chunk 0 is the database path itself; chunk i>0 is the
// path with a three-digit decimal suffix ("test.db" + "001"). Reads and
// writes are split at chunk boundaries exactly like the C's
// multiplexRead/multiplexWrite; a chunk size that is not a multiple of
// MAX_PAGE_SIZE (0x10000) is rounded up, so chunk boundaries are always
// page-aligned for every legal page size.
//
// The group is attached to every main-database file pager. With the default
// chunk size (2 GiB - 64 KiB) it is a faithful pass-through: every byte of a
// database smaller than one chunk lives in chunk 0, so normal file
// databases see identical behavior with one extra Stat at open.
package multiplex

import (
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// DefaultChunkSize is SQLITE_MULTIPLEX_CHUNK_SIZE: 2 GiB less 64 KiB.
const DefaultChunkSize = 2147418112

// maxPageSize is MAX_PAGE_SIZE (0x10000): the granularity chunk sizes are
// rounded up to.
const maxPageSize = 0x10000

// pendingByte is sqlite3PendingByte (global.c): the region starting here is
// never written, so a chunk boundary may not fall in the 64 KiB window
// before it.
const pendingByte = 0x40000000

// mxChunkNumber caps the chunk number (MX_CHUNK_NUMBER): chunk names carry
// three digits.
const mxChunkNumber = 299

// Group is one multiplexor file group: the collection of chunk files that
// collectively make up a single database. It satisfies the pager's file
// interface (byte-level ReadAt/WriteAt over the chunk list).
type Group struct {
	mu      sync.Mutex
	chunks  []*os.File // chunk handles; entry i open when non-nil
	base    string     // chunk 0's path (the database path)
	szChunk uint32     // chunk size in bytes (always a 64 KiB multiple)
	enabled bool
	// truncate mirrors bTruncate: when false (the MAIN_DB default) a
	// truncate above the mark truncates higher chunks to zero instead of
	// closing and deleting them.
	truncate bool
	// readOnly groups never create chunk files and fail writes.
	readOnly bool
	closed   bool
}

// FileInfo is the fs.FileInfo a Group's Stat reports: chunk 0's metadata
// with the size replaced by the virtual (aggregated) database size.
type FileInfo struct {
	real fs.FileInfo
	size int64
}

func (fi FileInfo) Name() string       { return fi.real.Name() }
func (fi FileInfo) Size() int64        { return fi.size }
func (fi FileInfo) Mode() fs.FileMode  { return fi.real.Mode() }
func (fi FileInfo) ModTime() time.Time { return fi.real.ModTime() }
func (fi FileInfo) IsDir() bool        { return fi.real.IsDir() }
func (fi FileInfo) Sys() interface{}   { return fi.real.Sys() }

// NewGroup wraps an already-open chunk-0 handle (openDatabaseFile's product)
// over basePath into a multiplex group and runs multiplexOpen's open-time
// analysis: the pending-byte chunk-size adjustment, and the chunk-1 probe
// that rederives the chunk size from chunk 0's on-disk size (a reopened
// database recovers its configured chunk size because every full chunk file
// is a 64 KiB multiple — multiplexOpen's "chunk size is set incorrectly, so
// fix it" branch).
func NewGroup(chunk0 *os.File, basePath string, readOnly bool) (*Group, error) {
	g := &Group{
		chunks:   []*os.File{chunk0},
		base:     basePath,
		szChunk:  DefaultChunkSize,
		enabled:  true,
		truncate: false, // MAIN_DB default: (flags & MAIN_DB)==0 is false
		readOnly: readOnly,
	}
	// multiplexOpen: keep the pending byte out of the last 64 KiB of a
	// chunk (a 64 KiB region following the pending byte is never written).
	for pendingByte%uint64(g.szChunk) >= uint64(g.szChunk-maxPageSize) {
		g.szChunk += maxPageSize
	}
	// The chunk-1 probe: an existing first overflow file whose chunk-0
	// size is a 64 KiB multiple pins the chunk size; a database LARGER
	// than the chunk size with no overflow file disables the multiplexor
	// (the intended chunk size is unknowable).
	info, err := chunk0.Stat()
	if err != nil {
		return nil, err
	}
	sz64 := info.Size()
	if sz64 > 0 {
		if _, serr := os.Stat(g.chunkName(1)); serr == nil {
			if sz64 == (sz64&^0xffff) && uint64(sz64) != uint64(g.szChunk) {
				g.szChunk = uint32(sz64)
			}
		} else if int64(sz64) > int64(g.szChunk) {
			g.enabled = false
		}
	}
	return g, nil
}

// chunkName returns the path of chunk i: the base path for chunk 0, else the
// base path with the three-digit chunk number appended
// (multiplexFilename; the 8.3 variant is not built).
func (g *Group) chunkName(i int) string {
	if i == 0 {
		return g.base
	}
	return fmt.Sprintf("%s%03d", g.base, i)
}

// chunkBase is obsolete: NewGroup takes the base path directly.

// subOpen returns the handle for chunk i, opening it when not yet open
// (multiplexSubOpen). create reports whether a missing chunk i>0 is created
// (writes) or reported missing (reads and probes). Chunk 0's handle comes
// pre-opened.
func (g *Group) subOpen(i int, create bool) (*os.File, error) {
	if i >= len(g.chunks) {
		if i > mxChunkNumber {
			return nil, fmt.Errorf("multiplex: chunk overflow")
		}
		g.chunks = append(g.chunks, make([]*os.File, i+1-len(g.chunks))...)
	}
	if g.chunks[i] != nil {
		return g.chunks[i], nil
	}
	name := g.chunkName(i)
	if i == 0 {
		// Chunk 0 arrives pre-opened from NewGroup; reopen defensively.
		f, err := os.OpenFile(name, os.O_RDWR, 0644)
		if err != nil {
			return nil, err
		}
		g.chunks[i] = f
		return f, nil
	}
	if _, err := os.Stat(name); err != nil {
		if !create || g.readOnly {
			return nil, err
		}
	}
	flags := os.O_RDWR
	if create && !g.readOnly {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(name, flags, 0644)
	if err != nil {
		return nil, err
	}
	g.chunks[i] = f
	return f, nil
}

// subSize returns chunk i's on-disk size without keeping it open
// (multiplexSubSize's "does not distinguish between non-existent files and
// zero-length files").
func (g *Group) subSize(i int) int64 {
	if i < len(g.chunks) && g.chunks[i] != nil {
		if info, err := g.chunks[i].Stat(); err == nil {
			return info.Size()
		}
		return 0
	}
	info, err := os.Stat(g.chunkName(i))
	if err != nil {
		return 0
	}
	return info.Size()
}

// ReadAt reads len(p) bytes at off, splitting across chunk boundaries
// (multiplexRead). A missing chunk past the read start fails the read like
// the C's SQLITE_IOERR_READ; a short final chunk surfaces io.EOF to the
// caller (os.File.ReadAt semantics).
func (g *Group) ReadAt(p []byte, off int64) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		f, err := g.subOpen(0, false)
		if err != nil {
			return 0, err
		}
		return f.ReadAt(p, off)
	}
	total := 0
	for len(p) > 0 {
		i := int(uint64(off) / uint64(g.szChunk))
		f, err := g.subOpen(i, true)
		if err != nil {
			return total, err
		}
		amt := int64(len(p))
		extra := (off % int64(g.szChunk)) + amt - int64(g.szChunk)
		if extra < 0 {
			extra = 0
		}
		amt -= extra
		n, rerr := f.ReadAt(p[:amt], off%int64(g.szChunk))
		total += n
		if rerr != nil || n < int(amt) {
			return total, rerr
		}
		p = p[n:]
		off += int64(n)
	}
	return total, nil
}

// WriteAt writes len(p) bytes at off, splitting across chunk boundaries and
// creating chunk files as needed (multiplexWrite).
func (g *Group) WriteAt(p []byte, off int64) (int, error) {
	if g.readOnly {
		return 0, fmt.Errorf("attempt to write a readonly database")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		f, err := g.subOpen(0, true)
		if err != nil {
			return 0, err
		}
		return f.WriteAt(p, off)
	}
	total := 0
	for len(p) > 0 {
		i := int(uint64(off) / uint64(g.szChunk))
		f, err := g.subOpen(i, true)
		if err != nil {
			return total, err
		}
		amt := int64(len(p))
		extra := (off % int64(g.szChunk)) + amt - int64(g.szChunk)
		if extra < 0 {
			extra = 0
		}
		amt -= extra
		n, werr := f.WriteAt(p[:amt], off%int64(g.szChunk))
		total += n
		if werr != nil {
			return total, werr
		}
		p = p[n:]
		off += int64(n)
	}
	return total, nil
}

// Truncate cuts the virtual file to size: the base chunk truncates at
// size%szChunk and higher chunks are truncated to zero (bTruncate false) or
// closed and deleted (bTruncate true) — multiplexTruncate.
func (g *Group) Truncate(size int64) error {
	if g.readOnly {
		return fmt.Errorf("attempt to write a readonly database")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		f, err := g.subOpen(0, true)
		if err != nil {
			return err
		}
		return f.Truncate(size)
	}
	iBase := int(uint64(size) / uint64(g.szChunk))
	for i := len(g.chunks) - 1; i > iBase; i-- {
		if g.chunks[i] == nil {
			continue
		}
		if g.truncate {
			g.chunks[i].Close()
			os.Remove(g.chunkName(i))
			g.chunks[i] = nil
		} else {
			g.chunks[i].Truncate(0)
		}
	}
	if iBase >= len(g.chunks) {
		return nil
	}
	f, err := g.subOpen(iBase, true)
	if err != nil {
		return err
	}
	return f.Truncate(size % int64(g.szChunk))
}

// Stat reports chunk 0's metadata with the aggregated virtual size
// (multiplexFileSize: i*szChunk + lastChunkSize over the non-empty chunks).
func (g *Group) Stat() (fs.FileInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, err := g.subOpen(0, false)
	if err != nil {
		return nil, err
	}
	real, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !g.enabled {
		return real, nil
	}
	var size int64
	for i := 0; ; i++ {
		sz := g.subSize(i)
		if sz == 0 {
			break
		}
		size = int64(i)*int64(g.szChunk) + sz
		if i > mxChunkNumber+1 {
			break
		}
	}
	return FileInfo{real: real, size: size}, nil
}

// Sync fsyncs every open chunk (multiplexSync).
func (g *Group) Sync() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, f := range g.chunks {
		if f != nil {
			if err := f.Sync(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close closes every open chunk handle (multiplexClose).
func (g *Group) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	var first error
	for i, f := range g.chunks {
		if f != nil {
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
			g.chunks[i] = nil
		}
	}
	return first
}

// SetEnabled toggles the multiplexor (MULTIPLEX_CTRL_ENABLE): a disabled
// group passes every operation through to chunk 0.
func (g *Group) SetEnabled(v bool) {
	g.mu.Lock()
	g.enabled = v
	g.mu.Unlock()
}

// Enabled reports the enabled flag (PRAGMA multiplex_enabled).
func (g *Group) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled
}

// SetChunkSize sets the chunk size, rounded up to a 64 KiB multiple
// (MULTIPLEX_CTRL_SET_CHUNK_SIZE); a value below 1 is a misuse error.
func (g *Group) SetChunkSize(v int64) error {
	if v < 1 {
		return fmt.Errorf("misuse of multiplexor")
	}
	g.mu.Lock()
	v = (v + maxPageSize - 1) &^ (maxPageSize - 1)
	g.szChunk = uint32(v)
	g.mu.Unlock()
	return nil
}

// ChunkSize reports the chunk size (PRAGMA multiplex_chunksize).
func (g *Group) ChunkSize() uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.szChunk
}

// SetMaxChunks is MULTIPLEX_CTRL_SET_MAX_CHUNKS: accepted, unenforced (the
// C removed the limit).
func (g *Group) SetMaxChunks(int64) {}

// FileCount returns the number of chunk files that exist on disk (PRAGMA
// multiplex_filecount: "the number of disk files currently in use"; chunks
// 0..k with no gap).
func (g *Group) FileCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for i := 0; i <= mxChunkNumber; i++ {
		if g.subSize(i) == 0 {
			break
		}
		n++
	}
	if n == 0 {
		n = 1 // chunk 0 always exists (it is the open database file)
	}
	return n
}

// Chunk0Stat reports chunk 0's raw os.FileInfo (the database file's own
// identity): os.SameFile-style checks type-assert *os.fileStat, which the
// Group's aggregated FileInfo does not satisfy.
func (g *Group) Chunk0Stat() (fs.FileInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, err := g.subOpen(0, false)
	if err != nil {
		return nil, err
	}
	return f.Stat()
}

// TruncateMode reports the truncate disposition (PRAGMA
// multiplex_truncate): when true a truncate above the mark closes and
// deletes higher chunks, when false (the MAIN_DB default) it truncates
// them to zero.
func (g *Group) TruncateMode() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.truncate
}

// SetTruncate sets the truncate disposition.
func (g *Group) SetTruncateMode(v bool) {
	g.mu.Lock()
	g.truncate = v
	g.mu.Unlock()
}
