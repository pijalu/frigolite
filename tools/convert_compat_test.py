#!/usr/bin/env python3
"""Generate Go tests from SQLite TCL tests - only SQL tests. C API and zlib tests excluded."""
import os, re

TEST_DIR = "/Users/muaddib/dev/frigolite/ori/sqlite/test"
OUTPUT_DIR = "/Users/muaddib/dev/frigolite"

C_API_RE = re.compile(r'sqlite3_(prepare|step|column|finalize|exec\b|limit|db_config|config|enable_shared|initialize|shutdown|malloc|free|realloc|memory_used|memory_highwater|randomness|sleep|strglob|stricmp|strnicmp|strlike|create_function|create_collation|create_module|overload|declare_vtab|table_column_metadata|db_filename|db_readonly|db_handle|next_stmt|commit_hook|rollback_hook|update_hook|preupdate|wal_hook|auto_extension|cancel_auto_extension|reset_auto_extension|set_authorizer|trace|progress_handler|file_control|test_control|keyword_|compileoption|db_cacheflush|snapshot|unlock_notify|log|vtab|db_config|txn_state|changes|total_changes|errcode|errstr|threadsafe|serialize|deserialize|hard_heap|soft_heap|release_memory|db_release_memory|db_status|status)')

# Unsupported SQL features - tests containing these will be excluded
# NOTE: Only features that the engine truly cannot handle should be here.
# Features that work as no-ops (ATTACH, SAVEPOINT, etc.) are NOT filtered.
UNSUPPORTED = re.compile(
    r'\b(WINDOW\s|OVER\s|'
    r'fts\d+\s*\(|rtree\s*\(|'
    r'WITHOUT\s+ROWID\s|\$\d+\b|zeroblob\s*\(|zipfile|'
    r'writecrash|'
    r'PRAGMA\s+(wal_|journal_mode=WAL|page_count|cache_flush|locking_mode|'
    r'schema_version|user_version|application_id|mmap_size|'
    r'soft_heap_limit|hard_heap_limit|threads|page_size=65536))',
    re.IGNORECASE
)
  
def has_unsupported_features(sql):
    """Check if SQL uses features the engine doesn't support."""
    if bool(UNSUPPORTED.search(sql)):
        return True
    # Additional TCL-specific checks
    # TCL variable references like $var (but not $N positional params like $1)
    if re.search(r'(?<!\w)\$[a-zA-Z_]\w*', sql):
        return True
    # TCL variable substitution ${var}
    if '${' in sql:
        return True
    # TCL brace escaping (uneven braces in SQL) 
    # Note: With proper brace extraction, SQL should never contain { or }
    if '{' in sql or '}' in sql:
        return True
    # TCL command embedded in SQL
    if re.search(r'\bsql\s*\{', sql):
        return True
    # TCL virtual table module
    if re.search(r'\btcl\s*\(', sql, re.IGNORECASE) or 'vtab_command' in sql:
        return True
    return False

def has_sql(content):
    for pattern in [r'execsql\s*\{([^}]*)\}', r'd(?:o_execsql|o_catchsql)?_test\s+\S+\s*\{([^}]*)\}', r'db\s+eval\s*\{([^}]*)\}']:
        for m in re.finditer(pattern, content):
            if m.group(1).strip():
                return True
    return False

# Pre-scan: identify files that are NOT SQL tests
# A file is considered non-SQL only if it has C API calls AND no extractable SQL
skip_files = set()
for fname in os.listdir(TEST_DIR):
    if not fname.endswith('.test'): continue
    filepath = os.path.join(TEST_DIR, fname)
    with open(filepath, 'r', errors='replace') as f:
        content = f.read()
    if C_API_RE.search(content) and not has_sql(content):
        skip_files.add(fname)

print(f"Skipping {len(skip_files)} non-SQL test files")

def go_escape(s):
    s = str(s)
    result = []
    for ch in s:
        code = ord(ch)
        if code == 0x5c:  # backslash
            result.append('\\\\')
        elif code == 0x22:  # double quote
            result.append('\\"')
        elif code == 0x0a:  # newline
            result.append('\\n')
        elif code == 0x0d:  # carriage return
            result.append('\\r')
        elif code == 0x09:  # tab
            result.append('\\t')
        elif code < 0x20:  # other control characters
            result.append('\\x%02x' % code)
        elif code >= 0x80:  # non-ASCII characters
            result.append('\\x%02x' % code)
        else:
            result.append(ch)
    return ''.join(result)

