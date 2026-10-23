# INSERT / UPDATE / DELETE performance comparison

Machine: Apple M4 Pro, macOS arm64. SQLite oracle: `/usr/bin/sqlite3` CLI. Existing Frigolite benchmarks use persisted file DBs, page_size=1024, cache_size=8192, locking_mode=EXCLUSIVE. Command: `go test . -run '^$' -bench '^BenchmarkPerf(Insert1|Update2|Update3|Delete1)$' -benchtime=1x -count=3`.

| Workload | Shape | Frigolite before | Frigolite after |
| --- | --- | ---: | ---: |
| Insert1 | 50,000 single-row INSERTs, one transaction | 64.3 ms | not rerun after change |
| Update2 | 2,000 point UPDATEs by indexed `a=` on 5,000-row table, one transaction | 11.7 ms | 12.5 ms |
| Update3 | `UPDATE t1 SET c=a` across 50,000 rows, one transaction | 12.61 s | 3.47 s |
| Delete1 | `DELETE FROM t1` on 50,000 rows, one transaction | 314.8 ms | 312 ms |

## Profiled optimization

`BenchmarkPerfUpdate3` improved ~3.6x after closing temporary seek cursors in `internal/btree/btree_update_inplace.go:seekLeafRow`. Each row rewrite called `OverwriteCellByRowID`, which used `seekLeafRow`; that helper opened and registered a cursor but never closed it. The cursor registry therefore grew by a cursor per row, and every subsequent mutation's `saveAllCursors` scanned the ever-growing registry. CPU profile before change: `saveAllCursors` 3.82 s cumulative (3.51 s flat); after: 0.33 s cumulative. SQLite's `saveAllCursors` in `/Users/muaddib/dev/sqlite/src/btree.c:806-815` walks live cursors. Closing helper cursor preserves behavior because helper returns page coordinates, not cursor state.

SQLite CLI timings are directional only: process startup included and scripts batch phases unlike API calls. A 50,000-row INSERT + full-table UPDATE + DELETE SQLite script took 60 ms wall (40 ms user); separate 5,000-row setup + 2,000 point updates took 10–20 ms wall. No meaningful engine-only ratio inferred from CLI comparison. The profiled registry leak was the justified optimization; remaining full-update cost still material, but profile after change is dominated by synchronous syscalls (~54% CPU profile), so further tuning needs separate I/O investigation.

Reproduce Frigolite numbers with command above. Benchmark shapes in `frigolite_perf_bench_test.go`. Persisted SQLite correctness test: `go test . -run '^TestSQLiteInsertUpdateDeletePersisted$' -count=1`.
