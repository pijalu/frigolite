#!/usr/bin/env python3
"""Generate Go tests from SQLite TCL tests - only SQL tests. C API and zlib tests excluded."""
import os, re

TEST_DIR = "/Users/muaddib/dev/frigolite/ori/sqlite/test"
OUTPUT_DIR = "/Users/muaddib/dev/frigolite"

C_API_RE = re.compile(r'sqlite3_(prepare|step|column|finalize|exec\b|limit|db_config|config|enable_shared|initialize|shutdown|malloc|free|realloc|memory_used|memory_highwater|randomness|sleep|strglob|stricmp|strnicmp|strlike|create_function|create_collation|create_module|overload|declare_vtab|table_column_metadata|db_filename|db_readonly|db_handle|next_stmt|commit_hook|rollback_hook|update_hook|preupdate|wal_hook|auto_extension|cancel_auto_extension|reset_auto_extension|set_authorizer|trace|progress_handler|file_control|test_control|keyword_|compileoption|db_cacheflush|snapshot|unlock_notify|log|vtab|db_config|txn_state|changes|total_changes|errcode|errstr|threadsafe|serialize|deserialize|hard_heap|soft_heap|release_memory|db_release_memory|db_status|status)')

# Unsupported SQL features - tests containing these will be excluded
UNSUPPORTED = re.compile(
    r'\b(WINDOW\s|OVER\s|FILTER\s*\(|WAL\s|ATTACH\s|DETACH\s|VACUUM\s|'
    r'SAVEPOINT\s|RELEASE\s|ROLLBACK\s+TO\s|REINDEX\s|ANALYZE\s|'
    r'CREATE\s+VIRTUAL\s+TABLE\s|fts\d+\s*\(|rtree\s*\(|'
    r'WITHOUT\s+ROWID\s|\$\d+\b|zeroblob\s*\(|zipfile|'
    r'writecrash|'
    r'PRAGMA\s+(wal_|journal_mode=WAL|page_count|cache_flush|locking_mode|'
    r'schema_version|user_version|application_id|mmap_size|'
    r'soft_heap_limit|hard_heap_limit|threads|page_size=65536))',
    re.IGNORECASE
)
  
def has_unsupported_features(sql):
    """Check if SQL uses features the engine doesn't support."""
    return bool(UNSUPPORTED.search(sql))

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
    s = s.replace('\\', '\\\\')
    s = s.replace('"', '\\"')
    s = s.replace('\n', '\\n')
    s = s.replace('\r', '\\r')
    if len(s) > 300:
        cutoff = 297
        while cutoff > 260 and s[cutoff-1] == '\\':
            cutoff -= 1
        s = s[:cutoff] + '...'
    return s

def extract_sql_pairs(content):
    """Extract (sql, expected) pairs from TCL test content.
    For do_execsql_test and do_catchsql_test, expected is the result after SQL.
    For execsql and db eval, expected is None."""
    pairs = []
    
    # do_execsql_test / do_catchsql_test: format is: command name { sql } [expected]
    for m in re.finditer(r'(do_execsql_test|do_catchsql_test)\s+(\S+)\s*\{([^}]*)\}\s*(\{[^}]*\}|[^\n]*?)(?=\n\S|$)', content, re.DOTALL):
        sql = m.group(3).strip()
        if not sql:
            continue
        expected_raw = m.group(4).strip() if m.group(4) else ""
        expected = expected_raw.strip()
        if not expected:
            expected = None
        pairs.append((sql, expected, m.group(1)))
    
    # execsql: no expected result
    for m in re.finditer(r'execsql\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "execsql"))
    
    # db eval { sql }
    for m in re.finditer(r'db\s+eval\s*\{([^}]*)\}', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "db_eval"))
    
    # db eval "sql"
    for m in re.finditer(r'db\s+eval\s+"([^"]*)"', content):
        sql = m.group(1).strip()
        if sql:
            pairs.append((sql, None, "db_eval"))
    
    return pairs

def generate(filename, content):
    func_name = re.sub(r'\.test$', '', filename)
    func_name = re.sub(r'[^a-zA-Z0-9]', '_', func_name)
    if not func_name or not func_name[0].isalpha(): func_name = 'f_' + func_name
    func_name = func_name[:80]
    
    pairs = extract_sql_pairs(content)
    if not pairs:
        return None
    
    # Deduplicate by SQL, keep the first occurrence with expected
    seen = {}
    unique_pairs = []
    for sql, expected, cmd_type in pairs:
        if sql not in seen:
            seen[sql] = True
            unique_pairs.append((sql, expected))
    
    lines = [f'// Auto-generated from {filename}']
    lines.append(f'func TestSQLite_{func_name}(t *testing.T) {{')
    lines.append('\tdb := setupDB(t)')
    lines.append('\tdefer db.Close()')
    
    unique_pairs = unique_pairs[:40]  # limit to 40 per test function
      
    for sql, expected in unique_pairs:
        go_sql = go_escape(sql)
        is_query = bool(re.match(r'\s*SELECT\b|\s*PRAGMA\b|\s*EXPLAIN\b', sql, re.IGNORECASE))
          
        if is_query:
            if expected is not None:
                go_exp = go_escape(expected)
                lines.append(f'\tcheckQueryResult(t, db.Query("{go_sql}"), "{go_exp}")')
            else:
                lines.append(f'\t_ = db.Query("{go_sql}")')
        else:
            lines.append(f'\t_ = db.Exec("{go_sql}")')
    
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
