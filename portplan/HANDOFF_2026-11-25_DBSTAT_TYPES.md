# Handoff — dbstat declared column types (2026-11-25)

Consumes audit F1. Genuine engine divergence, fixed.

## Evidence

`PRAGMA table_info(dbstat)` before the fix returned empty type strings for
all ten visible columns; sqlite3 3.54 reports `TEXT`, `TEXT`, `INTEGER`,
`TEXT`, `INTEGER` × 6. C's declaration is `../sqlite/src/dbstat.c:68-84`
(`zDbstatSchema`, including hidden `schema TEXT` and `aggregate BOOLEAN`).

## Change

- `internal/vtab/dbstat.go`: `dbStatVTab.ColumnTypes()` returns the twelve
  declared types from `zDbstatSchema` (implements `vtab.ColumnTypeInfo`).
- `internal/exec/vtab_eponymous.go`: `eponymousVtabColDefs` copies
  `ColumnTypes()` onto the rendered `ColumnDef`s, so `PRAGMA table_info`
  and `PRAGMA table_xinfo` show the declared types.
- `frigolite_dbstat_types_test.go`: `TestDBStatDeclaredColumnTypes` pins all
  ten visible name/type pairs against the sqlite3 3.54 output.

No on-disk format change; both engines still read each other's files through
the existing `TestS66SQLiteFileCompatibility`.

## Verification

- Native pin PASS; `gofmt` clean on changed Go files.
- Focused harness diffs vs t30: `stat` 22→20 (GONE `stat-0.0`, `stat-0.1b`,
  NEW=0); `dbstat`/`tabfunc`/`vtab`/`fts3tokenize`/`pragma` NEW=0.
- Full 12-shard harness at this change (run dir
  `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.VP8Nrh`,
  12/12 logs+status, no kills): leaf FAIL set 1016 vs t30 1028, **NEW=0**,
  GONE=12 (prior 10 + `stat-0.0`/`stat-0.1b`). Family 190 vs 192, NEW=0.
- `go test -run TestSOLID_ ./...`: rc=0, 27 packages ok.
- `go test -run TestS66SQLiteFileCompatibility -count=1 .`: PASS (no on-disk
  format change).

## Docs

- `portplan/AUDIT_2026-11-25_STATE_REVIEW.md` §5 records C1/C2/C3/C5/C6/C7
  reclassifications and the F1 fix.
- `.agents/lessons_learned.md`: vtab declared-types section.
- `TODO.md`: new DONE item.
