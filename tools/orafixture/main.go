// Command orafixture generates golden oracle fixtures for the unit
// conformance layer (UCL, portplan/UNIT_CONFORMANCE.md §U2).
//
// A *scenario* is a JSON file:
//
//	{
//	  "name": "fts4growth-x6",
//	  "pragmas": {"page_size": 1024},
//	  "sqlFiles": ["genesis_t1.sql"],
//	  "sql": ["CREATE VIRTUAL TABLE x6 USING fts4;", "..."]
//	}
//
// For every scenario the tool replays pragmas + sqlFiles + sql statements
// under the oracle CLI (/usr/bin/sqlite3), then copies the produced
// database beside the scenario as <name>.db and writes an ORACLE_VERSION
// marker per directory. Generation is deterministic (fixed pragmas, fixed
// SQL, no timestamps): running the tool twice must produce byte-identical
// fixtures (verify with -check).
//
// Usage:
//
//	go run ./tools/orafixture [-check] <scenario-file-or-dir>...
//
// The tool is topic-agnostic; FTS is its first user and later goals
// (JSON, WAL, P8) reuse it unchanged.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// oracleCLI is the behavioral ground truth (UCL rule U1).
const oracleCLI = "/usr/bin/sqlite3"

// Scenario mirrors portplan/UNIT_CONFORMANCE.md §4.
type Scenario struct {
	Name     string            `json:"name"`               // fixture base name (<name>.db)
	Pragmas  map[string]string `json:"pragmas,omitempty"`  // executed first, sorted by key
	SQLFiles []string          `json:"sqlFiles,omitempty"` // files appended verbatim before sql, relative to the scenario dir
	SQL      []string          `json:"sql,omitempty"`      // individual statements/commands
	// Backup, when true, additionally produces <name>-backup.db: the
	// oracle's own .backup output for the generated database. The src/dest
	// pair is the golden reference for page-level online-backup copy
	// semantics (backup.c).
	Backup bool `json:"backup,omitempty"`
	// EmptySource skips the SQL replay and commits a 0-byte <name>.db
	// (pager.c lazy creation: an opened-but-never-written database stays
	// empty); with Backup, <name>-backup.db is the oracle's rewrite of a
	// destination after backing up that empty source (backup.c NewDb
	// branch).
	EmptySource bool `json:"emptySource,omitempty"`
	// WalFiles additionally snapshots the live <name>.db-wal while the
	// oracle session is still open (a clean close auto-checkpoints and
	// truncates the WAL, so the snapshot must be taken mid-session).
	// Salt-1/Salt-2 are randomized per wal.c, so -check exempts -wal files
	// from byte comparison; structural equality is asserted by the
	// decoder-based conformance test (internal/pager/walview.go).
	WalFiles bool `json:"walFiles,omitempty"`
	// JournalFiles additionally snapshots the live <name>.db-journal
	// mid-session (journal_mode=PERSIST keeps the file after commit but
	// zeroes its header — pager.c pager_end_transaction PERSIST branch —
	// so only a mid-session copy preserves a decodable journal).
	// CksumInit is randomized per pager.c (writeJournalHdr), so -check
	// exempts -journal files from byte comparison for the same reason as
	// WalFiles.
	JournalFiles bool `json:"journalFiles,omitempty"`
}

func main() {
	check := flag.Bool("check", false, "regenerate into a temp dir and verify byte-identical fixtures instead of writing")
	flag.Parse()
	dirs := flag.Args()
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: orafixture [-check] <scenario-file-or-dir>...")
		os.Exit(2)
	}

	scenarios, err := collectScenarios(dirs)
	if err != nil {
		fatal(err)
	}
	if len(scenarios) == 0 {
		fatal(fmt.Errorf("no scenario JSON files found in %v", dirs))
	}

	outRoot := filepath.Join(os.TempDir(), "orafixture-out")
	os.RemoveAll(outRoot)
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		fatal(err)
	}

	for _, e := range scenarios {
		if err := generate(e, outRoot, dirs[0]); err != nil {
			fatal(fmt.Errorf("scenario %s: %w", e.scenario.Name, err))
		}
	}

	for _, dir := range uniqueDirs(scenarios) {
		rel := relPath(dirs[0], dir)
		if err := writeOracleVersion(dir, filepath.Join(outRoot, rel)); err != nil {
			fatal(err)
		}
	}

	if *check {
		if err := verifyIdentical(scenarios, outRoot, dirs[0]); err != nil {
			fatal(err)
		}
		fmt.Printf("OK: %d fixtures byte-identical\n", len(scenarios))
		return
	}
	if err := copyIntoRepos(scenarios, outRoot, dirs[0]); err != nil {
		fatal(err)
	}
	fmt.Printf("OK: wrote %d fixtures\n", len(scenarios))
}

