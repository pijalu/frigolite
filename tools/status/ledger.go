// Command ledger builds the GREEN-LEDGER (PORTPLAN §5g item 1): a
// machine-readable per-package expected-state seed derived from the most recent
// `tools/status/last_run.json` baseline PLUS the goal attribution parsed from
// PORTPLAN.md §4 markers and per-goal sub-plan target-package lists. It also
// surfaces any 60s-timeout-suspect FAIL packages so the operator can re-run
// them serially before the ledger is committed (the FAIL→PASS flips recorded
// here are the "expected" state at every later `--check`).
//
// Usage:
//
//	go run ./tools/status/ledger [-repo PATH] [-out PATH]
//
// The default output is `tools/status/ledger.json`. The default `--repo` is
// the directory containing go.mod (walked up from cwd).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ledgerFile is the on-disk shape of tools/status/ledger.json. It is the
// single source of truth for `tools/status --check`: each package's expected
// state plus its goal attribution (where available) and the evidence pointer
// (skip reason, NA_EVIDENCE link, or sub-plan filename).
type ledgerFile struct {
	Version          int                       `json:"version"`
	GeneratedAt      string                    `json:"generated_at"`
	BaselineStamp    string                    `json:"baseline_run_stamp"`
	BaselineTotal    int                       `json:"baseline_total"`
	BaselinePass     int                       `json:"baseline_pass"`
	BaselineFail     int                       `json:"baseline_fail"`
	BaselineSkip     int                       `json:"baseline_skip"`
	TimeoutSuspects  int                       `json:"timeout_suspects"`
	Packages         map[string]ledgerPackage  `json:"packages"`
	GoalAttributions map[string][]string       `json:"goal_attributions,omitempty"`
}

// ledgerPackage is the per-package entry: the expected state (pass/fail/skipped)
// plus the owning goal (e.g. "P6.FTS-A") and the evidence pointer that justifies
// the expected state. For passing packages the evidence is the baseline
// last_run.json stamp + the sub-plan filename; for skipped packages it is the
// skipTestFiles reason; for failing packages it is the last_run.json error tail
// + the goal that owns the drift-triage work.
type ledgerPackage struct {
	State    string `json:"state"`             // pass|fail|skipped
	Goal     string `json:"goal,omitempty"`    // P*.ID of the owning goal (or "" if OTHER)
	Evidence string `json:"evidence"`          // pointer: NA_EVIDENCE/§1 N-A/last_run stamp
	Source   string `json:"source,omitempty"`  // baseline last_run.json path
}

// goalTableRow captures one row of PORTPLAN.md §4: goal ID and the package
// list parsed out of the matching sub-plan file (`plan/goals/<ID>.md`,
// "## Target Packages (N)" section).
type goalTableRow struct {
	ID       string
	SubPlan  string
	Packages []string
}

// ledgerEntry mirrors tools/status/run.go's packageRun but is read-only.
type ledgerEntry struct {
	State    string `json:"state"`
	Duration string `json:"duration,omitempty"`
	Error    string `json:"error,omitempty"`
}

type ledgerRun struct {
	Version     int                   `json:"version"`
	GeneratedAt string                `json:"generated_at"`
	Packages    map[string]ledgerEntry `json:"packages"`
}

// timeoutThreshold is the floor for flagging a FAIL package as "timeout suspect"
// in the seed ledger (per §5g item 6). Anything that ran ≥55s in the parallel
// status run is treated as potentially a 60s timeout, not a real failure; the
// seed surfaces those by listing them under `timeout_suspects` so the operator
// re-runs them serially and decides whether to amend the ledger.
const timeoutThreshold = 55 * time.Second

// targetPackagesRE matches the "## Target Packages (N)" heading of a sub-plan.
var targetPackagesRE = regexp.MustCompile(`(?m)^## Target Packages\s*\(\d+\)\s*$`)

// packageListItemRE matches one `- `pkg`` line in a sub-plan target list. The
// optional `(pass)` / `(fail)` annotation is captured but ignored for the
// seed ledger (the live state comes from last_run.json, the sub-plan is only
// used for goal attribution).
var packageListItemRE = regexp.MustCompile(`(?m)^-\s+` + "`" + `([A-Za-z0-9_]+)` + "`" + `(?:[^*]*\((?:pass|fail)\))?`)

