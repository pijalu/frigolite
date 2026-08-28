#!/usr/bin/env bash
# Verify all applicable misc testgen packages pass (exit 0) or any fail (exit 1).
# Usage: scripts/verify_all_applicable.sh
#
# Reads plans/PACKAGES_TIER6C.txt (space-separated package names, '#' comments
# allowed) and runs `go test -tags testgen` on each, then the SOLID check.
set -u

LIST="plans/PACKAGES_TIER6C.txt"
[ -f "$LIST" ] || { echo "missing $LIST"; exit 1; }

PKGS=$(grep -vE '^\s*(#|$)' "$LIST" | tr ' ' '\n' | sed '/^$/d')
[ -n "$PKGS" ] || { echo "no packages listed in $LIST"; exit 1; }

echo "== verify_all_applicable: $(echo "$PKGS" | wc -l | tr -d ' ') packages =="
FAILED=0
for pkg in $PKGS; do
  if go test -tags testgen -count=1 -timeout 300s "./testgen/${pkg}/" >/tmp/verify_applicable.log 2>&1; then
    echo "ok   ${pkg}"
  else
    echo "FAIL ${pkg}"
    tail -5 /tmp/verify_applicable.log
    FAILED=1
  fi
done

if [ "$FAILED" -ne 0 ]; then
  echo "== APPLICABLE TIER HAS FAILURES =="
  exit 1
fi

echo "== all applicable green; SOLID check =="
if ! go test -run TestSOLID_ -count=1 ./... >/tmp/verify_solid_applicable.log 2>&1; then
  echo "SOLID FAILED"
  tail -20 /tmp/verify_solid_applicable.log
  exit 1
fi
echo "== APPLICABLE TIER PASS =="
