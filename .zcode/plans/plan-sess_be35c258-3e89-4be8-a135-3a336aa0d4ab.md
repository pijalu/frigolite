# PORTPLAN — Full Remaining-Work Plan

## State review (verified)
- HEAD `c96f4283` = P8.PAGER batch 7. Uncommitted tree = coherent **batch 8**: tcl2go value-returning-builtin do_test dispatch (`tools/tcl2go/flow.go` whitelist + `dotest.go` emitDoTestBodyComparison), engine `SQLITE_TOOBIG` mapping (`frigolite.go` errorCode), regenerated `testgen/cache`. `analyze9`/`fts3sort`/`notify2` diffs are regen-order noise (nondeterministic `arrayMapVars` map iteration at `tools/tcl2go/gen.go:145`).
- P8.PAGER ~13/24 green. Remaining reds (sub-plan T3.5 order): sqllimits1 (in progress), memsubsys1/2, bitvec (hang at :180), oserror (panic :134), sqldiff1 (:101/:140).
- Baseline 2026-09-06T06:33:30Z: 700 PASS / 235 FAIL / 271 SKIP / 13 timeout-suspects of 1,219; `tools/status --check` PASS.
- Queue after P8.PAGER (§5a-1, user-approved): RECOVER → ROLLBACK → VACUUM → FULL-SUITE-DRIFT → FTS-RESIDUE → PLANNER.bestindex → RTREE → FTS5 → DBDATA → DBSTAT → WAL-G7 → P9.PERF.
- Hygiene debt: `lessons_learned.md` ~4,960 lines (orphan bullets above the title, retired 2026-05 "pure-Go supersession" sections still present, ~2,100-line INCRVACUUM session detail) and no P8.PAGER entry; PORTPLAN §2/§4 P8.PAGER markers stale ("batches 1-2", "11/24") vs git (batches 3–7 landed).

## 0. Land batch 8 (immediate, first action)
1. Verify the WIP: `go test -tags testgen` on cache, memdb, sqllimits1, analyze9, fts3sort, notify2; `go build ./...`, vet, SOLID.
2. Hygiene: make `gen.go` arrayMapVars emission deterministic (sorted keys); re-regen; confirm only intended diffs remain (eliminates future regen churn).
3. Commit `P8.PAGER.batch8: <summary>` + push; update sub-plan T3.5 + PORTPLAN §4 row.

## 1. Finish P8.PAGER (T3.5 → close T4–T6)
Per-class batches; each fix: pure-Go discriminator → oracle (`/usr/bin/sqlite3`) → SQLite source (`/Users/muaddib/dev/sqlite/src/pager.c`, `btree.c`, `limit.c`) → implement → verify → commit+push.
1. **sqllimits1** (finish): remaining items from batch 7 note — bind catch-code mapping (TOOBIG just landed), strftime/group-by/order-by column limits, max_page_count limit, statement-too-long; use `Engine.SetLimit` seams.
2. **memsubsys1/2**: sqlite3_status/memory-counter contracts — implement the engine-visible subset + harness seam; classify the rest.
3. **bitvec**: instrument the :180 loop first; classify engine vs transpiler before any edit.
4. **oserror**: VFS fault injection is stripped by the transpiler — expected harness N/A; port the engine-visible I/O-error contracts to a native test, record NA_EVIDENCE.
5. **sqldiff1**: inspect how the harness computes the diff before classifying (native seam vs engine gap).
Close sequence: T4 full-suite run + ledger re-seed + `tools/status --check --check-against-cache` zero-flip (regen = suite-wide event, §5g-4) → T5 gates (build/vet/staticcheck/quality_gate on changed files/-race/SOLID) → T6: sub-plan close, PORTPLAN §2/§4 refresh (fix stale "batches 1-2"/"11/24" markers), `lessons_learned.md` P8.PAGER entry, commit+push.

