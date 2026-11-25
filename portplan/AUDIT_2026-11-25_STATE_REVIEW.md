# Frigolite state review — correctness, SQLite file compatibility, performance

**Date:** 2026-11-25 · **HEAD:** `830745db3` · **Oracle:** sqlite3 3.54.0 · **Mode:** review (evidence only; no engine changes in this document).

## Verdicts on the two hard requirements

- **SQL correctness (1:1): NOT MET.** The current full harness has 1,022 failing leaf cases (t30 baseline: 1,028). 374 of them are `result mismatch`, 113 `expected error but got success`, 39 `error mismatch` and 53 planner `query_assign mismatch`. Representative divergences are reproduced below. "NEW=0 vs t30" means *no regression*, not parity.
- **File compatibility (100% read/write): NOT ESTABLISHED, therefore NOT MET.** Only one narrow cross-engine test exists (`TestS66SQLiteFileCompatibility`, one 500-row two-column blob index shape). Baseline failures include file-format/extension-visible divergences (`stat`, `misc5` reopen, `dbstat` column types, WAL lock/trace state), so breadth is not proven.

Both gaps are tracked by the follow-up goals; this document is the issue inventory they consume.

## 1. Native test results (`go test -timeout 20m ./...`)

Command: `go test -timeout 20m ./... > /tmp/audit_native_all.log 2>&1` → **exit 1**.

```
panic: test timed out after 20m0s
running tests:
        TestSQLiteSuite (18m45s)
        TestSQLiteSuite/vacuummem (15s)
        TestSQLiteSuite/vacuummem/1.0 (15s)
FAIL     github.com/pijalu/frigolite  1200.585s
```

Package outcomes: **26 ok**, **2 FAIL** (root package + aggregate `FAIL` line), **15 no-test packages**. The root failure is *only* the single-process harness timeout (`TestSQLiteSuite`); no native non-harness test failed before the timeout. The sharded runner (`tools/run_json_shards.sh`) is the supported way to run the same suite and is used in §2.

## 2. Full harness (12 shards, fresh at this HEAD)

Binary `/tmp/auditgate/frigolite.test`; run dir `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.hfLDSm`; 12/12 shard logs and status files, no `panic: test timed out`/`signal: killed`.

| Metric | Current | t30 baseline | Delta |
|---|---:|---:|---:|
| leaf FAIL names | 1022 | 1028 | NEW=0, GONE=6 |
| family FAIL names | 190 | 192 | NEW=0, GONE=2 |

GONE=6 leaves: `insert-17.15`, `insert-5.3`, `resetdb-100`, `resetdb-300`, `temptable2-4.1.4/5.1.2`, `vacuum-into-500` — all fixed in commits already at HEAD. Family-level lines were counted separately; a broad `TestSQLiteSuite/\S+` regex wrongly reports 190 "NEW families".

Failure-signature census (primary assertion of each baseline leaf, from the same shard logs):

| Count | Signature | Interpretation |
|---:|---|---|
| 374 | `frigolite_harness_test.go:5829 result mismatch` | result-affecting divergence (needs per-case triage) |
| 113 | `:5709 expected error but got success` | engine accepts SQL/state SQLite rejects |
| 113 | `:6162 runtime variable mismatch` | TCL/VFS hook state, mostly harness-visible |
| 80 | `:5938 Tcl query error` (unsupported Tcl query) | harness fixture gap |
| 53 | `:5534 query_assign mismatch` | planner / `EXPLAIN QUERY PLAN` divergence |
| 45 | `:4771 unknown prepared statement` | harness fixture gap |
| 39 | `:5714 error mismatch` | observable error-text divergence |
| 211 | assorted exec errors / runtime expressions / no-detail | mixed engine + fixture |

Largest families: `normalize` 46, `e_blobopen` 30, `rtree1` 26, `skipscan5` 26, `wal2` 25, `json101` 24, `pragma` 23, `vtab1` 21.

## 3. Performance vs SQLite 3.54 (fresh paired run)

Commands (identical flags, literal-SQL mode, `-reps 1 -index -point-ops 200`):

```
cd benchmarks/perfbench/frigolite && go run . -reps 1 -index -point-ops 200
cd benchmarks/perfbench/sqlite    && go run . -reps 1 -index -point-ops 200
```

Raw logs: `/tmp/audit_perf_frigo.log`, `/tmp/audit_perf_sqlite.log`. Ratio = SQLite ÷ Frigolite (higher = Frigolite slower).

