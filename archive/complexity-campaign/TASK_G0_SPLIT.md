# TASK G0.5 — God-file Split (SOLID prerequisite, behavior-preserving)

> **Phase**: G0.5 (PREREQUISITE — must complete before G1 feature work)
> **Goal IDs**: G0.5.SELECT-SPLIT, G0.5.ALTER-SPLIT, G0.5.INSERT-SPLIT
> **Read first**: `PORTPLAN.md` §0, **`portplan/DESIGN.md` §A (execution model) +
> §B (this split)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started
>
> **Why before features:** `internal/exec/select.go` is **9039 lines**, `alter.go`
> 4657, `insert.go` 3737, `expression.go` 3566, `ddl.go` 3318. Every G1/G2/G4
> change lands in these god-files → merge-conflict hotspot + SOLID/gocyclo
> violations. Split first, behavior unchanged, so feature goals edit focused files.

---

## Objective

**Behavior-preserving refactor only.** Move code into sibling files within the same
package `internal/exec`. No signature changes, no logic changes, no API changes.
The entire existing test suite (hand-written + testgen) must stay byte-for-byte
green before/after.

**Mode**: refactor — prove equivalence with the full suite. Delete complexity
before adding abstraction (none added here — just relocation).

---

## Goal G0.5.SELECT-SPLIT — Split select.go

**Design** (DESIGN §B): split `select.go` (9039 ln) into:
- `select.go` — top-level `execSelect` dispatch + result assembly.
- `select_from.go` — FROM-clause resolution (tables, derived tables, value-lists).
- `select_join.go` — join execution (inner/left/right/full/cross/natural/using).
- `select_group.go` — GROUP BY / HAVING / DISTINCT / aggregate evaluation.
- `select_setop.go` — UNION / UNION ALL / INTERSECT / EXCEPT.
- `select_window.go` — window-function pass (currently stub; G4 fills — move as-is).
- `select_order.go` — ORDER BY / LIMIT / OFFSET / sort.
- `select_cte.go` — WITH / recursive CTE (currently stub; G4 fills — move as-is).
- `select_view.go` — view expansion + SQL serialization.
- `select_subquery.go` — scalar/correlated subquery, EXISTS, row-value.

**Rule:** each split file gets a package-level doc comment naming its
responsibility. Helpers stay `func` (not exported) — same package. Do not change
any function body.

**Verify command** (equivalence):
```bash
go build ./... && make quality && \
go test ./... && \
go test -tags testgen -count=1 -timeout 300s \
  ./testgen/select1/ ./testgen/select2/ ./testgen/select3/ ./testgen/select4/ \
  ./testgen/select5/ ./testgen/select6/ ./testgen/selectB/ ./testgen/selectG/ \
  ./testgen/join/ ./testgen/joinD/ ./testgen/view/ ./testgen/unionall/ \
  ./testgen/count/ ./testgen/distinct/ ./testgen/rowvalue/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && echo SELECT_SPLIT_OK
```
The testgen packages selected are the ones that exercise the split areas; they
must be IDENTICALLY green before and after the move (capture the before-state with
the same command).

**Todos**:
1. Capture the "before" verify output (above) as the equivalence baseline.
2. Split `select.go` into the target files, moving whole functions verbatim; fix
   imports per file; ensure each file compiles.
3. Run `make quality` — resolve any gocyclo/gocognit newly exposed (extract
   helpers *only if* a moved function now trips a limit; keep behavior identical).
4. Re-run the verify command; confirm byte-identical results vs baseline.
5. Commit `G0.5.SELECT-SPLIT: behavior-preserving split of select.go`; push.

---

## Goal G0.5.ALTER-SPLIT — Split alter.go

**Design** (DESIGN §B): split `alter.go` (4657 ln) into:
- `alter_rename.go` — RENAME TO / RENAME COLUMN + dependency rewrite.
- `alter_addcol.go` — ADD COLUMN (default, generated, NOT NULL).
- `alter_dropcol.go` — DROP COLUMN (slot removal, index cleanup, rebuild).
- `alter_rebuild.go` — table-rebuild SQL generation.

**Verify command**:
```bash
go build ./... && make quality && \
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/alter/ ./testgen/altercol/ ./testgen/altertab/ ./testgen/alterdropcol/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && echo ALTER_SPLIT_OK
```

**Todos**: same shape as SELECT-SPLIT (capture baseline → move verbatim → quality
→ verify identical → commit `G0.5.ALTER-SPLIT`).

---

## Goal G0.5.INSERT-SPLIT — Split insert.go

**Design** (DESIGN §B): split `insert.go` (3737 ln) into:
- `insert.go` — top-level `execInsert` dispatch + VALUES handling.
- `insert_select.go` — INSERT … SELECT.
- `insert_conflict.go` — ON CONFLICT / OR REPLACE / UPSERT resolution (ties to G4).
- `index_maintain.go` — `maintainIndexesOnInsert` + `checkUniqueIndex` + helpers
  (these are index-maintenance, logically separate from insert dispatch).

**Verify command**:
```bash
go build ./... && make quality && \
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/insert/ ./testgen/upsert/ ./testgen/conflict/ ./testgen/index/ \
  ./testgen/unique/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && echo INSERT_SPLIT_OK
```

**Todos**: same shape (baseline → move → quality → verify identical → commit
`G0.5.INSERT-SPLIT`).

---

## Definition of Done (this task)
- All three splits done; `internal/exec/` files are SOLID-sized; `make quality`
  passes; the full `go test ./...` + the captured testgen baselines are
  byte-identical before/after (zero behavioral change).
- `PORTPLAN.md` §5 G0.5 row → 🟢.