def tcl_variable_substitute(sql):
    """Substitute known TCL variables with their SQL equivalents."""
    # $::temp (tempdb available → TEMP)
    sql = re.sub(r'\$::temp\b', 'TEMP', sql)
    # ${::temp} (brace form)
    sql = re.sub(r'\$\{::temp\}', 'TEMP', sql)
    return sql

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
    return None  # Unbalanced braces

def extract_sql_pairs(content):
    """Extract (sql, expected) pairs from TCL test content in file order.
    For do_execsql_test and do_catchsql_test, expected is the result after SQL.
    For execsql and db eval, expected is None.
    Returns pairs in the order they appear in the file."""
    pairs = []
    
    # Phase 1: Extract do_execsql_test and do_catchsql_test using brace counting
    pattern = r'(do_execsql_test|do_catchsql_test)\s+(\S+)\s*'
    for m in re.finditer(pattern, content):
        cmd_type = m.group(1)
        pos = m.end()
        
        # Skip whitespace before SQL body opening brace
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        
        if pos >= len(content) or content[pos] != '{':
            continue
        
        # Extract SQL body using balanced brace matching
        result = extract_balanced_braces(content, pos)
        if result is None:
            continue
        sql_body, pos = result
        sql_body = sql_body.strip()
        if not sql_body:
            continue
        
        # Skip whitespace before expected result (or next test)
        while pos < len(content) and content[pos] in ' \t\n\r':
            pos += 1
        
        # Check for expected result (another balanced brace block)
        expected = None
        if pos < len(content) and content[pos] == '{':
            exp_result = extract_balanced_braces(content, pos)
            if exp_result is not None:
                expected_raw, _ = exp_result
                expected = expected_raw.strip()
                if not expected:
                    expected = None
        
        pairs.append((sql_body, expected, cmd_type, m.start()))
    
    # Phase 2: Extract execsql { ... } patterns
    for m in re.finditer(r'execsql\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "execsql", m.start()))
      
    # Phase 3: Match execsql [subst -nocommands { SQL }] patterns
    for m in re.finditer(r'execsql\s*\[subst -nocommands\s*\{([^}]*)\}\]', content):
        sql = m.group(1).strip()
        if sql:
            sql = tcl_variable_substitute(sql)
            pairs.append((sql, None, "execsql", m.start()))
    
    # Phase 4: Match execsql [subst { SQL }] patterns (full substitution)
    for m in re.finditer(r'execsql\s*\[subst\s+\{([^}]*)\}\]', content):
        sql = m.group(1).strip()
        if sql:
            sql = tcl_variable_substitute(sql)
            pairs.append((sql, None, "execsql", m.start()))
    
    # Phase 5: Match execsql { ... } inside ifcapable blocks
    for m in re.finditer(r'ifcapable\s+\w+\s*\{[^}]*execsql\s*\{([^}]*)\}[^}]*\}', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "execsql", m.start()))
    
    # Phase 6: Match db eval patterns
    for m in re.finditer(r'db\s+eval\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "db_eval", m.start()))
    
    for m in re.finditer(r'db\s+eval\s+"([^"]*)"', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "db_eval", m.start()))
    
    # Phase 7: Match reset_db
    for m in re.finditer(r'^reset_db\s*$', content, re.MULTILINE):
        pairs.append(('__RESET_DB__', None, 'reset_db', m.start()))
    
    # Sort by position in file to maintain original order
    pairs.sort(key=lambda x: x[3])
    
    # Remove duplicates (keep first occurrence) while preserving order
    seen = set()
    unique = []
    for sql, expected, cmd_type, pos in pairs:
        key = (sql, cmd_type)
        if key not in seen:
            seen.add(key)
            unique.append((sql, expected, cmd_type, pos))
    
    # Return only the first 3 fields (sql, expected, cmd_type)
    return [(sql, expected, cmd_type) for sql, expected, cmd_type, _ in unique]

