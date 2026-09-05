# tools/status — per-family testgen progress + GREEN-LEDGER anti-regression

The `status` tool (build: `go build ./tools/status/`) reports per-family
testgen progress for Frigolite and is the source of truth for the
GREEN-LEDGER anti-regression instrument (PORTPLAN §5g item 1).

## Subcommands

### (default) — status report

```
go run ./tools/status [--skip-run] [--audit] [--format text|markdown|json]
                      [--repo PATH] [--out PATH]
```

Classifies every `testgen/<pkg>` package into a feature family
(`tools/status/families.tsv`), then either runs `go test -tags testgen
./testgen/<pkg>/` for every active package (worker pool, default
concurrency 8) or, with `--skip-run`, reports the cached results from
`tools/status/last_run.json`. `--audit` enforces the
`portplan/NA_EVIDENCE.md` skip-evidence gate (§L).

### `ledger` — seed `tools/status/ledger.json`

```
go run ./tools/status ledger [--repo PATH] [--out PATH]
```

Reads the current `tools/status/last_run.json` baseline, parses
`PORTPLAN.md §4` goal-table rows + per-goal `plan/goals/<ID>.md` sub-plan
target-package lists, and writes the GREEN-LEDGER:
`tools/status/ledger.json`. Each package entry carries its expected
state (pass/fail/skipped), the owning goal ID, and the evidence pointer
(`plan/goals/<ID>.md` for passes; `portplan/NA_EVIDENCE.md §<ID>` for
skips; baseline stamp + drift-triage owner for fails).

Packages with duration ≥55s in the parallel baseline (i.e. the
60s/pkg timeout boundary) are flagged `stateTimeoutSuspect` and counted
in `timeout_suspects`. Per §5g item 6, those must be re-run serially
(`go test -tags testgen -count=1 -timeout 180s ./testgen/<pkg>/`) before
the ledger is trusted.

### `--check` — diff live run vs ledger

```
go run ./tools/status --check [--check-live]
                         [--check-allow-new] [--check-ledger PATH]
```

The anti-regression gate. By default, reads the cached `last_run.json` and
diffs it against `tools/status/ledger.json` (fast: <1s). Pass `--check-live`
to force a fresh `tools/status` suite (5–10 min) — useful at goal close when
the operator wants to confirm no flips since the last baseline.

In either mode, `--check` exits non-zero on any unexpected flip (pass→fail,
skipped→fail, fail→pass, or any missing package in the live run). The
flipped package name + states + owning goal are written to stderr.

Flags:

- `--check-live`: force a fresh `tools/status` suite instead of using the
  cached `last_run.json` (default: cache, fast).
- `--check-allow-new`: silently accept packages missing from the ledger
  (use after `go run ./tools/tcl2go` regenerates new test files; then
  re-seed the ledger with `tools/status ledger` to attribute them).
- `--check-ledger PATH`: use a non-default ledger file.

Exit codes:

- 0: no unexpected flips.
- 1: at least one flip; details on stderr.
- 2: ledger missing (refuses to compare against nothing).

### Recommended workflow

```bash
# After a fresh `tools/status` run (regenerated last_run.json):
go run ./tools/status ledger                            # seed ledger.json
go run ./tools/status --check                            # fast cache-mode diff (PASS expected)

# At every goal close (§5e item 4, §5g items 1, 2, 4, 7):
go run ./tools/status --check && \
  go test ./tools/status/ -count=1
# (Both default to <1s. The verify command above is the goal-close gate.)
```

## Files

| File | Purpose |
|------|---------|
| `main.go` | Subcommand dispatch + flag parsing. |
| `discover.go` | `testgen/<pkg>` static classification (family, skip counts). |
| `families.go` | `tools/status/families.tsv` glob→family loader. |
| `skips.go` | `tools/tcl2go/gen.go` skip-map parser (whole-file + per-test). |
| `runner.go` | Worker-pool `go test` runner (parallel/serial per-package). |
| `report.go` | Text/markdown/JSON report renderers. |
| `audit.go` | `--audit` skip-evidence gate. |
| `ledger.go` | `ledger` subcommand — seeds `ledger.json` from baseline + PORTPLAN §4. |
| `check.go` | `--check` mode — diff live run vs ledger, exit non-zero on flip. |
| `check_test.go` | 15 unit tests covering every flip class + ledger JSON validity. |
| `status_test.go` | Existing report/discovery/skip tests. |

## Dependencies

The tool reads (read-only):

- `tools/status/families.tsv` — family classification globs.
- `tools/status/last_run.json` — the most recent cached run.
- `PORTPLAN.md` (§4) — goal-table rows.
- `plan/goals/<ID>.md` — sub-plan target-package lists.
- `tools/tcl2go/{skiptestfiles,skiptests,skiptests2}.go` — skip-map sources.

It writes:

- `tools/status/ledger.json` — the GREEN-LEDGER (committed at
  `tools/status/ledger.json` since 2026-09-05).
- `tools/status/last_run.json` — refreshed on every default run.
- `tools/status/last_run_report.md` — refreshed with `--out PATH`.