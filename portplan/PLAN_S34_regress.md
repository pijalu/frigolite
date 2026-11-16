# PLAN S34 — pager-merge leaf regressions (pragma2-5.1/5.3, sharedA-1.1/1.2)

STATUS: COMPLETE (2026-11-16) — fix landed 8c8061993: pragma2 + sharedA families green (sharedA x3 repeat clean); two-direction sweep diff over pragma*/shared*/jrnlmode*/pager*/trans/attach/corrupt* = exactly the 4 leaves left the set, nothing new, 42 pre-existing failures unchanged.

## Symptom (all reproduce at HEAD ffd22bf60; also red at pre-S31 base dd045611d)

1. `pragma2/pragma2-5.1` — exec error "table t1 already exists" on
   `PRAGMA page_size=16384; CREATE TABLE t1(x); PRAGMA cache_size=2; …`
   after the fixture's `close_db / delete_file test.db / reopen_db test.db`.
2. `pragma2/pragma2-5.3` — `PRAGMA cache_spill(-51); PRAGMA cache_spill` got
   [2000] want [3]. Downstream of 5.1: the CREATE aborts the same exec batch
   before `PRAGMA cache_size=2` runs, so the read reports the default cache.
3. `sharedA/sharedA-1.1` — exec error "database disk image is malformed" on
   `CREATE TABLE t1(x); INSERT INTO t1 VALUES(123);` on the second connection.
4. `sharedA/sharedA-1.2` — same malformed on `SELECT * FROM t1 ORDER BY x;`.

## Root cause (diagnosed with dir-probes + a playback stack trace; S34DBG)

Engine leaves a DELETE-mode `test.db-journal` on disk after a ROLLBACK
(`Pager.Restore` snapshot path never ends the journal epoch) and after a close
whose pager has no dirty pages (`flushFilePagesLocked`'s empty-dirty path does
nothing for DELETE mode). The fixture then deletes only `test.db` and reopens:
the fresh 0-byte file carries a *valid-header, record-bearing* sidecar.

C classifies that sidecar in `hasHotJournal` (pager.c) via `pagerPagecount`:
**nPage is derived from the DATABASE FILE size, `ceil(fileSize/pageSize)` via
xFileSize — never from any in-memory page count** — and when `nPage==0` the
journal is "a remnant from a prior database … the journal file can be deleted":
no playback, journal unlinked, fresh db stays empty.

Frigolite's probe (internal/pager/journal.go hotJournalProbe) guards the same
case with `p.numPages == 0` — but `schema.Init()` bootstraps an empty file by
materializing page 1 in memory, so the reopened pager reports **numPages==1**
and the probe classifies the stale journal HOT: playback truncates the fresh
file to the journal header's dbOrigSize (mxPg=130/3) and writes the old
before-images → the old schema resurrects (pragma2 "t1 already exists") or the
fresh file becomes a header-over-zeros image (sharedA malformed).

C evidence:
- pager.c hasHotJournal: `rc = pagerPagecount(pPager, &nPage); … if( nPage==0
  && !jrnlOpen ){ … sqlite3OsDelete(pVfs, pPager->zJournal, 0); }`.
- pager.c pagerPagecount: `nPage = sqlite3WalDbsize(pPager->pWal);` else
  `nPage = (Pgno)((n+pPager->pageSize-1) / pPager->pageSize);` — file-size
  derived, ceil.
- pragma2.test 5.1 preamble `db close; forcedelete test.db; sqlite3 db
  test.db` (journal already gone in C: COMMIT/ROLLBACK delete it); sharedA.test
  0.1 ends with `ROLLBACK; …` + `forcedelete test.db`.
- pragma.c PragTyp_CACHE_SPILL / pcache.c sqlite3PcacheSetSpillsize: read =
  `max(numberOfCachePages, szSpill)`; `szSpill` KiB→pages conversion
  `(-1024*N)/(szPage+szExtra)`; engine already matches (pragma_header.go
  pragmaCacheSpillFor) — no engine change needed for 5.3 beyond 5.1.

## Fix (engine, not fixture)

`internal/pager/journal.go` hotJournalProbe: replace the `p.numPages == 0`
stale-journal guard with C's file-size-derived page count — stat the db file
(p.file.Stat(); fall back to the cached p.fileSize on stat error), compute
`ceil(size/pageSize)`, and classify `hotJournalDelete` when it is 0. This is
exactly C's hasHotJournal→pagerPagecount pipeline (the probe runs before any
header read, matching C's ordering).

Removed: the temporary S34DBG debug print + s34dbg_test.go scratch file.

Files: internal/pager/journal.go (probe only). No fixture/harness edits.

## Verification

- `FRIGOLITE_TEST=pragma2` and `FRIGOLITE_TEST=sharedA` family runs: 0 fails,
  sharedA ×3 repeat (flake watchlist).
- Leaf-level: the four leaves' fixture outputs match C's expectations
  (5.1→2, 5.3→3, sharedA 1.1/1.2 →123).
- Neighborhood sweeps (family-level, -count=1): patterns pragma, shared,
  jrnlmode, pager, trans, attach, corrupt, waljournal. Pre-fix failing
  name-set captured (/private/tmp/s34_base_names.txt, 46 leaves). Post-fix:
  diff by NAME SET in both directions; require: the 4 leaves leave the set,
  nothing new enters, pre-existing failures unchanged.

## Carded (observed, NOT fixed — out of the 4-leaf charter)

1. ROLLBACK leaves the DELETE-mode journal file on disk (Snapshot/Restore
   never ends the journal epoch); close with zero dirty pages also leaves it.
   C's rollback (pager_playback → pager_end_transaction → journalDelete)
   removes it. With the probe fix the state is handled C-style at the next
   open (journal deleted without playback) — the latent divergence is only
   the on-disk residue, not observable through any currently-green fixture.
2. Close mid-transaction on a non-WAL pager FLUSHES (commits) dirty pages
   (Pager.Close → flushAll); C rolls back (pagerUnlockAndRollback). Not
   covered by any currently-red leaf; unforced churn on green families risk.
