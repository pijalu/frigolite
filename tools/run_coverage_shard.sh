#!/usr/bin/env bash
#
# One shard of the root JSON harness coverage gate. CI runs one shard per
# hosted runner so no job stays alive long enough to be reclaimed; a separate
# merge job (tools/merge_coverage_gate.sh) combines the uploaded coverdata.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if (($# != 2)); then
  echo "usage: $0 SHARD_INDEX SHARD_COUNT" >&2
  exit 2
fi
index=$1
total=$2
if ! [[ $index =~ ^[0-9]+$ ]] || ! [[ $total =~ ^[1-9][0-9]*$ ]] || ((index >= total)); then
  echo "SHARD_INDEX / SHARD_COUNT must satisfy 0 <= index < count" >&2
  exit 2
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/frigolite-cov-shard.XXXXXX")"
trap 'rm -rf "$work"' EXIT

echo "== build sharded root coverage binary =="
go test -c -covermode=set -cover -o "$work/frigolite.test" .

echo "== run coverage shard $index of $total =="
set +e
FRIGOLITE_COVERDATA=1 \
  FRIGOLITE_SHARD_OFFSET="$index" FRIGOLITE_SHARD_TOTAL="$total" \
  FRIGOLITE_SHARD_TIMEOUT="${FRIGOLITE_SHARD_TIMEOUT:-30m}" \
  bash tools/run_json_shards.sh "$work/frigolite.test" 1 2>&1 | tee "$work/runner.log"
runner_status=${PIPESTATUS[0]}
set -e
echo "runner status=$runner_status (known baseline leaves are expected)"
run_dir="$(sed -n 's/^run_dir=//p' "$work/runner.log" | tail -n 1)"
tools/check_harness_leaves.sh "$run_dir"

artifact_dir="artifacts/coverage-shard-$index"
mkdir -p "$artifact_dir"
cp -R "$run_dir/shard-$index/coverdata" "$artifact_dir/coverdata"
echo "coverdata artifact: $artifact_dir/coverdata"
