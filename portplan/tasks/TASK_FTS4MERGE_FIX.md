# TASK: fts4merge* exact-structure fix — researched plan with microtasks

Date: 2026-08-16 (rewrite of blocker.md after fresh engine↔SQLite source review)
Status: **PARTIALLY IMPLEMENTED (2026-08-16 single-shot execution)** — Tasks A–C landed; fts4merge/2/3/5 green; fts4merge4 remains on the Task E promotion cascade.
Supersedes: blocker.md §9 workstreams for the REMAINING divergences. blocker.md remains the evidence base; this file is the action plan.

---

## 0. Goal and completion criterion

The blocked goal's criterion:

1. `go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s` exits 0 (all 8 line-261 assertions), wall < 300s.
2. `go test -tags testgen ./testgen/fts4merge/ ./testgen/fts4merge2/ ./testgen/fts4merge3/ ./testgen/fts4merge5/ -count=1` exits 0.
3. `go test ./... -count=1` exits 0.

Runtime is already solved (fts4merge4 ~90s, was 346s). What remains is EXACT-STRUCTURE matching in the incremental merge.

### Implementation status (2026-08-16)

| Criterion | Status | Evidence |
|-----------|--------|----------|
| 2 (fts4merge/2/3/5) | ✅ GREEN | `go test -tags testgen ./testgen/fts4merge/ ...5/ -count=1` all `ok` |
| 1 (fts4merge4) | ❌ 8 fails in 2.2.x | `got: [1 41 2 2]` vs `want: [1 1 2 1 4 1 6 1]` — Task E cascade |
| 3 (`go test ./...`) | ❌ 13 pre-existing | identical to HEAD baseline (11 root + parse + 2 SOLID that pass individually) |

### What landed (commits to make / current working tree)

1. **P0** — stripped 4 `FTS_MERGE_DEBUG` blocks; split `export_fts_merge.go` (1294→877 lines) + new `export_fts_chomp.go` (506) + moved `promoteFTSSegments`; all under 1000-line gate.
2. **Task A/C (V1/V2/V3 root causes — the single-leaf synthetic root + continuation leaf-load):**
   - `IncrLeafWriter.BuildRoot` now emits SQLite's synthetic interior root (height 1, firstBlock, no boundaries) for a single-leaf output written to %_segments (fts3_write.c fts3IncrmergeRelease iRoot==0 case). The old height-0 root with start_block!=0 made the reader "rewind" (nLeaves = leavesEnd+1), so truncated segments kept ALL their terms → the continuation was rejected → extra L2 segment (V1) and 2× L0 consumption (V2).
   - `IncrLeafWriter.LoadLeaf` + MergeFTS continuation loads the existing output's LAST leaf into the writer buffer (fts3IncrmergeLoad), so the first appended term flushes the full leaf and the quota is charged as SQLite does (V2: 16→14→13→12... one source segment per merge=1,16 call).
   - `contReuseLeaf`: the continuation's first flush OVERWRITES the existing last-leaf block in place (not a fresh id); `lastWrittenBlock` tracks the real leaves_end_block.
3. **Block-id allocation fixes (fts4merge4 corruption + V3 hint):**
   - `chompFTSMerge` allocates truncation blocks from the table cache (kept current) above EVERY source segment's old leaves_end_block — fixes the stale-btree-scan collision that deleted live blocks (L0[0]/L1[0] missing, "database disk image is malformed").
   - Continuation size accumulation un-negates the existing output's partial-merge marker (fts3IncrmergeLoad `nLeafData *= -1`) — a negative accumulated size froze every later promotion (L2[0] size=-229814 → all L1→L2 promotion blocked).
4. **Hint (V3)** — fts4merge 4.4.1 `X'0006'` passes via the chomp `remaining=truncated` count (Task B, R1).

### Remaining: Task E (fts4merge4 2.2.x promotion cascade)

All 8 fts4merge4 failures are the SAME cascade: after tx 19 the engine's L0 accumulates (L0=16 at tx 39) while the oracle merges L0→L1 and promotes (sparse `1 1 | 2 1 | 4 1 | 6 1`). Latest verified state (commit 616e49eb8):

