#!/usr/bin/env bash
#
# Coverage gate for the root JSON harness. The harness is sharded so a single
# go test process does not hit the 10m timeout; per-shard binary coverage data
# is merged back into one text profile before the checked-in ratchet baseline
# is applied (the historical 60% target is tracked as a separate goal).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

work="$(mktemp -d "${TMPDIR:-/tmp}/frigolite-coverage.XXXXXX")"
trap 'rm -rf "$work"' EXIT

echo "== build sharded root coverage binary =="
go test -c -covermode=set -cover -o "$work/frigolite.test" .

echo "== run sharded JSON harness with coverage =="
set +e
FRIGOLITE_COVERDATA=1 FRIGOLITE_SHARD_TIMEOUT=30m \
  bash tools/run_json_shards.sh "$work/frigolite.test" 12 >"$work/runner.log" 2>&1
runner_status=$?
set -e
cat "$work/runner.log"
echo "runner status=$runner_status (known baseline leaves are expected)"
run_dir="$(sed -n 's/^run_dir=//p' "$work/runner.log" | tail -n 1)"
tools/check_harness_leaves.sh "$run_dir"

echo "== merge shard coverage =="
cover_dirs=$(printf '%s,' "$run_dir"/shard-*/coverdata)
cover_dirs=${cover_dirs%,}
mkdir -p "$work/merged"
go tool covdata merge -i="$cover_dirs" -o="$work/merged"
go tool covdata textfmt -i="$work/merged" -o="$work/root.out"

echo "== coverage for selected packages =="
go test -timeout 30m -covermode=set -coverprofile="$work/other.out" \
  -run '^Test[^C]' ./frigodb/ ./internal/storage/ ./internal/util/

awk 'FNR == 1 && NR != 1 { next } { print }' \
  "$work/root.out" "$work/other.out" >"$work/coverage.out"

go tool cover -func="$work/coverage.out"
cov="$(go tool cover -func="$work/coverage.out" | awk '$1 == "total:" { gsub(/%/, "", $3); print $3 }')"
if [ -z "$cov" ]; then
    echo "FAIL: no coverage data" >&2
    exit 1
fi
baseline_file="$ROOT/tools/coverage_baseline.txt"
base="$(tr -d '[:space:]' < "$baseline_file")"
if [ -z "$base" ]; then
    echo "FAIL: empty coverage baseline: $baseline_file" >&2
    exit 1
fi
if awk "BEGIN {exit !($cov < $base)}"; then
    echo "FAIL: coverage $cov% < baseline $base%" >&2
    exit 1
fi
echo "Coverage: $cov% (baseline $base%)"
