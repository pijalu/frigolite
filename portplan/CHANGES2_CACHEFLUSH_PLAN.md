# changes2 / cacheflush Source-Faithful Support Plan

## Ground truth and reproduction

- `../sqlite/test/changes2.test:64-92` prepares once, steps/resets twice; final query expects two `2 changes` rows.
- `FRIGOLITE_TEST=changes2 go test . -run '^TestSQLiteSuite$' -count=1` fails 2.4: got empty `log` instead of two rows.
- Native `TestChanges2PreparedStatementLifecycle` covers DB.Prepare → Stmt.Step → Reset → intervening write → Step → Finalize and verifies both rows. `go test . -run '^TestChanges2PreparedStatementLifecycle$' -count=1` passes.
- Regression located real Stmt defect: `Stmt.Step` routed DML through `DB.Query`; fixed to use prepared execution for non-read statements. SQL-level native lifecycle now succeeds. Converter/harness still omits source C-API operations; fixture must not inject expected rows.

## cacheflush source contract

- `../sqlite/src/main.c:917-945`: iterate attached DBs only in WRITE transaction; `sqlite3PagerFlush` each; BUSY on any pager remembered while later attached pagers continue flushing.
- `../sqlite/src/pager.c:4680-4697`: flushes only dirty pages whose `nRef==0` via pagerStress; retains transaction and rollback journal.
- Frigolite pager tracks dirty pages, but has no equivalent page reference count/pin interface; `Pager` stores `pages map[uint32]*Page` and `dirty map[uint32]bool`, with no refcount. `Pager.Flush()` calls commit-phase `flushAll`, clears dirty state and finalizes journaling; unsafe substitute. Lock registry `ReadTxByOther` detects external prepared reads, but blocking on any such read is not equivalent to flushing only `nRef==0` dirty pages; current executor may have pinned active pages. Safe cacheflush requires a pager-level page pin/refcount and pagerStress-like API, plus attached-DB loop with busy aggregation. Current architecture cannot implement this source-faithfully without that foundational pager capability; direct flush or busy-check-only shim rejected.
- `FRIGOLITE_TEST=cacheflush go test . -run '^TestSQLiteSuite$' -count=1` fails only `cacheflush-2.2.3`: missing operation means expected `database is locked` but gets success.

## Progress and evidence

- changes2 implementation: converter now captures prepare/step/reset/finalize as ordered fixture events with opaque statement handle identity; runtime assertion marker carries source Tcl result expression. Harness executes against actual `Stmt` handles and resolves step result/connection `LastChanges`. Regenerated only `testdata/changes2.json` from `../sqlite/test/changes2.test`; final SELECT expectation is source-authored, not injected as rows. Converter marker test and exact harness family now pass.
- Native regression found two engine defects: `Stmt.Step` routed DML through `DB.Query`; changed to execute prepared DML. UPDATE RETURNING finalizer discarded `Changes`; `finishUpdate` now preserves count from RETURNING rows. Native tests pin both statement lifecycle and changes count.
- Focused changes2, attach2, converter packages, native lifecycle tests, and `git diff --check` pass. `FRIGOLITE_TEST=cacheflush ...` still fails 2.2.3 with expected SQLITE_BUSY missing.
- Latest full `go test ./...` after source fixture and engine fixes completed exit 1, `/tmp/frigolite-go-test-all-capi-fixtures.log`, root 361.197s; 1907 failure headers. changes2 no longer appears among failures; cacheflush-2.2.3 remains. Other residual failures from earlier full run still present; no claim of suite green. Other failures include check 7.x, collate7/collate9, vtab_alter 3.2, vtab_shared, walcrash2, where/where2/where3+ clusters, with2/with6, without_rowid2/3/4/5/7, plus `TestJoinHRightUsingSubqueryDuplicateName` and `TestP6_NestedJoinSameName`. Exact log `/tmp/frigolite-go-test-all-final.log`.

## Next steps

1. Complete native Stmt regression in public API; keep DML `Stmt.Step` fix and run public C-API conformance cases.
2. Extend converter to emit typed source-order prepared-statement events, retaining opaque handle identity. Runtime assertion must derive SQLITE_ROW/DONE and db changes from actual Stmt results; do not guess at conversion time. Add fixture/harness test first.
3. Implement pager reference counts and pagerStress-like dirty-page flush preserving active rollback transaction, then DB cacheflush iterating attached write pagers and aggregating BUSY while continuing other DBs. Add cross-connection read-lock and attached-db tests.
4. Regenerate only authoritative `changes2.json` and `cacheflush.json`; verify both focused families, attach2, diff check, and rerun full suite with residual log.
