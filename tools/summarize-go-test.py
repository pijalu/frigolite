#!/usr/bin/env python3
"""Summarize package and test durations from go test -json output."""
import collections
import json
import sys


def main(path):
    packages = collections.defaultdict(float)
    tests = []
    malformed = 0
    with open(path, encoding="utf-8") as stream:
        for line_number, line in enumerate(stream, 1):
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                malformed += 1
                continue
            if event.get("Action") not in {"pass", "fail", "skip"}:
                continue
            package = event.get("Package", "")
            elapsed = event.get("Elapsed", 0.0)
            if package and isinstance(elapsed, (int, float)):
                test_name = event.get("Test")
                if test_name:
                    tests.append((elapsed, package, test_name, event["Action"]))
                else:
                    packages[package] += elapsed

    print("Package elapsed totals (parallel test elapsed may exceed wall time):")
    for package, elapsed in sorted(packages.items(), key=lambda item: -item[1]):
        print(f"{elapsed:9.2f}s  {package}")
    print("\nSlowest reported test events:")
    for elapsed, package, test, action in sorted(tests, reverse=True)[:30]:
        print(f"{elapsed:9.2f}s  {action:4}  {package}  {test}")
    if malformed:
        print(f"\nWarning: ignored {malformed} malformed JSON event line(s)", file=sys.stderr)
    if not packages:
        print("No completed package events found; run may have been interrupted.", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} EVENTS.jsonl", file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1]))
