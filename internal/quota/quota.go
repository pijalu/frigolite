// Package quota implements the SQLite quota layer (src/test_quota.c).
//
// A quota group is a set of files whose combined size is capped: any write
// that would push the group past its limit invokes the group's callback
// (which may raise or zero the limit) and is refused with SQLITE_FULL
// otherwise. The layer tracks both database files (through the pager's
// growth hook) and stdio-like quota files (FOpen/FWrite/...).
//
// Semantics mirror src/test_quota.c exactly:
//
//   - quotaWrite / sqlite3_quota_fwrite: szNew = groupSize - fileSize + iEnd;
//     when szNew > limit && limit > 0 the callback may update the limit in
//     place; setting the limit to 0 disables enforcement (the write then
//     proceeds), mirroring the C retry check `szNew>iLimit && iLimit>0`.
//   - quotaSet with limit 0 marks the group for removal; the group
//     disappears only when its last open file closes (quotaGroupDeref).
//   - quotaShutdown fails with SQLITE_MISUSE while any file is open.
package quota

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Result codes (SQLite).
const (
	OK     = 0
	ERROR  = 1
	FULL   = 13
	MISUSE = 21
)

// Callback is invoked when a write would push a group past its limit:
// (filename, limit, size). size is the group's total size if the write
// completes. The callback may update the limit through the pointer;
// setting it to 0 disables enforcement for this write.
type Callback func(filename string, limit *int64, size int64)

// fileEntry tracks one file in a quota group: its logical size, its open
// reference count (database connections or stdio handles), and — for
// stdio files — the buffered write state.
type fileEntry struct {
	name          string
	size          int64 // logical size (buffered writes count)
	nref          int   // open handles
	deleteOnClose bool
	// stdio state (db files leave these zero)
	pos     int64
	rawSize int64   // true on-disk size (after flush)
	buf     []byte  // pending write buffer
	f       *os.File // backing file for stdio entries
}

// group is one quota rule: files whose names match the pattern share the
// combined size cap.
type group struct {
	pattern  string
	limit    int64
	callback Callback
	files    map[string]*fileEntry
}

var (
	mu          sync.Mutex
	groups      []*group
	initialized bool
	defaultVFS  bool
)

// Initialize mirrors sqlite3_quota_initialize: an unknown base VFS is an
// error, a second initialize without shutdown is SQLITE_MISUSE. An empty
// VFS name selects the default VFS.
func Initialize(vfs string, makeDefault bool) int {
	mu.Lock()
	defer mu.Unlock()
	switch vfs {
	case "", "unix", "unix-none", "unix-dotfile", "unix-excl", "unix-flock",
		"unix-namedsem", "unix-nolock", "unix-proxy", "win32", "win32-none":
	default:
		return ERROR
	}
	if initialized {
		return MISUSE
	}
	initialized = true
	defaultVFS = makeDefault
	groups = nil
	return OK
}

// Shutdown mirrors sqlite3_quota_shutdown: SQLITE_MISUSE while any file
// in any group is still open, otherwise the whole layer is torn down.
func Shutdown() int {
	mu.Lock()
	defer mu.Unlock()
	if !initialized {
		return MISUSE
	}
	for _, g := range groups {
		for _, f := range g.files {
			if f.nref > 0 {
				return MISUSE
			}
		}
	}
	initialized = false
	defaultVFS = false
	groups = nil
	return OK
}

// Active reports whether the quota layer is initialized. When active, a
// pager flush can fail with SQLITE_FULL, so statement snapshots must not
// be skipped (see exec.dmlCanSkipSnapshot).
func Active() bool {
	mu.Lock()
	defer mu.Unlock()
	return initialized
}

// InitializedDefault reports whether the quota layer is initialized as
// the default VFS (drives file_control_vfsname's "quota/<vfs>" report).
func InitializedDefault() bool {
	mu.Lock()
	defer mu.Unlock()
	return initialized && defaultVFS
}

