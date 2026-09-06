// Package quota: stdio-like quota file API (test_quota.c
// sqlite3_quota_fopen / fread / fwrite / fseek / ftell / fflush /
// ftruncate / fclose / file_size / file_truesize / file_available /
// ferror / rewind).
//
// A quota stream buffers writes in memory (C uses stdio buffering): the
// logical size (FileSize) counts buffered bytes immediately, while the
// true on-disk size (FileTrueSize) only advances at FFlush/FClose. The
// group cap is enforced by FWrite exactly like quotaWrite: a write whose
// end would push the group past the limit is first offered to the
// callback, then truncated to whole size-elements that fit.
package quota

import (
	"os"
	"path/filepath"
)

// Seek origins (match the C SEEK_* values used by the tests).
const (
	SeekSet = 0
	SeekCur = 1
	SeekEnd = 2
)

// File is an open quota stream.
type File struct {
	entry *fileEntry
	group *group
}

// fopenFlags maps the TCL/C mode string ("r", "r+", "w", "w+", "a", "a+",
// optionally suffixed with "b"/"t") to create/truncate/append flags.
func fopenFlags(mode string) (create, truncate, appendMode, readonly bool, ok bool) {
	base := ""
	for _, c := range mode {
		switch c {
		case 'r', 'w', 'a', '+':
			base += string(c)
		case 'b', 't':
			// ignored
		default:
			return false, false, false, false, false
		}
	}
	switch base {
	case "r":
		return false, false, false, true, true
	case "r+":
		return false, false, false, false, true
	case "w":
		return true, true, false, false, true
	case "w+":
		return true, true, false, false, true
	case "a":
		return true, false, true, false, true
	case "a+":
		return true, false, true, false, true
	}
	return false, false, false, false, false
}

// FOpen mirrors sqlite3_quota_fopen: open (or create) name under the
// matching group. A file with no matching group is still openable — it is
// simply not size-tracked (quota2-2.x: quota2c has no rule). Returns nil
// when a read-only mode names a missing file.
func FOpen(name, mode string) *File {
	create, truncate, appendMode, readonly, ok := fopenFlags(mode)
	if !ok {
		return nil
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil
	}
	flags := os.O_RDWR
	if readonly {
		flags = os.O_RDONLY
	}
	if create {
		flags |= os.O_CREATE
	}
	if truncate {
		flags |= os.O_TRUNC
	}
	if appendMode {
		flags |= os.O_APPEND
	}
	f, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		return nil
	}
	size := int64(0)
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}

	mu.Lock()
	defer mu.Unlock()
	g := findGroupLocked(abs)
	if g == nil {
		// Not quota-tracked: a plain unbuffered stream.
		return &File{entry: &fileEntry{name: abs, size: size, rawSize: size, nref: 1, f: f}}
	}
	e, existed := g.files[abs]
	if !existed {
		e = &fileEntry{name: abs, size: size, rawSize: size, f: f}
		g.files[abs] = e
	}
	e.nref++
	e.f = f
	e.rawSize = size
	if truncate {
		e.size = 0
		e.rawSize = 0
		e.buf = nil
	}
	// "a"/"a+" start positioned at EOF for writes.
	if appendMode {
		e.pos = e.size
	}
	return &File{entry: e, group: g}
}

// FWrite mirrors sqlite3_quota_fwrite: write size*nmemb bytes at the
// current position from buf, returning the number of BYTES written.
// Over-limit writes are first offered to the callback, then truncated to
// the whole size-elements that fit below the limit (quota_fwrite's
// nmemb truncation).
func FWrite(f *File, size, nmemb int, buf []byte) int {
	if f == nil || f.entry == nil || size <= 0 || nmemb <= 0 {
		return 0
	}
	if len(buf) > size*nmemb {
		buf = buf[:size*nmemb]
	}
	mu.Lock()
	defer mu.Unlock()
	e := f.entry
	iOfst := e.pos
	iEnd := iOfst + int64(len(buf))
	if e.size < iEnd && f.group != nil {
		g := f.group
		szNew := groupSizeLocked(g) - e.size + iEnd
		if szNew > g.limit && g.limit > 0 {
			if g.callback != nil {
				g.callback(e.name, &g.limit, szNew)
			}
			if szNew > g.limit && g.limit > 0 {
				// Cap the write to the whole elements that fit
				// (quota_fwrite: nmemb = (iEnd - iOfst)/size).
				allowed := g.limit - groupSizeLocked(g) + e.size
				if allowed < iOfst {
					allowed = iOfst
				}
				n := int((allowed - iOfst) / int64(size))
				if n > nmemb {
					n = nmemb
				}
				iEnd = iOfst + int64(n*size)
			}
		}
		e.size = iEnd
		if int64(len(buf)) > iEnd-iOfst {
			buf = buf[:iEnd-iOfst]
		}
	} else if e.size < iEnd {
		e.size = iEnd
	}
	// Buffer the write at the position (sparse gaps fill with zeros).
	for int64(len(e.buf)) < iOfst {
		e.buf = append(e.buf, 0)
	}
	if int64(len(e.buf)) < iEnd {
		e.buf = append(e.buf, make([]byte, int(iEnd)-len(e.buf))...)
	}
	copy(e.buf[iOfst:], buf)
	e.pos = iEnd
	return len(buf)
}

