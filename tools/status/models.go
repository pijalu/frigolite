package main

import "time"

// Package states reported by the tool.
const (
	statePass    = "pass"
	stateFail    = "fail"
	stateSkipped = "skipped"
	stateNotRun  = "not-run"
)

// pkgInfo is the full per-package record: static classification (family,
// skip counts, whole-file skip reason) plus dynamic run state.
type pkgInfo struct {
	name      string        // testgen package directory name
	family    string        // feature family from families.tsv
	state     string        // pass | fail | skipped | not-run
	testFiles int           // generated *_test.go files (excluding helpers)
	skipFiles int           // whole-file skipped test files
	skipTests int           // per-test skip markers across real files
	reason    string        // whole-file skip reason (when fully skipped)
	duration  time.Duration // go test wall time
	errTail   string        // tail of go test output on failure
}

// familySummary aggregates package counts for one family.
type familySummary struct {
	family string
	total  int
	pass   int
	fail   int
	skip   int
	notRun int
	pct    float64 // pass / total * 100
}

// report is the full report tree handed to renderers.
type report struct {
	generatedAt string
	packages    []*pkgInfo
	families    []familySummary
	total       int
	totalPass   int
	totalFail   int
	totalSkip   int
	totalNotRun int
}
