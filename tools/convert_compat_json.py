#!/usr/bin/env python3
"""Convert each SQLite TCL test file to a separate frigolite test data file."""
import os, json, re

TEST_DIR = "/Users/muaddib/dev/frigolite/ori/sqlite/test"
OUTPUT_DIR = "/Users/muaddib/dev/frigolite/testdata"

C_API_RE = re.compile(r'sqlite3_(prepare|step|column|finalize|exec\b|limit|db_config|config|enable_shared|initialize|shutdown|malloc|free|realloc|memory_used|memory_highwater|randomness|sleep|strglob|stricmp|strnicmp|strlike|create_function|create_aggregate|connection_pointer|create_collation|create_module|overload|declare_vtab|table_column_metadata|db_filename|db_readonly|db_handle|next_stmt|commit_hook|rollback_hook|update_hook|preupdate|wal_hook|auto_extension|cancel_auto_extension|reset_auto_extension|set_authorizer|trace|progress_handler|file_control|test_control|keyword_|compileoption|db_cacheflush|snapshot|unlock_notify|log|vtab|db_config|txn_state|changes|total_changes|errcode|errstr|threadsafe|serialize|deserialize|hard_heap|soft_heap|release_memory|db_release_memory|db_status|status)')

UNSUPPORTED_FEATURES = re.compile(
    r'\b(FILTER\s*\(|WAL\s|VACUUM\s|'
    r'SAVEPOINT\s|RELEASE\s|ROLLBACK\s+TO\s|REINDEX\s|'
    r'CREATE\s+VIRTUAL\s+TABLE\s|fts\d+\s*\(|rtree\s*\(|'
    r'WITHOUT\s+ROWID\s|zipfile|writecrash|'
    r'PRAGMA\s+(wal_|journal_mode=WAL|page_count|cache_flush|locking_mode|'
    r'schema_version|user_version|application_id|mmap_size|'
    r'soft_heap_limit|hard_heap_limit|threads|page_size=65536))',
    re.IGNORECASE)


# ifcapable features that are completely unsupported — entire blocks are skipped
UNSUPPORTED_IFCAPABLE = {
    'fts3', 'fts4', 'fts5', 'rtree', 'json1', 'icu', 'session',
    'dbstat', 'csv', 'dbdata', 'decimal', 'memorydb', 'shared_cache',
    'direct_read', 'dirread', 'windowfunc',
}

# ifcapable features that ARE supported at the block level
SUPPORTED_IFCAPABLE = {'altertable', 'trigger', 'view', 'explain'}


def find_ifcapable_blocks(content):
    """Find all ifcapable feature blocks and return list of (start, end) ranges to skip."""
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
    """Check if a position falls within any blocked ifcapable block."""
    for start, end in blocks:
        if start <= pos <= end:
            return True
    return False


def has_unsupported_features(sql):
    """Check if SQL uses features the engine doesn't support."""
    if UNSUPPORTED_FEATURES.search(sql):
        return True
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
    if re.search(r'\bMATCH\b', sql, re.IGNORECASE):
        return True
    if re.search(r'\bUSING\s*\(', sql, re.IGNORECASE):
        return True
    if re.search(r'\bjson_\w+\s*\(', sql, re.IGNORECASE):
        return True
    if re.search(r'\bRAISE\b', sql, re.IGNORECASE):
        return True
    if re.search(r'\bRETURNING\b', sql, re.IGNORECASE):
        return True
    if re.search(r'ON\s+CONFLICT\s*\(', sql, re.IGNORECASE):
        return True
    if re.search(r'\bzeroblob\b', sql, re.IGNORECASE):
        return True
    if re.search(r'\brandomblob\b', sql, re.IGNORECASE):
        return True
    return False


def extract_balanced_braces(text, start_pos):
    """Extract content inside balanced braces starting at start_pos ('{' character).
    Returns (content_without_braces, end_position_after_closing_brace) or None if unbalanced."""
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


def tcl_variable_substitute(sql):
    """Substitute known TCL variables with their SQL equivalents."""
    sql = re.sub(r'\$::temp\b', 'TEMP', sql)
    sql = re.sub(r'\$\{::temp\}', 'TEMP', sql)
    return sql


def last_sql(sql):
    """Return the last non-empty SQL statement from a multi-statement string."""
    statements = [s.strip() for s in sql.split(';') if s.strip()]
    return statements[-1] if statements else sql


