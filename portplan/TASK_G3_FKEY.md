# TASK G3.FKEY — Foreign keys (create, enforcement, ON actions)

> **Phase**: G3 (schema & constraints).
> **Goal**: G3.FKEY.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.INSERT/UPDATE/DELETE; G3.INDEX.
> **Current state: FAILING** — `fkey` fails.

## Objective
Foreign keys match SQLite: column + table FK definitions, `REFERENCES t(c)`
(parent key resolution — PK or UNIQUE; implicit if omitted), ON DELETE / ON
UPDATE actions (CASCADE, SET NULL, SET DEFAULT, RESTRICT, NO ACTION), `ON
CONFLICT` on FK, deferred vs immediate FK checks (`DEFERRABLE INITIALLY
DEFERRED`), `PRAGMA foreign_keys = ON/OFF` (default OFF — match SQLite), the
"foreign key mismatch" / "no such table" errors, and self-referential FKs.
Parent must have a PK or UNIQUE index on the referenced columns.

## Scope — testgen packages
`fkey`, `fkey_`. (`fkey2`/`fkey8` *fault* variants → N/A.)

## Pre-test file
`frigolite_p3_fkey_test.go` — `TestP3FKey_*`. Cases vs oracle:
- REFERENCES column + table form; parent key = PK then UNIQUE then error.
- ON DELETE/UPDATE CASCADE/SET NULL/SET DEFAULT/RESTRICT/NO ACTION.
- Insert into child with no parent → error; delete parent with children → action.
- Self-referential FK.
- `PRAGMA foreign_keys = ON/OFF` toggles enforcement (default OFF).
- Deferred FK: violation detected at COMMIT, not statement.
- Mismatch / missing parent table errors (exact text).

## SQLite source references
- `src/fkey.c` — FK lookup, ON action implementation, deferred check.
- `src/build.c` — FK definition parsing.
- `internal/exec/fk.go` — frigolite FK code.

## Steps
- [x] **G3.FKEY.1** Pre-test suite. Commit: `G3.FKEY.1: fkey pre-test suite`.
- [x] **G3.FKEY.2** Triage `fkey` failure via pure-Go test. Commit per fix:
      `G3.FKEY.2.<n>: <fix>`.
- [x] **G3.FKEY.3** ON DELETE/UPDATE actions (CASCADE/SET NULL/SET DEFAULT/
      RESTRICT/NO ACTION). Commit: `G3.FKEY.3: FK ON actions`.
- [x] **G3.FKEY.4** `PRAGMA foreign_keys` toggle (default OFF). Commit:
      `G3.FKEY.4: foreign_keys pragma`.
- [x] **G3.FKEY.5** Deferred FK checks at commit. Commit: `G3.FKEY.5: deferred FK`.
- [x] **G3.FKEY.6** Parent-key resolution + mismatch errors. Commit:
      `G3.FKEY.6: parent key resolution + errors`.
- [x] **G3.FKEY.7** fkey + fkey_ green. Commit: `G3.FKEY.7: fkey TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/fkey/ ./testgen/fkey_/ && \
go test -run 'TestP3FKey' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Foreign keys match SQLite: column + table REFERENCES, parent-key resolution (PK/UNIQUE/implicit), ON DELETE/UPDATE actions (CASCADE/SET NULL/SET DEFAULT/RESTRICT/NO ACTION), ON CONFLICT, deferred FK checks at COMMIT, PRAGMA foreign_keys toggle (default OFF), self-referential FKs, mismatch errors. fkey currently FAILS. See portplan/TASK_G3_FKEY.md." \
  completionCriterion "testgen fkey, fkey_ PASS and TestP3FKey pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/fkey/ ./testgen/fkey_/ && go test -run TestP3FKey -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G3.FKEY. fkey FAILS. FK code in internal/exec/fk.go. foreign_keys pragma defaults OFF (match SQLite).
Parent must have PK/UNIQUE on referenced columns.
Decisions: deferred checks run at COMMIT; immediate checks at statement.
Next: pre-tests, triage fkey, then ON actions + pragma + deferred.
Risks: FK ON action + trigger interaction ordering; multi-row cascade termination.
Carried limits: verifyCommand above.
```
