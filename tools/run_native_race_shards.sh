#!/usr/bin/env bash
#
# Native race tests in bounded shards. One go test invocation over all packages
# exceeds the 30m go-test timeout on CI runners; each shard stays below it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

shards=(
  '^TestA'
  '^TestB[a-su-z]'
  '^TestB[Tt]'
  '^TestC'
  '^Test[D-F]'
  '^Test[G-M]'
  '^Test[N-O]'
  '^TestP'
  '^Test[Q-T]'
  '^Test[U-Z]'
)

for pattern in "${shards[@]}"; do
  echo "== race shard $pattern =="
  FRIGOLITE_TEST=__none__ go test -timeout 30m -race -count=1 -run "$pattern" ./...
done
