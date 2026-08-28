package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot returns the repository root, which is two levels above this
// package (tools/status → tools → repo).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(dir))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// TestFamiliesCoverEveryTestgenPackage validates the family classification:
// every testgen/ package must map to a non-empty family, and the patterns
// must all be valid globs.
func TestFamiliesCoverEveryTestgenPackage(t *testing.T) {
	root := repoRoot(t)
	fams, err := loadFamilies(filepath.Join(root, "tools", "status", "families.tsv"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "testgen"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		count++
		fam := fams.classify(e.Name())
		if fam == "" {
			t.Errorf("testgen/%s maps to empty family", e.Name())
		}
	}
	if count == 0 {
		t.Fatal("no testgen packages found")
	}
	t.Logf("classified %d testgen packages", count)
}

// TestFamilyPatternsAreValid ensures every glob in families.tsv compiles.
func TestFamilyPatternsAreValid(t *testing.T) {
	root := repoRoot(t)
	fams, err := loadFamilies(filepath.Join(root, "tools", "status", "families.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range fams.patterns {
		if _, err := filepath.Match(p.pattern, "probe"); err != nil {
			t.Errorf("invalid glob %q: %v", p.pattern, err)
		}
	}
}

// TestParseSkipMaps reads the real skip map files and sanity-checks their
// contents.
func TestParseSkipMaps(t *testing.T) {
	root := repoRoot(t)
	skips, err := loadSkipMaps(root)
	if err != nil {
		t.Fatal(err)
	}
	// Baseline floor (not an exact count): packages get un-skipped as goals
	// complete, so the total only shrinks. 384 entries at the P6.FTS-WPORT
	// checkpoint; 336 after the P6.VTAB task12 mass un-skip (30 vtab harness
	// packages + later fts3/jsonb work); 327 after P7.LOCK-A un-skipped
	// lock,lock2-7,nolock,shmlock,superlock. Lower the floor only alongside
	// documented un-skip work.
	if len(skips.skipTestFiles) < 327 {
		t.Errorf("skipTestFiles has %d entries, want >= 327", len(skips.skipTestFiles))
	}
	if len(skips.skipTests) < 600 {
		t.Errorf("skipTests has %d entries, want >= 600", len(skips.skipTests))
	}
	// A couple of spot checks from the documented entries.
	for _, key := range []string{"alter2", "auth", "savepoint4", "vtab7"} {
		if _, ok := skips.skipTestFiles[key]; !ok {
			t.Logf("skipTestFiles has no %q (may be un-skipped)", key)
		}
	}
}

// TestAuditGate verifies the --audit gate in both directions: with the real
// portplan/NA_EVIDENCE.md (populated since S5.9) every whole-file skip reason
// is documented and audit succeeds; with an absent evidence file every skip
// is undocumented and audit fails.
func TestAuditGate(t *testing.T) {
	root := repoRoot(t)
	skips, err := parseSkipMaps(filepath.Join(root, "tools", "tcl2go", "gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	fams, err := loadFamilies(filepath.Join(root, "tools", "status", "families.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := discoverPackages(filepath.Join(root, "testgen"), skips, fams)
	if err != nil {
		t.Fatal(err)
	}

	evidencePath := filepath.Join(root, "portplan", "NA_EVIDENCE.md")
	if _, statErr := os.Stat(evidencePath); os.IsNotExist(statErr) {
		t.Skipf("%s is absent; nothing to verify", evidencePath)
	}
	// Success path: every skip reason is documented in the evidence file.
	if err := auditSkipReasons(root, evidencePath, pkgs); err != nil {
		t.Errorf("audit should pass with NA_EVIDENCE.md present, got: %v", err)
	}
	// Failure path: an absent evidence file must report undocumented skips.
	missingPath := filepath.Join(t.TempDir(), "NA_EVIDENCE.md")
	err = auditSkipReasons(root, missingPath, pkgs)
	if err == nil {
		t.Fatal("audit should fail when NA_EVIDENCE.md is absent")
	}
	if !strings.Contains(err.Error(), "skip") {
		t.Errorf("audit error should mention skips, got: %v", err)
	}
}

// TestPackageDetailSanity checks the detail rendering for representative
// package states.
func TestPackageDetailSanity(t *testing.T) {
	cases := []struct {
		p    *pkgInfo
		want string
	}{
		{&pkgInfo{name: "x", state: statePass, testFiles: 1}, "1 files"},
		{&pkgInfo{name: "x", state: stateSkipped, testFiles: 2, skipFiles: 2, reason: "not implemented"}, "2 files, 2 whole-file skip (not implemented)"},
		{&pkgInfo{name: "x", state: stateFail, testFiles: 1, errTail: "boom"}, "1 files — boom"},
	}
	for _, c := range cases {
		if got := packageDetail(c.p); got != c.want {
			t.Errorf("packageDetail(%s) = %q, want %q", c.p.name, got, c.want)
		}
	}
}

// TestDiscoverPackages runs discovery over the real testgen tree and checks
// the invariants the report depends on.
func TestDiscoverPackages(t *testing.T) {
	root := repoRoot(t)
	skips, err := parseSkipMaps(filepath.Join(root, "tools", "tcl2go", "gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	fams, err := loadFamilies(filepath.Join(root, "tools", "status", "families.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := discoverPackages(filepath.Join(root, "testgen"), skips, fams)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) < 600 {
		t.Errorf("discovered %d packages, want >= 600", len(pkgs))
	}
	var skipped, active int
	for _, p := range pkgs {
		if p.family == "" {
			t.Errorf("package %s has empty family", p.name)
		}
		if p.state == stateSkipped {
			skipped++
		} else {
			active++
		}
		if p.testFiles == 0 {
			t.Errorf("package %s has no test files", p.name)
		}
		if p.state == stateSkipped && p.reason == "" {
			t.Errorf("package %s is skipped but has no reason", p.name)
		}
	}
	if active == 0 {
		t.Error("no active packages found")
	}
	t.Logf("packages: %d total, %d skipped, %d active", len(pkgs), skipped, active)
}