def generate(filename, content):
    func_name = re.sub(r'\.test$', '', filename)
    func_name = re.sub(r'[^a-zA-Z0-9]', '_', func_name)
    if not func_name or not func_name[0].isalpha(): func_name = 'f_' + func_name
    func_name = func_name[:80]
    
    pairs = extract_sql_pairs(content)
    if not pairs:
        return None
    
    # Deduplicate by SQL (not cmd_type), keep the first occurrence with expected
    seen = {}
    unique_pairs = []
    for sql, expected, cmd_type in pairs:
     # Handle reset_db: create a fresh database
     if sql == '__RESET_DB__':
      unique_pairs.append(('__RESET_DB__', None, 'reset_db'))
      continue
     # Skip SQL with unsupported/TCL-specific features
     if has_unsupported_features(sql):
      continue
     if sql not in seen:
      seen[sql] = True
      unique_pairs.append((sql, expected, cmd_type))
 	
    if not unique_pairs:
     return None
    
    lines = [f'// Auto-generated from {filename}']
    lines.append(f'func TestSQLite_{func_name}(t *testing.T) {{')
    lines.append('\tdb := setupDB(t)')
    # Track databases for cleanup and reset
    lines.append('\tvar dbs []*DB')
    lines.append('\tdbs = append(dbs, db)')
    
    max_pairs = 60  # limit per test function (balance coverage vs runtime)
    unique_pairs = unique_pairs[:max_pairs]
    
    for sql, expected, cmd_type in unique_pairs:
        if sql == '__RESET_DB__':
            # Reset the database by closing and reopening
            lines.append('\tdb.Close()')
            lines.append('\tdb = setupDB(t)')
            lines.append('\tdbs = append(dbs, db)')
            continue
        
        go_sql = go_escape(sql)
        is_query = bool(re.match(r'\s*SELECT\b|\s*PRAGMA\b|\s*EXPLAIN\b', sql, re.IGNORECASE))
          
        if is_query:
            if expected is not None:
                go_exp = go_escape(expected)
                lines.append(f'\tcheckQueryResult(t, db.Query("{go_sql}"), "{go_exp}")')
            else:
                lines.append(f'\t_ = db.Query("{go_sql}")')
        elif cmd_type == 'do_catchsql_test':
            # catchsql tests expect a specific error code
            # Expected format: {N {message}} where N is error code
            # If N == 0, expect success; if N != 0, expect error
            if expected and expected.startswith('{0'):
                # Expected success (error code 0)
                lines.append(f'\tcheckExecOK(t, db.Exec("{go_sql}"))')
            else:
                # Expected error - verify that an error occurs
                # Using if-with-assignment to scope the variable
                lines.append(f'\tif err := db.Exec("{go_sql}").Error; err == nil {{')
                lines.append(f'\t\tt.Errorf("expected error but got none")')
                lines.append(f'\t}}')
        else:
            lines.append(f'\tcheckExecOK(t, db.Exec("{go_sql}"))')
    
    # Close all databases
    lines.append('\tfor _, d := range dbs { d.Close() }')
    lines.append('}')
    return '\n'.join(lines)

all_code = [
    'package frigolite',
    '',
    'import (',
    '\t"testing"',
    ')',
    '',
    '// Auto-generated SQLite compatibility test suite',
    '// Only includes SQL-related tests (C API and extension tests excluded)',
    '',
]

active = excluded = no_sql = 0
for fname in sorted(os.listdir(TEST_DIR)):
    if not fname.endswith('.test'): continue
    if fname in skip_files:
        excluded += 1
        continue
    filepath = os.path.join(TEST_DIR, fname)
    with open(filepath, 'r', errors='replace') as f:
        content = f.read()
    code = generate(fname, content)
    if code is None:
        no_sql += 1
        continue
    active += 1
    all_code.append(code)

output_file = os.path.join(OUTPUT_DIR, 'frigolite_sqlite_compat_test.go')
with open(output_file, 'w') as f:
    f.write('\n'.join(all_code))
print(f"Excluded (C API, non-SQL): {excluded}")
print(f"No extractable SQL: {no_sql}")
print(f"Generated active tests: {active}")
print(f"Total in ori/sqlite/test: {excluded+no_sql+active}")
