// Command sqlite measures system SQLite's per-operation cost on the same op
// shapes as ../frigolite, in the same literal mode: every call re-prepares one
// statement (prepare-per-call with inlined literals). CGo is required; it links
// the system libsqlite3.
//
//	go run . -index
package main

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>
*/
import "C"

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unsafe"
)

// DB wraps one sqlite3 connection for the benchmark.
type DB struct{ h *C.sqlite3 }

func main() {
	rows := flag.Int("rows", 50000, "table rows loaded by the insert phase")
	pointOps := flag.Int("point-ops", 20000, "point SELECT/UPDATE/DELETE operations")
	fileOps := flag.Int("file-ops", 5000, "file-backed autocommit INSERTs")
	reps := flag.Int("reps", 3, "repetitions (median reported)")
	withIndex := flag.Bool("index", false, "also measure shapes over a secondary index")
	scanPasses := flag.Int("scan-passes", 20, "full-scan passes")
	flag.Parse()

	o := options{rows: *rows, pointOps: *pointOps, fileOps: *fileOps,
		scanPasses: *scanPasses, withIndex: *withIndex}

	fmt.Printf("SQLITE libversion %s\n", C.GoString(C.sqlite3_libversion()))
	out := map[string][]float64{}
	for i := 0; i < *reps; i++ {
		if err := runOnce(o, out); err != nil {
			fmt.Fprintln(os.Stderr, "FATAL:", err)
			os.Exit(1)
		}
	}
	printMedians(out)
}

type options struct {
	rows       int
	pointOps   int
	fileOps    int
	scanPasses int
	withIndex  bool
}

// bench is one repetition's database plus its report sink.
type bench struct {
	db  *DB
	o   options
	out map[string][]float64
}

func runOnce(o options, out map[string][]float64) error {
	db, err := open(":memory:")
	if err != nil {
		return err
	}
	defer db.close()
	if err := db.exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT)"); err != nil {
		return err
	}
	if o.withIndex {
		if err := db.exec("CREATE INDEX i1 ON t(b)"); err != nil {
			return err
		}
	}
	b := &bench{db: db, o: o, out: out}
	b.phases()
	return b.filePhase()
}

func (b *bench) phases() {
	b.insert()
	b.pointSelect()
	b.indexedSelect()
	b.scan()
	b.countStar()
	b.groupBy()
	b.pointUpdate()
	b.indexedUpdate()
	b.pointDelete()
	b.indexedDelete()
}

func (b *bench) insert() {
	record(b.out, "insert", func() (float64, error) {
		t0 := time.Now()
		if err := b.db.exec("BEGIN"); err != nil {
			return 0, err
		}
		for i := 1; i <= b.o.rows; i++ {
			if err := b.db.execf("INSERT INTO t VALUES(%d,%d,'n%d')", i, i*2, i); err != nil {
				return 0, err
			}
		}
		if err := b.db.exec("COMMIT"); err != nil {
			return 0, err
		}
		return ops(b.o.rows, t0), nil
	})
}

func (b *bench) pointSelect() {
	record(b.out, "point-select", func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < b.o.pointOps; i++ {
			if err := b.db.execf("SELECT c FROM t WHERE a=%d", key(b.o.rows, i, 7919)); err != nil {
				return 0, err
			}
		}
		return ops(b.o.pointOps, t0), nil
	})
}

func (b *bench) indexedSelect() {
	if !b.o.withIndex {
		return
	}
	record(b.out, "indexed-select", func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < b.o.pointOps; i++ {
			if err := b.db.execf("SELECT a FROM t WHERE b=%d", key(b.o.rows, i, 7919)*2); err != nil {
				return 0, err
			}
		}
		return ops(b.o.pointOps, t0), nil
	})
}

// scan phases are throughput: rows (or passes) per second.
func (b *bench) scan() { b.throughput("scan", "SELECT sum(b) FROM t", true) }

func (b *bench) countStar() { b.throughput("count-star", "SELECT count(*) FROM t", true) }

func (b *bench) groupBy() {
	b.throughput("group-by", "SELECT b%7, count(*) FROM t GROUP BY 1", false)
}

func (b *bench) throughput(name, sql string, perRow bool) {
	record(b.out, name, func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < b.o.scanPasses; i++ {
			if err := b.db.exec(sql); err != nil {
				return 0, err
			}
		}
		n := float64(b.o.scanPasses)
		if perRow {
			n *= float64(b.o.rows)
		}
		return n / time.Since(t0).Seconds(), nil
	})
}

func (b *bench) pointUpdate() {
	record(b.out, "point-update", func() (float64, error) {
		return b.timedLoop(b.o.pointOps, func(i int) error {
			return b.db.execf("UPDATE t SET c='v%d' WHERE a=%d", i, key(b.o.rows, i, 7919))
		})
	})
}

