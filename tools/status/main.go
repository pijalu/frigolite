// Command status reports per-family testgen progress for Frigolite.
//
// It classifies every testgen/<pkg> package into a feature family
// (tools/status/families.tsv), computes static skip information from the
// generated test files and the tools/tcl2go/gen.go skip maps, then either
// runs `go test -tags testgen ./testgen/<pkg>/` for every active package
// (worker pool, default concurrency 8) or, with --skip-run, reports the
// cached results from tools/status/last_run.json.
//
// Output formats: text (default), markdown, json.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// defaultConcurrency is the default worker-pool size for running go test.
const defaultConcurrency = 8

// defaultTimeout is the per-package go test timeout.
const defaultTimeout = 60 * time.Second

type options struct {
	skipRun     bool
	audit       bool
	format      string
	concurrency int
	timeout     time.Duration
	repo        string
	out         string
}

func parseFlags() options {
	var o options
	flag.BoolVar(&o.skipRun, "skip-run", false, "report cached results from tools/status/last_run.json instead of running go test")
	flag.BoolVar(&o.audit, "audit", false, "fail (exit 1) if any whole-file skip lacks a portplan/NA_EVIDENCE.md entry")
	flag.StringVar(&o.format, "format", "text", "output format: text, markdown, json")
	flag.IntVar(&o.concurrency, "concurrency", defaultConcurrency, "number of parallel go test workers")
	flag.DurationVar(&o.timeout, "timeout", defaultTimeout, "per-package go test timeout")
	flag.StringVar(&o.repo, "repo", "", "repository root (default: walk up from cwd until go.mod is found)")
	flag.StringVar(&o.out, "out", "", "also write the report to this file")
	flag.Parse()
	return o
}

func main() {
	opts := parseFlags()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		os.Exit(1)
	}
}
