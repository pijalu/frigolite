# Handoff — NOTADB write gate + cross-engine matrix (2026-11-25)

Consumes audit F2 and F3.

## F2 — junk-header file reports NOTADB on writes

`misc5-4.1#01` writes `This is not really a database` to a file, reopens it,
then expects `file is not a database` on `CREATE TABLE`. Native repro:

- Before: `Open` succeeds (lazy, correct), `SELECT * FROM sqlite_master`
  → `file is not a database` (correct), `CREATE TABLE` →
  `attempt to write a readonly database` (wrong).
- sqlite3 3.54: `CREATE TABLE` → `file is not a database`.

Cause: the junk bytes put header byte 18 > 1, so `pager.Open` marks the
pager read-only. `Engine.execEntry` applied the READONLY write gate before
the per-statement `ValidateHeader` ran. Fix in
`internal/exec/engine_core.go`: call `e.pager.ValidateHeader()` ahead of the
READONLY gate and surface its error. Reads already validated later.

Pin: `frigolite_notadb_write_gate_test.go`
(`TestNotADBOnWriteStatement`, both CREATE and INSERT).

## F3 — broader cross-engine file matrix

`TestS66SQLiteFileCompatibility` covered one 500-row blob-index shape.
Added `frigolite_cross_engine_matrix_test.go` with four shapes —
rowid table with 3000-byte TEXT (overflow), WITHOUT ROWID, secondary index,
auto_vacuum with DELETE — and both directions (Frigolite writes → sqlite3
reads with `integrity_check`, and sqlite3 writes → Frigolite reads with
`integrity_check`), comparing complete row sets.

All four subtests pass with sqlite3 3.54 on this change. No engine change
was needed for the matrix — it is new coverage, not a bug fix.

## Verification

- Focused harness vs t30 (family-substring filter): `misc5` 2→1 (GONE
  `misc5-4.1#01`, NEW=0); `rdonly`, `openv2`, `corrupt`, `filefmt`, `attach`,
  `rdonly2` all NEW=0.
- Full 12-shard harness at this change (run dir
  `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.Vy21sY`,
  12/12 logs+status, no kills): leaf FAIL set 1015 vs t30 1028, **NEW=0**,
  GONE=13 (prior 12 + `misc5-4.1#01`). Family 190 vs 192, NEW=0.
- `go test -run TestSOLID_ ./...`: rc=0, 27 packages ok.
- Pins: `TestNotADBOnWriteStatement`, `TestCrossEngineFileMatrix` (4/4
  subtests), and `TestS66SQLiteFileCompatibility` all PASS.

## Docs

- `portplan/AUDIT_2026-11-25_STATE_REVIEW.md` §5 updated with F2/F3 outcomes.
- `.agents/lessons_learned.md`: header-before-READONLY and matrix sections.
- `TODO.md`: new DONE items.