// runLedger is the entry point for `tools/status ledger`. It prints the path
// of the written ledger (or exits non-zero on error) and is wired up from
// main.go so the same binary can both report and seed.
func runLedger(opts options) error {
	repo, err := findRepoRoot(opts.repo)
	if err != nil {
		return err
	}

	cachePath := filepath.Join(repo, "tools", "status", "last_run.json")
	baseline, err := readLedgerRun(cachePath)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}

	goals, err := loadGoalTableRows(repo)
	if err != nil {
		return fmt.Errorf("load goal table rows: %w", err)
	}

	pkg2goal := buildPackageGoalIndex(goals)
	pkgs, timeoutCount := ledgerPackagesFromBaseline(baseline, cachePath, pkg2goal)
	pass, fail, skip := countLedgerStates(pkgs)

	lf := ledgerFile{
		Version:          1,
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		BaselineStamp:    baseline.GeneratedAt,
		BaselineTotal:    len(baseline.Packages),
		BaselinePass:     pass,
		BaselineFail:     fail,
		BaselineSkip:     skip,
		TimeoutSuspects:  timeoutCount,
		Packages:         pkgs,
		GoalAttributions: buildGoalAttributions(goals),
	}

	return writeLedger(opts.ledgerOut, repo, &lf, timeoutCount)
}

// buildPackageGoalIndex inverts the goal → [pkg] mapping into a pkg → goal
// index. First goal wins (sub-plan files are non-overlapping by design; this
// guards against the unlikely case of a duplicate mention).
func buildPackageGoalIndex(goals []goalTableRow) map[string]string {
	idx := map[string]string{}
	for _, g := range goals {
		for _, p := range g.Packages {
			if _, ok := idx[p]; !ok {
				idx[p] = g.ID
			}
		}
	}
	return idx
}

// ledgerPackagesFromBaseline turns each baseline packageRun into a
// ledgerPackage, flagging any 60s-timeout-suspect FAIL packages via the
// stateTimeoutSuspect placeholder (which --check ignores).
func ledgerPackagesFromBaseline(baseline *ledgerRun, cachePath string, pkg2goal map[string]string) (map[string]ledgerPackage, int) {
	out := map[string]ledgerPackage{}
	timeoutCount := 0
	for name, e := range baseline.Packages {
		lp := ledgerPackage{
			State:  e.State,
			Goal:   pkg2goal[name],
			Source: cachePath,
		}
		lp.Evidence = ledgerEvidence(lp.State, baseline.GeneratedAt, pkg2goal[name], e)
		if lp.State == stateFail && isTimeoutSuspect(e.Duration) {
			timeoutCount++
			lp.State = stateTimeoutSuspect
		}
		out[name] = lp
	}
	return out, timeoutCount
}

// ledgerEvidence renders the human-readable evidence pointer for a ledger
// package. The state determines which template applies; the goal ID flows
// through to the sub-plan / NA_EVIDENCE section anchor.
func ledgerEvidence(state, stamp, goal string, e ledgerEntry) string {
	switch state {
	case statePass:
		return fmt.Sprintf("baseline %s pass (last_run.json); sub-plan %s",
			stamp, subPlanForGoal(goal))
	case stateSkipped:
		return "skipTestFiles reason; see portplan/NA_EVIDENCE.md §" +
			sectionForGoal(goal)
	case stateFail:
		return fmt.Sprintf("baseline %s FAIL; drift-triage owner %s; tail: %s",
			stamp, ownerFor(goal), trim(e.Error, 160))
	default:
		return ""
	}
}

// countLedgerStates tallies the pass/fail/skip totals across the ledger.
func countLedgerStates(pkgs map[string]ledgerPackage) (pass, fail, skip int) {
	for _, p := range pkgs {
		switch p.State {
		case statePass:
			pass++
		case stateFail:
			fail++
		case stateSkipped:
			skip++
		}
	}
	return
}

// buildGoalAttributions is the goal → sorted [pkg] mapping embedded in the
// ledger so consumers don't need to re-parse PORTPLAN.md.
func buildGoalAttributions(goals []goalTableRow) map[string][]string {
	out := map[string][]string{}
	for _, g := range goals {
		sorted := append([]string(nil), g.Packages...)
		sort.Strings(sorted)
		out[g.ID] = sorted
	}
	return out
}

// writeLedger serializes lf to disk and prints a one-line summary. If
// outPath is empty, the default tools/status/ledger.json is used.
func writeLedger(outPath, repo string, lf *ledgerFile, timeoutCount int) error {
	if outPath == "" {
		outPath = filepath.Join(repo, "tools", "status", "ledger.json")
	}
	b, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ledger: %w", err)
	}
	if err := os.WriteFile(outPath, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("ledger: wrote %d packages (%d pass, %d fail, %d skip, %d timeout-suspect) to %s\n",
		len(lf.Packages), lf.BaselinePass, lf.BaselineFail, lf.BaselineSkip, timeoutCount, outPath)
	if timeoutCount > 0 {
		fmt.Fprintf(os.Stderr,
			"ledger: %d FAIL packages with duration >= 55s — re-run them serially before trusting the ledger\n",
			timeoutCount)
	}
	return nil
}

