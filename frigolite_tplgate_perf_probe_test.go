package frigolite_test

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/pijalu/frigolite"
)

// TestPerfTplgateProbe measures template-cache throughput on varying-literal
// statements (the same-kind substitution path). Run as:
//
//	go test . -run TestPerfTplgateProbe -v -timeout 30m
//
// It prints ops/s for point SELECT / UPDATE / INSERT loops whose literals
// differ on every iteration (exact-text cache always misses; the template
// cache is the only reuse path).
func TestPerfTplgateProbe(t *testing.T) {
	if os.Getenv("TPLPROBE") == "" {
		t.Skip("set TPLPROBE=1 to run the perf probe")
	}
	const rows = 100_000
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(k INTEGER PRIMARY KEY, v TEXT, n INTEGER)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	{
		var sb []byte
		sb = append(sb, "INSERT INTO t(k,v,n) VALUES"...)
		for i := 0; i < rows; i++ {
			if i > 0 {
				sb = append(sb, ',')
			}
			sb = append(sb, []byte(fmt.Sprintf("(%d,'v%d',%d)", i, i, i))...)
		}
		if r := db.Exec(string(sb)); r.Error != nil {
			t.Fatal(r.Error)
		}
	}

	phase := func(name string, n int, run func(i int) error) {
		runtime.GC()
		start := time.Now()
		for i := 0; i < n; i++ {
			if err := run(i); err != nil {
				t.Fatalf("%s iter %d: %v", name, i, err)
			}
		}
		elapsed := time.Since(start)
		fmt.Printf("PROBE %s: %d ops in %v -> %.0f ops/s\n", name, n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	}

	// Point SELECT with a varying literal: k = <unique value each time>.
	phase("point-select", 200_000, func(i int) error {
		r := db.Query(fmt.Sprintf("SELECT v, n FROM t WHERE k = %d", i%rows))
		if r.Error != nil {
			return r.Error
		}
		if len(r.Rows) != 1 {
			return fmt.Errorf("got %d rows", len(r.Rows))
		}
		return nil
	})

	// UPDATE with varying SET and WHERE literals.
	phase("update", 200_000, func(i int) error {
		r := db.Exec(fmt.Sprintf("UPDATE t SET n = %d WHERE k = %d", 1_000_000+i, i%rows))
		if r.Error != nil {
			return r.Error
		}
		if r.Changes != 1 {
			return fmt.Errorf("changes %d", r.Changes)
		}
		return nil
	})

	// INSERT into a separate table with varying literals.
	if r := db.Exec("CREATE TABLE ins(k INTEGER PRIMARY KEY, v TEXT, n INTEGER)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	phase("insert", 100_000, func(i int) error {
		r := db.Exec(fmt.Sprintf("INSERT INTO ins(k,v,n) VALUES(%d,'x',%d)", 1_000_000+i, i))
		if r.Error != nil {
			return r.Error
		}
		return nil
	})
}

// TestPerfTplgateProbe2 is a lighter UPDATE probe: a small table so the
// per-statement engine cost (not row churn) dominates, with Prepare as the
// varying share. Set TPLPROBE=1.
func TestPerfTplgateProbe2(t *testing.T) {
	const rows = 10_000
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(k INTEGER PRIMARY KEY, v TEXT, n INTEGER)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for i := 0; i < rows; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO t(k,v,n) VALUES(%d,'v',%d)", i, i)); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	phase := func(name string, n int, run func(i int)) {
		runtime.GC()
		start := time.Now()
		for i := 0; i < n; i++ {
			run(i)
		}
		elapsed := time.Since(start)
		fmt.Printf("PROBE2 %s: %d ops in %v -> %.0f ops/s\n", name, n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	}
	phase("update-small", 100_000, func(i int) {
		if r := db.Exec(fmt.Sprintf("UPDATE t SET n = %d WHERE k = %d", 500_000+i, i%rows)); r.Error != nil {
			t.Fatal(r.Error)
		}
	})
	phase("point-select-small", 100_000, func(i int) {
		r := db.Query(fmt.Sprintf("SELECT v FROM t WHERE k = %d", i%rows))
		if r.Error != nil {
			t.Fatal(r.Error)
		}
	})
}

// TestPerfTplgateIns repeats the INSERT phase 3x for variance.
func TestPerfTplgateIns(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE ins(k INTEGER PRIMARY KEY, v TEXT, n INTEGER)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for round := 0; round < 3; round++ {
		runtime.GC()
		start := time.Now()
		for i := 0; i < 100_000; i++ {
			if r := db.Exec(fmt.Sprintf("INSERT INTO ins(k,v,n) VALUES(%d,'x',%d)", 1_000_000+i+round*100_000, i)); r.Error != nil {
				t.Fatal(r.Error)
			}
		}
		elapsed := time.Since(start)
		fmt.Printf("PROBE-INS round %d: %.0f ops/s\n", round, float64(100_000)/elapsed.Seconds())
	}
}
