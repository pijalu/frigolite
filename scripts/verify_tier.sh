#!/usr/bin/env bash
# Verify a tier's testgen packages all pass (exit 0) or any fail (exit 1).
# Usage: scripts/verify_tier.sh <tier>   e.g. scripts/verify_tier.sh 1
#
# Reads plans/PACKAGES_TIER<N>.txt (space-separated package names, '#' comments
# allowed) and runs `go test -tags testgen` on each, then the SOLID check.
set -u

TIER="${1:?usage: scripts/verify_tier.sh <tier>}"
LIST="plans/PACKAGES_TIER${TIER}.txt"
[ -f "$LIST" ] || { echo "missing $LIST"; exit 1; }

PKGS=$(grep -vE '^\s*(#|$)' "$LIST" | tr ' ' '\n' | sed '/^$/d')
[ -n "$PKGS" ] || { echo "no packages listed in $LIST"; exit 1; }

echo "== verify_tier ${TIER}: $(echo "$PKGS" | wc -l | tr -d ' ') packages =="
FAILED=0
for pkg in $PKGS; do
  if go test -tags testgen -count=1 -timeout 300s "./testgen/${pkg}/" >/tmp/verify_tier_${TIER}.log 2>&1; then
    echo "ok   ${pkg}"
  else
    echo "FAIL ${pkg}"
    tail -5 /tmp/verify_tier_${TIER}.log
    FAILED=1
  fi
done

if [ "$FAILED" -ne 0 ]; then
  echo "== TIER ${TIER} HAS FAILURES =="
  exit 1
fi

echo "== tier ${TIER} all green; SOLID check =="
if ! go test -run TestSOLID_ -count=1 ./... >/tmp/verify_solid_${TIER}.log 2>&1; then
  echo "SOLID FAILED"
  tail -20 /tmp/verify_solid_${TIER}.log
  exit 1
fi
echo "== TIER ${TIER} PASS =="
