# Session handoff — S52 slice 1: expridx1 imposter residue (2026-11-24, 0fec78348)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Engine/converter/fixture/pins are committed at
`0fec78348`; this docs commit closes the unit. Working tree clean afterwards.

## Landed

- `0fec78348` — `expridx1` 11→0 (family focused green), including the
  previously unreachable imposter-distorted seek/delete/integrity leaves:
  - Converter and fixture now honour `SQLITE_TESTCTRL_IMPOSTER`: the next
    `CREATE TABLE x1` gets a `writable_schema` rootpage rewrite to the index
    root, so DML distorts the index the way C's `TF_Imposter` does. Do not
    regenerate `testdata/expridx1.json` wholesale — generator scaffolding has
    unrelated drift.
  - Integrity/quickcheck skip the table that shares a root with an index
    (`schemaRootIsImposter`), matching C's `tableSkipIntegrityCheck`.
  - CREATE INDEX key text dewraps balanced outer parens in both write and
    query parsers, so `(b+0.0)` and WHERE `b+0.0` are the same canonical key.
  - Correlated expression bounds (`b+0.0=o.b+0.0`, `+c=o.c`) are evaluated
    against the outer row in planner and executor; an outer `o.c` is never
    mistaken for the sought table's own column.
  - WITHOUT ROWID seeks are re-enabled with a covered secondary-index loop:
    key columns + PK tail, unary `+col` recognized as covering `col`, and a
    regular-scan fallback when not covering.
  - WITHOUT ROWID secondary autoindexes are now maintained on DML. The
    clustered PK has no `sqlite_schema` index entry; any empty-SQL autoindex
    entry is a real UNIQUE index and its PK tail must be written.
  - Stale-key deletion: `DeleteIndexEntryByTail` locates an imposter-distorted
    entry by rowid/PK tail when the recomputed exact key is absent, mirroring
    C's cursor-positioned `OP_IdxDelete`. The fallback is gated to imposter
    roots; an ungated version deterministically broke `collate1`, `pager1`,
    `pagerfault`, and `conflict`.
  - `integrity_check` reports 1–2 float64 ULP drift as "stores an imprecise
    floating-point value for row N"; exactly 3 ULPs stays "row N missing".
    ULPs count in the drift direction (the step below a power of two is half
    the step above it).
  - Pure-Go pins (`TestExpridx1EnginePins`) and a converter imposter unit
    cover the four engine contracts.

## Gate evidence

- Focused `/tmp/t11_refactor_expridx1.log`: 0 failures. Pins and
  `TestSQLite3ImposterControlEmitsRootpageRewrite` green; converter package's
  7 other failures reproduce at clean HEAD.
- Neighbour focused raw-byte leaf sets equal to clean HEAD: `collate1`,
  `conflict`, `pager1`, `pagerfault`, `e_reindex`, `indexexpr2`,
  `without_rowid1`, `delete4`, `vacuum-into`.
- Final full gate `/tmp/t11_full4.log` (1150.308s): 1339 leaves. Raw-byte
  diff vs durable `/tmp/t10_full2.log` (1351): **NEW=0**, GONE=12 (all 11
  `expridx1` leaves + `temptable2-4.1.1` randomblob flake). vs
  `/tmp/s50_full3.log` (1383): NEW=0, GONE=44. vs clean HEAD the only NEW is
  `vacuum-into-500`, a full-gate order flake: focused current/HEAD runs have
  the identical 10-leaf set and the durable baseline already fails it.
  `/tmp/t11_full4.log` is leaf-identical to pre-refactor `/tmp/t11_full3.log`.
- Complexity: no new gocognit/gocyclo findings over the clean-HEAD baseline
  in touched files (new helpers extracted). Staticcheck and hard file-size
  findings are pre-existing. Pre-commit build + SOLID checks passed.

## Restart point

Next unit: **A2 / A9 engine slices**, then the `fts3*` residue (~53 leaves).
Memory: full gates need `-timeout 30m`; run detached (tool cap 300s) and diff
raw-byte leaf sets, never line/count greps.

## Risks / working rules

- Imposter emulation is test-only; the engine-visible marker is a shared
  table/index rootpage. Keep the delete fallback imposter-gated.
- WITHOUT ROWID PK is the clustered key and has no `sqlite_schema` index row;
  secondary UNIQUE autoindexes do, and must carry the PK tail on every DML.
- Expression-index keys must match canonical SQL exactly; correlated bounds
  are runtime constants, but any expression touching the sought table is a
  residual filter, not a bound.
- `collate1-8.2` was a real NEW regression from the WR seek path, fixed by
  populating the WR UNIQUE autoindex — not by disabling the seek again.
- Compare full gates by raw-byte leaf-name sets only (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Keep gates
  sequential and exclusive CPU; store logs under `/tmp`.
