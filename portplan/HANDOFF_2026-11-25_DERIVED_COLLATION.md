# Handoff — derived-table column name and collation (2026-11-25)

Consumes `portplan/AUDIT_2026-11-25_STATE_REVIEW.md` C4/C6/C7 triage.

## Triage (no engine change)

- **C6 `normalize-350`**: `SELECT x'abc'` (odd hex digits) errors in both
  engines with `unrecognized token: "x'abc'"`; the fixture's `stmt_finalize`
  names `stmt_10` while its `prepare_stmt` stores `code` — converter artifact.
  Same variable mismatch drives the other 45 `normalize` leaves.
- **C7 `triggerC-3.3.1`**: `SELECT * FROM t3b` → `no such table: t3b` in
  sqlite3 3.54 too; fixture creates `t3` only. Converter artifact.
- **C1 `e_blobopen-3.2.5`**: fixture dropped the incremental-blob write of
  `xxx`; both engines return the stored blob. Converter artifact.
- **C3 `join3-3.1`**: fixture's expected join-limit text is missing its
  runtime number; sqlite3 with no `t1` gives `no such table: t1`, same as
  Frigolite. Converter artifact.

## Genuine engine bug fixed (C4-adjacent, `e_select-1.7.4b.*`)

Oracle (`sqlite3` 3.54) with the fixture's t5/t6 setup:

```
SELECT * FROM (SELECT a COLLATE nocase, b FROM t6) AS x, t5 ON (x.a=t5.a)
  SQLite: aa|cc|AA|cc / bb|DD|BB|dd
  Frigolite (before): 0 rows
```

Two independent losses:

1. `selectColumnName` used `exprResultName` for a top-level COLLATE item, so
   the derived column was named `a COLLATE nocase` instead of `a`. C's
   `sqlite3ColumnsFromExprList` skips COLLATE via
   `sqlite3ExprSkipCollateAndLikely` (../sqlite/src/select.c:2271).
2. Derived `ColumnDef`s carried only names (`resultColumnDefs`), so NOCASE
   never reached the join ON comparison. C's `sqlite3SubqueryColumnTypes`
   copies `sqlite3ExprCollSeq` onto each Column (select.c:2427-2432).

### Changes

- `internal/execquery/select_colnames.go`: `skipCollateForName` unwraps
  top-level COLLATE before bare-ref naming in `selectColumnName`.
- `internal/execquery/select_join.go` (`materializeSubqueryJoin`): copy
  `e.selectOutputCollations(subquery)` into `rightDefs[i].Collate`.
- `internal/execquery/select_join_derived.go` (`buildSubqueryRowMaps` plain
  path): wrap values with `CollatedValue` from the inherited collation, the
  same way `buildViewRowMaps` already does; the qualified key now uses the
  wrapped value too.
- `frigolite_derived_collation_test.go`: `TestDerivedTableCollationAndColumnName`
  pins both the `[a]` column name and the exact two rows.

## Verification

- Pre-change repro: Frigolite 0 rows vs SQLite two rows (oracle outputs above).
- Post-change: Frigolite returns the same two rows; pin PASS; `gofmt` clean.
- Focused harness diffs vs t30 (correct family-substring filter): `e_select`
  9→6 (GONE `1.7.4b.1/.2/.3`, NEW=0); `select`/`join`/`subquery`/`view`/
  `cte`/`colname`/`union`/`values` all NEW=0.
- Full 12-shard harness at this change (run dir
  `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.xOU4EE`,
  12/12 logs+status, no kills): leaf FAIL set 1018 vs t30 baseline 1028,
  **NEW=0**, GONE=10 (the 6 pre-existing HEAD fixes plus the three
  `e_select-1.7.4b.*` leaves fixed here and `resetdb-300`, a documented
  unseeded-randomblob flake). Family FAIL set 190 vs 192, NEW=0.
- `go test -run TestSOLID_ ./...`: rc=0, 27 packages ok (`/tmp/derived_solid.log`).

## Docs

- `TODO.md`: new DONE item.
- `.agents/lessons_learned.md`: "Derived-table columns carry name AND
  collation" section.
- This handoff.
