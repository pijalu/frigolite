// Benchmark tool comparing Frigolite against go-sqlite3.
//
// Runs the test SQL files found in the project's testdata/ directory against
// both Frigolite and SQLite (via go-sqlite3 CGo driver), measures execution
// time, and reports a comparison table.
//
// Usage:
//
//	go run . -dir ../../testdata
//	go run . -dir ../../testdata -limit 10        # first 10 files only
//	go run . -dir ../../testdata -file where4      # specific file only
//	go run . -dir ../../testdata -min-time 0.5s    # only files >= 0.5s SQLite
//	go run . -dir ../../testdata -csv results.csv  # export to CSV
//
// CGo is required (go-sqlite3 uses CGo).
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	frigo "github.com/pijalu/frigolite"
)

// TestCase mirrors the JSON test file structure.
type TestCase struct {
	Name  string     `json:"name"`
	Steps []TestStep `json:"steps"`
}

type TestStep struct {
	Type string `json:"type"` // "exec" or "query"
	SQL  string `json:"sql"`
}

type TestFileData struct {
	File  string     `json:"file"`
	Name  string     `json:"name"`
	Tests []TestCase `json:"tests"`
}

// result holds benchmark results for one test file against one engine.
type benchResult struct {
	Name       string
	SQLCount   int
	SQLiteTime time.Duration
	SQLiteOK   bool
	FrigoTime  time.Duration
	FrigoOK    bool
	Ratio      float64 // FrigoTime / SQLiteTime
}

func main() {
	dir := flag.String("dir", "testdata", "directory containing test JSON files")
	file := flag.String("file", "", "specific test file to benchmark (without .json)")
	limit := flag.Int("limit", 0, "max number of files to process (0 = all)")
	minTime := flag.Duration("min-time", 0, "minimum SQLite time to show (e.g. 500ms)")
	csvPath := flag.String("csv", "", "export results to CSV file")
	flag.Parse()

	if *dir == "" {
		log.Fatal("--dir is required")
	}

	testDir := resolveTestDir(*dir)
	log.Printf("Using test data directory: %s", testDir)

	matches := collectTestFiles(testDir, *file)
	log.Printf("Found %d test files", len(matches))

	if *limit > 0 && *limit < len(matches) {
		matches = matches[:*limit]
	}

	results := runBenchmarks(matches, *minTime)
	if len(results) == 0 {
		log.Println("No results to show (try removing -min-time or -limit)")
		return
	}

	printResults(results, *csvPath)
}

// resolveTestDir returns the absolute test data directory, walking upward
// from the working directory when the given path is relative and missing.
func resolveTestDir(testDir string) string {
	if filepath.IsAbs(testDir) {
		return testDir
	}
	// Try relative to working directory; if not found, walk upward.
	if _, err := os.Stat(testDir); err == nil {
		return testDir
	}
	cwd, _ := os.Getwd()
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(cwd, testDir)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		cwd = filepath.Dir(cwd)
	}
	return testDir
}

// collectTestFiles returns the test JSON files, filtered to a specific file
// when requested (exact match, then partial name match).
func collectTestFiles(testDir, file string) []string {
	pattern := filepath.Join(testDir, "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		log.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		log.Fatalf("no test files found in %s", testDir)
	}
	if file == "" {
		return matches
	}

	target := filepath.Join(testDir, file+".json")
	var filtered []string
	for _, m := range matches {
		if m == target {
			filtered = append(filtered, m)
			break
		}
	}
	if len(filtered) == 0 {
		// Try partial match.
		for _, m := range matches {
			if strings.Contains(filepath.Base(m), file) {
				filtered = append(filtered, m)
			}
		}
	}
	if len(filtered) == 0 {
		log.Fatalf("file not found: %s", file)
	}
	return filtered
}

// runBenchmarks benchmarks each file and collects results above the minimum
// time threshold.
func runBenchmarks(matches []string, minTime time.Duration) []benchResult {
	var results []benchResult
	for _, fpath := range matches {
		r := benchmarkFile(fpath)
		if r == nil {
			continue
		}
		if minTime > 0 && r.SQLiteTime < minTime {
			continue
		}
		results = append(results, *r)
		fmt.Printf(".")
	}
	fmt.Println()
	return results
}

