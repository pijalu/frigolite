# TASK G4.DATETIME — Date/time functions

> **Phase**: G4 (functions & expressions).
> **Goal**: G4.DATETIME.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.EXPR; G1.TYPES.
> **Current state: FAILING** — `date` fails.

## Objective
Date/time functions match SQLite: `date()`, `time()`, `datetime()`,
`julianday()`, `strftime()`, `unixepoch()`, modifiers (`'+N days'`, `'start of
month'`, `'weekday N'`, `'utc'`, `'localtime'`, etc.), the `'now'` time-value,
`'subsec'`/`'subsecond'`, and parsing of the various time-value formats
(YYYY-MM-DD, YYYY-MM-DDTHH:MM, HH:MM:SS, Julian day, unix epoch). Timezone
handling: SQLite uses UTC internally; `localtime`/`utc` modifiers convert.

> **Note on determinism:** `'now'` and `localtime` are time-dependent. Tests
> using them must either pin the time or compare structurally. Triage each
> failure: if it's purely clock-dependent, handle via the transpiler/runner
> (freeze time) — but the *engine* must compute correctly given a fixed input.

## Scope — testgen packages
`date`, `timediff`.

## Pre-test file
`frigolite_p4_datetime_test.go` — `TestP4Datetime_*`. Cases vs oracle (fixed
inputs only):
- `date('2020-01-01')`, `datetime(...)`, `time(...)`, `julianday(...)`.
- Modifiers: `'+1 day'`, `'-1 month'`, `'start of year'`, `'weekday 0'`,
  `'utc'`, `'localtime'`, `'subsec'`.
- `strftime('%Y-%m-%d %H:%M', t)` all format codes.
- `unixepoch()`; `julianday()` round-trip.
- Parsing variants (with/without T, with/without seconds, timezone suffix).
- NULL / invalid input → NULL.

## SQLite source references
- `src/date.c` — `sqlite3DateTime`, `parseYyyyMmDd`, modifier parsing, `strftime`.

## Steps
- [ ] **G4.DATETIME.1** Pre-test suite (fixed inputs). Commit: `G4.DATETIME.1: datetime pre-test suite`.
- [ ] **G4.DATETIME.2** Triage `date` failure via pure-Go test (fixed inputs).
      Commit per fix: `G4.DATETIME.2.<n>: <fix>`.
- [ ] **G4.DATETIME.3** Modifier engine (arith, start-of, weekday, utc/localtime,
      subsec). Commit: `G4.DATETIME.3: modifiers`.
- [ ] **G4.DATETIME.4** strftime format codes completeness. Commit:
      `G4.DATETIME.4: strftime`.
- [ ] **G4.DATETIME.5** Time-value parsing variants + julian day / unixepoch.
      Commit: `G4.DATETIME.5: time-value parsing`.
- [ ] **G4.DATETIME.6** date + timediff green (time-dependent cases handled).
      Commit: `G4.DATETIME.6: datetime TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/date/ ./testgen/timediff/ && \
go test -run 'TestP4Datetime' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Date/time functions match SQLite: date/time/datetime/julianday/strftime/unixepoch, all modifiers (+N units, start of *, weekday N, utc, localtime, subsec), 'now' time-value, time-value parsing variants (YYYY-MM-DD[T]HH:MM[:SS], Julian day, unix epoch), UTC-internal model. date currently FAILS. See portplan/TASK_G4_DATETIME.md." \
  completionCriterion "testgen date, timediff PASS and TestP4Datetime pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/date/ ./testgen/timediff/ && go test -run TestP4Datetime -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G4.DATETIME. date FAILS. Date engine in src/date.c; port to internal/function/. SQLite uses UTC
internally; localtime/utc modifiers convert. 'now'/localtime are time-dependent — tests must pin time.
Decisions: engine computes from fixed input; clock-dependent variance handled at test-runner level.
Next: pre-tests with fixed inputs, triage date, then modifiers + strftime + parsing.
Risks: modifier edge cases (weekday wrap, month-end clamping); strftime float seconds formatting.
Carried limits: verifyCommand above.
```
