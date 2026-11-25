# Plan — S65: multi-word `expr` converter fidelity (2026-11-26)

Status: LANDED at `c1cc2486e`; full gate NEW=0 vs t28/t10.

Mode: test-first — the converter defect reproduces deterministically in the
checked-in `temptable2.json`; engine behavior is already pinned by the
pure-Go `TestS64TempPageCount`.

## Evidence

- TCL source (`../sqlite/test/temptable2.test:121-124`, `147-150`):
  - `set n [db one {PRAGMA temp.page_count}]` followed by
    `expr ($n >280 && $n <300)` with expectation `1`.
  - `expr ($n >270 && $n <290)` with expectation `1`.
- Current fixture (`testdata/temptable2.json`):
  - `temptable2-4.1.4`/`5.1.2` emit only `query_assign` with
    `expect: "1"`; the range assertion is lost.
  - Focused log `/tmp/t29_fix_temptable2.log`: got `[316]`/`[304]`, want `[1]`.
- Converter code (`tools/tclconvert/tcl/interp_sql.go:1321`): the trailing
  `expr` branch requires `len(lastCmd) == 2`. The unbraced parenthesized
  TCL expression is parsed as multiple words (`expr`, `($n`, `>280`,
  `&&`, `$n`, `<300)`), so the branch never emits a `runtime_expr`.
- Harness already compares `Step.ExpectedExpr` at runtime for `query`
  (`frigolite_harness_test.go:5780`) and `runtime_expr`
  (`frigolite_harness_test.go:6259`).

## Change (one coherent slice)

1. Add a converter unit pin in `tools/tclconvert/tcl/` for a `do_test` body
   with `set n [db one {...}]` followed by multi-word
   `expr ($n >280 && $n <300)`. Require:
   - `query_assign` carries the variable and no expectation;
   - a `runtime_expr` step carries the joined expression and `Expected: "1"`.
2. In `cmdDoTest`, make the trailing-expr branch accept `len(lastCmd) >= 2`
   and join `lastCmd[1:]` with spaces before
   `exprReadsRuntimeVar`/`bakeKnownExprVars`, matching `commandExpr`'s
   `strings.Join(args, " ")` behavior. Extract the branch to a small helper
   if the normalized quality baseline moves.
3. Patch only the two checked-in fixture cases (do not regenerate the whole
   family): convert `temptable2-4.1.4` and `temptable2-5.1.2` to a
   `query_assign` without `expect` plus a `runtime_expr` step preserving the
   TCL expression and expectation.
4. Do not change the engine's page-count behavior without an oracle ruling:
   first make the fixture assert the original boolean, then adjudicate
   316/304 against the TCL ranges with the SQLite CLI.

## Adjudication

- SQLite CLI 3.54 with page_size=1024: the two TCL sequences produce
  temp page counts 287 and 277, inside the asserted ranges.
- Frigolite's staged probe produced 303/305 (and the focused harness
  316/304): table-leaf density is exact (127 pages for 500 rows, same as
  SQLite), but index pages use 176 pages versus SQLite's ~128.
- `dbstat` histogram: SQLite index leaves are mostly 4 cells/page
  (1×1, 20×2, 49×3, 78×4); Frigolite is 12×1, 59×2, 58×3, 49×4.
- The local index split (`splitLeafMulti` +
  `balanceIndexDividerPartitions`) does not gather sibling index leaves
  the way C's `balance_nonroot` does. Full index-balance parity is a
  separate btree-density unit; S65 only restores the fixture's true
  assertion and does not weaken or retarget it.

## Verify

- Converter pin RED before the code change, GREEN after.
- `FRIGOLITE_TEST=temptable2 go test -run '^TestSQLiteSuite$' -v`: no NEW/GONE
  leaves versus `/tmp/t29_full_gate2.log` leaf set.
- `go test ./tools/tclconvert/... -count=1` and
  `go test ./internal/... -count=1`.
- Normalized quality diff versus `/tmp/t29_fix2_quality.log`: NEW=0.
- Detached full gate with raw-byte leaf diff versus
  `/tmp/t29_full_gate2.log` and `/tmp/t10_full2.log`: NEW=0.

## Final verification

- Code commit: `c1cc2486e`; pre-commit build/SOLID checks passed.
- Converter pin RED→GREEN (`/tmp/t30_pin_red.log`, `/tmp/t30_pin_green.log`).
- Converter package failures exactly match the 8-name baseline; no NEW.
- `go test ./internal/... -count=1` green (`/tmp/t30_internal.log`).
- Quality normalized NEW=0/GONE=0 (`/tmp/t30_quality.log`).
- Detached full gate `/tmp/t30_full_gate.log` (1220 leaves): NEW=0 vs t28
  and t10; only delta vs t29 is the known resetdb-300 load flap.
- The two `temptable2` leaves remain, now asserting the original TCL ranges;
  index-density parity is carded as S66.

## Risks

- A generic `len(lastCmd) >= 2` change could capture unrelated `expr` forms;
  the `exprReadsRuntimeVar` gate remains mandatory.
- Focused fixture patching avoids whole-family converter drift (S63 lesson).
- Never run focused gates concurrently in one worktree.
