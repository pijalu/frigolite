#!/usr/bin/env python3
"""
Test benchmark and categorization tool.

Measures each test file against both SQLite and Frigolite,
categorizes by speed, and produces a comparison table.

Usage:
  python3 tools/benchmark_tests.py                    # benchmark all test files
  python3 tools/benchmark_tests.py --fast-only        # only show fast tests
  python3 tools/benchmark_tests.py --slow-only        # only show slow/medium tests
  python3 tools/benchmark_tests.py --compare FILE     # compare specific file
  python3 tools/benchmark_tests.py --export-csv FILE  # export results as CSV
"""

import json, time, subprocess, sys, glob, os, argparse

# Timeout per test file (seconds)
PER_FILE_TIMEOUT = 15

def run_sqlite_safe(sql_blocks, timeout=PER_FILE_TIMEOUT):
    """Run SQL against SQLite with timeout, return (elapsed_seconds, ok_flag, output)"""
    if not sql_blocks:
        return 0.0, True, ""
    combined_sql = '\n'.join(sql_blocks) + '\n'
    start = time.time()
    try:
        result = subprocess.run(
            ['sqlite3', ':memory:'],
            input=combined_sql,
            capture_output=True, text=True, timeout=timeout
        )
        elapsed = time.time() - start
        return elapsed, result.returncode == 0, result.stderr[:200] if result.returncode != 0 else ""
    except subprocess.TimeoutExpired:
        return timeout, False, "TIMEOUT"
    except FileNotFoundError:
        return 0.0, False, "sqlite3 not found"
    except Exception as e:
        return time.time() - start, False, str(e)


def categorize(time_sec):
    if time_sec < 0.1:
        return "FAST"
    elif time_sec < 1.0:
        return "MED"
    else:
        return "SLOW"


def load_test_sql(filepath):
    """Load all SQL from a test file, preserving execution order."""
    with open(filepath) as f:
        data = json.load(f)
    sql_blocks = []
    for t in data.get('tests', []):
        for step in t.get('steps', []):
            sql = step.get('sql', '').strip()
            if sql:
                sql_blocks.append(sql)
    return sql_blocks


def benchmark_file(filepath, run_frigolite=False):
    """Benchmark a single test file against SQLite (and optionally Frigolite)."""
    fname = os.path.basename(filepath)
    base = fname.replace('.json', '')
    
    sql_blocks = load_test_sql(filepath)
    if not sql_blocks:
        return None
    
    # SQLite benchmark
    sqlite_time, sqlite_ok, sqlite_err = run_sqlite_safe(sql_blocks)
    
    result = {
        'name': base,
        'file': fname,
        'sql_count': len(sql_blocks),
        'sqlite_time': round(sqlite_time, 4),
        'sqlite_ok': sqlite_ok,
        'sqlite_err': sqlite_err,
        'category': categorize(sqlite_time),
    }
    
    # Frigolite benchmark (optional, slow)
    if run_frigolite:
        frig_start = time.time()
        frig_result = subprocess.run(
            ['go', 'test', '-v', '-count=1', '-run', f'^TestSQLiteSuite/{base}$', '.'],
            capture_output=True, text=True, timeout=60,
            cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # project root
        )
        result['frigolite_time'] = round(time.time() - frig_start, 4)
        result['frigolite_ok'] = frig_result.returncode == 0
    
    return result