// readLedgerRun loads tools/status/last_run.json in a way that is independent
// of tools/status/run.go's cacheFile (which is unexported). We only need the
// per-package state/duration/error and the generated_at stamp.
func readLedgerRun(path string) (*ledgerRun, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lr ledgerRun
	if err := json.Unmarshal(b, &lr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &lr, nil
}

// isTimeoutSuspect reports whether a FAIL package's duration in the parallel
// baseline run is close enough to the 60s/pkg timeout that the FAIL is
// ambiguous — it might have passed given more time, or it might have a real
// hang/bug. Per §5g item 6, those must be re-run serially before being
// counted red. We accept any duration ≥ 55s.
func isTimeoutSuspect(dur string) bool {
	if dur == "" {
		return false
	}
	d, err := time.ParseDuration(dur)
	if err != nil {
		return false
	}
	return d >= timeoutThreshold
}

// trim truncates s to max bytes for a compact ledger evidence field.
func trim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max-3] + "..."
	}
	return s
}

// subPlanForGoal returns the relative path of the sub-plan file for a goal ID
// (or "—" when the package is unattributed drift owned by FULL-SUITE-DRIFT).
func subPlanForGoal(id string) string {
	if id == "" {
		return "FULL-SUITE-DRIFT"
	}
	return fmt.Sprintf("plan/goals/%s.md", id)
}

// sectionForGoal returns the NA_EVIDENCE.md section anchor for a goal ID.
func sectionForGoal(id string) string {
	if id == "" {
		return "FULL-SUITE-DRIFT"
	}
	return id
}

// ownerFor returns the goal that owns the drift-triage work for a FAIL package
// not already attributed to a sub-plan.
func ownerFor(id string) string {
	if id == "" {
		return "FULL-SUITE-DRIFT"
	}
	return id
}

// loadGoalTableRows parses PORTPLAN.md §4 to enumerate every goal ID, then for
// each one parses the matching plan/goals/<ID>.md sub-plan to extract the
// "## Target Packages (N)" list. The §4 row is the authority on goal
// existence; the sub-plan is the authority on the package list (the §4 row
// only counts them, never enumerates).
func loadGoalTableRows(repo string) ([]goalTableRow, error) {
	portplanPath := filepath.Join(repo, "PORTPLAN.md")
	planBody, err := os.ReadFile(portplanPath)
	if err != nil {
		return nil, err
	}

	// Find the §4 section and stop at the next top-level (## / #) heading so
	// we don't pick up §5a queue items like "GREEN-LEDGER" that don't have
	// sub-plans.
	start := strings.Index(string(planBody), "## 4. Complete Goal Index")
	if start < 0 {
		return nil, fmt.Errorf("PORTPLAN.md §4 not found")
	}
	rest := string(planBody[start:])
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		end = len(rest)
	}
	section := rest[:end]

	// Goal-table row pattern: `| \`P1.CRUD\` | [`P1.CRUD.md`](plan/goals/P1.CRUD.md) | N | ...`
	rowRE := regexp.MustCompile(
			"`([A-Z][0-9]?(?:\\.[A-Z0-9_-]+)?)`\\s*\\|\\s*\\[`([^`]+)`\\]\\(plan/goals/([^)]+)\\)",
		)
	matches := rowRE.FindAllStringSubmatch(section, -1)

	seen := map[string]bool{}
	var rows []goalTableRow
	for _, m := range matches {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		subPath := filepath.Join(repo, "plan", "goals", m[3])
		pkgs, err := readTargetPackages(subPath)
		if err != nil {
			// Sub-plan missing — likely a queued goal with no body yet
			// (e.g. P6.FTS5 before its sub-plan is written). Skip
			// silently; the goal attribution will simply be empty for
			// any packages that fall in that bucket.
			continue
		}
		rows = append(rows, goalTableRow{ID: id, SubPlan: subPath, Packages: pkgs})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// readTargetPackages parses the "## Target Packages (N)" list out of a
// sub-plan file. Returns an empty slice if the section is absent.
func readTargetPackages(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	body := string(b)
	loc := targetPackagesRE.FindStringIndex(body)
	if loc == nil {
		return nil, nil
	}
	rest := body[loc[1]:]
	// Stop at the next ## heading.
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	matches := packageListItemRE.FindAllStringSubmatch(rest, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out, nil
}