# Resume P8.PRAGMA — Complete the Implementation

## Where things stand (verified)

**Active goa goal "bright.toad" = P8.PRAGMA** (7 turns used; paused on a provider rate limit, re-activated by you). Its verify command covers 10 packages: `pragma, pragma2-6, quota, quota2, quota_glob, tkt2686`.

**Committed so far (today, 5 commits):** quota-glob matcher + `sqlite3_quota_*` transpiler helpers (7b1756b7); `max_page_count` engine + pager enforcement + tkt2686 un-skip (4722fb8c); transpiler catch+while loop (dc840187); `tclConnRegistry` connection dispatch (80890af0); quota/quota2 re-skip (81b49836).

**Uncommitted:** `internal/execpragma/execpragma.go` (+119 lines) — pragma.test triage: SYNCHRONOUS/TEMP_STORE "may not be changed inside a transaction" guards, new `PRAGMA error` (TESTCTRL_PRAGMA_ERROR mirror) and `PRAGMA temp_store_directory` handlers.

**Key facts found:**
- `tools/status/last_run.json` + `ledger.json` were seeded 2026-09-05T00:00Z — **before** all 5 commits — so the live pass/fail state of the 8 un-skipped packages is unknown and must be established first.
- `frigolite_maxpage_test.go` (sub-plan next-action 3) is still missing.
- quota/quota2 testgen are regenerated as no-op stubs (skip maps consulted at transpile time).
- **Conflict to resolve:** the 81b49836 quota/quota2 re-skip violates (a) the mandatory lessons-learned rule *"No skipping missing engine features — IMPLEMENT the behaviour"* (pure-Go supersession retired), (b) the goal's own completion criterion ("all 10 un-skipped and passing"), and (c) sub-plan Phase 4 (`internal/quota` mirroring `ext/misc/quota.c`). Your directive "the complete implementation must be done" settles it: **implement the quota VFS and un-skip quota/quota2**.

## Execution steps

**T0 — Live baseline (§5b item 0b):** run the goal verify command for the 10 packages; record per-package serial states in the sub-plan before further edits.

**T1 — Commit in-progress triage diff:** verify build/vet/staticcheck/quality_gate on `execpragma.go`, confirm pragma package state, commit checkpoint.

**T2 — max_page_count closeout:** confirm tkt2686 green (fix testdata if not); add `frigolite_maxpage_test.go` native regression test (getter/setter, clamp to current page count per `src/pragma.c:1720`/OP_MaxPgcnt, INSERT past cap → "database or disk is full"), oracle-verified against `/usr/bin/sqlite3`.

**T3 — pragma*.test triage (failure-driven, one package at a time):** known classes from the sub-plan: `lock_proxy_file` (pragma.test 14.x — check oracle contract; genuine VFS-mechanism N/A would need NA_EVIDENCE), temp_store_directory 9.x, ERROR-pragma tests, pragma6 1.2 transpiler gap. Method per guidelines: pure-Go discriminator → oracle → port from `src/pragma.c` — no skips without evidence.

**T4 — Quota VFS engine (the main remaining piece):**
- New `internal/quota` package mirroring `ext/misc/quota.c`: quota groups keyed by `quotaStrglob` patterns, per-file size tracking, enforcement on file growth, callback hook `(filename, limit, size)` whose limit-extension round-trips (quota-2.2.x), initialize/shutdown lifecycle with SQLITE_MISUSE semantics (double-init; shutdown with open files — quota-2.4.1).
- Pager hook at the file-growth point returning SQLITE_FULL "database or disk is full" (quota-2.1.4). **Pager edits are the prime drift-suspect class** (fts4opt/fts3snippet regression family) — keep the hook minimal, pin pager behavior with native regression tests.
- Transpiler: bridge existing `tclQuota*` helpers to `internal/quota`; add `file_control_vfsname` → `quota/<vfs>` (quota-2.1.2.1).
- Remove quota/quota2 from `skiptestfiles.go`, regen testgen (suite-wide event per §5g item 4 → full suite run in the same checkpoint), iterate to green.

**T5 — §5e strict close:** full verify command green; build/vet/staticcheck/quality_gate(changed)/-race/SOLID; `tools/status` full run → confirm only goal-target flips vs baseline → re-seed ledger → `--check` PASS; refresh `last_run_report.md`; update sub-plan close status + PORTPLAN §4 P8.PRAGMA row (live marker + run stamp) + §2 totals; NA_EVIDENCE entries for any residual skips; commit AND push; mark goa goal complete.

## Verification
- Verify command: `go build ./... && go vet ./... && go test -run TestSOLID_ ./... && go test -tags testgen ./testgen/pragma/ ./testgen/pragma2/ ./testgen/pragma3/ ./testgen/pragma4/ ./testgen/pragma5/ ./testgen/pragma6/ ./testgen/quota/ ./testgen/quota2/ ./testgen/quota_glob/ ./testgen/tkt2686/ -count=1 -timeout 300s`
- `tools/status --check` as the no-flip gate at close; oracle `/usr/bin/sqlite3` for every behavior fix; UCL native tests for max_page_count + quota seams.

Estimated remaining effort: ~5–8 focused turns (sub-plan budget: Phase 3 ≈2–4, Phase 4 ≈3, Phase 5 ≈1–2).