package vtab

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FsdirModule implements the fsdir table-valued function (ext/misc/fileio.c):
// FSDIR_SCHEMA "(name,mode,mtime,data,level,path HIDDEN,dir HIDDEN)" over the
// directory tree rooted at its first argument.
type FsdirModule struct{}

// NewFsdirModule builds the fsdir module.
func NewFsdirModule() *FsdirModule { return &FsdirModule{} }

// Eponymous implements EponymousModule.
func (m *FsdirModule) Eponymous() bool { return true }

// Columns returns the declared schema columns.
func (m *FsdirModule) Columns() []string {
	return []string{"name", "mode", "mtime", "data", "level", "path", "dir"}
}

// HiddenColumns marks path and dir as HIDDEN.
func (m *FsdirModule) HiddenColumns() map[int]bool { return map[int]bool{5: true, 6: true} }

// Create implements Module.
func (m *FsdirModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module.
func (m *FsdirModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// CreateWithValues implements ValueModule so TVF arguments bind at runtime.
func (m *FsdirModule) CreateWithValues(valArgs []interface{}) (VirtualTable, error) {
	if len(valArgs) < 1 || len(valArgs) > 2 {
		return nil, fmt.Errorf("fsdir requires one or two arguments")
	}
	path, _ := valArgs[0].(string)
	dir := ""
	if len(valArgs) == 2 {
		dir, _ = valArgs[1].(string)
	}
	return &fsdirVTab{root: path, base: dir, columns: m.Columns()}, nil
}

// eponymous fsdir (FROM fsdir with no argument): the scan root comes from
// the hidden dir column constraint at execution time (vtabH 3.x queries
// `FROM fsdir WHERE dir = '.'`), so a zero-argument connect is legal and
// only produces an unconstrained instance. Argument forms keep the
// fileio.c path/base semantics below.
func (m *FsdirModule) connect(args []string) (VirtualTable, error) {
	if len(args) == 0 {
		return &fsdirVTab{columns: m.Columns(), flat: true}, nil
	}
	if len(args) < 1 || len(args) > 2 {
		return nil, fmt.Errorf("fsdir requires one or two arguments")
	}
	a := strings.TrimSpace(unquoteVtabArg(strings.TrimSpace(args[0])))
	v := &fsdirVTab{root: a, columns: m.Columns()}
	if len(args) == 2 {
		v.base = strings.TrimSpace(unquoteVtabArg(strings.TrimSpace(args[1])))
	}
	return v, nil
}

// SetHiddenConstraint implements HiddenConstraintSetter: the eponymous
// zero-argument fsdir form binds its scan root through the hidden dir column
// (`FROM fsdir WHERE dir = '.'`, vtabH 3.x).
func (v *fsdirVTab) SetHiddenConstraint(col string, val interface{}) error {
	if col != "dir" {
		return fmt.Errorf("no such column: %s", col)
	}
	text, isStr := val.(string)
	if !isStr {
		return fmt.Errorf("fsdir: unusable dir value")
	}
	v.root = text
	return nil
}

type fsdirVTab struct {
	root    string
	base    string
	columns []string
	flat    bool // eponymous zero-argument form: single-level listing
}

// Columns implements ColumnInfo.
func (v *fsdirVTab) Columns() []string { return v.columns }

// HiddenColumns implements HiddenColumnInfo.
func (v *fsdirVTab) HiddenColumns() map[int]bool { return map[int]bool{5: true, 6: true} }

// BestIndex accepts the default full-scan plan.
func (v *fsdirVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open walks the tree rooted at the bound path, sorted for determinism.
// fileio.c fsdirFilter: when the hidden dir (base) column is bound, the scan
// root becomes base+"/"+path and names are reported relative to base
// (zipfile.test 12.5: fsdir('.', 'subdir') → ".", "./x1.txt", ...).
func (v *fsdirVTab) Open() (Cursor, error) {
	type rowT struct {
		name  string
		mode  uint32
		mtime int64
		level int
	}
	root := v.root
	if v.base != "" {
		root = v.base + "/" + root
	}
	// Eponymous zero-argument mode (`FROM fsdir WHERE dir = '.'`, vtabH
	// 3.x): one flat, non-recursive listing — the bound directory itself
	// followed by its immediate children, names reported as written.
	if v.flat {
		info, serr := os.Stat(root)
		if serr != nil || !info.IsDir() {
			return &sliceCursor{}, nil
		}
		cursorRows := [][]interface{}{{
			root, uint32(info.Mode().Perm()) | 0040000, info.ModTime().Unix(),
			nil, int64(1), root, "",
		}}
		names := make([]string, 0)
		byName := map[string]os.DirEntry{}
		if entries, rerr := os.ReadDir(root); rerr == nil {
			for _, e := range entries {
				names = append(names, e.Name())
				byName[e.Name()] = e
			}
		}
		sort.Strings(names)
		for _, n := range names {
			fi, ierr := byName[n].Info()
			if ierr != nil {
				continue
			}
			md := uint32(fi.Mode().Perm())
			if fi.IsDir() {
				md |= 0040000
			} else {
				md |= 0100000
			}
			cursorRows = append(cursorRows, []interface{}{
				n, int64(md), fi.ModTime().Unix(), nil, int64(2), root, "",
			})
		}
		return &sliceCursor{rows: cursorRows}, nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return &sliceCursor{}, nil // SQLite yields no rows for unreadable roots
	}
	var rows []rowT
	level := 1
	// fileio.c emits the base directory itself as the first row
	// (zipfile.test filters it with WHERE name!='test_unzip').
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		rows = append(rows, rowT{root, uint32(fi.Mode().Perm()) | 0040000, fi.ModTime().Unix(), level})
	}
	stack := []struct {
		path  string
		level int
	}{{root, level}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, rerr := os.ReadDir(cur.path)
		if rerr != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		byName := map[string]fs.DirEntry{}
		for _, e := range entries {
			names = append(names, e.Name())
			byName[e.Name()] = e
		}
		sort.Strings(names)
		for _, n := range names {
			e := byName[n]
			// Raw concatenation like fileio.c's mprintf("%s/%s") — no
			// path cleaning, so "./x1.txt" style names survive the
			// base-prefix trim (zipfile.test 12.5).
			full := cur.path + "/" + n
			fi, serr := e.Info()
			if serr != nil {
				continue
			}
			mt := fi.ModTime().Unix()
			md := uint32(fi.Mode().Perm())
			if fi.IsDir() {
				md |= 0040000
				rows = append(rows, rowT{full, md, mt, cur.level + 1})
				stack = append(stack, struct {
					path  string
					level int
				}{full, cur.level + 1})
			} else {
				rows = append(rows, rowT{full, md | 0100000, mt, cur.level + 1})
			}
		}
	}
	_ = info
	cursorRows := make([][]interface{}, 0, len(rows))
	srcPath := root
	for _, r := range rows {
		name := r.name
		if v.base != "" && strings.HasPrefix(name, v.base+"/") {
			name = strings.TrimPrefix(name, v.base+"/")
		}
		cursorRows = append(cursorRows, []interface{}{
			name, int64(r.mode), r.mtime, nil, int64(r.level), srcPath, v.base,
		})
	}
	return &sliceCursor{rows: cursorRows}, nil
}

// sliceCursor iterates pre-built rows (first row already positioned).
type sliceCursor struct {
	rows    [][]interface{}
	idx     int
	started bool
}

// Next implements Cursor.
func (c *sliceCursor) Next() bool {
	if !c.started {
		c.started = true
		return len(c.rows) > 0
	}
	c.idx++
	return c.idx < len(c.rows)
}

// Column implements Cursor.
func (c *sliceCursor) Column(idx int) (interface{}, error) {
	if c.idx < 0 || c.idx >= len(c.rows) || idx >= len(c.rows[c.idx]) {
		return nil, fmt.Errorf("no row")
	}
	return c.rows[c.idx][idx], nil
}

// Close implements Cursor.
func (c *sliceCursor) Close() error { return nil }

// ReadFileFunc implements readfile(path): whole-file blob content. Missing or
// unreadable paths yield SQL NULL without an error (sqlite3 shell parity:
// SELECT readfile('/nope') → NULL).
func ReadFileFunc(path string) (interface{}, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	return string(b), nil
}

// WriteFileFunc implements writefile(name, data[, mode[, mtime]]): writes
// bytes to disk (creating parent directories) and returns the byte count.
func WriteFileFunc(name string, data []byte, mode *uint32, mtime *int64) (int64, error) {
	d := filepath.Dir(name)
	if d != "" && d != "." {
		if err := os.MkdirAll(d, 0777); err != nil {
			return 0, fmt.Errorf("cannot create directory %s", d)
		}
	}
	perm := fs.FileMode(0644)
	if mode != nil {
		p := *mode
		if p&0040000 != 0 {
			if mkerr := os.Mkdir(name, fs.FileMode(p&07777)); mkerr == nil {
				return 0, nil
			}
		} else {
			perm = fs.FileMode(p & 0777)
		}
	}
	if err := os.WriteFile(name, data, perm); err != nil {
		return 0, fmt.Errorf("cannot open file: %s", name)
	}
	if mode != nil {
		os.Chmod(name, fs.FileMode(*mode&0777))
	}
	if mtime != nil {
		os.Chtimes(name, time.Unix(*mtime, 0), time.Unix(*mtime, 0))
	}
	return int64(len(data)), nil
}
