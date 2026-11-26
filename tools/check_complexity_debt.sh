#!/usr/bin/env bash
#
# Complexity debt ratchet: existing over-threshold functions are recorded in
# tools/complexity_baseline.txt; new or worsened ones fail the gate. This
# keeps the debt visible without letting it grow, unlike a fixed threshold
# that the current engine can no longer satisfy.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

baseline="$ROOT/tools/complexity_baseline.txt"
work="$(mktemp -d "${TMPDIR:-/tmp}/frigolite-complexity.XXXXXX")"
trap 'rm -rf "$work"' EXIT

GO_FILES=$(find . -path ./third_party -prune -o -path ./testgen -prune -o -name '*.go' ! -name '*_test.go' -print)

normalize() {
    local tool="$1"
    shift
    "$@" $GO_FILES 2>/dev/null | awk -v tool="$tool" -F '[[:space:]]+' '
        {
            score = $1
            pkg = $2
            loc = $NF
            fn = ""
            for (i = 3; i < NF; i++) fn = fn (fn == "" ? "" : " ") $i
            sub(/:[0-9]+:[0-9]+$/, "", loc)
            printf "%s\t%s\t%s\t%s\t%s\n", tool, score, pkg, fn, loc
        }
    '
}

{
    normalize gocyclo gocyclo -over 15
    normalize gocognit gocognit -over 30
} | sort -t $'\t' -k1,1 -k5,5 -k3,3 -k4,4 >"$work/current"

if [ "${FRIGOLITE_UPDATE_COMPLEXITY_BASELINE:-}" = "1" ]; then
    cp "$work/current" "$baseline"
    echo "updated $baseline ($(wc -l < "$baseline") entries)"
    exit 0
fi

if [ ! -f "$baseline" ]; then
    echo "FAIL: missing complexity baseline: $baseline" >&2
    exit 1
fi

awk -F '\t' '
    FNR == NR {
        base[$1 SUBSEP $2 SUBSEP $3 SUBSEP $4] = $5
        next
    }
    {
        key = $1 SUBSEP $2 SUBSEP $3 SUBSEP $4
        if (!(key in base)) {
            printf "NEW: %s %s.%s (%s) score %s\n", $1, $3, $4, $5, $2 > "/dev/stderr"
            bad = 1
        } else if (($5 + 0) > (base[key] + 0)) {
            printf "WORSE: %s %s.%s (%s) score %s, baseline %s\n", $1, $3, $4, $5, $2, base[key] > "/dev/stderr"
            bad = 1
        }
        seen[key] = 1
    }
    END {
        for (key in base) {
            if (!(key in seen)) {
                split(key, part, SUBSEP)
                printf "REMOVED (baseline may need refresh): %s %s.%s (%s)\n", part[1], part[3], part[4], part[5]
            }
        }
        exit bad
    }
' "$baseline" "$work/current"