| Phase | Frigolite ops/s | SQLite ops/s | Ratio | Prior log ratio |
|---|---:|---:|---:|---:|
| `count-star` | 1,636,104,239 | 11,616,695,515 | 7.10× | 2.13× |
| `indexed-delete` | 141,697 | 689,675 | 4.87× | 3.36× |
| `insert` | 170,650 | 659,519 | 3.86× | 4.20× |
| `scan` | 20,106,279 | 75,867,733 | 3.77× | 2.94× |
| `indexed-select` | 176,776 | 641,711 | 3.63× | 2.90× |
| `indexed-update` | 144,504 | 572,793 | 3.96× | 3.15× |
| `group-by` | 87 | 261 | 3.00× | 2.47× |
| `point-delete` | 239,330 | 569,732 | 2.38× | 2.06× |
| `point-select` | 359,658 | 729,041 | 2.03× | 1.40× |
| `point-update` | 270,468 | 517,576 | 1.91× | 1.73× |
| `file-insert` | 10,800 | 5,237 | **0.48× (2.1× faster)** | 0.48× |

Single-repetition noise is visible against the prior log; `count-star` and `point-select` moved most. The stable finding is the 2–5× indexed/rowid CRUD gap and 3–7× aggregate/scan gap. `batch-point-select` exists only in the Frigolite driver (no SQLite counterpart in this harness), so it is excluded from the table. The older `benchmarks/PERF_REPORT_2026-09-28.md` (60–15,000×) predates later allocator/planner work; use the fresh numbers as the current baseline.

## 4. Severity-ranked issue inventory

Evidence keys: `shard-N.log:L` = line in the fresh run dir above; `frigolite_harness_test.go:L` = assertion site. All family repros use the documented substring filter:
`FRIGOLITE_TEST=<family> go test -run '^TestSQLiteSuite$' -count=1 .`

### Severity 1 — result-affecting SQL divergence (correctness)

| ID | Issue | Evidence | Repro |
|---|---|---|---|
| C1 | `e_blobopen-3.2.5`: `SELECT c FROM x1 WHERE a=2` returns `???`, SQLite expects `xxx` (blob/value rendering). | `shard-11.log:2705`; `testdata/e_blobopen.json` case `e_blobopen-3.2.5`; assertion `frigolite_harness_test.go:5829` | `FRIGOLITE_TEST=e_blobopen ...` |
| C2 | `fkey1-8.3`: `REINDEX;` succeeds where SQLite errors (`expected error but got success`). | `shard-9.log:1728`; assertion `:5709` | `FRIGOLITE_TEST=fkey1 ...` |
| C3 | `join3-3.1`: got `no such table: t1`, want `at most 64 terms in a join`; error text and limit both divergent. | `shard-9.log:12333`; assertion `:5714` | `FRIGOLITE_TEST=join3 ...` |
| C4 | `skipscan5-1.3.3`: EQP reports `SEARCH t1 USING INDEX i1`, fixture wants `*SCAN t1*`; planner divergence is an observable SQL result. | `shard-3.log:35984`; `internal/execquery/explain.go:38-105` | `FRIGOLITE_TEST=skipscan5 ...` |
| C5 | `vtab1-4.1`: `ORDER BY b` query-assign got `sort`, want `nosort`. | `shard-8.log:20117`; assertion `:5534` | `FRIGOLITE_TEST=vtab1 ...` |
| C6 | `normalize-350`: `prepare: unrecognized token: "x'abc'"`; lexer has hex-blob handling at `internal/sql/lexer.go:527-545`, so the failure is in the prepare/AST path. | `shard-7.log:14114`; assertion `:4673` | `FRIGOLITE_TEST=normalize ...` |
| C7 | `triggerC-3.3.1`: `SELECT * FROM t3b` → `no such table: t3b`; fixture `testdata/triggerC.json` creates `t3` but never `t3b` → likely fixture-conversion artifact, triage before engine edit. | `shard-5.log:28922`; case data | `FRIGOLITE_TEST=triggerC ...` |

### Severity 2 — SQLite file-format / extension compatibility

| ID | Issue | Evidence | Repro |
|---|---|---|---|
| F1 | `stat-0.0`: `PRAGMA table_info(dbstat)` returns NULL types where SQLite declares `name TEXT ... pgsize INTEGER`; `dbstat` exposes column *names* only. | `shard-5.log:25189`; `internal/vtab/dbstat.go:56-58` (`Columns()`, no type info) | `FRIGOLITE_TEST=stat ...` |
| F2 | `misc5/misc5-4.1#01`: reopen schema prime → `file is not a database`; file reopen/prime path rejects a file SQLite accepts. | `shard-7.log:3323`; assertion `:4337` | `FRIGOLITE_TEST=misc5 ...` |
| F3 | Cross-engine coverage is one shape only: `TestS66SQLiteFileCompatibility` (`frigolite_s66_cross_engine_test.go:41`), 500-row two-column blob index, both directions. No matrix for WAL, without-rowid, overflow, FTS, virtual tables, or auto_vacuum page reuse. | `go test -run 'Compat\|CrossEngine\|SQLiteFile' -list ... .` lists exactly 1 test | `go test -run TestS66SQLiteFileCompatibility -count=1 .` |
| F4 | WAL-family leaves (`wal2` 25, plus `wal3`) include lock/trace-state mismatches; partial WAL mode coverage remains (AGENTS: WAL in progress). | harness census above; `internal/pager/wal.go` (1385 lines, hard-size gate) | `FRIGOLITE_TEST=wal2 ...` |

