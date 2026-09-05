// Command status reports per-family testgen progress for Frigolite.
//
// It classifies every testgen/<pkg> package into a feature family
// (tools/status/families.tsv), computes static skip information from the
// generated test files and the tools/tcl2go/gen.go skip maps, then either
// runs `go test -tags testgen ./testgen/<pkg>/` for every active package
// (worker pool, default concurrency 8) or, with --skip-run, reports the
// cached results from tools/status/last_run.json.
//
// Subcommands:
//
//	(default)  report — render the current status table (text/markdown/json)
//	ledger     seed tools/status/ledger.json from last_run.json + PORTPLAN §4
//	check      diff live run vs ledger; exit non-zero on unexpected flip
//
// Output formats: text (default), markdown, json.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// defaultConcurrency is the default worker-pool size for running go test.
const defaultConcurrency = 8

// defaultTimeout is the per-package go test timeout.
const defaultTimeout = 60 * time.Second

type options struct {
	subcommand        string
	skipRun           bool
	audit             bool
	format            string
	concurrency       int
	timeout           time.Duration
	repo              string
	out               string
	ledgerOut         string
	check             bool
	checkAllowNew     bool
	checkAgainstCache bool
	checkLedger       string
}

func main() {
	opts := parseFlags()
	// `--check` is the primary entry point. Subcommand `check` is an alias.
	if opts.check || opts.subcommand == "check" {
		if err := runCheck(opts); err != nil {
			fmt.Fprintln(os.Stderr, "check:", err)
			os.Exit(1)
		}
		return
	}
	switch opts.subcommand {
	case "":
		if err := run(opts); err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			os.Exit(1)
		}
	case "ledger":
		if err := runLedger(opts); err != nil {
			fmt.Fprintln(os.Stderr, "ledger:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "status: unknown subcommand %q\n", opts.subcommand)
		os.Exit(2)
	}
}

// subcommandAliases is the set of bare first-arg tokens that should be parsed
// as a subcommand instead of being swallowed by the flag package (which
// would otherwise stop parsing flags at the first non-flag token, breaking
// forms like `tools/status check --check-against-cache`).
var subcommandAliases = map[string]bool{
	"check":  true,
	"ledger": true,
}

func parseFlags() options {
	var o options
	// Peek at os.Args to detect a subcommand at the first position. If
	// found, strip it so the flag parser sees the remaining tokens. This
	// lets users write `tools/status check --check-against-cache` instead
	// of being forced to put flags first.
	rest := os.Args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && subcommandAliases[rest[0]] {
		o.subcommand = rest[0]
		rest = rest[1:]
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.BoolVar(&o.skipRun, "skip-run", false, "report cached results from tools/status/last_run.json instead of running go test")
	fs.BoolVar(&o.audit, "audit", false, "fail (exit 1) if any whole-file skip lacks a portplan/NA_EVIDENCE.md entry")
	fs.StringVar(&o.format, "format", "text", "output format: text, markdown, json")
	fs.IntVar(&o.concurrency, "concurrency", defaultConcurrency, "number of parallel go test workers")
	fs.DurationVar(&o.timeout, "timeout", defaultTimeout, "per-package go test timeout")
	fs.StringVar(&o.repo, "repo", "", "repository root (default: walk up from cwd until go.mod is found)")
	fs.StringVar(&o.out, "out", "", "also write the report to this file")
	// Check mode flags. `--check` is the primary trigger; the rest are modifiers.
	fs.BoolVar(&o.check, "check", false, "diff a live run (or cached last_run.json) against tools/status/ledger.json; exit non-zero on unexpected flip")
	fs.BoolVar(&o.checkAllowNew, "check-allow-new", false, "do not exit non-zero on packages missing from the ledger")
	fs.BoolVar(&o.checkAgainstCache, "check-against-cache", false, "diff against cached last_run.json instead of running a fresh live run")
	fs.StringVar(&o.checkLedger, "check-ledger", "", "path to ledger.json (default: tools/status/ledger.json)")
	// Ledger subcommand modifier.
	fs.StringVar(&o.ledgerOut, "ledger-out", "", "path to ledger.json output (default: tools/status/ledger.json)")
	if err := fs.Parse(rest); err != nil {
		os.Exit(2)
	}
	return o
}

// jsonDecode is the JSON decoder used by ledger.go / check.go / check_test.go.
// Tests inject failures via this variable to exercise error paths without
// rewriting the surrounding code.
var jsonDecode = func(b []byte, v interface{}) error {
	return json.Unmarshal(b, v)
}