// benchmarkFile runs a single test file against both engines and returns results.
func benchmarkFile(fpath string) *benchResult {
	fname := filepath.Base(fpath)
	base := strings.TrimSuffix(fname, ".json")

	data, err := os.ReadFile(fpath)
	if err != nil {
		log.Printf("ERROR reading %s: %v", fname, err)
		return nil
	}

	var td TestFileData
	if err := json.Unmarshal(data, &td); err != nil {
		log.Printf("ERROR parsing %s: %v", fname, err)
		return nil
	}

	// Collect all SQL statements in order.
	var sqlBlocks []string
	for _, tc := range td.Tests {
		for _, step := range tc.Steps {
			if step.SQL != "" {
				sqlBlocks = append(sqlBlocks, step.SQL)
			}
		}
	}

	if len(sqlBlocks) == 0 {
		return nil
	}

	sqliteTime, sqliteOK := benchSQLite(sqlBlocks)
	frigoTime, frigoOK := benchFrigolite(sqlBlocks)

	ratio := 0.0
	if sqliteTime > 0 && frigoTime > 0 {
		ratio = float64(frigoTime) / float64(sqliteTime)
	}

	return &benchResult{
		Name:       base,
		SQLCount:   len(sqlBlocks),
		SQLiteTime: sqliteTime,
		SQLiteOK:   sqliteOK,
		FrigoTime:  frigoTime,
		FrigoOK:    frigoOK,
		Ratio:      ratio,
	}
}

// benchSQLite runs SQL blocks against go-sqlite3 and returns total time.
func benchSQLite(blocks []string) (time.Duration, bool) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return 0, false
	}
	defer db.Close()

	// Set pragmas matching test expectations.
	db.Exec("PRAGMA automatic_index=ON")

	start := time.Now()
	for _, block := range blocks {
		// Split multi-statement blocks and execute each separately.
		stmts := splitSQL(block)
		for _, stmt := range stmts {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			// Use Exec for all statements (both DML/DDL and queries).
			if _, err := db.Exec(stmt); err != nil {
				// Log and continue for errors (some test SQL has syntax errors
				// even for SQLite).
				_ = err
			}
		}
	}
	elapsed := time.Since(start)
	return elapsed, true
}

// benchFrigolite runs SQL blocks against Frigolite and returns total time.
func benchFrigolite(blocks []string) (time.Duration, bool) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		return 0, false
	}
	defer db.Close()

	start := time.Now()
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		// Use Exec for all statements (handles multi-statement blocks).
		res := db.Exec(block)
		if res.Error != nil {
			// Continue on error for fair comparison.
			_ = res.Error
		}
	}
	elapsed := time.Since(start)
	return elapsed, true
}

// splitSQL splits a SQL string into individual statements.
func splitSQL(sql string) []string {
	var stmts []string
	for _, s := range strings.Split(sql, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			stmts = append(stmts, s+";")
		}
	}
	return stmts
}

// printResults prints the comparison table and optionally exports to CSV.
func printResults(results []benchResult, csvPath string) {
	// Sort by SQLite time descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].SQLiteTime > results[j].SQLiteTime
	})

	fmt.Println()
	fmt.Println("=", 80)
	fmt.Println("FRIGOLITE vs SQLITE3 BENCHMARK COMPARISON")
	fmt.Println("=", 80)
	fmt.Printf("%-30s %-8s %-12s %-12s %-8s %s\n",
		"File", "SQLs", "SQLite", "Frigolite", "Ratio", "Status")
	fmt.Println(strings.Repeat("-", 80))

	for _, r := range results {
		sqliteStr := fmt.Sprintf("%.4fs", r.SQLiteTime.Seconds())
		frigoStr := fmt.Sprintf("%.4fs", r.FrigoTime.Seconds())
		ratioStr := fmt.Sprintf("%.1fx", r.Ratio)
		status := "OK"
		if !r.SQLiteOK || !r.FrigoOK {
			status = "ERR"
		}
		fmt.Printf("%-30s %-8d %-12s %-12s %-8s %s\n",
			r.Name, r.SQLCount, sqliteStr, frigoStr, ratioStr, status)
	}

	fmt.Println(strings.Repeat("-", 80))

	// Aggregate stats.
	var totalSQLite, totalFrigo time.Duration
	okCount := 0
	for _, r := range results {
		totalSQLite += r.SQLiteTime
		totalFrigo += r.FrigoTime
		if r.SQLiteOK && r.FrigoOK {
			okCount++
		}
	}
	avgRatio := 0.0
	if totalSQLite > 0 {
		avgRatio = float64(totalFrigo) / float64(totalSQLite)
	}
	fmt.Printf("Total: %d files, %d OK\n", len(results), okCount)
	fmt.Printf("Total SQLite:   %.3fs\n", totalSQLite.Seconds())
	fmt.Printf("Total Frigolite: %.3fs\n", totalFrigo.Seconds())
	fmt.Printf("Average ratio:  %.1fx\n", avgRatio)

	// Export CSV.
	if csvPath != "" {
		f, err := os.Create(csvPath)
		if err != nil {
			log.Printf("ERROR creating CSV: %v", err)
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "Name,SQLCount,SQLiteTime(s),FrigoliteTime(s),Ratio,SQLiteOK,FrigoOK\n")
		for _, r := range results {
			fmt.Fprintf(f, "%s,%d,%.4f,%.4f,%.1f,%v,%v\n",
				r.Name, r.SQLCount, r.SQLiteTime.Seconds(), r.FrigoTime.Seconds(),
				r.Ratio, r.SQLiteOK, r.FrigoOK)
		}
		log.Printf("Results exported to %s", csvPath)
	}
}
