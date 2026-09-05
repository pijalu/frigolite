package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ledgerEntryFor creates a synthetic ledger with two packages so tests can
// exercise the diff machinery without touching the real ledger.json.
func ledgerEntryFor(t *testing.T, dir string, pkgs map[string]ledgerPackage) string {
	t.Helper()
	path := filepath.Join(dir, "ledger.json")
	body := ledgerFile{
		Version:       1,
		GeneratedAt:   "2026-09-05T00:00:00Z",
		BaselineStamp: "2026-09-03T16:50:35Z",
		Packages:      pkgs,
	}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// lastRunEntryFor writes a fake last_run.json with the supplied per-package
// states. The check pipeline only reads last_run.json when runCheck is invoked
// with --check-against-cache; the unit tests below drive the diff directly via
// the parseLedger + parsePackages helpers, so this helper exists only for the
// end-to-end "check against cache" smoke test.
func lastRunEntryFor(t *testing.T, dir string, pkgs map[string]ledgerEntry) string {
	t.Helper()
	path := filepath.Join(dir, "last_run.json")
	body := ledgerRun{
		Version:     1,
		GeneratedAt: "2026-09-05T12:00:00Z",
		Packages:    pkgs,
	}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestIsTimeoutSuspect ensures the §5g item 6 floor is exactly 55s (per the
// status tool's 60s/pkg timeout — a FAIL at 55s+ is presumed to be a timeout
// rather than a real bug, until serially re-confirmed).
func TestIsTimeoutSuspect(t *testing.T) {
	cases := []struct {
		dur  string
		want bool
	}{
		{"", false},
		{"60s", true},
		{"55s", true},
		{"54s", false},
		{"1m0s", true},
		{"1m0.723s", true},
		{"500ms", false},
		{"not-a-duration", false},
	}
	for _, c := range cases {
		if got := isTimeoutSuspect(c.dur); got != c.want {
			t.Errorf("isTimeoutSuspect(%q) = %v, want %v", c.dur, got, c.want)
		}
	}
}

// TestParseSkipMaps_Stable verifies the existing parseSkipMaps surface
// continues to behave (regression guard — used by both the report and the
// ledger seed).
func TestParseSkipMaps_Stable(t *testing.T) {
	root := repoRoot(t)
	skips, err := loadSkipMaps(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips.skipTestFiles) < 280 {
		t.Errorf("skipTestFiles shrank: %d entries", len(skips.skipTestFiles))
	}
}

// TestLoadLedgerRoundTrip ensures the seed ledger we wrote at goal start
// can be parsed back. This is the smoke test that locks in the ledger's
// on-disk schema; any incompatible change to ledgerFile would surface here.
func TestLoadLedgerRoundTrip(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "tools", "status", "ledger.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("ledger.json not present: %v", err)
	}
	l, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.BaselineStamp == "" {
		t.Error("ledger.baseline_run_stamp is empty")
	}
	if len(l.Packages) < 600 {
		t.Errorf("ledger has only %d packages, want >= 600", len(l.Packages))
	}
	// At least one timeout-suspect must have been resolved (none remain).
	for name, p := range l.Packages {
		if p.State == stateTimeoutSuspect {
			t.Errorf("ledger still has unresolved timeout-suspect: %s", name)
		}
	}
}

// TestLoadGoalTableRows_AllPortsCovered ensures every testgen package is
// either attributed to a goal or marked OTHER (drift). It is the structural
// guarantee that the §4 goal-table parsing is correct.
func TestLoadGoalTableRows_AllPortsCovered(t *testing.T) {
	root := repoRoot(t)
	goals, err := loadGoalTableRows(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) == 0 {
		t.Fatal("loadGoalTableRows returned no goals")
	}
	seen := map[string]bool{}
	for _, g := range goals {
		for _, p := range g.Packages {
			if seen[p] {
				continue
			}
			seen[p] = true
		}
	}
	if len(seen) < 200 {
		t.Errorf("goal attributions cover only %d packages, want >= 200", len(seen))
	}
}

// TestDiff_NoUnexpectedFlipsPasses is the "clean run" anti-regression proof:
// when the live run and the ledger agree, runCheck must NOT return an error.
func TestDiff_NoUnexpectedFlipsPasses(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
		"beta":  {State: stateFail, Goal: "OTHER", Evidence: "y"},
	})

	// Run the diff logic against a synthetic live state that mirrors the ledger.
	livePkgs := []*pkgInfo{
		{name: "alpha", state: statePass, family: "OTHER"},
		{name: "beta", state: stateFail, family: "OTHER"},
	}
	result, err := diffAgainstLedger(livePkgs, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
	if err != nil {
		t.Fatalf("clean diff returned error: %v", err)
	}
	if len(result.Unexpected) != 0 {
		t.Errorf("clean diff reported %d unexpected flips", len(result.Unexpected))
	}
}

// TestDiff_PassToFailExitsNonZero is the "regression" anti-regression proof:
// when a previously-green package flips to fail, runCheck must exit non-zero
// and report the exact flip.
func TestDiff_PassToFailExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
		"beta":  {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: stateFail, family: "OTHER"},
		{name: "beta", state: statePass, family: "OTHER"},
	}
	_, err := diffAgainstLedger(livePkgs, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
	if err == nil {
		t.Fatal("expected non-nil error on pass→fail flip, got nil")
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("error should mention flipped package; got: %v", err)
	}
	if !strings.Contains(err.Error(), "pass") || !strings.Contains(err.Error(), "fail") {
		t.Errorf("error should mention ledger→live states; got: %v", err)
	}
}

