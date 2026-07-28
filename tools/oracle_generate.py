#!/usr/bin/env python3
"""
Oracle generator: run each extracted SQL against real sqlite3 to capture actual
expected output. This eliminates false expectations from lossy TCL conversion.

Usage: python3 tools/oracle_generate.py [--testdir <dir>] [--outdir <dir>]

For each TCL test file, extract SQL/expected pairs (same logic as converters),
then run each SQL against sqlite3 and capture the real output as the new
expected value.

Key rules:
- Each test case gets its own temp-file DB (matching setupDB behaviour).
- reset_db / __RESET_DB__ creates a fresh DB.
- All exec steps within a test case are run sequentially in one sqlite3 session.
- Query steps capture the result output as the expected value.
- Output format: space-separated values (matching flattenResult), NULL → "NULL".
- If sqlite3 errors on a step, the original expected is kept.
"""

import os, sys, json, subprocess, re, tempfile, argparse

TEST_DIR = "/Users/muaddib/dev/frigolite/ori/sqlite/test"
OUTPUT_DIR = "/Users/muaddib/dev/frigolite/testdata"

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from convert_compat_json import extract_tests, C_API_RE

SQLITE3 = "sqlite3"


def run_sqlite3_batch(sql_script, timeout=30):
    """Run a multi-statement SQL script against a fresh :memory: sqlite3.
    Uses -batch -noheader -list for consistent output format.
    Returns (stdout, stderr, returncode)."""
    cmd = [
        SQLITE3, ":memory:",
        "-batch",
        "-noheader",
        "-list",
        ".nullvalue NULL",
    ]
    try:
        proc = subprocess.run(
            cmd,
            input=sql_script.encode("utf-8"),
            capture_output=True,
            timeout=timeout,
        )
        stdout = proc.stdout.decode("utf-8", errors="replace")
        stderr = proc.stderr.decode("utf-8", errors="replace")
        return stdout.strip(), stderr.strip(), proc.returncode
    except subprocess.TimeoutExpired:
        return None, "TIMEOUT", -1
    except FileNotFoundError:
        return None, "sqlite3 not found", -1


def format_oracle_output(stdout):
    """Convert sqlite3 -list -noheader output to space-separated values.
    sqlite3 outputs one column per line (like .mode list).
    Rows are separated by blank lines or next column set.
    We join values in a simple space-separated list matching flattenResult."""
    if not stdout:
        return ""
    # In -list mode each line is a single value
    lines = [l.strip() for l in stdout.split("\n") if l.strip()]
    return " ".join(lines)


def is_query_sql(sql):
    """Check if SQL is a query (returns rows)."""
    first_stmt = sql.strip().split(";")[0].strip().upper()
    return first_stmt.startswith("SELECT") or first_stmt.startswith("PRAGMA") or first_stmt.startswith("EXPLAIN")


def generate_oracle(tests):
    """Process test cases, running each test's SQL through sqlite3
    to capture real expected output for query results."""
    for tc in tests:
        name = tc.get("name", "")
        if name == "__RESET_DB__":
            continue

        # Collect all SQL statements for this test case
        batch_parts = []
        query_indices = []  # (step_index, is_query, has_original_expect)
        
        for i, step in enumerate(tc.get("steps", [])):
            sql = step.get("sql", "")
            if not sql:
                continue
            step_type = step.get("type", "exec")
            has_expect = "expect" in step
            is_query = step_type == "query" or is_query_sql(sql)
            
            batch_parts.append(sql)
            if is_query:
                query_indices.append((i, True, has_expect))
            else:
                query_indices.append((i, False, has_expect))

        if not batch_parts:
            continue

        # Execute all statements together in one sqlite3 session
        script = ";\n".join(batch_parts) + ";\n"
        stdout, stderr, rc = run_sqlite3_batch(script)

        if rc != 0:
            # If sqlite3 errors, some statements may have failed.
            # Fall back to executing statements one at a time to isolate failures.
            for i, step in enumerate(tc.get("steps", [])):
                sql = step.get("sql", "")
                if not sql:
                    continue
                step_type = step.get("type", "exec")
                has_expect = "expect" in step
                is_query = step_type == "query" or is_query_sql(sql)
                
                if is_query and has_expect:
                    # Try just this query in a fresh DB (without setup)
                    # This won't give the right result without setup, so skip
                    pass
            continue

        # Parse output: each row produces one line per column in -list mode.
        # We need to map output back to individual queries.
        if stdout:
            lines = [l.strip() for l in stdout.split("\n") if l.strip()]
        else:
            lines = []

        # Execute again to get per-query output
        # Strategy: for each query step, execute all prior setup + that query
        # This ensures the DB state is correct for each query
        for step_idx, is_qry, has_exp in query_indices:
            if not (is_qry and has_exp):
                continue

            step = tc["steps"][step_idx]
            # Execute all statements up to and including this query
            up_to_sql = ";\n".join(batch_parts[:step_idx + 1]) + ";\n"
            q_stdout, q_stderr, q_rc = run_sqlite3_batch(up_to_sql)

            if q_rc == 0 and q_stdout:
                # The last statement in the batch is the query we want.
                # Extract its output from the full stdout.
                formatted = format_oracle_output(q_stdout)
                if formatted:
                    step["expect"] = formatted

    return tests