// Set mirrors sqlite3_quota_set: find-or-create the group; a limit of 0
// on a new pattern is a no-op; on an existing pattern it marks the group
// for removal once its last file closes (quotaGroupDeref).
func Set(pattern string, limit int64, cb Callback) int {
	mu.Lock()
	defer mu.Unlock()
	if !initialized {
		// C requires initialize first; the TCL harness always does. Match
		// the C behavior of creating the group anyway (gQuota groups list
		// is independent of isInitialized).
		initialized = true
	}
	for _, g := range groups {
		if g.pattern == pattern {
			g.limit = limit
			g.callback = cb
			if limit == 0 {
				derefGroupLocked(g)
			}
			return OK
		}
	}
	if limit <= 0 {
		return OK
	}
	groups = append([]*group{{
		pattern:  pattern,
		limit:    limit,
		callback: cb,
		files:    map[string]*fileEntry{},
	}}, groups...)
	return OK
}

// derefGroupLocked removes a group whose limit is 0 and which has no open
// files (test_quota.c quotaGroupDeref). Caller holds mu.
func derefGroupLocked(g *group) {
	if g.limit != 0 {
		return
	}
	for _, f := range g.files {
		if f.nref > 0 {
			return
		}
	}
	for i, gg := range groups {
		if gg == g {
			groups = append(groups[:i], groups[i+1:]...)
			break
		}
	}
}

// findGroupLocked returns the group whose pattern matches name (test_quota.c
// quotaFindGroup uses quotaStrglob). Caller holds mu.
func findGroupLocked(name string) *group {
	for _, g := range groups {
		if Strglob(g.pattern, name) {
			return g
		}
	}
	return nil
}

// Strglob is the quota glob matcher — a direct transliteration of
// test_quota.c quotaStrglob: '*' matches any sequence (collapsing
// consecutive '*'/'?'), '?' one character, "[...]" character classes with
// '^' negation, ranges, and a literal ']' first member ("[]*?]" matches
// ']', '*', '?'), a '/' in the pattern matches either '/' or '\\' in the
// name, and '\\' escapes the next pattern character.
func Strglob(zGlob, z string) bool {
	var c, c2, cx byte
	var invert, seen bool
	for len(zGlob) > 0 {
		c = zGlob[0]
		zGlob = zGlob[1:]
		if c == '*' {
			// Consume consecutive '*' and '?'; '?' consumes one name char.
			c = 0
			for len(zGlob) > 0 {
				c = zGlob[0]
				zGlob = zGlob[1:]
				if c != '*' && c != '?' {
					break
				}
				if c == '?' {
					if len(z) == 0 {
						return false
					}
					z = z[1:]
				}
				c = 0
			}
			if c == 0 {
				return true
			}
			if c == '[' {
				// C: quotaStrglob(zGlob-1, z) — the recursive call
				// re-processes the '[' class at each position.
				for len(z) > 0 && !Strglob("["+zGlob, z) {
					z = z[1:]
				}
				return len(z) > 0
			}
			// cx: the alternate character a '/' pattern char matches.
			if c == '/' {
				cx = '\\'
			} else {
				cx = c
			}
			for {
				for len(z) > 0 && z[0] != c && z[0] != cx {
					z = z[1:]
				}
				if len(z) == 0 {
					return false
				}
				if Strglob(zGlob, z[1:]) {
					return true
				}
				z = z[1:]
			}
		} else if c == '?' {
			if len(z) == 0 {
				return false
			}
			z = z[1:]
		} else if c == '[' {
			priorC := byte(0)
			seen = false
			invert = false
			if len(z) == 0 {
				return false
			}
			c = z[0]
			z = z[1:]
			if len(zGlob) == 0 {
				return false
			}
			c2 = zGlob[0]
			zGlob = zGlob[1:]
			if c2 == '^' {
				invert = true
				if len(zGlob) == 0 {
					return false
				}
				c2 = zGlob[0]
				zGlob = zGlob[1:]
			}
			if c2 == ']' {
				if c == ']' {
					seen = true
				}
				if len(zGlob) == 0 {
					return false
				}
				c2 = zGlob[0]
				zGlob = zGlob[1:]
			}
			for c2 != 0 && c2 != ']' {
				if c2 == '-' && len(zGlob) > 0 && zGlob[0] != ']' && priorC > 0 {
					if len(zGlob) == 0 {
						return false
					}
					c2 = zGlob[0]
					zGlob = zGlob[1:]
					if c >= priorC && c <= c2 {
						seen = true
					}
					priorC = 0
				} else {
					if c == c2 {
						seen = true
					}
					priorC = c2
				}
				if len(zGlob) == 0 {
					c2 = 0
					break
				}
				c2 = zGlob[0]
				zGlob = zGlob[1:]
			}
			// C: (seen ^ invert)==0 → fail.
			if c2 == 0 || seen == invert {
				return false
			}
		} else if c == '/' {
			if len(z) == 0 || (z[0] != '/' && z[0] != '\\') {
				return false
			}
			z = z[1:]
		} else {
			if len(z) == 0 || z[0] != c {
				return false
			}
			z = z[1:]
		}
	}
	return len(z) == 0
}