def main():
    parser = argparse.ArgumentParser(description='Benchmark SQLite test files')
    parser.add_argument('--fast-only', action='store_true', help='Only show fast tests')
    parser.add_argument('--slow-only', action='store_true', help='Only show slow/medium tests')
    parser.add_argument('--compare', type=str, help='Compare a specific test file')
    parser.add_argument('--export-csv', type=str, help='Export results to CSV file')
    parser.add_argument('--run-frigolite', action='store_true', help='Also run Frigolite (slow)')
    parser.add_argument('--limit', type=int, default=0, help='Limit number of files to process')
    args = parser.parse_args()
    
    # Determine project root (directory containing this script's parent)
    script_dir = os.path.dirname(os.path.abspath(__file__))
    project_root = os.path.dirname(script_dir)
    testdata_dir = os.path.join(project_root, 'testdata')
    
    if not os.path.isdir(testdata_dir):
        print(f"Error: testdata directory not found at {testdata_dir}")
        sys.exit(1)
    
    # Single file comparison mode
    if args.compare:
        filepath = os.path.join(testdata_dir, args.compare + '.json')
        if not os.path.exists(filepath):
            filepath = args.compare  # try as-is
        if not os.path.exists(filepath):
            print(f"Error: file not found: {args.compare}")
            sys.exit(1)
        result = benchmark_file(filepath, run_frigolite=True)
        if not result:
            print(f"Error: no SQL found in {filepath}")
            sys.exit(1)
        print(f"\n{'='*60}")
        print(f"Comparison for: {result['name']}")
        print(f"{'='*60}")
        print(f"  SQL count:  {result['sql_count']}")
        print(f"  SQLite:     {result['sqlite_time']:.4f}s {'OK' if result['sqlite_ok'] else 'ERR'}")
        print(f"  Frigolite:  {result['frigolite_time']:.4f}s {'PASS' if result['frigolite_ok'] else 'FAIL'}")
        if result['sqlite_time'] > 0 and result['frigolite_time'] > 0:
            ratio = result['frigolite_time'] / result['sqlite_time']
            print(f"  Ratio:      {ratio:.1f}x slower")
        return
    
    # Batch mode: process all test files
    test_files = sorted(glob.glob(os.path.join(testdata_dir, '*.json')))
    print(f"Processing {len(test_files)} test files...\n")
    
    results = []
    errors = 0
    timeouts = 0
    
    for i, fpath in enumerate(test_files):
        if args.limit and i >= args.limit:
            break
        
        base = os.path.basename(fpath).replace('.json', '')
        
        result = benchmark_file(fpath)
        if result is None:
            continue
        
        results.append(result)
        
        # Print progress
        symbol = {
            'FAST': '.',
            'MED': 'm',
            'SLOW': 'S',
        }.get(result['category'], '?')
        
        if result.get('sqlite_err') == 'TIMEOUT':
            symbol = 'T'
            timeouts += 1
        elif not result['sqlite_ok']:
            symbol = 'E'
            errors += 1
        
        print(f"{symbol}", end='', flush=True)
        if (i + 1) % 80 == 0:
            print(f"  {i+1}/{len(test_files)}")
    
    print(f"\n  {len(test_files)}/{len(test_files)} done\n")
    
    # Categorize
    by_cat = {'FAST': [], 'MED': [], 'SLOW': []}
    for r in results:
        by_cat[r['category']].append(r)
    
    # Summary table
    print(f"{'='*80}")
    print(f"SUMMARY")
    print(f"{'='*80}")
    print(f"  Total: {len(results)} files")
    print(f"  FAST:  {len(by_cat['FAST'])} files (< 0.1s)")
    print(f"  MED:   {len(by_cat['MED'])} files (< 1.0s)")
    print(f"  SLOW:  {len(by_cat['SLOW'])} files (>= 1.0s)")
    print(f"  Errors: {errors}  Timeouts: {timeouts}")
    print(f"  Total SQLite batch time: {sum(r['sqlite_time'] for r in results):.3f}s\n")
    
    # Slowest files table
    if args.slow_only or not args.fast_only:
        all_sorted = sorted(results, key=lambda x: x['sqlite_time'], reverse=True)
        print(f"{'FILE':<30} {'CAT':<6} {'SQLs':<6} {'Time(s)':<10} {'Status'}")
        print(f"{'-'*60}")
        for r in all_sorted[:30]:
            status = 'OK' if r['sqlite_ok'] else ('TIMEOUT' if r.get('sqlite_err') == 'TIMEOUT' else 'ERR')
            print(f"  {r['name']:<28} {r['category']:<6} {r['sql_count']:<6} {r['sqlite_time']:<10.4f} {status}")
    
    # Fast files (only if --fast-only)
    if args.fast_only:
        fast_sorted = sorted(by_cat['FAST'], key=lambda x: x['sql_count'], reverse=True)
        print(f"\n{'FAST FILES':<30} {'SQLs':<6} {'Time(s)'}")
        print(f"{'-'*45}")
        for r in fast_sorted[:20]:
            print(f"  {r['name']:<28} {r['sql_count']:<6} {r['sqlite_time']:<.4f}")
    
    # Export CSV
    if args.export_csv:
        import csv
        with open(args.export_csv, 'w', newline='') as csvfile:
            fieldnames = ['name', 'category', 'sql_count', 'sqlite_time', 'sqlite_ok']
            writer = csv.DictWriter(csvfile, fieldnames=fieldnames)
            writer.writeheader()
            for r in sorted(results, key=lambda x: x['name']):
                writer.writerow({k: r[k] for k in fieldnames})
        print(f"\nResults exported to {args.export_csv}")


if __name__ == '__main__':
    main()