def main():
    parser = argparse.ArgumentParser(
        description="Generate oracle expected values from sqlite3"
    )
    parser.add_argument(
        "--testdir", default=TEST_DIR,
        help=f"TCL test directory (default: {TEST_DIR})"
    )
    parser.add_argument(
        "--outdir", default=OUTPUT_DIR,
        help=f"JSON output directory (default: {OUTPUT_DIR})"
    )
    parser.add_argument(
        "--dry-run", action="store_true",
        help="Show what would be changed without writing"
    )
    parser.add_argument(
        "--limit", type=int, default=0,
        help="Limit processing to N files (for testing)"
    )
    args = parser.parse_args()

    if not os.path.isdir(args.testdir):
        print(f"Error: test directory not found: {args.testdir}")
        sys.exit(1)

    os.makedirs(args.outdir, exist_ok=True)

    test_files = sorted(
        f for f in os.listdir(args.testdir) if f.endswith(".test")
    )

    # Pre-scan: skip C API files
    skip_files = set()
    for fname in test_files:
        filepath = os.path.join(args.testdir, fname)
        with open(filepath, "r", errors="replace") as f:
            content = f.read()
        if C_API_RE.search(content):
            skip_files.add(fname)

    print(f"Skipping {len(skip_files)} C API test files")
    print(f"Processing {len(test_files) - len(skip_files)} test files...")

    processed = 0
    updated = 0
    errors = 0

    for fname in test_files:
        if fname in skip_files:
            continue
        if args.limit and processed >= args.limit:
            break

        filepath = os.path.join(args.testdir, fname)
        with open(filepath, "r", errors="replace") as f:
            content = f.read()

        tests = extract_tests(content)
        if not tests:
            continue

        # Count original expected values
        orig_expects = sum(
            1 for tc in tests for s in tc.get("steps", []) if "expect" in s
        )

        # Generate oracle values
        tests = generate_oracle(tests)

        # Count changed expected values
        new_expects = sum(
            1 for tc in tests for s in tc.get("steps", []) if "expect" in s
        )

        out_name = re.sub(r"\.test$", "", fname)
        out_name = re.sub(r"[^a-zA-Z0-9]", "_", out_name)
        if not out_name or not out_name[0].isalpha():
            out_name = "f_" + out_name
        out_name = out_name[:80]

        out_path = os.path.join(args.outdir, f"{out_name}.json")
        test_data = {"file": fname, "name": out_name, "tests": tests}

        if args.dry_run:
            if orig_expects > 0:
                print(f"  [DRY RUN] {out_name}.json: {orig_expects} expects")
            processed += 1
            continue

        with open(out_path, "w") as f:
            json.dump(test_data, f, indent=2)

        processed += 1
        if orig_expects > 0:
            updated += 1

    print(f"\nDone. {processed} files processed, {updated} updated, {errors} errors.")


if __name__ == "__main__":
    main()
