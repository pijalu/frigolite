# tools/status — per-family testgen progress + GREEN-LEDGER anti-regression

The `status` tool (build: `go build ./tools/status/`) reports per-family
testgen progress for Frigolite and is the source of truth for the
GREEN-LEDGER anti-regression instrument (PORTPLAN §5g item 1).

## Parallel SQLite JSON harness shards

`TestSQLiteSuite` accepts optional `FRIGOLITE_SHARD_INDEX` and
`FRIGOLITE_SHARD_COUNT` environment variables. Set both to partition eligible
fixture-family basenames deterministically by observed complete family runtimes
where available, using largest-first greedy assignment to the currently lightest
shard. Timing evidence is partial: 15 complete family durations are currently
seeded from logs on shards 0, 1, and 3; shard 2 was interrupted before any family
summary. All other families are explicitly treated as unknown and fall back to
fixture bytes plus 128 bytes per case.
Same-basename alternate encodings stay together.
Existing `FRIGOLITE_TEST` filtering and slow/unsupported skips are applied
before assigning indices. With neither variable set, harness selection is
unchanged; set both to enable sharding. Index is zero-based and must be less
than positive count.

Build once, then run every shard concurrently in isolated working directories
(so fixture-created database files cannot collide). `tools/run_json_shards.sh`
uses Bash PID arrays, waits for every child, preserves logs and writes one exit
status file per shard:

```bash
bin="$(mktemp -d)"
go test -c -o "$bin/frigolite.test" .
bash tools/run_json_shards.sh "$bin/frigolite.test" 4
```

The binary exits nonzero if any selected family fails. Logs retain detailed per-family
outcomes in each shard log; choose shard count based on available memory, because
families have highly variable peak memory. Run this code from Bash (not zsh): the
PID and status arrays are Bash features. Every `wait` is collected before any status
file is written, including when one or more children fail.

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
60s/pkg timeout boundary) are flagged `stateTimeoutSuspect` and counted in
`timeout_suspects`. Per §5g item 6, those must be re-run serially
(`go test -tags testgen -count=1 -timeout 180s ./testgen/<pkg>/`) before the
ledger is trusted.

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
- `--check-allow-new`: silently accept packages missing from the ledger (use
after `go run ./tools/tcl2go` regenerates new test files; then re-seed ledger).
- `--check-ledger PATH`: use a non-default ledger file.

Exit codes: 0 means no unexpected flips; 1 means a flip; 2 means ledger missing.

### Recommended workflow

```bash
go run ./tools/status ledger
go run ./tools/status --check
go test ./tools/status/ -count=1
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
| `check.go` | `--check` mode — diff live run vs ledger. |
| `check_test.go` | Tests for flip classes + ledger JSON validity. |
| `status_test.go` | Existing report/discovery/skip tests. |

## Dependencies

The tool reads (read-only): `tools/status/families.tsv`, `tools/status/last_run.json`,
`PORTPLAN.md`, `plan/goals/<ID>.md`, and skip-map sources under `tools/tcl2go/`.
It writes `tools/status/ledger.json` and `tools/status/last_run.json`.
