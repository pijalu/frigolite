#!/bin/bash

# golang-check: run all Go static analysis and file-size checks
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$(go env GOMOD)")" 2>/dev/null && pwd || pwd)"
HAS_ERRORS=0

# Analyze production sources only. Generated testgen fixtures and tests have
# separate compatibility/size policies and are covered by their own gates.
GO_FILES=$(find "$PROJECT_DIR" -type f -name '*.go' \
  -not -path '*/vendor/*' -not -path '*/third_party/*' -not -path '*/.git/*' -not -path '*/testgen/*' \
  -not -name '*_test.go' -print)

if [ -z "$GO_FILES" ]; then
  echo "FAIL: no production Go files found" >&2
  exit 1
fi

echo "=== gocognit (cognitive complexity > 15) ==="
printf '%s\n' "$GO_FILES" | xargs gocognit -over 15 || { echo "FAIL: cognitive complexity exceeded"; HAS_ERRORS=1; }

echo ""
echo "=== gocyclo (cyclomatic complexity > 12) ==="
printf '%s\n' "$GO_FILES" | xargs gocyclo -over 12 || { echo "FAIL: cyclomatic complexity exceeded"; HAS_ERRORS=1; }

echo ""
echo "=== staticcheck ==="
staticcheck "$PROJECT_DIR/..." || { echo "FAIL: staticcheck issues found"; HAS_ERRORS=1; }

echo ""
echo "=== go file size (hard max 1000, soft target 500) ==="
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$SCRIPT_DIR/go-file-size-check.sh" || { echo "FAIL: file size limits exceeded"; HAS_ERRORS=1; }

echo ""
if [ "$HAS_ERRORS" -eq 0 ]; then
  echo "✓ All checks passed"
else
  echo "✗ Some checks failed"
fi
exit $HAS_ERRORS