// RegisterDBFile records a database file opened by the pager (test_quota.c
// quotaOpen: the file's size joins the group's combined size). No-op when
// the layer is not initialized or no group matches.
func RegisterDBFile(path string) {
	mu.Lock()
	defer mu.Unlock()
	if !initialized {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	g := findGroupLocked(abs)
	if g == nil {
		return
	}
	if f, ok := g.files[abs]; ok {
		f.nref++
		return
	}
	size := int64(0)
	if st, err := os.Stat(abs); err == nil {
		size = st.Size()
	}
	g.files[abs] = &fileEntry{name: abs, size: size, nref: 1, rawSize: size}
}

// UnregisterDBFile releases a database file's reference (pager close) and
// derefs the group.
func UnregisterDBFile(path string) {
	mu.Lock()
	defer mu.Unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	for _, g := range groups {
		if f, ok := g.files[abs]; ok && f.nref > 0 {
			f.nref--
			derefGroupLocked(g)
			return
		}
	}
}

// CheckDBFileGrowth mirrors test_quota.c quotaWrite for the pager's
// file-extension point: growing the file at path to newSize is checked
// against the group cap; the callback may update the limit; over-limit
// growth returns SQLITE_FULL ("database or disk is full"). On success the
// tracked size is updated. No-op when the layer is not initialized or no
// group matches.
func CheckDBFileGrowth(path string, newSize int64) error {
	mu.Lock()
	defer mu.Unlock()
	if !initialized {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	g := findGroupLocked(abs)
	if g == nil {
		return nil
	}
	f := g.files[abs]
	if f == nil {
		size := int64(0)
		if st, serr := os.Stat(abs); serr == nil {
			size = st.Size()
		}
		f = &fileEntry{name: abs, size: size, nref: 1, rawSize: size}
		g.files[abs] = f
	}
	if f.size >= newSize {
		return nil
	}
	szNew := groupSizeLocked(g) - f.size + newSize
	if szNew > g.limit && g.limit > 0 {
		if g.callback != nil {
			g.callback(abs, &g.limit, szNew)
		}
		if szNew > g.limit && g.limit > 0 {
			return errors.New("database or disk is full")
		}
	}
	f.size = newSize
	return nil
}

// groupSizeLocked sums the tracked sizes of the group's files.
func groupSizeLocked(g *group) int64 {
	total := int64(0)
	for _, f := range g.files {
		total += f.size
	}
	return total
}

// GroupDump / FileDump describe the layer state for sqlite3_quota_dump.
type GroupDump struct {
	Pattern string
	Limit   int64
	Size    int64
	Files   []FileDump
}

type FileDump struct {
	Name          string
	Size          int64
	RefCount      int
	DeleteOnClose bool
}

// Dump mirrors test_quota_dump: one entry per group, newest group first.
func Dump() []GroupDump {
	mu.Lock()
	defer mu.Unlock()
	var out []GroupDump
	for _, g := range groups {
		gd := GroupDump{Pattern: g.pattern, Limit: g.limit, Size: groupSizeLocked(g)}
		for _, f := range g.files {
			gd.Files = append(gd.Files, FileDump{Name: f.name, Size: f.size, RefCount: f.nref, DeleteOnClose: f.deleteOnClose})
		}
		out = append(out, gd)
	}
	return out
}
