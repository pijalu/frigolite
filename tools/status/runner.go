package main

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// errTailLen is how much of go test's combined output to keep on failure,
// for the per-package detail report.
const errTailLen = 2000

// runPackages runs `go test -tags testgen -count=1 ./testgen/<pkg>/` for
// every non-skipped package, bounded by a worker pool of the given size.
// Each package gets opts.timeout via both the go test -timeout flag and an
// exec context.
func runPackages(repo string, pkgs []*pkgInfo, workers int, timeout time.Duration) ([]*pkgInfo, error) {
	var active []*pkgInfo
	for _, p := range pkgs {
		if p.state != stateSkipped {
			active = append(active, p)
		}
	}
	if workers <= 0 {
		workers = defaultConcurrency
	}

	// Heavyweight packages (multi-GB transient working sets on big FTS
	// merges: fts4merge4, fts5bigpl) run EXCLUSIVELY — one at a time, no
	// sibling — so two of them never peak together. Their Go heap transient
	// (GOGC doubling over a 2GB live merge) plus a concurrent worker's own
	// footprint pushed total memory to 9GB+ and made the machine unusable
	// during the run (2026-10-03); results were unaffected, the wall clock
	// and RAM were not. Serialize them through the same worker pool by
	// gating every other job behind a mutex held only for their duration.
	jobs := make(chan *pkgInfo)
	var wg sync.WaitGroup
	var heavyMu sync.Mutex
	heavy := heavyPackages()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if heavy[p.name] {
					heavyMu.Lock()
					runOne(repo, p, timeout)
					heavyMu.Unlock()
					continue
				}
				runOne(repo, p, timeout)
			}
		}()
	}
	for _, p := range active {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	return pkgs, nil
}

// heavyPackages names the census packages whose transient working set
// reaches multiple GB (large FTS merge corpora). They run one at a time.
func heavyPackages() map[string]bool {
	return map[string]bool{
		"fts4merge4": true,
		"fts5bigpl":  true,
	}
}

// runOne executes go test for a single package and records pass/fail,
// duration, and the output tail on failure.
func runOne(repo string, p *pkgInfo, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test",
		"-tags", "testgen",
		"-count=1",
		"-timeout", timeout.String(),
		"./testgen/"+p.name,
	)
	cmd.Dir = repo

	start := time.Now()
	out, err := cmd.CombinedOutput()
	p.duration = time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		p.state = stateFail
		p.errTail = "timeout: " + timeout.String()
		return
	}
	if err != nil {
		p.state = stateFail
		p.errTail = outputTail(out, errTailLen)
		return
	}
	p.state = statePass
}

// outputTail returns the last n bytes of a command's combined output as a
// trimmed string.
func outputTail(out []byte, n int) string {
	if len(out) > n {
		out = out[len(out)-n:]
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	return s
}
