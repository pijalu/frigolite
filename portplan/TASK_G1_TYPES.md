# TASK G1.TYPES — Types, affinity, CAST

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.TYPES.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.CREATE.
> **Current state: DONE** — all 8 testgen packages + TestP1Types pre-tests PASS.

## Objective
The SQLite type/affinity model is correct and complete: storage classes
(INTEGER/REAL/TEXT/BLOB/NULL), declared type → affinity mapping, affinity
application on insert/comparison/CAST, NUMERIC affinity quirks, integer-vs-real
canonicalization, the `INTREAL` internal type, and exact error text for bad
casts/conversions. `TYPEOF()` and `CAST(x AS T)` must match the oracle.

## Scope — testgen packages
`affinity`, `cast`, `numcast`, `types`, `intpkey`, `intreal`, `nulls`, `null`.

## Pre-test file
`frigolite_p1_types_test.go` — `TestP1Types_*`. Cases vs oracle:
- Declared type → affinity: rules in `src/build.c` (TEXT/NUMERIC/INTEGER/REAL/BLOB/NONE).
- Insert coercion: `'5'`→INTEGER col = 5; `3.0`→TEXT col = '3.0'; `x'..'`→TEXT.
- NUMERIC affinity: keeps integer if lossless, else real; `'3.0'`→3 (int).
- CAST to each type incl. CAST(x AS NUMERIC), CAST of blob/text/real↔int.
- TYPEOF() for each storage class; after CAST.
- Integer PRIMARY KEY (intpkey): stored as integer rowid; `rowid` aliasing.
- INTREAL: a REAL with integer value (internal) — compare/typeof behavior.
- NULL: `IS NULL`, storage class NULL, aggregates skip NULL.

## SQLite source references
- `src/build.c` — `sqlite3Affinity` (declared type → affinity).
- `src/vdbemem.c` / `src/vdbeapi.c` — value storage classes, applyAffinity.
- `src/expr.c` — comparison affinity application.
- `src/func.c` — `TYPEOF`.

## Steps
- [x] **G1.TYPES.1** Pre-test suite. Commit: `G1.TYPES.1: types pre-test suite`.
- [x] **G1.TYPES.2** Declared-type→affinity mapping exactly per `sqlite3Affinity`
  (substring rules: contains INT→INTEGER; CHAR/CLOB/TEXT→TEXT; BLOB→BLOB; REAL/FLOA/DOUB→REAL; else NUMERIC).
  Commit: `G1.TYPES.2: affinity mapping`.
- [x] **G1.TYPES.3** Insert coercion per column affinity (applyAffinity).
  Commit: `G1.TYPES.3: insert coercion`.
- [x] **G1.TYPES.4** CAST correctness for all target types incl. NUMERIC and
  error/edge cases (overflow, blob→text hex?). Commit: `G1.TYPES.4: CAST`.
- [x] **G1.TYPES.5** integer PRIMARY KEY / intpkey rowid aliasing + INTREAL.
  Commit: `G1.TYPES.5: intpkey + INTREAL`.
- [x] **G1.TYPES.6** NUMERIC affinity lossless-int / real canonicalization.
  Commit: `G1.TYPES.6: NUMERIC canonicalization`.
- [x] **G1.TYPES.7** testgen affinity/types/cast/numcast/intpkey/intreal/nulls/null green.
  Commit: `G1.TYPES.7: types TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/affinity/ ./testgen/cast/ ./testgen/numcast/ ./testgen/types/ ./testgen/intpkey/ ./testgen/intreal/ ./testgen/nulls/ ./testgen/null/ && \
go test -run 'TestP1Types' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Complete SQLite type/affinity model: storage classes, declared-type→affinity mapping (sqlite3Affinity), insert/comparison coercion, CAST, NUMERIC canonicalization, intpkey rowid aliasing, INTREAL, TYPEOF. types & affinity currently FAIL. See portplan/TASK_G1_TYPES.md." \
  completionCriterion "testgen affinity, cast, numcast, types, intpkey, intreal, nulls, null PASS and TestP1Types pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/affinity/ ./testgen/cast/ ./testgen/numcast/ ./testgen/types/ ./testgen/intpkey/ ./testgen/intreal/ ./testgen/nulls/ ./testgen/null/ && go test -run TestP1Types -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.TYPES. cast PASSes; types/affinity FAIL. Affinity mapping in src/build.c
sqlite3Affinity; coercion in vdbemem.c applyAffinity. Shared with G1.WHERE (comparison
affinity) and G1.CREATE (STRICT).
Decisions: affinity arithmetic lives here; CREATE structure in G1.CREATE.
Next: pre-tests, then affinity mapping, then coercion + CAST.
Risks: NUMERIC int/real canonicalization is subtle; coordinate with float formatting (G1.SELECT).
Carried limits: verifyCommand above.
```
