# TASK G4.NUMERIC — Numeric functions, math, NaN, zeroblob

> **Phase**: G4 (functions & expressions).
> **Goal**: G4.NUMERIC.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.EXPR; G1.TYPES (float formatting).
> **Current state: PARTIAL** — `round` PASSes; `nan` likely fails.

## Objective
Numeric functions match SQLite: `ABS`, `ROUND` (incl. 2nd-arg digits),
`RANDOM`, `RANDOMBLOB`, `MIN`/`MAX` (scalar + aggregate), `SIGN`, `POW`/`POWER`,
`SQRT`, math extension functions (trig, log, exp, floor, ceil, etc.), `MOD`,
`TOTAL`/`SUM`, NaN/Inf handling (NaN compare/sort/printf), `ZEROBLOB`,
`LIKELY`/`LIKELY`/`UNLIKELY`. Note: many math functions were registered as stubs
(HANDOVER §3); this task makes them *correct* (returning real values, not NULL).

## Scope — testgen packages
`round`, `nan`, `zeroblob`, `unhex`, `percentile`, `atof`, `fpconv`, `ieee`.
(`atof`/`fpconv`/`ieee` test SQLite internal float algorithms — triage to N/A if
they're purely internal, keep the applicable parts.)

## Pre-test file
`frigolite_p4_numeric_test.go` — `TestP4Numeric_*`. Cases vs oracle:
- ABS/ROUND (neg digits, default 0, float result formatting).
- RANDOM determinism (non-deterministic — test range/type only).
- Math functions: SQRT(2), POW(2,10), SIN/PI/etc. — compare to oracle tolerance
  where SQLite doesn't guarantee exact; otherwise exact.
- NaN/Inf: `1e400`→Inf, `0/0.0`→NaN, comparisons, ORDER BY with NaN, printf of NaN.
- ZEROBLOB(n); RANDOMBLOB length.
- MOD; SIGN.

## SQLite source references
- `src/func.c` — `absFunc`, `roundFunc`, `randomFunc`, `signFunc`, math funcs.
- `src/printf.c` — NaN/Inf text rendering.
- `src/math.c` (extension) — trig/log/exp.

## Steps
- [ ] **G4.NUMERIC.1** Pre-test suite. Commit: `G4.NUMERIC.1: numeric pre-test suite`.
- [ ] **G4.NUMERIC.2** Audit stub math functions (HANDOVER §3 listed ~30 as stubs);
      implement correctly (not NULL). Commit: `G4.NUMERIC.2: real math functions`.
- [ ] **G4.NUMERIC.3** NaN/Inf handling in compare/sort/printf. Commit:
      `G4.NUMERIC.3: NaN/Inf`.
- [ ] **G4.NUMERIC.4** ROUND edge cases (negative digits, float formatting).
      Commit: `G4.NUMERIC.4: ROUND`.
- [ ] **G4.NUMERIC.5** ZEROBLOB/RANDOMBLOB/MOD/SIGN. Commit: `G4.NUMERIC.5: blob/mod/sign`.
- [ ] **G4.NUMERIC.6** Triage atof/fpconv/ieee → N/A the internal-algorithm parts,
      keep applicable. round/nan/zeroblob/unhex/percentile green.
      Commit: `G4.NUMERIC.6: numeric TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/round/ ./testgen/nan/ ./testgen/zeroblob/ ./testgen/unhex/ ./testgen/percentile/ && \
go test -run 'TestP4Numeric' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Numeric functions match SQLite: ABS, ROUND (digits), RANDOM/RANDOMBLOB, MIN/MAX, SIGN, MOD, POW/SQRT, math extension (trig/log/exp) currently stubs→make real, NaN/Inf compare/sort/printf, ZEROBLOB. round PASSes; many math funcs are stubs returning NULL. See portplan/TASK_G4_NUMERIC.md." \
  completionCriterion "testgen round, nan, zeroblob, unhex, percentile PASS; no math function returns a stub NULL; TestP4Numeric pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/round/ ./testgen/nan/ ./testgen/zeroblob/ ./testgen/unhex/ ./testgen/percentile/ && go test -run TestP4Numeric -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G4.NUMERIC. round PASSes. ~30 math functions are stubs (return NULL) per HANDOVER §3 — implement them.
NaN/Inf text + compare/sort in internal/value/ + internal/function/. Float formatting shared with G1.SELECT.
Decisions: atof/fpconv/ieee are internal-algorithm tests → likely N/A; triage honestly.
Next: pre-tests, audit+implement stub math funcs, then NaN/Inf, then ROUND edge cases.
Risks: transcendental precision vs SQLite (use same algorithm where possible); NaN ordering is platform-defined in SQLite.
Carried limits: verifyCommand above.
```