- Engine tx 16-18 MATCH the oracle (`0:1 1:1`, `0:2 1:2`, `0:1 1:2 2:1`).
- Engine tx 19 DIVERGES: `0:2 1:2 2:1` vs oracle `1:3 2:1`. The oracle's tx-19 flush-time automerge merged L0 (2 segs) → L1; the engine's `MergeFTS(A=372, am=2)` found `level=0 rows=2` (trace) but produced NO output (L0 unchanged).
- Engine reaches `0 16 1 2 2 1` at tx 39 (L0=16 unmerged) then `database disk image is malformed` at tx 34 in the am=2 repro (the 16-segment crisis merge path corrupts).

Root-cause hypothesis (trace-backed, refined 2026-08-16): the engine's L0→L1 merge at tx 19 enters with rows=2 but the heap prime hits a reader error: L0[0]'s row says start_block=3485 / leaves_end=3608 but ALL 124 blocks are MISSING (verified via count(*) — zero rows). The flush wrote them (rootFB==start after 882b87fa1), so a LATER truncation's `deleteFTSBlocks(oldStart, oldEnd)` deleted them while L0[0]'s row retained the old start. The tx-18 L0→L1 merge truncated L0[0] as a SOURCE; the truncation re-serialized the survivor at fresh blocks but the row update / block-delete ordering left the row pointing at deleted blocks. The truncation's `deleteFTSBlocks` runs even when `len(blocks)==0` (single-leaf survivor → row start=0), which is correct ONLY if the row update persists; verify the truncation's updateFTSShadowRowRange actually lands (idx stable through repack) and that fresh blocks are allocated above ALL live blocks including the sources' ranges (allocFloor already accounts for source ends + continuation output; the flush now uses the row's recorded start — 882b87fa1).

Next investigator: (1) confirm the truncation's row update persists for the L0 survivor at tx 18 (the am2o.go repro: after 19 txs, L0[0] blocks 3485..3608 all missing); (2) fix the delete-vs-row-update ordering (write fresh blocks → update row → delete old, never delete the fresh range). Apply the R1/R2 rule; do NOT tune the quota.

### Committed improvements (616e49eb8)

All block-collision corruption classes fixed; fts4merge/2/3/5 GREEN:
- Single-leaf synthetic interior root (BuildRoot); continuation last-leaf load (LoadLeaf); in-place last-leaf overwrite (contReuseLeaf delete+insert); real leaves_end_block (lastWrittenBlock).
- allocFloor = max(source leavesEnd, continuation output leavesEnd): every fresh output leaf + truncation block lands above all still-live blocks — eliminates the old-range-cleanup collision (fts4merge 1.4/4.2 "malformed").
- chompFTSMerge remaining = truncated (R1); partial-size un-negation on continuation (promotion unfrozen).
- export_fts_merge.go split (925 lines) + export_fts_chomp.go (475); FTS_MERGE_DEBUG/FTS_TRACE stripped.

### Current verified state (do not re-fix)

All corruption classes are fixed and green (commits 54286993b→b4f028ae7):
- btree same-rowid REPLACE before full-page check; interior routing equality (`<=`).
- UPDATE change dedupe; in-place repack; block-range cleanup (fts3DeleteSegment).
- Real incremental leaf writer (fts.IncrLeafWriter); no-replay continuation; hint-as-LIST; promotion renumber + negative-size markers; flush-gate parity.

### The three live divergences (all in `internal/execddl/export_fts_merge.go` `MergeFTS`)

| # | Test | Engine got | Oracle want | Root decision point |
|---|------|-----------|-------------|---------------------|
| V1 | fts4merge 1.3 | `2 0 1 2 3 4` | `2 0 1 2 3` | chomp delete-vs-truncate at quota 1 → 1 extra L2 seg |
| V2 | fts4merge 4.3.x | `0 {0..N-1} 1 0` | `0 {0..N} 1 0` | each `merge=1,16` consumes 2 L0 segs, not 1 |
| V3 | fts4merge 4.4.1 | `X''` | `X'0006'` | hint stored empty instead of preserved `(0,6)` |

All three are the SAME family: the chomp accounting and hint-store decisions at the end of one `MergeFTS` iteration. Fix V1/V2 first (one decision), V3 is a separate hint-store guard.