// FRead mirrors sqlite3_quota_fread: read size*nmemb bytes from the
// current position and return the FULL size-elements read (C fread
// semantics: quota2-1.3 requests 7 items of 1001 from a 4000-byte stream
// and receives 3 items = 3003 bytes).
func FRead(f *File, size, nmemb int) []byte {
	if f == nil || f.entry == nil || size <= 0 || nmemb <= 0 {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	e := f.entry
	avail := int64(len(e.buf)) - e.pos
	if avail <= 0 {
		return make([]byte, 0)
	}
	items := int(avail) / size
	if items > nmemb {
		items = nmemb
	}
	out := make([]byte, items*size)
	copy(out, e.buf[e.pos:e.pos+int64(items*size)])
	e.pos += int64(items * size)
	return out
}

// FSeek mirrors sqlite3_quota_fseek (fseek semantics; negative offsets
// allowed for SEEK_END/SEEK_CUR). Returns 0 on success, non-zero on error.
func FSeek(f *File, offset int64, whence int) int {
	if f == nil || f.entry == nil {
		return -1
	}
	mu.Lock()
	defer mu.Unlock()
	e := f.entry
	var target int64
	switch whence {
	case SeekSet:
		target = offset
	case SeekCur:
		target = e.pos + offset
	case SeekEnd:
		target = e.size + offset
	default:
		return -1
	}
	if target < 0 {
		return -1
	}
	e.pos = target
	return 0
}

// FTell mirrors sqlite3_quota_ftell.
func FTell(f *File) int64 {
	if f == nil || f.entry == nil {
		return 0
	}
	mu.Lock()
	defer mu.Unlock()
	return f.entry.pos
}

// FRewind mirrors sqlite3_quota_rewind.
func FRewind(f *File) {
	if f == nil || f.entry == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	f.entry.pos = 0
}

// FFlush mirrors sqlite3_quota_fflush: push the buffered bytes to disk.
// hardSync requests an fsync (the test's second argument).
func FFlush(f *File, hardSync bool) {
	if f == nil || f.entry == nil || f.entry.f == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	e := f.entry
	if len(e.buf) > 0 {
		if _, err := e.f.WriteAt(e.buf, 0); err == nil {
			e.rawSize = int64(len(e.buf))
		}
	}
	if hardSync {
		_ = e.f.Sync()
	}
}

// FTruncate mirrors sqlite3_quota_ftruncate: resize the logical stream and
// push the new content to disk.
func FTruncate(f *File, size int64) {
	if f == nil || f.entry == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	e := f.entry
	if size < int64(len(e.buf)) {
		e.buf = e.buf[:size]
	} else {
		for int64(len(e.buf)) < size {
			e.buf = append(e.buf, 0)
		}
	}
	if e.pos > size {
		e.pos = size
	}
	e.size = size
	if e.f != nil {
		if _, err := e.f.WriteAt(e.buf, 0); err == nil {
			e.rawSize = size
			_ = e.f.Truncate(size)
		}
	}
}

// FileSize mirrors sqlite3_quota_file_size: the logical size including
// buffered writes.
func FileSize(f *File) int64 {
	if f == nil || f.entry == nil {
		return -1
	}
	mu.Lock()
	defer mu.Unlock()
	return f.entry.size
}

// FileTrueSize mirrors sqlite3_quota_file_truesize: the on-disk size.
func FileTrueSize(f *File) int64 {
	if f == nil || f.entry == nil {
		return -1
	}
	mu.Lock()
	defer mu.Unlock()
	return f.entry.rawSize
}

// FileAvailable mirrors sqlite3_quota_file_available: the readable bytes
// between the current position and the end.
func FileAvailable(f *File) int64 {
	if f == nil || f.entry == nil {
		return -1
	}
	mu.Lock()
	defer mu.Unlock()
	return int64(len(f.entry.buf)) - f.entry.pos
}

// FError mirrors sqlite3_quota_ferror: 0 when no error occurred.
func FError(f *File) int {
	if f == nil || f.entry == nil {
		return 0
	}
	return 0
}

// FClose mirrors sqlite3_quota_fclose: flush and release the stream.
func FClose(f *File) {
	if f == nil || f.entry == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	e := f.entry
	if len(e.buf) > 0 && e.f != nil {
		if _, err := e.f.WriteAt(e.buf, 0); err == nil {
			e.rawSize = int64(len(e.buf))
		}
	}
	if e.f != nil {
		_ = e.f.Close()
		e.f = nil
	}
	e.nref--
	if e.deleteOnClose && e.nref == 0 {
		_ = os.Remove(e.name)
	}
	if f.group != nil {
		derefGroupLocked(f.group)
	}
}

// Remove mirrors sqlite3_quota_remove: drop the named file from every
// group's size tracking and delete it from disk.
func Remove(filename string) {
	mu.Lock()
	defer mu.Unlock()
	abs, err := filepath.Abs(filename)
	if err != nil {
		return
	}
	for _, g := range groups {
		if f, ok := g.files[abs]; ok {
			if f.f != nil {
				_ = f.f.Close()
			}
			if f.nref == 0 {
				delete(g.files, abs)
				_ = os.Remove(abs)
				derefGroupLocked(g)
			}
		}
	}
}

// TrackFile adds filename to its matching quota group (tracking its
// on-disk size) and returns the size — test_quota.c sqlite3_quota_file
// ("sqlite3_quota_file FILENAME": the file joins the group). Returns 0
// when no group matches or the file cannot be stat'ed.
func TrackFile(filename string) int64 {
	mu.Lock()
	defer mu.Unlock()
	abs, err := filepath.Abs(filename)
	if err != nil {
		return 0
	}
	g := findGroupLocked(abs)
	if g == nil {
		return 0
	}
	size := int64(0)
	if st, serr := os.Stat(abs); serr == nil {
		size = st.Size()
	}
	if f, ok := g.files[abs]; ok {
		f.size = size
	} else {
		g.files[abs] = &fileEntry{name: abs, size: size, rawSize: size}
	}
	return size
}