// collectScenarios expands each argument (file or directory) into scenario
// entries paired with their source directory.
type entry struct {
	scenario Scenario
	dir      string // directory holding the JSON (and its sqlFiles/fixtures)
}

func collectScenarios(args []string) ([]entry, error) {
	var entries []entry
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			es, err := dirScenarioEntries(arg)
			if err != nil {
				return nil, err
			}
			entries = append(entries, es...)
			continue
		}
		e, err := loadScenarioFile(arg)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := checkUniqueNames(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// dirScenarioEntries loads all *.json scenarios of a directory (sorted).
func dirScenarioEntries(dir string) ([]entry, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		e, err := loadScenarioFile(f)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// checkUniqueNames rejects empty or duplicated scenario names.
func checkUniqueNames(entries []entry) error {
	names := map[string]bool{}
	for _, e := range entries {
		if e.scenario.Name == "" {
			return fmt.Errorf("scenario in %s: missing name", e.dir)
		}
		if names[e.scenario.Name] {
			return fmt.Errorf("duplicate scenario name %q", e.scenario.Name)
		}
		names[e.scenario.Name] = true
	}
	return nil
}

func loadScenarioFile(path string) (entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return entry{}, err
	}
	var sc Scenario
	if err := json.Unmarshal(data, &sc); err != nil {
		return entry{}, fmt.Errorf("%s: %w", path, err)
	}
	return entry{scenario: sc, dir: filepath.Dir(path)}, nil
}

// generate replays one scenario under the oracle CLI inside outRoot,
// producing <outRoot>/<relPath>/<name>.db.
func generate(e entry, outRoot, baseDir string) error {
	workDir := filepath.Join(outRoot, relPath(baseDir, e.dir))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	dbPath := filepath.Join(workDir, e.scenario.Name+".db")

	switch sidecars := liveSidecars(e); {
	case e.scenario.EmptySource:
		// No replay: the source fixture IS the 0-byte file (pager.c lazy
		// creation). The oracle never touches it.
		if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
			return err
		}
	case len(sidecars) > 0:
		if err := generateLiveSnapshot(e, dbPath, sidecars); err != nil {
			return err
		}
	default:
		if err := replayPlain(e, dbPath); err != nil {
			return err
		}
	}
	if e.scenario.Backup {
		if err := oracleBackup(dbPath, filepath.Join(workDir, e.scenario.Name+"-backup.db")); err != nil {
			return err
		}
	}
	return nil
}

// replayPlain replays the scenario in one shot and requires a non-empty db.
func replayPlain(e entry, dbPath string) error {
	script, err := buildScript(e)
	if err != nil {
		return err
	}
	cmd := exec.Command(oracleCLI, dbPath)
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oracle failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if fi, err := os.Stat(dbPath); err != nil || fi.Size() == 0 {
		return fmt.Errorf("oracle produced no database")
	}
	return nil
}

// oracleBackup runs the oracle's .backup INTO bakPath: the destination
// half of the src/dest fixture pair (page-level copy semantics reference,
// backup.c). .backup ?DB? FILE copies FROM the connection INTO FILE.
func oracleBackup(dbPath, bakPath string) error {
	os.Remove(bakPath)
	cmd := exec.Command(oracleCLI, dbPath, ".backup '"+bakPath+"'")
	var berr bytes.Buffer
	cmd.Stderr = &berr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oracle .backup failed: %v: %s", err, strings.TrimSpace(berr.String()))
	}
	return nil
}