func (b *bench) indexedUpdate() {
	if !b.o.withIndex {
		return
	}
	record(b.out, "indexed-update", func() (float64, error) {
		return b.timedLoop(b.o.pointOps, func(i int) error {
			return b.db.execf("UPDATE t SET c='u%d' WHERE b=%d", i, key(b.o.rows, i, 7919)*2)
		})
	})
}

func (b *bench) pointDelete() {
	record(b.out, "point-delete", func() (float64, error) {
		return b.timedLoop(b.o.pointOps, func(i int) error {
			return b.db.execf("DELETE FROM t WHERE a=%d", key(b.o.rows, i, 7919))
		})
	})
}

func (b *bench) indexedDelete() {
	if !b.o.withIndex {
		return
	}
	record(b.out, "indexed-delete", func() (float64, error) {
		return b.timedLoop(b.o.pointOps/10, func(i int) error {
			return b.db.execf("DELETE FROM t WHERE b=%d", key(b.o.rows, i, 7919)*2)
		})
	})
}

// timedLoop runs n statements inside one transaction and returns ops/s.
func (b *bench) timedLoop(n int, stmt func(i int) error) (float64, error) {
	t0 := time.Now()
	if err := b.db.exec("BEGIN"); err != nil {
		return 0, err
	}
	for i := 0; i < n; i++ {
		if err := stmt(i); err != nil {
			return 0, err
		}
	}
	if err := b.db.exec("COMMIT"); err != nil {
		return 0, err
	}
	return ops(n, t0), nil
}

func (b *bench) filePhase() error {
	dir, err := os.MkdirTemp("", "sqlitebench")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	db, err := open(filepath.Join(dir, "b.db"))
	if err != nil {
		return err
	}
	defer db.close()
	if err := db.exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT)"); err != nil {
		return err
	}
	record(b.out, "file-insert", func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < b.o.fileOps; i++ {
			if err := db.execf("INSERT INTO t VALUES(%d,%d,'n%d')", i+1, i*2, i); err != nil {
				return 0, err
			}
		}
		return ops(b.o.fileOps, t0), nil
	})
	return nil
}

func printMedians(out map[string][]float64) {
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		xs := out[n]
		sort.Float64s(xs)
		fmt.Printf("SQLITE %-18s %14.0f ops/s\n", n, xs[len(xs)/2])
	}
}

// record runs one phase and appends its ops/s to out under name.
func record(out map[string][]float64, name string, phase func() (float64, error)) {
	v, err := phase()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", name, err)
		os.Exit(1)
	}
	out[name] = append(out[name], v)
}

// key maps iteration i onto a table row, with a stride that walks the whole
// table instead of clustering on the hot pages.
func key(rows, i, stride int) int { return (i*stride)%rows + 1 }

func ops(n int, t0 time.Time) float64 { return float64(n) / time.Since(t0).Seconds() }

func open(path string) (*DB, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	var h *C.sqlite3
	if rc := C.sqlite3_open(cp, &h); rc != C.SQLITE_OK {
		return nil, fmt.Errorf("sqlite3_open(%s): rc=%d", path, rc)
	}
	return &DB{h}, nil
}

func (d *DB) close() { C.sqlite3_close(d.h) }

func (d *DB) errmsg() string { return C.GoString(C.sqlite3_errmsg(d.h)) }

// exec prepares and steps every statement in sql, one statement per call.
func (d *DB) exec(sql string) error {
	cs := C.CString(sql)
	defer C.free(unsafe.Pointer(cs))
	tail := cs
	for {
		var stmt *C.sqlite3_stmt
		var ztail *C.char
		if rc := C.sqlite3_prepare_v2(d.h, tail, -1, &stmt, &ztail); rc != C.SQLITE_OK {
			return fmt.Errorf("prepare %.60q: %s", C.GoString(tail), d.errmsg())
		}
		if stmt == nil {
			return nil
		}
		if err := d.stepAll(stmt, tail); err != nil {
			C.sqlite3_finalize(stmt)
			return err
		}
		C.sqlite3_finalize(stmt)
		tail = ztail
	}
}

func (d *DB) stepAll(stmt *C.sqlite3_stmt, sql *C.char) error {
	for {
		rc := C.sqlite3_step(stmt)
		if rc == C.SQLITE_ROW {
			continue
		}
		if rc != C.SQLITE_DONE {
			return fmt.Errorf("step %.60q: %s", C.GoString(sql), d.errmsg())
		}
		return nil
	}
}

func (d *DB) execf(format string, args ...interface{}) error {
	return d.exec(fmt.Sprintf(format, args...))
}