## 2–4. P8.RECOVER, P8.ROLLBACK, P8.VACUUM (in §5a-1 order)
Each goal opens per §5b: UCL tranche (oracle fixtures BEFORE engine edits) + anti-drift baseline recorded in the sub-plan; DoD §5e; close with full run + `--check`.
- **RECOVER** (recover_pkg, 1 pkg): `.recover` over corrupt DBs — port recovery semantics (src/recover.c); corrupted-DB corpus fixtures from the oracle. Note overlap with page-read seams later reused by DBDATA.
- **ROLLBACK** (exclusive, exclusive2, rollback, rollback2): rollback-journal fidelity (hot journal, ROLLBACK semantics); watch the super-journal boundary documented in P7.WAL-E (mjournal).
- **VACUUM** (vacuum..vacuum6, 6 pkgs): VACUUM / VACUUM INTO contracts.

## 5. FULL-SUITE-DRIFT (largest triage: ~235 red / 271 skip)
- **Tranche 0 (already attributed)**: the 5 INCRVACUUM-window regressions — lock, pcache, corruptB, fts3corrupt4, tkt_fc62af4523 (suspects: btree RelocatePage/updateParentChildPtr + CrossConnLockError classes; bisect evidence in the Blocker Register).
- Then per-class tranches: pre-squash drift (select1/insert/where class, fails already at `ba771a6b`), then remaining reds; serially re-confirm the 13 timeout-suspects (§5g-6); every skip flip needs NA_EVIDENCE; `tools/status --check` between tranches; UCL per class.

## 6–7. P6.FTS-RESIDUE, P7.PLANNER.bestindex
- **FTS-RESIDUE** on the FTS-WPORT structural-port base: fts4langid (3 assertions left) + fts4merge4 (automerge level distribution per fts3_write.c).
- **bestindex**: vtab xBestIndex contract (bestindex1-9/B/C/E/F/G) + autoanalyze1 — ends the DEFERRED skip class.

## 8–11. P6.MODULES (one module at a time, UCL per module)
- **RTREE** (17 red of 27): rtree module port (src/rtree/rtree.c); slice 1 delivers `vtab.Database`/`PageSource` seams (reused by DBDATA).
- **FTS5** (mission-critical; 0 of 144 TCL sources converted): full FTS5 engine — own shadow tables (%_data), index writer/reader, query language, bm25/aux functions — sliced into tokenizer→index→query→aux tranches, oracle transcripts before each engine edit; convert the 144 fts5*.test sources to testgen.
- **DBDATA**: sqlite_dbdata vtab on RTREE slice-1 seams (stub-green today; expected to go red then green).
- **DBSTAT**: dbstat vtab + first testgen conversion of dbstat.test.

## 12. P7.WAL-G7
Port src/wal.c wal-index (`-shm`) header + lock-bitmap protocol: multi-connection frame visibility, read-marks, protocol/lock bits. Un-skips the N-A-G7 families (walprotocol/walsetlk/walrestart/snapshot/shared). The UCL base already exists (walview decoder + testdata/walconformance) — extend it first.

## 13. P9.PERF (final closeout)
Perf packages; repo-wide legacy golang-check remediation to zero (§5d); final full-suite status refresh + cited run stamp; doc sync (AGENTS.md still points at `portplan/DESIGN.md` and `portplan/tasks/TASK_G*.md`, both now under archive/).

## Binding method for every goal (unchanged)
- §5b: UCL tranche + anti-drift baseline before engine edits; §5e strict DoD (zero FAIL, all gates, SOLID, -race, no-regression full run, oracle-verified, NA_EVIDENCE for remaining skips, status artifacts + cited run stamp); §5g: `tools/status --check --check-against-cache` no-flip gate at every close; §8: update sub-plan + PORTPLAN row, commit AND push per batch (`P8.PAGER.batchN: ...`).
- Triage: pure-Go test first (engine vs transpiler); oracle = WHAT, SQLite source = WHY/HOW; NO SIMPLIFY; NO TRY/FAIL (step back → source → written fix plan → execute).
- Session-start housekeeping: consolidate `lessons_learned.md` (remove orphan header debris + retired 2026-05 supersession sections, compress INCRVACUUM session detail) and add the P8.PAGER entry at goal close.

First action on approval: verify and land batch 8 (deterministic regen included), then proceed to sqllimits1.