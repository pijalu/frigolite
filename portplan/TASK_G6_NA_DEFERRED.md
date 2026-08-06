# TASK G6.NA — Keep N/A and DEFERRED documentation + harness map in sync

> **Phase**: G6 (finalization).
> **Goal**: G6.NA.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.

## Objective
Every testgen package that is **not applicable** (C internals, fault injection,
corruption, custom VFS, FTS, JSON, shell, perf, rtree, sessions) or **deferred**
(WAL / shared-memory concurrency) is formally documented with a one-line reason
in `plans/NOT_APPLICABLE.md` / `plans/DEFERRED.md` **and** reflected in the
harness `unsupportedTestFiles` map in `frigolite_harness_test.go`. This is not a
place to hide engine bugs — an exclusion must have evidence.

This task is **continuous**: as G1–G6.TRIAGE prove packages are genuinely N-A,
they land here. This task file defines the *process* and the *invariants* that
must hold at the end.

## Invariants (must hold at plan completion)
1. **One source of truth for categories.** `plans/NOT_APPLICABLE.md` and
   `plans/DEFERRED.md` are the authoritative categorization. Counts there must
   match reality: ~228 N-A, ~33 DEFERRED, ~353 applicable.
2. **Harness map matches docs.** Every package listed as N-A in the doc appears
   in `unsupportedTestFiles` (JSON harness) with the same reason. (The testgen
   system has no exclusion map — N-A testgen packages simply fail and are
   documented here; that's expected.)
3. **Evidence per exclusion.** Each N-A entry has a one-line reason citing the C
   feature it exercises that frigolite will not provide (e.g. "FTS3/4/5 shadow
   tables not implemented", "tests sqlite3 malloc fault injection").
4. **No engine bug hidden.** If triage ever shows a package's failure is an
   engine bug, it is **removed** from N-A and fixed — never left excluded.

## Scope — categories (already documented; this task maintains them)
- **C API:** capi, capi3, bind, bindxfer, tableapi, tclsqlite, backup, backup_,
  carrayfault, cffault → N-A (pure Go, no C API).
- **Fault injection / OOM / fuzz:** all `*fault`, `*malloc*`, `*ioerr`, `*crash`,
  `*fuzz*`, `*corrupt*` → N-A.
- **Custom VFS:** avfs, cksumvfs, tvfs, vfs, unixexcl, multiplex, mmap,
  memjournal, subjournal, shmlock, lock, superlock, backcompat, syscall, oserror,
  shortread, sync, fallocate, filectrl, filefmt, reservedbytes, chunksize,
  pagesize, p_8_3_, uri, openv, dbpage, dbdata, dbstatus → N-A.
- **FTS3/4/5, RTree, JSON, session, RBU, zipfile:** → N-A.
- **Window functions:** → N-A (not implemented; could be future work).
- **WAL / shared-memory concurrency (33):** → DEFERRED (`plans/DEFERRED.md`).

## Steps (ongoing)
- [ ] **G6.NA.1** When a triage proves N-A, add the package to the right doc
      section + the harness map (same commit as the triage decision).
      Commit: `G6.NA.<n>: document <pkg> as N-A (<reason>)`.
- [ ] **G6.NA.2** Periodically reconcile counts: doc total vs `unsupportedTestFiles`
      map size vs actual `testgen/` directory. Fix drift.
      Commit: `G6.NA.reconcile: sync N-A/DEFERRED counts`.
- [ ] **G6.NA.3** Final sweep: confirm no N-A package is secretly an engine bug
      (spot-check 5 random N-A packages by writing a pure-Go test; if it passes,
      un-exclude and fix). Commit: `G6.NA.audit: N-A spot-check`.

## Verify command
```bash
# Docs parse + harness map references existing files + counts sane
go test -run 'TestSOLID_' -count=1 . && \
go build ./... && \
echo "N-A doc entries:" && grep -c '^| `' plans/NOT_APPLICABLE.md
```

## Goal create command
```
goal create \
  objective "Maintain authoritative N/A and DEFERRED categorization: keep plans/NOT_APPLICABLE.md + plans/DEFERRED.md in sync with the harness unsupportedTestFiles map; ensure every exclusion has a one-line evidence-backed reason; spot-check that no N-A package hides an engine bug. See portplan/TASK_G6_NA_DEFERRED.md." \
  completionCriterion "N/A + DEFERRED docs match harness map; every exclusion has evidence; counts (~228 N-A, ~33 DEFERRED) reconcile with testgen/ reality." \
  verifyCommand "go test -run TestSOLID_ -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G6.NA (documentation maintenance). Source of truth: plans/NOT_APPLICABLE.md + plans/DEFERRED.md;
harness map in frigolite_harness_test.go (unsupportedTestFiles). testgen has no exclusion map — N-A pkgs fail and are documented here.
Decisions: every exclusion needs evidence; never exclude an engine bug — un-exclude and fix instead.
Next: as G6.TRIAGE proves N-A, add entries; periodically reconcile counts; final spot-check audit.
Risks: drift between docs and harness map; engine bugs smuggled into N-A — audit catches these.
Carried limits: verifyCommand above.
```
