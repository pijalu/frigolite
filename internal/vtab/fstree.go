package vtab

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// FsTreeModule implements the fstree eponymous virtual table, a faithful port
// of src/test_fs.c (fstreeConnect/fstreeBestIndex/fstreeFilter). The whole
// filesystem below a constraint-derived scan directory is visible through
// columns (path, size, data). Rows are produced in the same order as the C
// implementation's recursive CTE over fsdir: a FIFO queue seeded with the
// scan directory's children (raw readdir order, dot-prefixed names skipped);
// each dequeued directory appends its own children. Paths are absolute-style
// ("dir/name"; the root scan yields "/name"), symlinked directories ARE
// followed (the CTE recurses through fsdir, whose opendir follows symlinks),
// size/data are read through an fd opened per row and are NULL for anything
// that is not a regular file (S_ISREG gate).
type FsTreeModule struct{}

// NewFsTreeModule builds the fstree module.
func NewFsTreeModule() *FsTreeModule { return &FsTreeModule{} }

// Eponymous implements EponymousModule: test_fs.c registers fstree with
// sqlite3_create_module and fstreeConnect is both xCreate and xConnect.
func (m *FsTreeModule) Eponymous() bool { return true }

// NeedsLimitPushdown implements LimitPushdown: the scan from "/" is
// unbounded, so the core's LIMIT must stop the cursor (the VDBE ceases to
// consume rows once the LIMIT is satisfied — e.g. vtabH 3.1 reads exactly
// $num_root_files rows from the root level).
func (m *FsTreeModule) NeedsLimitPushdown() bool { return true }

// Create implements Module (xCreate == xConnect parity).
func (m *FsTreeModule) Create(args []string) (VirtualTable, error) {
	if len(args) != 0 {
		// C: argc != 3 (module+db+table only) -> "wrong number of arguments".
		return nil, fmt.Errorf("wrong number of arguments")
	}
	return &fstreeVTab{}, nil
}

// Connect implements Module (xConnect).
func (m *FsTreeModule) Connect(args []string) (VirtualTable, error) {
	return m.Create(args)
}

type fstreeVTab struct {
	scanDir string // bound ?1 (zDir/nDir: the pattern prefix up to the last '/')
	wild    [2]byte
}

// Columns implements ColumnInfo (fstreeConnect declares
// "CREATE TABLE xyz(path, size, data);").
func (v *fstreeVTab) Columns() []string { return []string{"path", "size", "data"} }

// BestIndex accepts the default plan; the engine keeps GLOB/LIKE/EQ on path
// as residual (C never sets the omit flag) and re-applies it per row.
func (v *fstreeVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// SetPathConstraint implements PathFilterSink: absorb the usable path
// constraint (xFilter argv[0] + idxNum), deriving the scan directory per
// fstreeFilter: walk the pattern from the platform prefix ("" on unix) until
// the first wildcard, remembering the position of the LAST '/' seen; bind
// zDir[0:nDir], forcing nDir to at least 1 (the C "if( nDir==0 ) nDir = 1;"
// quirk). An equality constraint has no wildcards, so the scan directory is
// the constraint path's parent directory.
func (v *fstreeVTab) SetPathConstraint(value string, op PathConstraintOp) {
	switch op {
	case PathConstraintGlob:
		v.wild = [2]byte{'*', '?'}
	case PathConstraintLike:
		v.wild = [2]byte{'_', '%'}
	default: // PathConstraintEq: aWild stays {0, 0} (no wildcard characters)
		v.wild = [2]byte{0, 0}
	}
	nDir := 0 // position of the last '/' before the first wildcard
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != 0 && (c == v.wild[0] || c == v.wild[1]) {
			break
		}
		if c == '/' {
			nDir = i
		}
	}
	if nDir == 0 {
		nDir = 1 // C quirk: at least the first character is bound
	}
	v.scanDir = value[:nDir]
}

