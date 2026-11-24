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
