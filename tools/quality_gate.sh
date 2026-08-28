#!/usr/bin/env bash
#
# Frigolite quality gate — the authoritative strict gate.
#
# Checks (with no arguments — whole repo, non-test, non-third_party Go files):
#   staticcheck ./...
#   gocognit -over 15   (cognitive complexity: every function <= 15)
#   gocyclo  -over 12   (cyclomatic complexity: every function <= 12)
#   file size           (hard max 1000 lines, soft target 500)
#
# With file arguments, only those files are checked for complexity/size
# (staticcheck always runs repo-wide). This is how per-element goals scope
# their verify commands:  tools/quality_gate.sh <file>...
#
# Exit code 0 = gate passes. Non-zero = gate fails (soft file-size warnings
# alone do not fail the gate; only the hard 1000-line max does).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ "$#" -gt 0 ]; then
    GO_FILES="$*"
else
    GO_FILES=$(find . -path ./third_party -prune -o -path ./testgen -prune -o -name '*.go' ! -name '*_test.go' -print)
fi

fail=0

echo "== staticcheck =="
if ! staticcheck ./...; then
    echo "FAIL: staticcheck" >&2
    fail=1
else
    echo "OK"
fi

echo "== gocognit (threshold 15) =="
out=$(gocognit -over 15 $GO_FILES 2>&1 || true)
if [ -n "$out" ]; then
    echo "$out"
    echo "FAIL: cognitive complexity exceeds 15" >&2
    fail=1
else
    echo "OK"
fi

echo "== gocyclo (threshold 12) =="
out=$(gocyclo -over 12 $GO_FILES 2>&1 || true)
if [ -n "$out" ]; then
    echo "$out"
    echo "FAIL: cyclomatic complexity exceeds 12" >&2
    fail=1
else
    echo "OK"
fi

echo "== file size (hard max 1000 lines, soft target 500) =="
hard=0
soft=0
for f in $GO_FILES; do
    [ -f "$f" ] || continue
    n=$(wc -l < "$f")
    if [ "$n" -gt 1000 ]; then
        echo "FAIL (hard): $f ($n lines)" >&2
        hard=1
    elif [ "$n" -gt 500 ]; then
        echo "WARN (soft): $f ($n lines)" >&2
        soft=1
    fi
done
if [ "$hard" -eq 1 ]; then
    echo "FAIL: file exceeds hard max 1000 lines" >&2
    fail=1
else
    echo "OK (hard max)"
fi
[ "$soft" -eq 1 ] && echo "NOTE: $soft file(s) exceed the 500-line soft target (not failing)"

exit "$fail"