// fstreeChildPaths lists one directory's entry paths in raw readdir order
// (fsdir's cursor walks readdir directly; os.ReadDir would sort), skipping
// dot-prefixed names ("name NOT LIKE '.%'"). The CTE base row composes
// CASE WHEN dir='/' THEN ” ELSE dir END || '/' || name; on unix the root
// prefix is "" so root children are "/name" while deeper children keep the
// parent path verbatim ("dir/name").
func fstreeChildPaths(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil // opendir failure (ENOTDIR, EACCES, ...) yields no rows
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil
	}
	prefix := dir
	if dir == "/" {
		prefix = "" // CASE WHEN dir=?2 ('/') THEN ?3 ('') ELSE dir END
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.HasPrefix(n, ".") {
			continue
		}
		out = append(out, prefix+"/"+n)
	}
	return out
}

// fstreeCursor is the FIFO row queue: the CTE's UNION ALL recursion pops the
// front row, emits it, and appends its directory children to the back —
// breadth-first by level, readdir order within each parent.
type fstreeCursor struct {
	queue   []string
	f       *os.File // fd open on the current row (C keeps pCsr->fd)
	cur     string
	started bool
	done    bool
}

// Open positions at the first row (xFilter returns fstreeNext).
func (v *fstreeVTab) Open() (Cursor, error) {
	dir := v.scanDir
	if dir == "" {
		// No usable constraint: zRoot "/" (unix), nRoot=1.
		dir = "/"
	}
	c := &fstreeCursor{queue: fstreeChildPaths(dir)}
	c.advance()
	return c, nil
}

// advance pops the front row as current and appends its children when it is
// a directory, then opens the row's fd (C's fstreeNext: open + sqlite3_step).
func (c *fstreeCursor) advance() {
	c.closeFile()
	if len(c.queue) == 0 {
		c.done = true
		return
	}
	path := c.queue[0]
	c.queue = c.queue[1:]
	// The CTE recurses on EVERY row via fsdir(dir=d); opendir follows
	// symlinks, so the recursion test is a stat (not an lstat).
	if children := fstreeChildPaths(path); children != nil {
		c.queue = append(c.queue, children...)
	}
	c.cur = path
	c.f, _ = os.Open(path) // open failure -> NULL size/data (fstat on -1)
}

// closeFile releases the current row's fd.
func (c *fstreeCursor) closeFile() {
	if c.f != nil {
		c.f.Close()
		c.f = nil
	}
}

// Next implements Cursor (the first row is positioned by Open; the first
// Next serves it — the zipCursor convention).
func (c *fstreeCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return true
	}
	c.advance()
	return !c.done
}

// statCurrent returns the fstat of the row's open fd (nil when the fd could
// not be opened — C's fstat(-1) fails and the S_ISREG test stays false).
func (c *fstreeCursor) statCurrent() os.FileInfo {
	if c.f == nil {
		return nil
	}
	fi, err := c.f.Stat()
	if err != nil {
		return nil
	}
	return fi
}

// Column implements Cursor. path is the CTE row; size/data come from the row
// fd and are NULL unless the path is a regular file (fstreeColumn's
// S_ISREG(sBuf.st_mode) gate). A short read is a disk I/O error
// (SQLITE_IOERR parity).
func (c *fstreeCursor) Column(idx int) (interface{}, error) {
	switch idx {
	case 0:
		return c.cur, nil
	case 1:
		if fi := c.statCurrent(); fi != nil && fi.Mode().IsRegular() {
			return fi.Size(), nil
		}
		return nil, nil
	case 2:
		fi := c.statCurrent()
		if fi == nil || !fi.Mode().IsRegular() {
			return nil, nil
		}
		data := make([]byte, fi.Size())
		if _, err := io.ReadFull(c.f, data); err != nil {
			// C returns SQLITE_IOERR when read() yields fewer bytes than
			// st_size; the core maps that to "disk I/O error".
			return nil, fmt.Errorf("disk I/O error")
		}
		return data, nil
	}
	return nil, fmt.Errorf("fstree: invalid column index %d", idx)
}

// Close implements Cursor.
func (c *fstreeCursor) Close() error {
	c.closeFile()
	return nil
}
