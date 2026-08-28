package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// cacheFile is the on-disk shape of tools/status/last_run.json.
type cacheFile struct {
	Version     int                   `json:"version"`
	GeneratedAt string                `json:"generated_at"`
	Concurrency int                   `json:"concurrency"`
	Timeout     string                `json:"timeout"`
	Packages    map[string]packageRun `json:"packages"`
}

// packageRun records the dynamic result of one testgen package.
type packageRun struct {
	State    string `json:"state"` // pass | fail | skipped
	Duration string `json:"duration,omitempty"`
	Error    string `json:"error,omitempty"`
}

// run is the top-level orchestration: load static info, run or load cache,
// render the report, and persist the cache after a real run.
func run(opts options) error {
	repo, err := findRepoRoot(opts.repo)
	if err != nil {
		return err
	}

	pkgs, generatedAt, err := loadStaticInfo(repo, opts)
	if err != nil {
		return err
	}

	report := buildReport(pkgs, generatedAt)

	if opts.audit {
		if err := auditSkipReasons(repo, filepath.Join(repo, "portplan", "NA_EVIDENCE.md"), pkgs); err != nil {
			return err
		}
	}

	out, err := renderReport(report, opts.format)
	if err != nil {
		return err
	}
	fmt.Print(out)
	if opts.out != "" {
		if err := os.WriteFile(opts.out, []byte(out), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", opts.out, err)
		}
	}
	return nil
}

// loadStaticInfo discovers packages and applies the run cache (or runs the
// packages when --skip-run is not set), returning the packages and the
// generated-at timestamp for the report.
func loadStaticInfo(repo string, opts options) ([]*pkgInfo, string, error) {
	skips, err := loadSkipMaps(repo)
	if err != nil {
		return nil, "", fmt.Errorf("parse skip maps: %w", err)
	}

	fams, err := loadFamilies(filepath.Join(repo, "tools", "status", "families.tsv"))
	if err != nil {
		return nil, "", fmt.Errorf("load families: %w", err)
	}

	pkgs, err := discoverPackages(filepath.Join(repo, "testgen"), skips, fams)
	if err != nil {
		return nil, "", fmt.Errorf("discover packages: %w", err)
	}

	cachePath := filepath.Join(repo, "tools", "status", "last_run.json")

	if opts.skipRun {
		generatedAt, err := loadOrSkipCache(cachePath, pkgs)
		return pkgs, generatedAt, err
	}

	results, err := runPackages(repo, pkgs, opts.concurrency, opts.timeout)
	if err != nil {
		return nil, "", fmt.Errorf("run packages: %w", err)
	}
	if err := saveCache(cachePath, results, opts); err != nil {
		return nil, "", fmt.Errorf("save cache: %w", err)
	}
	return pkgs, time.Now().UTC().Format(time.RFC3339), nil
}

// loadOrSkipCache applies the cached run results when --skip-run is set,
// returning the cache's generated-at time (or now when no cache exists).
func loadOrSkipCache(cachePath string, pkgs []*pkgInfo) (string, error) {
	cached, err := loadCache(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No cache yet: report static skips only; active packages
			// are "not-run". This keeps --skip-run usable on a fresh
			// checkout (and the G0.STATUS verify command).
			fmt.Fprintf(os.Stderr, "status: no last_run.json yet; active packages reported as not-run (run without --skip-run to populate it)\n")
			return time.Now().UTC().Format(time.RFC3339), nil
		}
		return "", fmt.Errorf("load cache: %w", err)
	}
	applyCache(pkgs, cached)
	return cached.GeneratedAt, nil
}

// renderReport renders the report in the requested format.
func renderReport(report *report, format string) (string, error) {
	switch format {
	case "text":
		return renderText(report), nil
	case "markdown":
		return renderMarkdown(report), nil
	case "json":
		return renderJSON(report), nil
	default:
		return "", fmt.Errorf("unknown format %q (want text|markdown|json)", format)
	}
}

// findRepoRoot locates the repository root by walking up from cwd until a
// directory containing go.mod is found. An explicit repo path is used
// verbatim.
func findRepoRoot(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
			return "", fmt.Errorf("--repo %s has no go.mod", explicit)
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found walking up from %s (use --repo)", dir)
		}
		dir = parent
	}
}

// loadCache reads the cached results file. A missing file returns
// os.ErrNotExist.
func loadCache(path string) (*cacheFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// applyCache overlays cached dynamic states onto the freshly-discovered
// package list, so static skip/family info stays current while pass/fail
// comes from the cached run.
func applyCache(pkgs []*pkgInfo, cached *cacheFile) {
	for _, p := range pkgs {
		if p.state == stateSkipped {
			continue // whole-file skip: never run, static state wins
		}
		if r, ok := cached.Packages[p.name]; ok {
			p.state = r.State
			p.duration = parseDuration(r.Duration)
			p.errTail = r.Error
		} else {
			p.state = stateNotRun
		}
	}
}

// saveCache writes the dynamic results of a real run.
func saveCache(path string, results []*pkgInfo, opts options) error {
	c := cacheFile{
		Version:     1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Concurrency: opts.concurrency,
		Timeout:     opts.timeout.String(),
		Packages:    make(map[string]packageRun, len(results)),
	}
	for _, p := range results {
		c.Packages[p.name] = packageRun{
			State:    p.state,
			Duration: formatDuration(p.duration),
			Error:    p.errTail,
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func parseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.Round(time.Millisecond).String()
}

// packageList sorts packages by name for deterministic output.
// packageList sorts packages by family then name for deterministic output.
func packageList(pkgs []*pkgInfo) []*pkgInfo {
	sorted := append([]*pkgInfo(nil), pkgs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].family != sorted[j].family {
			return sorted[i].family < sorted[j].family
		}
		return sorted[i].name < sorted[j].name
	})
	return sorted
}
