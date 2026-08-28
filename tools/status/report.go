package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// buildReport aggregates per-package records into per-family summaries and
// overall totals.
func buildReport(pkgs []*pkgInfo, generatedAt string) *report {
	r := &report{packages: packageList(pkgs), generatedAt: generatedAt}

	byFamily := map[string][]*pkgInfo{}
	var familyNames []string
	for _, p := range pkgs {
		if _, ok := byFamily[p.family]; !ok {
			familyNames = append(familyNames, p.family)
		}
		byFamily[p.family] = append(byFamily[p.family], p)
	}
	sort.Strings(familyNames)

	for _, fam := range familyNames {
		fs := familySummary{family: fam}
		for _, p := range byFamily[fam] {
			fs.total++
			r.total++
			switch p.state {
			case statePass:
				fs.pass++
				r.totalPass++
			case stateFail:
				fs.fail++
				r.totalFail++
			case stateSkipped:
				fs.skip++
				r.totalSkip++
			default:
				fs.notRun++
				r.totalNotRun++
			}
		}
		if fs.total > 0 {
			fs.pct = float64(fs.pass) / float64(fs.total) * 100
		}
		r.families = append(r.families, fs)
	}
	return r
}

// renderText emits the default terminal table: a per-family summary, a
// total row, and one line per package with state and skip info.
func renderText(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Frigolite testgen status  (generated %s)\n", r.generatedAt)
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%-18s %6s %6s %6s %6s %7s\n", "FAMILY", "TOTAL", "PASS", "FAIL", "SKIP", "PCT"))
	b.WriteString(strings.Repeat("-", 58) + "\n")
	for _, fs := range r.families {
		fmt.Fprintf(&b, "%-18s %6d %6d %6d %6d %6.1f%%\n",
			fs.family, fs.total, fs.pass, fs.fail, fs.skip, fs.pct)
	}
	b.WriteString(strings.Repeat("-", 58) + "\n")
	fmt.Fprintf(&b, "%-18s %6d %6d %6d %6d %6.1f%%\n",
		"TOTAL", r.total, r.totalPass, r.totalFail, r.totalSkip,
		percent(r.totalPass, r.total))
	b.WriteString("\n")

	b.WriteString("PACKAGES\n")
	b.WriteString(fmt.Sprintf("%-18s %-14s %-9s %s\n", "PKG", "FAMILY", "STATE", "DETAIL"))
	b.WriteString(strings.Repeat("-", 80) + "\n")
	for _, p := range r.packages {
		detail := packageDetail(p)
		fmt.Fprintf(&b, "%-18s %-14s %-9s %s\n", p.name, p.family, p.state, detail)
	}
	return b.String()
}

// packageDetail renders the detail column for one package line.
func packageDetail(p *pkgInfo) string {
	var parts []string
	if p.testFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d files", p.testFiles))
	}
	if p.skipFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d whole-file skip", p.skipFiles))
	}
	if p.skipTests > 0 {
		parts = append(parts, fmt.Sprintf("%d tests skipped", p.skipTests))
	}
	if p.state == stateSkipped && p.reason != "" {
		return fmt.Sprintf("%s (%s)", strings.Join(parts, ", "), shortReason(p.reason))
	}
	if p.state == stateFail && p.errTail != "" {
		return strings.Join(parts, ", ") + " — " + shortReason(p.errTail)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// shortReason truncates a skip reason or error tail for the terminal table.
func shortReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

// renderMarkdown emits the same report as markdown tables for STATUS.md.
func renderMarkdown(r *report) string {
	var b strings.Builder
	b.WriteString("# Frigolite Testgen Status\n\n")
	fmt.Fprintf(&b, "_generated %s_\n\n", r.generatedAt)

	b.WriteString("## Per-family summary\n\n")
	b.WriteString("| Family | Total | Pass | Fail | Skip | Not-run | Pct |\n")
	b.WriteString("|---|---|---|---|---|---|---:|\n")
	for _, fs := range r.families {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %.1f%% |\n",
			fs.family, fs.total, fs.pass, fs.fail, fs.skip, fs.notRun, fs.pct)
	}
	fmt.Fprintf(&b, "| **TOTAL** | **%d** | **%d** | **%d** | **%d** | **%d** | **%.1f%%** |\n",
		r.total, r.totalPass, r.totalFail, r.totalSkip, r.totalNotRun, percent(r.totalPass, r.total))
	b.WriteString("\n")

	b.WriteString("## Packages\n\n")
	b.WriteString("| Pkg | Family | State | Detail |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, p := range r.packages {
		detail := packageDetail(p)
		detail = strings.ReplaceAll(detail, "|", `\|`)
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", p.name, p.family, p.state, detail)
	}
	return b.String()
}

// renderJSON emits the full report as a machine-readable JSON document.
func renderJSON(r *report) string {
	type pkgJSON struct {
		Name      string `json:"name"`
		Family    string `json:"family"`
		State     string `json:"state"`
		TestFiles int    `json:"test_files,omitempty"`
		SkipFiles int    `json:"skip_files,omitempty"`
		SkipTests int    `json:"skip_tests,omitempty"`
		Reason    string `json:"reason,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	type famJSON struct {
		Family string  `json:"family"`
		Total  int     `json:"total"`
		Pass   int     `json:"pass"`
		Fail   int     `json:"fail"`
		Skip   int     `json:"skip"`
		NotRun int     `json:"not_run,omitempty"`
		Pct    float64 `json:"pct"`
	}
	doc := struct {
		GeneratedAt string    `json:"generated_at"`
		Total       int       `json:"total"`
		Pass        int       `json:"pass"`
		Fail        int       `json:"fail"`
		Skip        int       `json:"skip"`
		NotRun      int       `json:"not_run"`
		Pct         float64   `json:"pct"`
		Families    []famJSON `json:"families"`
		Packages    []pkgJSON `json:"packages"`
	}{
		GeneratedAt: r.generatedAt,
		Total:       r.total,
		Pass:        r.totalPass,
		Fail:        r.totalFail,
		Skip:        r.totalSkip,
		NotRun:      r.totalNotRun,
		Pct:         percent(r.totalPass, r.total),
	}
	for _, fs := range r.families {
		doc.Families = append(doc.Families, famJSON{
			Family: fs.family, Total: fs.total, Pass: fs.pass,
			Fail: fs.fail, Skip: fs.skip, NotRun: fs.notRun, Pct: fs.pct,
		})
	}
	for _, p := range r.packages {
		doc.Packages = append(doc.Packages, pkgJSON{
			Name: p.name, Family: p.family, State: p.state,
			TestFiles: p.testFiles, SkipFiles: p.skipFiles, SkipTests: p.skipTests,
			Reason: p.reason, Error: p.errTail,
		})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Sprintf("{\"error\": %q}\n", err.Error())
	}
	return string(b) + "\n"
}

func percent(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}
