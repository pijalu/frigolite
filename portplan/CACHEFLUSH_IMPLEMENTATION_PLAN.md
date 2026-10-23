# sqlite3_db_cacheflush implementation plan

## Contract and call-path evidence

SQLite `main.c` visits attached DB contexts in schema order, only in WRITE transaction state. It remembers first BUSY while continuing later DBs. SQLite `pager.c::sqlite3PagerFlush` writes dirty pages with `nRef==0` through pagerStress; it is not commit or rollback-journal finalization.

Frigolite `DB.Exec` invokes `Engine.Exec` synchronously. `Engine.Exec` in `internal/exec/engine_core.go:775-791` marks statement trees and defers `releaseStatementTrees(treeMark)`; defer runs before return. `execDispatch` executes statement work before returning. Thus cacheflush at a standalone Engine/API boundary can regard completed statement cursors as released; no global pin table is needed.

## Ordered work and current evidence

1. Added `Pager.CacheFlush` separate from commit flush. Rollback-journal mode writes dirty cached images without clearing dirty state or finalizing journals; growth quota and external-file stamp are updated. WAL mode appends non-commit frames and index entries without publishing the shared header; pager snapshots rewind spilled frames on rollback/savepoint.
2. Added `Engine.CacheFlush` attach-order traversal and public `DB.CacheFlush`. It skips clean contexts, checks prepared and transaction-level readers, records BUSY while continuing to auxiliary pagers. Native tests cover dirty accounting, transaction/journal preservation, rollback/savepoint, reader BUSY, aux disk visibility, WAL uncommitted visibility and rewind.
3. Added TCL converter marker and harness status handling; regenerated only `testdata/cacheflush.json` from `../sqlite/test/cacheflush.test`. Converter regression pins emitted operation marker.
4. Verified focused `cacheflush`, `changes2`, `attach2`, native DB/pager/WAL tests, converter marker test, and `git diff --check` pass.
5. Full `go test ./... -count=1` completed exit 1. Root `TestSQLiteSuite` failed in 298 families (156.34s); `TestJoinHRightUsingSubqueryDuplicateNameUsesFirstColumn` and `TestP6_NestedJoinSameName` also failed. Output/status in `portplan/CACHEFLUSH_FULL_SUITE.{stdout,stderr,status}`; no green claim.

## Remaining

- Explicit read transaction registration already exists through `registerSharedTx` in `execSelect`; cacheflush now checks `SharedTxByOther` in addition to prepared-read `ReadTxByOther`. Exact cacheflush source fixture passes, including BUSY and continued auxiliary flush.
- WAL spill now appends frames with commit-size 0, adds private wal-index entries while leaving the published header unchanged, and preserves write-lock state. Pager snapshots capture the WAL frame/checksum position; rollback/savepoint restore truncates uncommitted spilled suffix before releasing writer lock. Native tests cover no premature commit marker, invisible rows to a second reader, rollback rewind, and subsequent committed write.
- Full-suite run `go test ./... -count=1` completed exit 1. Root package `TestSQLiteSuite` failed (156.34s) across 298 suite families; `TestJoinHRightUsingSubqueryDuplicateNameUsesFirstColumn` and `TestP6_NestedJoinSameName` also failed. Output/status are recorded in `portplan/CACHEFLUSH_FULL_SUITE.{stdout,stderr,status}` and failing-family names in `CACHEFLUSH_FULL_SUITE.failed-families`. No green claim.
- Focused cacheflush, changes2, attach2, native DB/pager/WAL tests, converter marker test, and `git diff --check` pass. Full-suite failures remain outside those focused checks.

## Constraints

Preserve unrelated worktree changes. Do not call commit-time `Pager.Flush`; do not add global cursor/page pin tracking. SQLite is ground truth; regression tests precede converter attribution.