def extract_tests(content):
    """Extract test cases from TCL test content in file order."""
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
    
    # Phase 1: do_execsql_test / do_catchsql_test with balanced brace matching
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
    
    # Phase 2: do_test patterns (handle execsql/catchsql inside)
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
        # Extract expected result
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
        
        # Find execsql/catchsql inside the TCL body
        # Try [subst -nocommands { SQL }]
        subst_match = re.search(r'execsql\s*\[subst -nocommands\s*\{([^}]*)\}\]', tcl_body)
        if subst_match:
            sql = subst_match.group(1).strip()
            if sql:
                sql = tcl_variable_substitute(sql)
                entries.append((m.start(), "execsql", sql, expected, test_name))
                continue
        
        # Try [subst { SQL }]
        subst_match = re.search(r'execsql\s*\[subst\s+\{([^}]*)\}\]', tcl_body)
        if subst_match:
            sql = subst_match.group(1).strip()
            if sql:
                sql = tcl_variable_substitute(sql)
                entries.append((m.start(), "execsql", sql, expected, test_name))
                continue
        
        # Try execsql { ... }
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
        
        # Try catchsql { ... }
        cs_match = re.search(r'catchsql\s*\{', tcl_body)
        if cs_match:
            inner_start = tcl_body.index('{', cs_match.start())
            inner_result = extract_balanced_braces(tcl_body, inner_start)
            if inner_result is not None:
                sql, _ = inner_result
                sql = sql.strip()
                if sql:
                    entries.append((m.start(), "catchsql", sql, expected, test_name))
                    continue
    
    # Phase 3: execsql { ... } (standalone, not inside do_test blocks)
    for m in re.finditer(r'execsql\s*\{([^}]*)\}', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        sql = m.group(1).strip()
        if not sql:
            continue
        # Check if this execsql is inside a do_test that we already captured
        # We do this by checking if any already-captured entry is at the same position
        already_covered = False
        for e in entries:
            if abs(e[0] - m.start()) < 10:
                already_covered = True
                break
        if not already_covered:
            entries.append((m.start(), "execsql", sql, None, None))
    
    # Phase 4: execsql [subst -nocommands { SQL }]
    for m in re.finditer(r'execsql\s*\[subst -nocommands\s*\{([^}]*)\}\]', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        sql = m.group(1).strip()
        if sql:
            sql = tcl_variable_substitute(sql)
            already_covered = any(abs(e[0] - m.start()) < 10 for e in entries)
            if not already_covered:
                entries.append((m.start(), "execsql", sql, None, None))
    
    # Phase 5: execsql [subst { SQL }]
    for m in re.finditer(r'execsql\s*\[subst\s+\{([^}]*)\}\]', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        sql = m.group(1).strip()
        if sql:
            sql = tcl_variable_substitute(sql)
            already_covered = any(abs(e[0] - m.start()) < 10 for e in entries)
            if not already_covered:
                entries.append((m.start(), "execsql", sql, None, None))
    
    # Phase 6: db eval { }
    for m in re.finditer(r'db\s+eval\s*\{([^}]*)\}', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "db_eval", sql, None, None))
    
    # Phase 7: db eval " "
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
    
    # Phase 9: db close + sqlite3 db
    for m in re.finditer(r'db\s+close\s*\n\s*sqlite3\s+db\s', content):
        if is_position_blocked(m.start(), blocked_ranges):
            continue
        entries.append((m.start(), "reset_db", None, None, None))
    
    # Phase 10: db auth (authorizer setup)
    # Detect `db auth procname` calls where procname is a TCL proc that
    # defines which operations to deny/allow.
    proc_names = re.findall(r'proc\s+(\w+)\s*\{[^}]*\}\s*\{', content)
    db_auth_calls = re.findall(r'db\s+auth\s+(\w+)', content)
    for auth_proc in set(db_auth_calls):
        if auth_proc in proc_names:
            # Find the proc body and the db auth call position
            proc_match = re.search(r'proc\s+' + re.escape(auth_proc) + r'\s*\{[^}]*\}\s*\{', content)
            auth_match = re.search(r'db\s+auth\s+' + re.escape(auth_proc), content)
            if proc_match and auth_match:
                auth_pos = auth_match.start()
                body_start = proc_match.end()
                body_result = extract_balanced_braces(content, body_start - 1)
                if body_result:
                    body, _ = body_result
                    # Parse simple deny rules: if {$t=="ACTION"} { return "RESULT" }
                    for rule_match in re.finditer(r'\{\$t=="([^"]*)"\}\s*\{[^}]*return\s+"([^"]*)"[^}]*\}', body):
                        action = rule_match.group(1)
                        result = rule_match.group(2)
                        if result == "SQLITE_DENY":
                            entries.append((auth_pos, "auth", None, None, None, action, "SQLITE_DENY"))
                        elif result == "SQLITE_OK":
                            entries.append((auth_pos, "auth", None, None, None, action, "SQLITE_OK"))
      
    entries.sort(key=lambda x: x[0])
    
    # Find orphan virtual tables: CREATE VIRTUAL TABLE statements that will be filtered,
    # whose tables are subsequently referenced by other SQL statements.
    orphan_tables = set()
    for pos, cmd_type, sql, expected, name, *extra in entries:
        if cmd_type == "auth":
            continue
        if sql and sql.strip().upper().startswith('CREATE VIRTUAL TABLE'):
            if has_unsupported_features(sql):
                m = re.search(r'CREATE\s+VIRTUAL\s+TABLE\s+(\S+)\s+USING\s+(\S+)', sql, re.IGNORECASE)
                if m:
                    tbl = m.group(1)
                    # Strip schema prefix
                    if '.' in tbl:
                        parts = tbl.split('.')
                        if parts[0].upper() in ('MAIN', 'TEMP', 'TEMPORARY'):
                            tbl = parts[1]
                    orphan_tables.add(tbl.upper())
    
    def references_table(sql, table_name):
        """Check if SQL references the given table by its name."""
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
    
    for pos, cmd_type, sql, expected, name, *extra in entries:
        if cmd_type == "reset_db":
            flush()
            tests.append({"name": "__RESET_DB__", "steps": [{"type": "reset_db"}]})
            continue
          
        if cmd_type == "auth":
            # Auth step: extra = [action, result]
            action = extra[0] if len(extra) > 0 else ""
            result = extra[1] if len(extra) > 1 else ""
            if action and result:
                current_steps.append({"type": "auth", "action": action, "result": result})
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


def main():
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    
    skip_files = set()
    for fname in os.listdir(TEST_DIR):
        if not fname.endswith('.test'):
            continue
        filepath = os.path.join(TEST_DIR, fname)
        with open(filepath, 'r', errors='replace') as f:
            content = f.read()
        if C_API_RE.search(content):
            skip_files.add(fname)
        # Also skip files that use TCL-level API wrappers (the sqlite3_ prefix is stripped in TCL)
        if re.search(r'\b(set_authorizer|create_function|create_collation|create_module|'
                     r'overload|declare_vtab|progress_handler|wal_hook|auto_extension|'
                     r'commit_hook|rollback_hook|update_hook|preupdate_hook|'
                     r'snapshot_get|snapshot_open|snapshot_free)\s',
                     content, re.IGNORECASE):
            skip_files.add(fname)
    
    print(f"Skipping {len(skip_files)} C API test files")
    
    active = excluded = no_sql = 0
    for fname in sorted(os.listdir(TEST_DIR)):
        if not fname.endswith('.test'):
            continue
        if fname in skip_files:
            excluded += 1
            continue
        filepath = os.path.join(TEST_DIR, fname)
        with open(filepath, 'r', errors='replace') as f:
            content = f.read()
        
        tests = extract_tests(content)
        if not tests:
            no_sql += 1
            continue
        
        out_name = re.sub(r'\.test$', '', fname)
        out_name = re.sub(r'[^a-zA-Z0-9]', '_', out_name)
        if not out_name or not out_name[0].isalpha():
            out_name = 'f_' + out_name
        out_name = out_name[:80]
        
        test_data = {
            "file": fname,
            "name": out_name,
            "tests": tests
        }
        
        out_path = os.path.join(OUTPUT_DIR, f"{out_name}.json")
        with open(out_path, 'w') as f:
            json.dump(test_data, f, indent=2)
        active += 1
    
    total_tests = 0
    for fname in os.listdir(OUTPUT_DIR):
        if fname.endswith('.json'):
            with open(os.path.join(OUTPUT_DIR, fname)) as f:
                data = json.load(f)
                total_tests += len(data.get("tests", []))
    
    print(f"Excluded (C API, non-SQL): {excluded}")
    print(f"No extractable SQL: {no_sql}")
    print(f"Generated test data files: {active}")
    print(f"Total test cases: {total_tests}")


if __name__ == "__main__":
    main()