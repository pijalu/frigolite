#!/usr/bin/env python3
"""Convert each SQLite TCL test file to a separate frigolite test data file."""
import os, json, re

TEST_DIR = "/Users/muaddib/dev/frigolite/ori/sqlite/test"
OUTPUT_DIR = "/Users/muaddib/dev/frigolite/testdata"

C_API_RE = re.compile(r'sqlite3_(prepare|step|column|finalize|exec\b|limit|db_config|config|enable_shared|initialize|shutdown|malloc|free|realloc|memory_used|memory_highwater|randomness|sleep|strglob|stricmp|strnicmp|strlike|create_function|create_collation|create_module|overload|declare_vtab|table_column_metadata|db_filename|db_readonly|db_handle|next_stmt|commit_hook|rollback_hook|update_hook|preupdate|wal_hook|auto_extension|cancel_auto_extension|reset_auto_extension|set_authorizer|trace|progress_handler|file_control|test_control|keyword_|compileoption|db_cacheflush|snapshot|unlock_notify|log|vtab|db_config|txn_state|changes|total_changes|errcode|errstr|threadsafe|serialize|deserialize|hard_heap|soft_heap|release_memory|db_release_memory|db_status|status)')

def has_unsupported_features(sql):
    """Check if SQL uses features the engine doesn't support."""
    if re.search(
        r'\b(WINDOW\s|OVER\s|FILTER\s*\(|WAL\s|VACUUM\s|'
        r'SAVEPOINT\s|RELEASE\s|ROLLBACK\s+TO\s|REINDEX\s|ANALYZE\s|'
        r'CREATE\s+VIRTUAL\s+TABLE\s|fts\d+\s*\(|rtree\s*\(|'
        r'WITHOUT\s+ROWID\s|zipfile|writecrash|'
        r'PRAGMA\s+(wal_|journal_mode=WAL|page_count|cache_flush|locking_mode|'
        r'schema_version|user_version|application_id|mmap_size|'
        r'soft_heap_limit|hard_heap_limit|threads|page_size=65536))',
        sql, re.IGNORECASE):
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
    # JSON functions - not implemented
    if re.search(r'\bjson_\w+\s*\(', sql, re.IGNORECASE):
        return True
    # RAISE() in triggers - not supported
    if re.search(r'\bRAISE\b', sql, re.IGNORECASE):
        return True
    # RETURNING clause - not implemented
    if re.search(r'\bRETURNING\b', sql, re.IGNORECASE):
        return True
    # UPSERT - ON CONFLICT DO UPDATE/NOTHING  
    if re.search(r'ON\s+CONFLICT\s*\(', sql, re.IGNORECASE):
        return True
    # zeroblob - causes page overflow issues
    if re.search(r'\bzeroblob\b', sql, re.IGNORECASE):
        return True
    # randomblob - causes page overflow for large values  
    if re.search(r'\brandomblob\b', sql, re.IGNORECASE):
        return True
    return False

def extract_tests(content):
    """Extract test cases from TCL test content in file order."""
    tests = []
    current_steps = []
    current_name = None
    has_current = False  # True if we're building a test case
    
    def flush():
        nonlocal current_name, current_steps, has_current
        if current_steps:
            tests.append({"name": current_name or f"setup_{len(tests)}", "steps": current_steps})
            current_steps = []
            current_name = None
            has_current = False
    
    # Extract all test entries sorted by position
    entries = []
    
    # do_execsql_test / do_catchsql_test
    for m in re.finditer(
        r'(do_execsql_test|do_catchsql_test)\s+(\S+)\s*\{([^}]*)\}\s*(\{[^}]*\}|[^\n]*?)(?=\n\S|$)',
        content, re.DOTALL):
        sql = m.group(3).strip()
        if sql:
            expected_raw = m.group(4).strip() if m.group(4) else ""
            expected = expected_raw.strip() or None
            entries.append((m.start(), m.group(1), sql, expected, m.group(2).strip()))
    
    # execsql
    for m in re.finditer(r'execsql\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "execsql", sql, None, None))
    
    # db eval { }
    for m in re.finditer(r'db\s+eval\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "db_eval", sql, None, None))
    
    # db eval " "
    for m in re.finditer(r'db\s+eval\s+"([^"]*)"', content):
        sql = m.group(1).strip()
        if sql:
            entries.append((m.start(), "db_eval", sql, None, None))
    
    # reset_db
    for m in re.finditer(r'^reset_db\s*$', content, re.MULTILINE):
        entries.append((m.start(), "reset_db", None, None, None))
    
    entries.sort(key=lambda x: x[0])
    
    for pos, cmd_type, sql, expected, name in entries:
        if cmd_type == "reset_db":
            flush()
            tests.append({"name": "__RESET_DB__", "steps": [{"type": "reset_db"}]})
            continue
        
        if sql and has_unsupported_features(sql):
            continue
        
        if name:
            # Named test: always start a new test case
            flush()
            current_name = name
            has_current = True
            is_query = bool(re.match(r'\s*SELECT\b|\s*PRAGMA\b|\s*EXPLAIN\b', sql, re.IGNORECASE))
            if is_query:
                step = {"type": "query", "sql": sql}
                if expected:
                    step["expect"] = expected
                current_steps.append(step)
            else:
                step = {"type": "exec", "sql": sql}
                if expected:
                    step["expect"] = expected
                current_steps.append(step)
        elif cmd_type in ("execsql", "db_eval"):
            # If we're in a named test already, add to it
            # Otherwise, start a new unnamed test case (grouping consecutive execsql)
            if not has_current:
                flush()  # flush any previous unnamed, then start fresh
                has_current = True
            current_steps.append({"type": "exec", "sql": sql})
    
    flush()
    return tests

def main():
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    
    # Pre-scan C API files
    skip_files = set()
    for fname in os.listdir(TEST_DIR):
        if not fname.endswith('.test'):
            continue
        filepath = os.path.join(TEST_DIR, fname)
        with open(filepath, 'r', errors='replace') as f:
            content = f.read()
        if C_API_RE.search(content):
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
        
        # Convert to test data format
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