// buildScript assembles the deterministic SQL program: pragmas (sorted),
// then sqlFiles contents, then inline sql.
func buildScript(e entry) (string, error) {
	var b strings.Builder
	// .bail on: any statement error must fail the run (no silent partial fixtures).
	b.WriteString(".bail on\n")
	keys := make([]string, 0, len(e.scenario.Pragmas))
	for k := range e.scenario.Pragmas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "PRAGMA %s=%s;\n", k, e.scenario.Pragmas[k])
	}
	for _, f := range e.scenario.SQLFiles {
		data, err := os.ReadFile(filepath.Join(e.dir, f))
		if err != nil {
			return "", fmt.Errorf("sqlFile %s: %w", f, err)
		}
		b.Write(data)
		b.WriteString("\n")
	}
	for _, s := range e.scenario.SQL {
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// writeOracleVersion records the oracle version per fixture directory
// (UCL rule 5) in both source tree and work copy.
func writeOracleVersion(srcDir, workDir string) error {
	out, err := exec.Command(oracleCLI, ":memory:", "SELECT sqlite_version();").Output()
	if err != nil {
		return fmt.Errorf("oracle version probe: %w", err)
	}
	version := strings.TrimSpace(string(out)) + "\n"
	for _, dir := range []string{srcDir, workDir} {
		if err := os.WriteFile(filepath.Join(dir, "ORACLE_VERSION"), []byte(version), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fixtureFiles lists the files orafixture owns for a scenario: the replayed
// database and, when requested, the oracle .backup output beside it.
func fixtureFiles(e entry) []string {
	names := []string{e.scenario.Name + ".db"}
	if e.scenario.Backup {
		names = append(names, e.scenario.Name+"-backup.db")
	}
	if e.scenario.WalFiles {
		names = append(names, e.scenario.Name+".db-wal")
	}
	if e.scenario.JournalFiles {
		names = append(names, e.scenario.Name+".db-journal")
	}
	return names
}

// byteStable reports whether a fixture is regenerated deterministically.
// WAL salts (wal.c) and journal cksumInit (pager.c writeJournalHdr) are
// randomized per file, so -wal/-journal fixtures are exempt from byte
// comparison; their structural equality is asserted by the decoder-based
// conformance tests (internal/pager walview.go/jrnlview.go).
func byteStable(name string) bool {
	return !strings.HasSuffix(name, "-wal") && !strings.HasSuffix(name, "-journal")
}

// verifyIdentical compares every generated fixture against its committed
// counterpart in the repository (DoD: regeneration is byte-identical).
func verifyIdentical(entries []entry, workRoot, baseDir string) error {
	for _, e := range entries {
		for _, f := range fixtureFiles(e) {
			if err := verifyFixture(e, f, workRoot, baseDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyFixture compares one regenerated fixture against the committed one.
func verifyFixture(e entry, f, workRoot, baseDir string) error {
	got, err := os.ReadFile(filepath.Join(workRoot, relPath(baseDir, e.dir), f))
	if err != nil {
		return fmt.Errorf("%s: %w", f, err)
	}
	want, err := os.ReadFile(filepath.Join(e.dir, f))
	if err != nil {
		return fmt.Errorf("%s: committed fixture missing (%w)", f, err)
	}
	if !byteStable(f) {
		// Randomized salts/nonce: presence + non-empty is all that
		// is byte-level checkable; structure is decoder-verified.
		if len(got) == 0 || len(want) == 0 {
			return fmt.Errorf("%s: regenerated fixture empty", f)
		}
		return nil
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s: regenerated fixture differs from committed one", f)
	}
	return nil
}

// copyIntoRepos copies generated fixtures back beside their scenarios.
func copyIntoRepos(entries []entry, workRoot, baseDir string) error {
	for _, e := range entries {
		for _, f := range fixtureFiles(e) {
			src := filepath.Join(workRoot, relPath(baseDir, e.dir), f)
			if err := copyFile(src, filepath.Join(e.dir, f)); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFile copies src to dst, closing both sides.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); err == nil {
			err = cerr
		}
	}()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

func uniqueDirs(entries []entry) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, e := range entries {
		if !seen[e.dir] {
			seen[e.dir] = true
			dirs = append(dirs, e.dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// relPath renders dir relative to base, falling back to the leaf name
// when dir is not under base.
func relPath(base, dir string) string {
	if rel, err := filepath.Rel(base, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return filepath.Base(dir)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "orafixture: %v\n", err)
	os.Exit(1)
}