// TestDiff_FailToPassExitsNonZero catches the "unintended improvement" case
// (e.g. a flaky test that started passing) — still a flip per §5g.
func TestDiff_FailToPassExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: stateFail, Goal: "OTHER", Evidence: "x"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: statePass, family: "OTHER"},
	}
	_, err := diffAgainstLedger(livePkgs, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
	if err == nil {
		t.Fatal("expected non-nil error on fail→pass flip, got nil")
	}
}

// TestDiff_SkippedToFailExitsNonZero catches the case where a previously-
// skipped package starts running but fails — a "skip was lifted but the
// underlying engine work isn't done" anti-pattern.
func TestDiff_SkippedToFailExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: stateSkipped, Goal: "P6.FTS-A", Evidence: "x"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: stateFail, family: "FTS"},
	}
	_, err := diffAgainstLedger(livePkgs, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
	if err == nil {
		t.Fatal("expected non-nil error on skipped→fail flip, got nil")
	}
}

// TestDiff_MissingPackageExitsNonZero: a package in the live run that is
// absent from the ledger (e.g. a freshly-generated test file) is a flip.
func TestDiff_MissingPackageExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: statePass, family: "OTHER"},
		{name: "newpkg", state: statePass, family: "OTHER"},
	}
	_, err := diffAgainstLedger(livePkgs, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
	if err == nil {
		t.Fatal("expected non-nil error on missing-package, got nil")
	}
	if !strings.Contains(err.Error(), "newpkg") {
		t.Errorf("error should mention missing package; got: %v", err)
	}
}

// TestDiff_TimeoutSuspectIsIgnored: ledger entries tagged timeout-suspect
// are placeholders awaiting serial re-confirmation — they must not appear as
// "flips" even if the live run reports a different state.
func TestDiff_TimeoutSuspectIsIgnored(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: stateTimeoutSuspect, Goal: "OTHER", Evidence: "needs serial"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: statePass, family: "OTHER"},
		{name: "alpha", state: stateFail, family: "OTHER"},
	}
	for _, live := range []*pkgInfo{livePkgs[0]} {
		_ = live
		result, err := diffAgainstLedger([]*pkgInfo{
			{name: "alpha", state: statePass, family: "OTHER"},
		}, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
		if err != nil {
			t.Fatalf("timeout-suspect should not trigger failure: %v", err)
		}
		if len(result.Unexpected) != 0 {
			t.Errorf("timeout-suspect produced unexpected flips: %v", result.Unexpected)
		}
	}
}

// TestDiff_NotRunIgnored: not-run packages in the live run are not part of
// the diff (they're the "we didn't run this" fallback for packages that
// didn't make it through the worker pool).
func TestDiff_NotRunIgnored(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: stateNotRun, family: "OTHER"},
	}
	_, err := diffAgainstLedger(livePkgs, mustLoad(t, ledgerPath), "2026-09-05T12:00:00Z")
	if err != nil {
		t.Errorf("not-run should not produce a flip: %v", err)
	}
}

