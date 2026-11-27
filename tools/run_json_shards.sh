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

# GitHub-hosted runners have far less memory than the dev box. Cap the number
# of concurrent -race test binaries; CI sets 6. The default keeps local runs
# unchanged. Shards still run in index order and every status is recorded.
parallel=${FRIGOLITE_SHARD_PARALLEL:-$shards}
if ! [[ $parallel =~ ^[1-9][0-9]*$ ]]; then
  echo "FRIGOLITE_SHARD_PARALLEL must be positive" >&2
  exit 2
fi

root=$(mktemp -d "${TMPDIR:-/tmp}/frigolite-shards.XXXXXX")
for ((i = 0; i < shards; i++)); do
  mkdir "$root/shard-$i"
  ln -s "$PWD/testdata" "$root/shard-$i/testdata"
done

statuses=()
result=0
heartbeat_pid=""
cleanup() {
  if [ -n "$heartbeat_pid" ]; then
    kill "$heartbeat_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT

for ((start = 0; start < shards; start += parallel)); do
  end=$((start + parallel))
  if ((end > shards)); then
    end=$shards
  fi
  batch_pids=()
  for ((i = start; i < end; i++)); do
    (
      cd "$root/shard-$i"
      common_args=(-test.run '^TestSQLiteSuite$' -test.v -test.timeout="${FRIGOLITE_SHARD_TIMEOUT:-30m}")
      if [ -n "${FRIGOLITE_COVERDATA:-}" ]; then
        coverdir="$root/shard-$i/coverdata"
        mkdir -p "$coverdir"
        FRIGOLITE_SHARD_INDEX="$i" FRIGOLITE_SHARD_COUNT="$shards" \
          "$binary" "${common_args[@]}" -test.gocoverdir="$coverdir" >"$root/shard-$i.log" 2>&1
      else
        FRIGOLITE_SHARD_INDEX="$i" FRIGOLITE_SHARD_COUNT="$shards" \
          "$binary" "${common_args[@]}" >"$root/shard-$i.log" 2>&1
      fi
    ) &
    batch_pids+=("$!")
  done

  # Long shards emit no stdout (their -test.v output goes to per-shard logs).
  # A heartbeat keeps CI progress visible and prevents an idle-step kill.
  (
    while :; do
      sleep 300
      alive=0
      for pid in "${batch_pids[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
          alive=1
          break
        fi
      done
      ((alive)) || break
      echo "harness shards still running (batch $start..$((end - 1)))..."
    done
  ) &
  heartbeat_pid=$!

  for ((i = start, j = 0; i < end; i++, j++)); do
    status=0
    wait "${batch_pids[$j]}" || status=$?
    statuses[$i]=$status
    ((status == 0)) || result=1
  done
  kill "$heartbeat_pid" 2>/dev/null || true
  wait "$heartbeat_pid" 2>/dev/null || true
  heartbeat_pid=""
done

for ((i = 0; i < shards; i++)); do
  printf '%s\n' "${statuses[$i]}" >"$root/shard-$i.status"
  if [[ ${statuses[$i]} -ne 0 ]]; then
    echo "=== shard $i failed (last 80 lines) ===" >&2
    tail -n 80 "$root/shard-$i.log" >&2
  fi
done
printf 'run_dir=%s\n' "$root"
exit "$result"