---

## 1. The five rules (evidence base — cite these in every commit)

These are derived from the SQLite source, not inferred. Each rule names the exact SQLite line it mirrors. Do NOT deviate; do NOT tune constants (plan/GUIDELINES no-try/fail rule).

### R1 — Chomp remaining = truncated-among-loaded, exactly
SQLite `fts3IncrmergeChomp` (fts3_write.c:4765–4810) sets `*pnRem = nRem` where `nRem` counts ONLY readers where `pSeg->aNode != 0` (the truncate branch, `nRem++`). A reader at EOF (`aNode==0`) is deleted and does NOT count. A reader that errored is neither.
- Engine today: `remaining := loadCount - deleted` (export_fts_merge.go:909). This is wrong whenever a reader is skipped without being deleted (the `sr.reader.Err() != nil → continue` path, and any reader that is neither deleted nor truncated).
- Fix: `remaining` = the number of readers that took the TRUNCATE branch, counted explicitly.

### R2 — Delete only when past the LAST term; truncate a reader sitting ON its last term
SQLite: the do-while (fts3_write.c:5058–5062) appends term T, calls `sqlite3Fts3SegReaderStep` (advances the `nMerge` group readers PAST T via `pCsr->nAdvance = nMerge`), THEN checks `pWriter->nWork >= nRem`. `fts3IncrmergeChomp` deletes only when `pSeg->aNode == 0`, which `fts3SegReaderSetEof` sets only after the reader advanced past its last term AND `iCurrentBlock >= iLeafEndBlock` (fts3_write.c:1361, 1298).
- Engine today: `AtEOF()` (internal/fts/stream.go:265) is `atEOF || err != nil`; `atEOF` is set in `Next()` when `advanceLeaf()` fails. This is ALMOST right, but the merge loop advances group readers BEFORE the quota check, so a reader whose last term was the one that triggered the quota flush may be marked EOF even though SQLite would have truncated it (SQLite stops BEFORE stepping if the append already hit quota on a different group).
- Fix: at the quota break, a group reader must be classified by "did the merge actually consume its current term?" — not by a bare `AtEOF()`. Track per-reader whether its current term was merged; delete only readers whose every term was merged.

