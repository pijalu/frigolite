// Demo: bulk insert performance test.
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/pijalu/frigolite"
)

func main() {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	fmt.Println("=== Bulk Insert Performance Demo ===")
	fmt.Println()

	db.Exec("CREATE TABLE perf (id INTEGER, val1 INTEGER, val2 TEXT)")

	const N = 1000

	start := time.Now()
	for i := 0; i < N; i++ {
		sql := fmt.Sprintf("INSERT INTO perf VALUES (%d, %d, 'value_%d')", i, i*2, i)
		res := db.Exec(sql)
		if res.Error != nil {
			log.Fatal(res.Error)
		}
	}
	elapsed := time.Since(start)
	fmt.Printf("Inserted %d rows in %v (%.0f rows/sec)\n", N, elapsed, float64(N)/elapsed.Seconds())

	// Read all
	start = time.Now()
	res := db.Query("SELECT * FROM perf")
	if res.Error != nil {
		log.Fatal(res.Error)
	}
	readElapsed := time.Since(start)
	fmt.Printf("Read %d rows in %v (%.0f rows/sec)\n", len(res.Rows), readElapsed, float64(len(res.Rows))/readElapsed.Seconds())

	// Query with WHERE
	start = time.Now()
	res = db.Query("SELECT * FROM perf WHERE val1 > 500")
	filterElapsed := time.Since(start)
	fmt.Printf("Filtered %d of %d rows in %v\n", len(res.Rows), N, filterElapsed)

	fmt.Println("\n=== Perf Demo Complete ===")
}
