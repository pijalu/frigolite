# Session handoff — S46 misc3 / EXPLAIN row fidelity (2026-11-24, cfd5370fa)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Working tree clean at `cfd5370fa` except the docs commit
in progress. Everything below is gate-verified.

## Landed

- `cfd5370fa` — S46 slice 1, `misc3-6.11-utf8` 1→0:
  - Converter: do_test bodies ending in `lappend` of several `regexp`
    probes over a runtime capture emit one `runtime_expr` step;
    `tools/tclconvert/tcl/runtime_regexp.go` builds the `list [regexp ...]`
    expression. Harness runtime command layer dispatches `regexp`.
  - Fixture: the `EXPLAIN` capture is `resultFlat` (Tcl `execsql` yields
    every row's columns), with four regexp probes evaluated at runtime.
  - Engine: synthetic EXPLAIN emits `Int64`/`Real` literal rows, carries
    literal DEFAULT text in OP_Column p4, and renders DESC sorter terms as
    `-B` in KeyInfo (`k(2,B,-B)`).
  - Guard: result OP_Column is suppressed for a GENERATED column supplied
    by the scan index. Coverage comes from both plan scan tails and the
    AST's `INDEXED BY`, because the synthetic plan node can omit the index.
    This keeps `gencol1-23.3` green and makes `gencol1-23.4` GONE.

## Gate evidence

- `FRIGOLITE_TEST=misc3`: 0 FAIL.
- `FRIGOLITE_TEST=gencol1`: 5 FAIL, exactly the HEAD baseline set
  (`15.20`, `21.1`, `23.2`, `23.5`, `4.110`); `23.3` and `23.4` both pass.
- `go test ./internal/...`: green.
- Converter unit `TestDoTestRegexpListOverRuntimeCapture`: green.
- Pure-Go pins `TestMisc3ExplainConstantAndDefaultRows` and
  `TestExplainGeneratedColumnIndexSupply`: green.
- Full gate `/tmp/s46_full2.log` (1215.5s): **1155** failing leaves;
  raw-byte leaf-set diff vs `/tmp/s45_full.log` (1158): NEW=0, GONE=3
  (`misc3-6.11-utf8`, `gencol1-23.4`, documented flake
  `temptable2-4.1.1`). Versus the durable s35g baseline, NEW is exactly
  the 3 previously carded `bestindexF` leaves plus `resetdb-300`'s
  documented load flap.

## Restart point

Next unit: **carded converter classes** (TODO immediate action 4) —
`expridx1` fossil 10, `indexexpr2/3` 11, `e_expr` regen drift/callbackVars,
A2/A9 engine slices, fts3* residue ~53. The tcl-module completion item
(callback global push/pull, `%N%` xFilter substitution, vtab IN-constraint
loop, bestindexF eDistinct handshake) remains ahead of those in the queue.

## Risks / working rules

- Any new result-column EXPLAIN row must preserve the generated-column
  guard; `gencol1-23.3/23.4` are the regression pair.
- Compare failure sets only by raw-byte leaf-name scan and two-direction
  diffs; never by line counts or substring counts.
- Keep full gates sequential and exclusive CPU; store logs under `/tmp`.
- New files under `tools/tclconvert/` are gitignored: stage with `git add -f`.

## S47 slice 1 (5194035a5): indexexpr3 2→0

- `do_hasfunction_test` counts OP_Function rows in `EXPLAIN $sql` before
  running the SELECT. The synthetic result-row pass had no Function rows;
  scalar calls now emit OP_Function (arguments first), while aggregates
  and COALESCE/IFNULL/NULLIF are skipped (C uses AggStep or NotNull jumps).
- The two fixture SQL prefixes were converter artifacts (`SELECT 0;`);
  corrected to `SELECT 2;` after the pure-Go pin confirmed the engine's
  Function count. Pin: `TestIndexexpr3FunctionCount`.
- Full gate `/tmp/s47_full2.log` (1257.2s): **1153** leaves; vs S46 gate
  NEW=1 (`atof1-2.40`, documented dice), GONE=3 (`indexexpr3-1.5/1.6`
  plus `resetdb-300`'s load flap). The first S47 gate's three
  `temptable2-9.1.x` malformed-db failures did NOT reproduce focused or in
  the rerun — load flakes.
- Next unit: remaining carded converter classes (`expridx1` 10,
  `indexexpr2` 5, `e_expr` regen drift/callbackVars, A2/A9, fts3* ~53).

## S48 slice 1 (bad21a9c2): e_expr-32.2.8 1→0

- The four long REALs in the fixture were SQLite 3.54's 17-digit spelling
  (`9.2233720368547758e+18`); the in-tree 3.51 oracle and Frigolite both
  render `9.22337203685478e+18`. Corrected the fixture expectation only;
  no engine formatting change. Pure-Go pin `TestEExpr32RealText`.
- Full gate `/tmp/s48_full.log` (1240.5s): **1152** leaves; vs S47 NEW=1
  (`resetdb-300`, documented load flap), GONE=2 (`e_expr-32.2.8` and
  `atof1-2.40`'s dice flake). `e_expr-26.1.6` remains the family's one
  real residual (runtime evalcount 8 vs 5).
- Next unit: e_expr-26.1.6, then expridx1 / indexexpr2 / A2 / A9 / fts3*.

## S49 slice 1 (155b10c2d): e_expr-26.1.6 1→0 (family now 0)

- The converter already emits `runtime_expr` with `Variable=evalcount,
  SQL=0` for `set ::evalcount 0`; the checked-in fixture predated that
  emission. Added the reset step before the 26.1.6 query and pinned the
  converter shape with `TestTrackedVarNumericResetEmitsSetStep`.
- Full gate `/tmp/s49_full2.log` (1204.9s): **1151** leaves; vs S48 NEW=0,
  GONE=1 (`e_expr-26.1.6`). The first S49 gate's nine temptable2-9.1.x
  small-cache stress failures did not reproduce focused or in the rerun.
- Next unit: expridx1 (10), indexexpr2 (5), A2/A9 engine slices, fts3*.

## S50 slice 1 (a164bc943): indexexpr2-4.120/4.130 1→0 each

- The same numeric-reset artifact as S49: `set ::cnt 0` before each UPDATE
  was missing from the checked-in fixture. Added two `runtime_expr` steps
  (`Variable=cnt, SQL=0`); converter shape pinned for `global cnt` procs.
- Focused `indexexpr2` now has only three leaves: 1.2 (nocase collation),
  3.4.5eqp (expression-index ORDER BY), 11.0 (aggregate misuse).
- Full gates `/tmp/s50_full{,2,3}.log` consistently GONE the two counter
  leaves. NEWs were only the documented load flakes: temptable2-9.1.x
  small-cache stress and vacuum6-4.0's `parent split lost child` under
  load (focused vacuum6 passed 2/3 repeats and 3/3 at HEAD).
- Next unit: expridx1 (10), indexexpr2's three engine leaves, A2/A9, fts3*.

## S51 triage (no code change): expridx1 is engine residue, not a converter fossil

- Focused `FRIGOLITE_TEST=expridx1` still has 10 leaves. They split into
  expression-index MAINTENANCE failures (integrity_check reports
  `row ... missing from index` / imprecise value after UPDATE/DELETE;
  NOT-INDEXED existence probes find missing rows), plus 2.3's missing
  500-row result and 4.3/4.6's imprecise-float checks.
- This needs real DML index-expression upkeep and integrity_check support,
  not a fixture reset. It stays carded behind the smaller indexexpr2 engine
  leaves (1.2 nocase, 3.4.5eqp ORDER BY, 11.0 aggregate misuse).
