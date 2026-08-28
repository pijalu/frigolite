# Sub-Plan: P6.DEFERRED — Deferred WAL/Concurrency Packages

> **Parent**: `plans/subplans/P6_TRIAGE.md` (§G6.DEFERRED)
> **Scope**: 33 testgen packages that require WAL mode or shared-memory
> concurrency — features frigolite does not implement (rollback journal only).
> These packages are **deferred** (future work), not excluded and not N/A.

---

## Goal

```
Objective: Document the 33 WAL/concurrency testgen packages as deferred
(future work), not excluded. Each deferred package is documented with the
feature it requires (WAL journaling, shared-memory locking, threads).
Completion criterion: plans/DEFERRED.md documents each deferred package and
the feature it requires.
Verify: test -f plans/DEFERRED.md
Fresh context: true
```

## Package list (33)

| Category | Packages | Count |
|----------|----------|-------|
| WAL | wal, wal64, walbak, walbig, walblock, walckptnoop, walcksum, walcrash, walfault, walhook, walmode, walnoshm, waloverwrite, walpersist, walprotocol, walrestart, walro, walrofault, walseh, walsetlk, walsetlk_, walshared, walslow, walthread, walvfs, nockpt | 26 |
| Concurrency | thread, mutex, shared, shared_, sharedA, sharedB, sharedlock | 7 |

## Why deferred

- WAL mode requires a completely different journaling and concurrency model
  (write-ahead log files, checkpoints, shared-memory index, snapshot readers).
- Frigolite uses the rollback journal exclusively.
- These are legitimate future goals, not exclusions.
- They remain failing/vacuous — a visible signal for future WAL work.

## Steps

- [x] **1. Run all 33 deferred packages** — record pass/fail/status per package.
      Observed: 21 FAIL (incl. 2 transpiler compile failures, 3 timeouts),
      12 vacuous PASS (unsupported commands transpile to no-ops).
- [x] **2. Write `plans/DEFERRED.md`** — the authoritative document: per-package
      feature required, current status, and the WAL implementation roadmap.
      Commit: `G6.DEFERRED.1: document deferred WAL/concurrency packages`
- [x] **3. Update `plans/NOT_APPLICABLE.md`** if needed (cross-references).
- [x] **4. Update `plans/subplans/P6_TRIAGE.md`** G6.DEFERRED checkbox.
- [x] **5. Commit + push** all changes.

## Verify

```bash
test -f plans/DEFERRED.md
```

## Notes

- `snapshot`, `snapshot_` testgen packages exist and use the WAL snapshot C API
  (`sqlite3_snapshot_get`). They currently PASS vacuously (unsupported commands
  are no-ops) and are **not** in the 33-package deferred list; they are tracked
  as effectively-applicable edge cases (see `plans/DEFERRED.md` §Edge cases).
- Harness (`testdata/*.json`) equivalents are already excluded with DEFERRED
  reasons in `unsupportedTestFiles` (frigolite_harness_test.go) — see G6.NA.2.