### Severity 3 — performance

| ID | Issue | Evidence | Repro |
|---|---|---|---|
| P1 | Indexed UPDATE/SELECT/DELETE 3.6–4.9× slower; rowid point ops ~2×; scan/count-star 3.8–7.1×. | §3 fresh table; `benchmarks/R14_RESEARCH.md` | perfbench commands in §3 |
| P2 | `PERF` TODO item still open; older report's 8–15,000× figures were dominated by allocation/GC, now reduced but not closed. | `TODO.md:99-104` | `go test -run '^TestNativeSecondaryIndexPerf' .` (if pin exists) |

### Severity 4 — quality / gate debt

| ID | Issue | Evidence | Repro |
|---|---|---|---|
| Q1 | `tools/quality_gate.sh` exits 1: 10 staticcheck findings, 220 functions over gocognit 15, 202 over gocyclo 12, 18 files over the 1000-line hard limit. Worst: `tools/tclconvert/tcl/interp_sql.go` gocognit 121, 1699 lines; `internal/execddl/alter_rename.go` gocognit 104, 1335 lines. | `/tmp/audit_quality_gate.log`; `TODO.md:105-108` | `bash tools/quality_gate.sh` |
| Q2 | `go test ./...` cannot finish under 20m in one process because `TestSQLiteSuite` runs unsharded; normal `go test ./...` is therefore red without `-timeout` + sharding. | `/tmp/audit_native_all.log` | `go test ./...` |

## 5. Triage outcomes (post-audit investigations)

- **C1** (`e_blobopen-3.2.5`): the converted fixture dropped the incremental-blob write of `xxx`; both engines return the stored blob bytes. Converter artifact.
- **C2** (`fkey1-8.3`): fixture has only `REINDEX;` and no corrupt-database setup; sqlite3 3.54 also succeeds. Converter artifact.
- **C3** (`join3-3.1`): fixture's expected join-limit text lost its runtime number; sqlite3 3.54 with the fixture's missing `t1` reports `no such table: t1`, same as Frigolite. Converter artifact.
- **C5** (`vtab1-4.1`): row values match (`2 5`); only the harness TCL `sortMarker` differs (`sort` vs `nosort`), not a SQL result divergence.
- **C6** (`normalize-350` + 45 siblings): `SELECT x'abc'` errors identically in both engines; fixture `prepare_stmt` stores `code` while `stmt_finalize` names `stmt_4`/`stmt_10`. Converter variable mismatch.
- **C7** (`triggerC-3.3.1`): sqlite3 3.54 also reports `no such table: t3b`; the converted case never creates `t3b`. Converter artifact.

**Fixed**: C4-adjacent `e_select-1.7.4b.1/.2/.3` (derived-table name + collation, commit `5d87ce273`) and F1 (`dbstat` declared column types).
**Fixed later**: F2 (junk-header file now reports `file is not a database` on writes, ahead of the READONLY gate; `Open` stays lazy) and F3 (four-shape cross-engine matrix added, both read directions). **F4 reclassified**: the `wal2` residue is dominated by `:6162 runtime variable mismatch` — the converted TCL test expects C `xShmLock` traces (`{4 1 lock exclusive}` …) that Frigolite's in-process VFS does not emit; `wal2-10.1.4/10.2.3` expect `unable to open database file` from a VFS that denies xShm, another harness/VFS observable rather than a SQL result divergence. No SQL row-set/error mismatch was found in the sampled leaves. **Out of scope for this goal**: P1/P2 perf (next queued goal) and Q1/Q2 quality debt.

## 6. Scope note for follow-up goals

- "NEW=0 vs t30" is a *no-regression* gate, not a parity gate. Closing the two hard requirements needs the 1,022 failing leaves driven down (after separating genuine engine divergences from harness/fixture artifacts) and a substantially broader cross-engine file matrix.
- The follow-up goals consume this inventory: correctness + file-compat fixes first, then profiled performance remediation. No engine code was changed by this review.
