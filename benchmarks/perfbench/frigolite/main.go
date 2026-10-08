// Command frigo measures frigolite's per-operation cost on the CRUD op shapes
// used by benchmarks/R11_RESEARCH.md and benchmarks/R13_RESEARCH.md, in the
// only mode the public API allows: one freshly built SQL string per operation
// (literal mode, no prepare/bind).
//
// Run alongside ../sqlite (same flags) and compare the printed ops/s:
//
//	go run . -index
//	go run ./sqlite -index
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	frigolite "github.com/pijalu/frigolite"
)

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
	db  *frigolite.DB
	o   options
	out map[string][]float64
}

func runOnce(o options, out map[string][]float64) error {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		return err
	}
	defer db.Close()
	if err := exec(db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT)"); err != nil {
		return err
	}
	if o.withIndex {
		if err := exec(db, "CREATE INDEX i1 ON t(b)"); err != nil {
			return err
		}
	}
	b := &bench{db: db, o: o, out: out}
	b.phases()
	return filePhase(o, out)
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
		if err := exec(b.db, "BEGIN"); err != nil {
			return 0, err
		}
		for i := 1; i <= b.o.rows; i++ {
			if err := execf(b.db, "INSERT INTO t VALUES(%d,%d,'n%d')", i, i*2, i); err != nil {
				return 0, err
			}
		}
		if err := exec(b.db, "COMMIT"); err != nil {
			return 0, err
		}
		return ops(b.o.rows, t0), nil
	})
}

func (b *bench) pointSelect() {
	record(b.out, "point-select", func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < b.o.pointOps; i++ {
			q := fmt.Sprintf("SELECT c FROM t WHERE a=%d", key(b.o.rows, i, 7919))
			if r := b.db.Query(q); r.Error != nil {
				return 0, r.Error
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
			q := fmt.Sprintf("SELECT a FROM t WHERE b=%d", key(b.o.rows, i, 7919)*2)
			if r := b.db.Query(q); r.Error != nil {
				return 0, r.Error
			}
		}
		return ops(b.o.pointOps, t0), nil
	})
}

// scanPhases are the throughput phases: rows (or passes) per second.
func (b *bench) scan() {
	b.throughput("scan", "SELECT sum(b) FROM t", true)
}

func (b *bench) countStar() {
	b.throughput("count-star", "SELECT count(*) FROM t", true)
}

func (b *bench) groupBy() {
	b.throughput("group-by", "SELECT b%7, count(*) FROM t GROUP BY 1", false)
}

func (b *bench) throughput(name, sql string, perRow bool) {
	record(b.out, name, func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < b.o.scanPasses; i++ {
			if err := exec(b.db, sql); err != nil {
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
			return execf(b.db, "UPDATE t SET c='v%d' WHERE a=%d", i, key(b.o.rows, i, 7919))
		})
	})
}

func (b *bench) indexedUpdate() {
	if !b.o.withIndex {
		return
	}
	record(b.out, "indexed-update", func() (float64, error) {
		return b.timedLoop(b.o.pointOps, func(i int) error {
			return execf(b.db, "UPDATE t SET c='u%d' WHERE b=%d", i, key(b.o.rows, i, 7919)*2)
		})
	})
}

func (b *bench) pointDelete() {
	record(b.out, "point-delete", func() (float64, error) {
		return b.timedLoop(b.o.pointOps, func(i int) error {
			return execf(b.db, "DELETE FROM t WHERE a=%d", key(b.o.rows, i, 7919))
		})
	})
}

func (b *bench) indexedDelete() {
	if !b.o.withIndex {
		return
	}
	record(b.out, "indexed-delete", func() (float64, error) {
		return b.timedLoop(b.o.pointOps/10, func(i int) error {
			return execf(b.db, "DELETE FROM t WHERE b=%d", key(b.o.rows, i, 7919)*2)
		})
	})
}

// timedLoop runs n statements inside one transaction and returns ops/s.
func (b *bench) timedLoop(n int, stmt func(i int) error) (float64, error) {
	t0 := time.Now()
	if err := exec(b.db, "BEGIN"); err != nil {
		return 0, err
	}
	for i := 0; i < n; i++ {
		if err := stmt(i); err != nil {
			return 0, err
		}
	}
	if err := exec(b.db, "COMMIT"); err != nil {
		return 0, err
	}
	return ops(n, t0), nil
}

func filePhase(o options, out map[string][]float64) error {
	dir, err := os.MkdirTemp("", "frigobench")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	db, err := frigolite.Open(filepath.Join(dir, "b.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := exec(db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT)"); err != nil {
		return err
	}
	record(out, "file-insert", func() (float64, error) {
		t0 := time.Now()
		for i := 0; i < o.fileOps; i++ {
			if err := execf(db, "INSERT INTO t VALUES(%d,%d,'n%d')", i+1, i*2, i); err != nil {
				return 0, err
			}
		}
		return ops(o.fileOps, t0), nil
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
		fmt.Printf("FRIGO %-18s %14.0f ops/s\n", n, xs[len(xs)/2])
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

func exec(db *frigolite.DB, sql string) error {
	if r := db.Exec(sql); r.Error != nil {
		return fmt.Errorf("exec %.60q: %w", sql, r.Error)
	}
	return nil
}

func execf(db *frigolite.DB, format string, args ...interface{}) error {
	return exec(db, fmt.Sprintf(format, args...))
}
