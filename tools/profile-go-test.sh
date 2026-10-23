#!/usr/bin/env bash
# Run the complete Go suite with JSON events retained for post-run timing analysis.
set -uo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

output=${1:-artifacts/go-test-profile.jsonl}
timeout=${GO_TEST_TIMEOUT:-30m}
case "$output" in /*) ;; *) output="$repo_root/$output" ;; esac
mkdir -p "$(dirname "$output")"

printf 'Running full suite; Go timeout: %s\nJSON events: %s\n' "$timeout" "$output"
set +e
go test -json -timeout "$timeout" ./... >"$output"
test_status=$?
set -e

python3 tools/summarize-go-test.py "$output"
printf '\ngo test exit status: %d\n' "$test_status"
exit "$test_status"
