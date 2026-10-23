#!/usr/bin/env bash
set -euo pipefail

if (($# != 2)); then
  echo "usage: $0 TEST_BINARY SHARD_COUNT" >&2
  exit 2
fi
binary=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
shards=$2
if ! [[ $shards =~ ^[1-9][0-9]*$ ]]; then
  echo "shard count must be positive" >&2
  exit 2
fi
root=$(mktemp -d "${TMPDIR:-/tmp}/frigolite-shards.XXXXXX")
for ((i = 0; i < shards; i++)); do
  mkdir "$root/shard-$i"
  ln -s "$PWD/testdata" "$root/shard-$i/testdata"
done
pids=()
for ((i = 0; i < shards; i++)); do
  (
    cd "$root/shard-$i"
    FRIGOLITE_SHARD_INDEX="$i" FRIGOLITE_SHARD_COUNT="$shards" \
      "$binary" -test.run '^TestSQLiteSuite$' -test.v >"$root/shard-$i.log" 2>&1
  ) &
  pids+=("$!")
done
result=0
statuses=()
for pid in "${pids[@]}"; do
  status=0
  wait "$pid" || status=$?
  statuses+=("$status")
  ((status == 0)) || result=1
done
for ((i = 0; i < shards; i++)); do
  printf '%s\n' "${statuses[$i]}" >"$root/shard-$i.status"
done
printf 'run_dir=%s\n' "$root"
exit "$result"
