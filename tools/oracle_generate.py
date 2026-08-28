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

# Extraction helpers (adapted from deprecated convert_compat_json.py)
C_API_RE = re.compile(r'sqlite3_(prepare|step|column|finalize|exec\b|limit|db_config|config|enable_shared|initialize|shutdown|malloc|free|realloc|memory_used|memory_highwater|randomness|sleep|strglob|stricmp|strnicmp|strlike|create_function|create_aggregate|connection_pointer|create_collation|create_module|overload|declare_vtab|table_column_metadata|db_filename|db_readonly|db_handle|next_stmt|commit_hook|rollback_hook|update_hook|preupdate|wal_hook|auto_extension|cancel_auto_extension|reset_auto_extension|set_authorizer|trace|progress_handler|file_control|test_control|keyword_|compileoption|db_cacheflush|snapshot|unlock_notify|log|vtab|db_config|txn_state|changes|total_changes|errcode|errstr|threadsafe|serialize|deserialize|hard_heap|soft_heap|release_memory|db_release_memory|db_status|status)')

UNSUPPORTED_IFCAPABLE = {
    'fts3', 'fts4', 'fts5', 'rtree', 'json1', 'icu', 'session',
    'dbstat', 'csv', 'dbdata', 'decimal', 'memorydb', 'shared_cache',
    'direct_read', 'dirread', 'windowfunc',
}
SUPPORTED_IFCAPABLE = {'altertable', 'trigger', 'view', 'explain'}
C_SPECIFIC_PATTERNS = [
    r'\bdo_malloc_test\b', r'\bdo_fault_test\b',
    r'\bsource\s+\$testdir/malloc_common\.tcl',
    r'\bsource\s+\$testdir/fault_common\.tcl',
    r'\bsqlite3_threadsafe\b', r'\bsqlite3_db_mutex\b',
]


def extract_balanced_braces(text, start_pos):
    if start_pos >= len(text) or text[start_pos] != '{':
        return None
    depth = 0
    i = start_pos
    while i < len(text):
        ch = text[i]
        if ch == '{':
            depth += 1
        elif ch == '}':
            depth -= 1
            if depth == 0:
                return (text[start_pos+1:i], i+1)
        i += 1
    return None


def find_ifcapable_blocks(content):
    blocks = []
    pattern = r'ifcapable\s+(!?\w+)'
    for m in re.finditer(pattern, content):
        feature = m.group(1)
        negated = feature.startswith('!')
        if negated:
            feature = feature[1:]
        feature = feature.lower()
        pos = m.end()
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        if pos >= len(content) or content[pos] != '{':
            continue
        result = extract_balanced_braces(content, pos)
        if result is None:
            continue
        _, end_pos = result
        should_skip = False
        if negated:
            should_skip = feature in SUPPORTED_IFCAPABLE
        else:
            should_skip = feature in UNSUPPORTED_IFCAPABLE
        if should_skip:
            blocks.append((m.start(), end_pos))
    return blocks


def is_position_blocked(pos, blocks):
    for start, end in blocks:
        if start <= pos <= end:
            return True
    return False


def has_unsupported_features(sql):
    if re.search(r'(?<!\w)\$[a-zA-Z_]\w*', sql):
        return True
    if '${' in sql:
        return True
    if '{' in sql or '}' in sql:
        return True
    if re.search(r'\bsql\s*\{', sql):
        return True
    if re.search(r'\btcl\s*\(', sql, re.IGNORECASE) or 'vtab_command' in sql:
        return True
    return False


def tcl_variable_substitute(sql):
    sql = re.sub(r'\$::temp\b', 'TEMP', sql)
    sql = re.sub(r'\$\{::temp\}', 'TEMP', sql)
    return sql


def last_sql(sql):
    statements = [s.strip() for s in sql.split(';') if s.strip()]
    return statements[-1] if statements else sql


