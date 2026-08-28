# TASK G4.PRINTF — PRINTF / FORMAT and general functions

> **Phase**: G4 (functions & expressions).
> **Goal**: G4.PRINTF.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.EXPR; G1.TYPES (float formatting).
> **Current state: DONE** — printf, func2–func9 testgen PASS; TestP4Printf pre-tests PASS.

## Objective
`PRINTF`/`FORMAT` matches SQLite's custom printf (`%!` extensions: `%!.15g`,
`%,d` alternate-form grouping, `%Q`/`%q` quoting, `%w` for identifiers, `%.20s`,
`%c`, `%x`/`%X`/`%o`/`%b`/`%b` binary, alternate `#` flag, plus standard
conversions). This is a known sharp edge — SQLite's printf is *not* C printf.
Also covers the general function suites `func2`–`func9` (misc scalar funcs,
type-of, iif, min/max, coalesce, etc.) and `printf`.

## Scope — testgen packages
`printf`, `func2`, `func3`, `func4`, `func5`, `func6`, `func7`, `func8`, `func9`.
(`func_pkg`, `spellfix`, `closure`, `hidden` → triage; many are extension/N-A.)

## Pre-test file
`frigolite_p4_printf_test.go` — `TestP4Printf_*`. Cases vs oracle:
- `%d %s %f %x %X %o %c %u` standard.
- `%!` extensions: `%!.15g` (float), `%,d` (grouping), `%Q`/`%q` (quote), `%w`
  (identifier double-quote-escape), `%b` (binary), `%n`, `%s` with NULL.
- Width/precision/flags: `%-10.2f`, `%+d`, `%#x`, `% d`, `%05d`.
- NULL argument handling for `%s`/`%Q`/`%q`.
- `FORMAT()` alias.

## SQLite source references
- `src/printf.c` — `sqlite3_str`, `sqlite3_mprintf`, the `%!` engine. **This is
  the spec** — frigolite's printf must mirror its conversions.

## Steps
- [x] **G4.PRINTF.1** Baseline run of printf + func2–func9; record results.
      Commit: `G4.PRINTF.1: printf baseline`.
- [x] **G4.PRINTF.2** Pre-test suite. Commit: `G4.PRINTF.2: printf pre-test suite`.
- [x] **G4.PRINTF.3** Triage printf failures via pure-Go test; implement missing
      `%!` conversions from src/printf.c. Commit: `G4.PRINTF.3: printf %! engine`.
- [x] **G4.PRINTF.4** Triage func2–func9: each is a mix of scalar funcs — fix
      engine bugs, N/A the extension-only ones (with evidence).
      Commit per package: `G4.PRINTF.4.<n>: func<N>`.
- [x] **G4.PRINTF.5** printf + func2–func9 green. Commit: `G4.PRINTF.5: printf TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/printf/ ./testgen/func2/ ./testgen/func3/ ./testgen/func4/ ./testgen/func5/ ./testgen/func6/ ./testgen/func7/ ./testgen/func8/ ./testgen/func9/ && \
go test -run 'TestP4Printf' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "PRINTF/FORMAT matches SQLite's custom printf (%! extensions: %!.15g, %,d grouping, %Q/%q quote, %w identifier, %b binary, # flag) plus standard conversions; and func2-func9 general scalar functions correct. Spec is src/printf.c. See portplan/TASK_G4_PRINTF.md." \
  completionCriterion "testgen printf, func2-func9 PASS and TestP4Printf pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/printf/ ./testgen/func2/ ./testgen/func3/ ./testgen/func4/ ./testgen/func5/ ./testgen/func6/ ./testgen/func7/ ./testgen/func8/ ./testgen/func9/ && go test -run TestP4Printf -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G4.PRINTF. Baseline TBD. SQLite printf is NOT C printf — spec is src/printf.c (%! engine).
func2-func9 are mixed scalar funcs; triage each.
Decisions: %! extensions implemented from src/printf.c verbatim semantics.
Next: baseline, pre-tests, implement %! engine, then func2-9.
Risks: printf is a sharp edge (float formatting, NULL quoting); cross-check every conversion against sqlite3.
Carried limits: verifyCommand above.
```
