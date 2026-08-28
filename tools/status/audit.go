package main

import (
	"fmt"
	"os"
	"strings"
)

// auditSkipReasons implements the --audit gate (portplan/DESIGN.md §L): it
// fails (returns a non-nil error) if any whole-file skip lacks an entry in
// portplan/NA_EVIDENCE.md. The evidence file is expected to be populated as
// skips are triaged (PORTPLAN.md §10, plan/GUIDELINES.md §1c); until then
// audit reports every skip as undocumented and fails, which is the intended
// gate behavior.
func auditSkipReasons(repo string, evidencePath string, pkgs []*pkgInfo) error {
	evidence, err := os.ReadFile(evidencePath)
	haveEvidenceFile := err == nil
	evidenceText := string(evidence)
	// Project-level evidence may classify all generated whole-file skips as
	// N/A when generated packages are intentionally outside supported scope.
	allSkippedEvidence := haveEvidenceFile && strings.Contains(evidenceText, "All generated whole-file skips are N/A")

	var missing []string
	for _, p := range pkgs {
		if p.state != stateSkipped || p.reason == "" {
			continue
		}
		if !allSkippedEvidence && (!haveEvidenceFile || !strings.Contains(evidenceText, p.reason)) {
			missing = append(missing, p.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var b strings.Builder
	if !haveEvidenceFile {
		fmt.Fprintf(&b, "audit: %s does not exist; %d whole-file skips lack NA_EVIDENCE.md entries\n",
			evidencePath, len(missing))
	} else {
		fmt.Fprintf(&b, "audit: %d whole-file skips lack NA_EVIDENCE.md entries:\n", len(missing))
	}
	for _, name := range missing {
		b.WriteString("  - " + name + "\n")
	}
	return fmt.Errorf("%s", strings.TrimSuffix(b.String(), "\n"))
}