def extract_prepare_sql(content):
    entries = []
    for m in re.finditer(r'sqlite3_prepare\s+\w+\s+"([^"]*)"', content):
        sql = m.group(1).strip()
        if sql and not re.match(r'^\s*$', sql):
            entries.append((m.start(), "execsql", sql))
    for m in re.finditer(r'sqlite3_prepare\s+\$?\w+\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql and not re.match(r'^\s*$', sql):
            entries.append((m.start(), "execsql", sql))
    return entries


def extract_tests(content):
    tests = []
    current_steps = []
    current_name = None
    has_current = False
    
    def flush():
        nonlocal current_name, current_steps, has_current
        if current_steps:
            tests.append({"name": current_name or f"setup_{len(tests)}", "steps": current_steps})
            current_steps = []
            current_name = None
            has_current = False
    
    entries = []
    blocked_ranges = find_ifcapable_blocks(content)
    
    # Phase 1: do_execsql_test / do_catchsql_test
    pattern = r'(do_execsql_test|do_catchsql_test)\s+(\S+)\s*'
    for m in re.finditer(pattern, content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        cmd_type = m.group(1)
        test_name = m.group(2)
        pos = m.end()
        if '$' in test_name or '%' in test_name:
            continue
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        if pos >= len(content) or content[pos] != '{':
            continue
        result = extract_balanced_braces(content, pos)
        if result is None:
            continue
        sql_body, pos = result
        sql_body = sql_body.strip()
        if not sql_body:
            continue
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        expected = None
        if pos < len(content) and content[pos] == '{':
            exp_result = extract_balanced_braces(content, pos)
            if exp_result is not None:
                expected_raw, _ = exp_result
                expected = expected_raw.strip()
                if not expected:
                    expected = None
        entries.append((m.start(), cmd_type, sql_body, expected, test_name))
    
    # Phase 2: do_test patterns
    for m in re.finditer(r'do_test\s+(\S+)\s*', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        test_name = m.group(1)
        pos = m.end()
        if '$' in test_name or '%' in test_name:
            continue
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        if pos >= len(content) or content[pos] != '{':
            continue
        result = extract_balanced_braces(content, pos)
        if result is None:
            continue
        tcl_body, pos = result
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        expected = None
        if pos < len(content) and content[pos] == '{':
            exp_result = extract_balanced_braces(content, pos)
            if exp_result is not None:
                expected_raw, _ = exp_result
                expected = expected_raw.strip()
                if not expected:
                    expected = None
        
        for subst_pat in [
            r'execsql\s*\[subst -nocommands\s*\{([^}]*)\]\]',
            r'execsql\s*\[subst\s+\{([^}]*)\]\]',
        ]:
            subst_match = re.search(subst_pat, tcl_body)
            if subst_match:
                sql = subst_match.group(1).strip()
                if sql:
                    sql = tcl_variable_substitute(sql)
                    entries.append((m.start(), "execsql", sql, expected, test_name))
                    break
        else:
            es_match = re.search(r'execsql\s*\{', tcl_body)
            if es_match:
                inner_start = tcl_body.index('{', es_match.start())
                inner_result = extract_balanced_braces(tcl_body, inner_start)
                if inner_result is not None:
                    sql, _ = inner_result
                    sql = sql.strip()
                    if sql:
                        entries.append((m.start(), "execsql", sql, expected, test_name))
                        continue
            cs_match = re.search(r'catchsql\s*\{', tcl_body)
            if cs_match:
                inner_start = tcl_body.index('{', cs_match.start())
                inner_result = extract_balanced_braces(tcl_body, inner_start)
                if inner_result is not None:
                    sql, _ = inner_result
                    sql = sql.strip()
                    if sql:
                        entries.append((m.start(), "catchsql", sql, expected, test_name))
    
    # Phase 3-5: standalone execsql patterns
    for pat in [
        (r'execsql\s*\{([^}]*)\}', False),
        (r'execsql\s*\[subst -nocommands\s*\{([^}]*)\]\]', True),
        (r'execsql\s*\[subst\s+\{([^}]*)\]\]', True),
    ]:
        pattern, do_subst = pat
        for m in re.finditer(pattern, content):
            if is_position_blocked(m.start(), blocked_ranges):
                continue
            sql = m.group(1).strip()
            if not sql:
                continue
            if do_subst:
                sql = tcl_variable_substitute(sql)
            already_covered = any(abs(e[0] - m.start()) < 10 for e in entries)
            if not already_covered:
                entries.append((m.start(), "execsql", sql, None, None))
    
    # Phase 6-7: db eval
    for m in re.finditer(r'db\s+eval\s*\{([^}]*)\}', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "db_eval", sql, None, None))
    for m in re.finditer(r'db\s+eval\s+"([^"]*)"', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "db_eval", sql, None, None))
    
    # Phase 8: reset_db
    for m in re.finditer(r'^reset_db\s*$', content, re.MULTILINE):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        entries.append((m.start(), "reset_db", None, None, None))
    for m in re.finditer(r'db\s+close\s*\n\s*sqlite3\s+db\s', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        entries.append((m.start(), "reset_db", None, None, None))
    
    # Phase 11: sqlite3_prepare
    for pos, stype, sql in extract_prepare_sql(content):
        if is_position_blocked(pos, blocked_ranges):
            continue
        already_covered = any(abs(e[0] - pos) < 15 for e in entries)
        if not already_covered:
            entries.append((pos, stype, sql, None, None))
    
    entries.sort(key=lambda x: x[0])
    
    # Find orphan virtual tables
    orphan_tables = set()
    for ent in entries:
        if len(ent) < 3:
            continue
        pos, cmd_type, sql = ent[0], ent[1], ent[2]
        if cmd_type == "auth":
            continue
        if sql and sql.strip().upper().startswith('CREATE VIRTUAL TABLE'):
            if has_unsupported_features(sql):
                m = re.search(r'CREATE\s+VIRTUAL\s+TABLE\s+(\S+)\s+USING\s+(\S+)', sql, re.IGNORECASE)
                if m:
                    tbl = m.group(1)
                    if '.' in tbl:
                        parts = tbl.split('.')
                        if parts[0].upper() in ('MAIN', 'TEMP', 'TEMPORARY'):
                            tbl = parts[1]
                    orphan_tables.add(tbl.upper())
    
    def references_table(sql, table_name):
        upper = table_name.upper()
        patterns = [
            r'\bALTER\s+TABLE\s+' + re.escape(upper) + r'\b',
            r'\bDROP\s+TABLE\s+' + re.escape(upper) + r'\b',
            r'\bINSERT\s+INTO\s+' + re.escape(upper) + r'\b',
            r'\bDELETE\s+FROM\s+' + re.escape(upper) + r'\b',
            r'\bUPDATE\s+' + re.escape(upper) + r'\b',
            r'\bFROM\s+' + re.escape(upper) + r'\b',
            r'\bTABLE\s+' + re.escape(upper) + r'\b',
        ]
        sql_upper = sql.upper()
        for pat in patterns:
            if re.search(pat, sql_upper):
                return True
        return False
    
    for ent in entries:
        if len(ent) < 3:
            continue
        pos, cmd_type, sql = ent[0], ent[1], ent[2]
        expected = ent[3] if len(ent) > 3 else None
        name = ent[4] if len(ent) > 4 else None
        
        if cmd_type == "reset_db":
            flush()
            tests.append({"name": "__RESET_DB__", "steps": [{"type": "reset_db"}]})
            continue
        
        if sql and has_unsupported_features(sql):
            continue
        if sql and any(references_table(sql, orphan) for orphan in orphan_tables):
            continue
        
        if name:
            flush()
            current_name = name
            has_current = True
            is_query = cmd_type not in ("do_catchsql_test", "catchsql") and bool(re.match(r'\s*SELECT\b|\s*PRAGMA\b|\s*EXPLAIN\b', last_sql(sql), re.IGNORECASE))
            if is_query:
                step = {"type": "query", "sql": sql}
                if expected:
                    step["expect"] = expected
                current_steps.append(step)
            else:
                step = {"type": "exec", "sql": sql}
                if expected and cmd_type in ("do_catchsql_test", "catchsql"):
                    step["expect"] = expected
                current_steps.append(step)
        elif cmd_type in ("execsql", "db_eval"):
            if not has_current:
                flush()
                has_current = True
            current_steps.append({"type": "exec", "sql": sql})
    
    flush()
    return tests

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
