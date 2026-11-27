#!/usr/bin/env bash
#
# Merge per-shard coverdata uploaded by tools/run_coverage_shard.sh, then
# apply the same strict coverage ratchet as the monolithic gate.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

artifact_root="${1:?usage: merge_coverage_gate.sh ARTIFACT_ROOT}"
[ -d "$artifact_root" ] || { echo "FAIL: no artifact root: $artifact_root" >&2; exit 1; }

work="$(mktemp -d "${TMPDIR:-/tmp}/frigolite-cov-merge.XXXXXX")"
trap 'rm -rf "$work"' EXIT

cover_dirs=()
while IFS= read -r -d '' file; do
  dir="$(dirname "$file")"
  seen=0
  for existing in "${cover_dirs[@]}"; do
    if [ "$existing" = "$dir" ]; then seen=1; break; fi
  done
  if ((seen == 0)); then cover_dirs+=("$dir"); fi
done < <(find "$artifact_root" -type f -name '*.covmeta' -print0)
if ((${#cover_dirs[@]} != 24)); then
  echo "FAIL: expected 24 coverdata directories, got ${#cover_dirs[@]} under $artifact_root" >&2
  exit 1
fi

echo "== merge shard coverage =="
joined=$(IFS=,; printf '%s' "${cover_dirs[*]}")
go tool covdata merge -i="$joined" -o="$work/merged"
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
