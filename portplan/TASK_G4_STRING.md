# TASK G4.STRING — String functions (substr, instr, replace, trim, like, quote, etc.)

> **Phase**: G4 (functions & expressions).
> **Goal**: G4.STRING.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.EXPR; G1.TYPES.
> **Current state: MOSTLY PASSING** — instr, substr, round, quote PASS; `like` FAILS.

## Objective
All string functions match SQLite byte-for-byte: `SUBSTR`/`SUBSTRING` (1-indexed,
negative start from end, length clamp, UTF-8 aware), `INSTR`, `REPLACE`, `TRIM`/
`LTRIM`/`RTRIM` (with chars argument), `UPPER`/`LOWER`, `LENGTH` (chars vs
`LENGTH(x'..')` = bytes), `QUOTE`, `HEX`/`UNHEX`, `CHAR`, `UNICODE`, `SOUNDEX`,
`LIKE` (with ESCAPE, case rules), `GLOB`, `PRINTF` (shared with G4.PRINTF). Much
prior work exists (G4.STRING in MASTER_PLAN); this task closes `like` and any
remaining string gaps.

## Scope — testgen packages
`instr`, `substr`, `like`, `quote`, `hexlit`, `blob`, `regexp`, `trim`.
(`instrfault` → N/A.)

## Pre-test file
`frigolite_p4_string_test.go` — `TestP4String_*` (exists — extend for `like`).
Cases vs oracle (focus on `like`):
- LIKE `%`, `_`, literal `%` via ESCAPE; case-insensitivity (ASCII).
- LIKE on non-ASCII / blob.
- LIKE pattern too long error; LIKE with NULL operand.
- Regression for substr/instr/replace/trim/quote already green.

## SQLite source references
- `src/func.c` — `substrFunc`, `instrFunc`, `replaceFunc`, `trimFunc`, `likeFunc`,
  `quoteFunc`, `charFunc`, `unicodeFunc`, `soundexFunc`.
- `src/sqliteLimit.h` — `SQLITE_MAX_LIKE_PATTERN_LENGTH`.

## Steps
- [ ] **G4.STRING.1** Extend pre-test for `like`; record failure. Commit:
      `G4.STRING.1: extend LIKE pre-test`.
- [ ] **G4.STRING.2** Triage `like` failure via pure-Go test. Likely: ESCAPE
      handling, case-insensitivity on non-ASCII, or pattern-too-long. Fix
      `internal/function/`. Commit: `G4.STRING.2: LIKE fixes`.
- [ ] **G4.STRING.3** Verify substr/instr/replace/trim/quote/hexlit/blob/regexp
      still PASS (regression). Commit: `G4.STRING.3: string regression check`.
- [ ] **G4.STRING.4** like + full string set green. Commit: `G4.STRING.4: string TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/instr/ ./testgen/substr/ ./testgen/like/ ./testgen/quote/ ./testgen/hexlit/ ./testgen/blob/ ./testgen/regexp/ && \
go test -run 'TestP4String' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "All string functions match SQLite: SUBSTR/SUBSTRING (1-indexed, negative start, UTF-8), INSTR, REPLACE, TRIM/LTRIM/RTRIM (chars arg), UPPER/LOWER, LENGTH (chars vs blob bytes), QUOTE, HEX/UNHEX, CHAR, UNICODE, SOUNDEX, LIKE (ESCAPE, case rules, pattern-too-long), GLOB. instr/substr/quote PASS; like currently FAILS. Much prior work (G4.STRING). See portplan/TASK_G4_STRING.md." \
  completionCriterion "testgen instr, substr, like, quote, hexlit, blob, regexp PASS and TestP4String pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/instr/ ./testgen/substr/ ./testgen/like/ ./testgen/quote/ ./testgen/hexlit/ ./testgen/blob/ ./testgen/regexp/ && go test -run TestP4String -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G4.STRING. instr/substr/quote/round PASS. like FAILS. Functions in internal/function/.
SUBSTR is 1-indexed, negative start counts from end, UTF-8 aware. LENGTH(blob)=bytes, LENGTH(text)=chars.
Decisions: LIKE ASCII-case-insensitive; pattern > SQLITE_MAX_LIKE_PATTERN_LENGTH errors.
Next: extend LIKE pre-test, triage like, regression-check the green set.
Risks: UTF-8 boundary handling in SUBSTR/SUBSTRING; ESCAPE char validation.
Carried limits: verifyCommand above.
```
