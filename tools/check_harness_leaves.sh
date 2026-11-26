#!/usr/bin/env bash
#
# Compare a sharded JSON harness run against the checked-in baseline leaf set.
# The harness runner exits non-zero for every known failure, so CI must check
# the leaf delta instead of relying on the runner status alone.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
run_dir="${1:?usage: check_harness_leaves.sh RUN_DIR [BASELINE] [STOCHASTIC]}"
baseline="${2:-$ROOT/tools/harness_baseline_leaves.txt}"
stochastic="${3:-$ROOT/tools/harness_stochastic_leaves.txt}"

[ -d "$run_dir" ] || { echo "FAIL: no run dir: $run_dir" >&2; exit 1; }
[ -f "$baseline" ] || { echo "FAIL: no baseline: $baseline" >&2; exit 1; }

work="$(mktemp -d "${TMPDIR:-/tmp}/frigolite-leaves.XXXXXX")"
trap 'rm -rf "$work"' EXIT

if grep -ahE 'panic: test timed out|fatal error:|signal: killed' "$run_dir"/shard-*.log; then
  echo "FAIL: shard crashed or timed out" >&2
  exit 1
fi

{ grep -ah -- '--- FAIL: TestSQLiteSuite/' "$run_dir"/shard-*.log || true; } \
  | sed -E 's/.*--- FAIL: (TestSQLiteSuite[^ ]*).*/\1/' \
  | sort -u >"$work/current.txt"

python3 - "$work/current.txt" "$baseline" "$stochastic" <<'PY'
import sys


def names(path):
    if not path:
        return set()
    return {
        line.strip()
        for line in open(path, errors="replace")
        if line.strip() and not line.lstrip().startswith("#")
    }


def leaves(items):
    ordered = sorted(items)
    return [name for name in ordered if not any(other.startswith(name + "/") for other in ordered)]


current = leaves(names(sys.argv[1]))
baseline = leaves(names(sys.argv[2]))
stochastic = names(sys.argv[3])
new_all = set(current) - set(baseline)
ignored = sorted(new_all & stochastic)
new = sorted(new_all - stochastic)
gone = sorted(set(baseline) - set(current))
print("harness leaves: current=%d baseline=%d NEW=%d STOCHASTIC=%d GONE=%d" %
      (len(current), len(baseline), len(new), len(ignored), len(gone)))
for name in new[:50]:
    print("NEW: " + name)
if len(new) > 50:
    print("... %d more NEW leaves" % (len(new) - 50))
for name in ignored:
    print("STOCHASTIC: " + name)
for name in gone[:20]:
    print("GONE: " + name)
if len(gone) > 20:
    print("... %d more GONE leaves" % (len(gone) - 20))
sys.exit(1 if new else 0)
PY