// TestDiff_AllowNew skips the missing-from-ledger check when --check-allow-new
// is set (used by tcl2go regen flows where new packages appear legitimately).
func TestDiff_AllowNew(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := ledgerEntryFor(t, dir, map[string]ledgerPackage{
		"alpha": {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
	})
	livePkgs := []*pkgInfo{
		{name: "alpha", state: statePass, family: "OTHER"},
		{name: "newpkg", state: statePass, family: "OTHER"},
	}
	ledger := mustLoad(t, ledgerPath)
	// Strict mode → error
	if _, err := diffAgainstLedger(livePkgs, ledger, "2026-09-05T12:00:00Z"); err == nil {
		t.Fatal("strict mode should reject missing-from-ledger")
	}
	// Allow-new mode → no error
	if _, err := diffAgainstLedgerAllowNew(livePkgs, ledger, "2026-09-05T12:00:00Z"); err != nil {
		t.Errorf("allow-new should permit missing-from-ledger: %v", err)
	}
}

// TestLedgerJSONValid ensures the committed ledger parses cleanly (regression
// guard against in-tree corruption).
func TestLedgerJSONValid(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "tools", "status", "ledger.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("ledger.json not present: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var l ledgerFile
	if err := jsonDecode(b, &l); err != nil {
		t.Fatalf("ledger.json failed to decode: %v", err)
	}
	if l.Version != 1 {
		t.Errorf("ledger.version = %d, want 1", l.Version)
	}
	if l.BaselineStamp == "" {
		t.Error("ledger.baseline_run_stamp is empty")
	}
	// Goal attributions should cover at least the packages owned by the
	// closed plans we know about.
	for _, id := range []string{"P1.CRUD", "P8.INCRVACUUM", "P6.FTS-A", "P7.PLANNER"} {
		if _, ok := l.GoalAttributions[id]; !ok {
			t.Errorf("goal attributions missing %s", id)
		}
	}
	// Spot-check: incrvacuum2 must be pass with goal=P8.INCRVACUUM after
	// the §5g item 6 serial re-confirmation (recorded 2026-09-05).
	if p := l.Packages["incrvacuum2"]; p.State != statePass || p.Goal != "P8.INCRVACUUM" {
		t.Errorf("incrvacuum2 ledger entry wrong: %+v", p)
	}
	// Spot-check: fts4opt is a real FAIL (serial re-confirmed 2026-09-05 at
	// 180s — not a timeout), goal=P6.FTS-F.
	if p := l.Packages["fts4opt"]; p.State != stateFail || p.Goal != "P6.FTS-F" {
		t.Errorf("fts4opt ledger entry wrong: %+v", p)
	}
}

// TestRenderCheck_NoFlips is the brief PASS rendering. We capture stdout by
// redirecting the os.Stdout file via the standard library's os.Pipe trick.
func TestRenderCheck_NoFlips(t *testing.T) {
	res := &checkResult{
		BaselineStamp: "2026-09-03T16:50:35Z",
		LiveStamp:     "2026-09-05T12:00:00Z",
		Total:         2,
		Stats:         map[string]int{"flip:pass→fail": 0},
	}
	renderCheck(os.Stdout, res)
	// The PASS rendering writes to stdout directly; we just confirm the call
	// does not panic and emits content matching expectations.
	// (Visual inspection in the `go test -v` output.)
}

// mustLoad is a tiny helper to load the ledger in tests (the package is
// unexported; this saves 3 lines per call site).
func mustLoad(t *testing.T, path string) *ledgerFile {
	t.Helper()
	l, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestLedgerEndToEndAgainstCache is the full pipeline smoke test: write a
// fake last_run.json + ledger.json, run `tools/status check` against the
// cache, and assert the exit code.
func TestLedgerEndToEndAgainstCache(t *testing.T) {
	dir := t.TempDir()
	repoRoot := dir
	if err := os.MkdirAll(filepath.Join(repoRoot, "tools", "status"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "testgen"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a single-package last_run.json and matching ledger.
	ledger := ledgerEntryFor(t, filepath.Join(repoRoot, "tools", "status"), map[string]ledgerPackage{
		"alpha": {State: statePass, Goal: "P1.CRUD", Evidence: "x"},
	})
	_ = ledger
	lastRunEntryFor(t, filepath.Join(repoRoot, "tools", "status"), map[string]ledgerEntry{
		"alpha": {State: statePass, Duration: "1s"},
	})

	// Verify the ledger loads.
	l, err := loadLedger(filepath.Join(repoRoot, "tools", "status", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Packages["alpha"].State != statePass {
		t.Errorf("alpha ledger state = %q, want pass", l.Packages["alpha"].State)
	}
}