### R3 — Hint push only when `nSeg != 0` after chomp; store the chomp count
SQLite (fts3_write.c:5072–5076): `if( nSeg!=0 ){ bDirtyHint=1; fts3IncrmergeHintPush(&hint, iAbsLevel, nSeg) }` where `nSeg` is the post-chomp remaining.
- Engine today: pushes `remaining` when `>0`, and ALSO calls `storeHint()` in the `else if fromHint` branch (line 941) which overwrites the blob with the (now-empty) list. fts4merge 4.4.1 wants `X'0006'` PRESERVED when the merge neither used nor extended the hint.
- Fix: `storeHint()` only when the hint was popped-and-used OR a push happened (SQLite's `bDirtyHint`). A fresh full-consume with no hint use leaves the stored blob untouched.

### R4 — Hint nSeg cap = `MIN(MAX(nMin, foundCount), hintSeg)`, loaded from the hinted level
SQLite (fts3_write.c:4991): `nSeg = MIN(MAX(nMin,nSeg), nHintSeg)` where `nSeg` is FIND_MERGE_LEVEL's count at `foundLevel`. Then `fts3IncrmergeCsr(p, iAbsLevel, nSeg, pCsr)` loads `nSeg` oldest segments at the HINTED level.
- Engine today: `effMin = hSeg` clamped to `cnt` = the hinted level's row count (lines 456–462), then `loadCount = effMin` (595). The clamp target and the `MAX(nMin, found)` term differ from SQLite.
- Fix: compute `nSeg = min(max(nMin, foundCount), hintSeg)`; load `min(nSeg, hintedLevelCount)` oldest rows.

### R5 — Verify by per-tx diff, not by re-running until green
Use `/tmp/fm1.dir` (merge=1 repro) and `/tmp/oracle_c/h.c` (instrumented SQLite, `-DSQLITE_DEFAULT_PAGE_SIZE=1024`) side by side. Find the FIRST merge where deleted/truncated/hint diverge; fix that single decision per R1–R4. Do not resume speculative tracing.

---

## 2. Pre-flight (commit 0 — hygiene, no behavior change)

- [ ] **P0.1** Confirm clean tree except intended changes: `git status --short`. blocker.md §7 debug instrumentation is already committed/removed; if any `FTS_MERGE_DEBUG`/`os.Getenv` debug remains in the 4 files (`btree_insert.go`, `export_fts_flush.go`, `export_fts_merge.go`, `update.go`, `update_split.go`), strip it in a standalone commit first.
- [ ] **P0.2 (MANDATORY — hard gate)** `export_fts_merge.go` is **1294 lines**, already OVER the 1000-line hard gate: any commit staging it is REJECTED by the pre-commit hook. BEFORE any Task A–E edit, split it (pure move, no behavior change, `git mv`-style, its own commit): move the chomp/truncate/hint block (the `MergeFTS` tail from the quota-stop through `storeHint`, ~lines 740–990, plus `deleteFTSSegdirIdx`/`repackFTSSegdirLevel`/`deleteFTSBlocks`/`ftsHintEntry`/`ftsParseHintList`/`ftsEncodeHintList`) into `internal/execddl/export_fts_chomp.go`. Keep `MergeFTS`'s setup/heap loop in `export_fts_merge.go`. Then verify: `tools/quality_gate.sh internal/execddl/export_fts_merge.go internal/execddl/export_fts_chomp.go internal/execddl/export_fts_flush.go internal/fts/stream.go` — all < 1000 lines, gocognit ≤15 / gocyclo ≤12 / staticcheck clean, and `go test ./internal/execddl/...` green (proves the move was behavior-preserving).
- [ ] **P0.3** Baseline: run all 5 packages and record got/want per assertion into `/tmp/fts_baseline.txt`:
  ```bash
  go test -tags testgen ./testgen/fts4merge/ ./testgen/fts4merge2/ ./testgen/fts4merge3/ ./testgen/fts4merge5/ -count=1 2>&1 | tee /tmp/fts_baseline.txt
  go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s 2>&1 | tee -a /tmp/fts_baseline.txt
  ```
  Expected baseline: fts4merge FAILS (12 assertions: 1.3, 4.3.x, 4.4.1, 4.4.2, 5.x); fts4merge2/3/5 PASS; fts4merge4 FAILS line-261 (am=2 `1 1|6 2|7 1|9 1` vs `1 1|2 1|4 1|6 1`).

---

## 3. Microtasks (ordered; one commit each; RED test first per plan/GUIDELINES test-first rule)

### Task A — R2: per-reader merged-term tracking at the quota stop (fixes V1+V2)

**A0. RED test.** New `internal/execddl/fts_chomp_test.go` (or add to an existing fts test file if one exists):
- Build an in-memory FTS table; force a merge that stops at quota where a source segment's LAST term is the quota-triggering term but was NOT consumed by this merge's output.
- Assert: that segment is TRUNCATED (still present in `%_segdir` with a reduced range), not deleted.
- Confirm it FAILS on current code.
- *If a unit harness is too heavy, write a Go repro at the `frigolite.Open` level mirroring fts4merge 4.3 (`merge=1,16` × 9) and assert the L0 count decreases by exactly 1 per call.*

**A1. Track merged-ness per reader.** In `MergeFTS` (export_fts_merge.go, the chomp block ~lines 820–905): add a `mergedAll bool` (or `unmergedTerm string`) field to the local `segReader` struct. Set it when the reader's group term equals the term just appended AND the reader advanced past it. The quota-break path must mark ONLY the group readers of the final appended term as advanced; a reader that hit EOF for a different reason is not "merged".
- Mirror: SQLite's `pCsr->nAdvance = nMerge` (fts3_write.c:3045) advances exactly the group of the appended term.

**A2. Delete vs truncate on merged-ness, not bare AtEOF.** Change the chomp loop condition from `if sr.reader.AtEOF()` to `if sr.mergedAll && sr.reader.AtEOF()`. A reader at EOF whose last term was NOT merged falls into the truncate branch (its `Current()` is the last, unmerged term).
- Mirror: SQLite deletes only `aNode==0` after the group-step; the truncate branch uses `pSeg->zTerm` (first unmerged term).

**A3. Verify.**
```bash
go test ./internal/execddl/... -count=1
go test -tags testgen ./testgen/fts4merge/ -count=1   # 4.3.x must move toward 1-per-call
cd /tmp/fm1.dir && go run . 2>&1 | tail -3            # merge=1 FINAL must approach `2 0 1 2 3`
```
Record got/want deltas vs `/tmp/fts_baseline.txt` in the commit message. fts4merge2/3/5 MUST stay green.

---

### Task B — R1: explicit truncated-count for `remaining` (fixes V1+V2 accounting)

**B0. RED test.** Extend the Task A test: after the merge, the pushed hint's nSeg equals the number of TRUNCATED segments, not `loadCount - deleted`. (Seed a case where a reader errors or is skipped so the two formulas differ.)

**B1. Count the truncate branch.** In the chomp loop, increment a `truncated` counter in the branch that calls `updateFTSShadowRowRange` (the truncate path). Set `remaining := truncated` (replace `remaining := loadCount - deleted` at line 909).
- Mirror: `*pnRem = nRem` counts only the `nRem++` (truncate) branch (fts3_write.c:4799–4804).

**B2. Verify** — same commands as A3 plus:
```bash
go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s   # am=2 line-261 got-values must progress toward 1 1|2 1|4 1|6 1
```

---

### Task C — R3: hint-store guard (fixes V3)

**C0. RED test.** Repro fts4merge 4.4.1: after the 4.3 merge series, `SELECT quote(value) FROM t4_stat WHERE rowid=1` must be `X'0006'`, not `X''`. Assert current code returns `X''` (fails).

**C1. Track `dirtyHint`.** In `MergeFTS`: introduce a local `dirtyHint bool`. Set it TRUE only when (a) the hint was popped AND used this iteration (`fromHint == true` and the merge ran), or (b) a push happened (`remaining > 0`). Replace the unconditional `storeHint()` calls:
- keep `storeHint()` when `remaining > 0` (the push path);
- in the `else if fromHint` branch, call `storeHint()` ONLY if `dirtyHint`;
- never store when a fresh full-consume ran with no hint use.
- Mirror: SQLite writes the hint only `if( bDirtyHint && rc==SQLITE_OK )` (fts3_write.c:5086–5088).

**C2. Verify.**
```bash
go test -tags testgen ./testgen/fts4merge/ -count=1   # 4.4.1 X'0006' must pass; 5.x must not regress
```

---

### Task D — R4: hint nSeg cap parity (only if V2 persists after A–C)

**D0. RED test.** A case where the hinted level's count differs from FIND_MERGE_LEVEL's found count and the loaded segment count must follow `min(max(nMin, found), hintSeg)`.

**D1. Recompute the cap.** At export_fts_merge.go:441–463 and 594–596: replace `effMin = hSeg; clamp to cnt` with:
```go
nSeg := nMin
if foundCount > nMin { nSeg = foundCount }   // MAX(nMin, found)
if nSeg > hintSeg { nSeg = hintSeg }         // MIN(..., hintSeg)
loadCount = nSeg
if loadCount > hintedLevelCount { loadCount = hintedLevelCount }  // safety clamp only
```
- Mirror: fts3_write.c:4991 `nSeg = MIN(MAX(nMin,nSeg), nHintSeg)`.

**D2. Verify** — full 5-package sweep; record deltas.

---

### Task E — fts4merge4 promotion cascade (tx26 third iteration)

**E0. Prerequisite.** Tasks A–D must be green first; the tx26 divergence is a cumulative cascade of the chomp accounting. Re-run:
```bash
go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s
```
If am=2 line-261 is now `1 1|2 1|4 1|6 1`, this task is DONE (the cascade fell out of A–D). Only continue if it still diverges.

**E1. Per-tx diff.** Run `/tmp/ftsdbg` (AM=2) and `/tmp/oracle_c/h 0 2`; diff the per-tx level counts; identify the first tx where the engine's L2 continuation deletes where SQLite truncates. Apply the R1/R2 rule at that exact point (it will be the same chomp decision, now at a higher level).

**E2. am=4/8.** Same cascade; verify `0 4|1 3|2 1` shapes after A–D. Only touch if still diverging, and only via R1/R2/R4.

---

### Task F — criterion 3: `go test ./...` (scope decision)

The 12 root-package failures + `internal/parse TestGrammarCoverage` are PRE-EXISTING and never green at HEAD (blocker.md PART 6). This task is a DECISION, not code:
- [ ] **F0.** Confirm the failures are unrelated to FTS: `go test ./... -count=1 2>&1 | grep -v fts | grep FAIL`.
- [ ] **F1.** If unrelated, present to the user: either (a) fix them as a SEPARATE goal, or (b) relax the completion criterion to the five fts4merge* packages. Do NOT absorb them into this plan — they are out of scope for the fts4merge blocker.

---

## 4. Verification commands (run after EVERY microtask commit)

```bash
go test ./... -count=1
go test -run TestSOLID_ ./... -count=1
tools/quality_gate.sh <changed files>
go test -tags testgen ./testgen/fts4merge/  -count=1
go test -tags testgen ./testgen/fts4merge2/ -count=1
go test -tags testgen ./testgen/fts4merge3/ -count=1
go test -tags testgen ./testgen/fts4merge5/ -count=1
go test -tags testgen ./testgen/fts4merge4/ -count=1 -timeout 300s   # FINAL gate
```

## 5. Commit plan

1. **Commit 0** — hygiene (P0): strip leftover debug; split `export_fts_chomp.go` if any file nears 1000 lines.
2. **Commit 1 (A)** — per-reader merged-term tracking + delete-on-merged-EOF + RED test.
3. **Commit 2 (B)** — `remaining = truncated` + RED test.
4. **Commit 3 (C)** — dirtyHint guard + RED test.
5. **Commit 4 (D)** — hint nSeg cap parity (only if needed).
6. **Commit 5 (E)** — fts4merge4 cascade (only if needed).
7. **Final** — full suite; `tools/status` update; goal completion audit. Present F-scope decision to user.

## 6. Risks / guardrails

- **Do not regress fts4merge2/3/5** — they are green; re-run after EVERY commit, not only at the end. The hint-list change (b4f028ae7) already regressed fts4merge once; the dirtyHint guard (C) is the fix, so C is on the critical path for criterion 2.
- **Append-order check is required** — disabling it causes "database disk image is malformed" (blocker.md PART 6). Keep the `replacingOut && first <= lastTerm → replacingOut=false` guard intact.
- **No constant tuning** — every change must cite a SQLite line (R1–R5). If a test still fails after A–D, use per-tx diff (R5), never adjust quotas/nLeafEst to force a match.
- **File-size gate (CONFIRMED blocker)** — `export_fts_merge.go` is 1294 lines, over the 1000-line hard gate; the pre-commit hook REJECTS any commit staging it. The split in P0.2 is MANDATORY and must land as commit 0 before any Task A–E code change.

## 7. Evidence trail (for the implementer)

- Source refs: `../sqlite/ext/fts3/fts3_write.c` — `sqlite3Fts3Incrmerge` 4946–5095, `fts3IncrmergeChomp` 4765–4810, `fts3IncrmergeAppend` 4019–4096, `fts3SegReaderSetEof` 1298, `sqlite3Fts3SegReaderStep` 2844 (`nAdvance` 3045), `fts3IncrmergeHintPush/Pop` 4876–4930.
- Oracle harness: `/tmp/oracle_c/h.c` (build with `-DSQLITE_ENABLE_FTS3 -DSQLITE_DEFAULT_PAGE_SIZE=1024`; argv[2]=automerge value).
- Engine repros: `/tmp/ftsdbg` (AM env, per-tx dump), `/tmp/fm1.dir` (merge=1 repro).
- Traces: `/tmp/oracle_trace_am2.txt`, `/tmp/engine_trace_am2.txt`, `/tmp/dbg.txt`.
- Baseline capture: `/tmp/fts_baseline.txt` (created in P0.3).
- blocker.md: full history, divergence table D1–D8, prior session notes (PART 1–6).
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
