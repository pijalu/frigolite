# PLAN — S47 slice 1: indexexpr3-1.5/1.6 Function-count marker (2026-11-24)

## State

`indexexpr3-1.5` and `1.6` bake `SELECT 0;` into the fixture. The Tcl
`do_hasfunction_test` proc first counts `Function` opcodes in `EXPLAIN $sql`
through a `db eval` callback, then runs `SELECT $nFunction; $sql`. The
converter cannot evaluate the runtime `db eval` at conversion time and
substitutes 0. The two leaves fail only on the marker row: the real query
row already matches.

## Contract

- SQLite's program for both SQL bodies emits two `Function` opcodes
  (`json_extract` plus `json_insert` / the `coalesce` wrapper's extract).
- The fixture expectation is `2 {"y":"two"}`.

## Approach

1. Pure-Go pin: create the indexexpr3 schema, assert the engine's EXPLAIN
   contains exactly two `Function` rows for each SQL, and assert the query
   returns `{"y":"two"}`.
2. The pin initially found 0 `Function` rows: the synthetic result-row pass
   emitted only Column/literal opcodes. Emit `Function` rows for non-aggregate
   scalar calls (arguments first), skipping COALESCE/IFNULL/NULLIF which C
   compiles with NotNull/String8 jumps. This makes the runtime count real.
3. Correct the two fixture SQL prefixes from `SELECT 0;` to `SELECT 2;`
   (same precedent as bestindexF-1.4.1: a converter-time artifact, not an
   engine gap). Do not weaken the expected row set.
4. Verify focused family, a converter-unit sanity check, full-suite
   failure-name-set gate, then commit code/fixture + docs.

## Verification

- `FRIGOLITE_TEST=indexexpr3`: 0 FAIL.
- Pure-Go pin green.
- Full gate NEW=0 vs the S46 gate, with `indexexpr3` 2→0.
