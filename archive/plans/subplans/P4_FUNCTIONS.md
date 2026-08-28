# Sub-Plan: P4 — Functions & Expressions (5 sub-goals)

> **Prerequisite**: P1, P2 complete.
> **Packages**: 32

---

## G4.STRING — String Functions

### Goal
```
Objective: All string functions — SUBSTR, INSTR, REPLACE, TRIM/LTRIM/RTRIM,
UPPER/LOWER, LENGTH, QUOTE, HEX/UNHEX, CHAR, UNICODE, SOUNDEX.
Completion criterion: testgen instr, substr, hexlit, blob, quote, regexp PASS.
Verify: go test -tags testgen ./testgen/instr/ ./testgen/substr/ ./testgen/hexlit/ ./testgen/blob/ ./testgen/quote/ ./testgen/regexp/ -count=1 && go test -run TestP4String -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p4_string_test.go`
Test each function with edge cases, compared against sqlite3 oracle:
- SUBSTR(str, start), SUBSTR(str, start, len) — negative start, UTF-8
- INSTR(haystack, needle)
- REPLACE(str, find, repl)
- TRIM/LTRIM/RTRIM with and without chars argument
- UPPER/LOWER — UTF-8 aware
- LENGTH — character count vs byte count
- QUOTE — SQL-literal quoting
- HEX/UNHEX — blob conversion
- CHAR(n, ...) — codepoint to string
- UNICODE(str) — first codepoint

### Steps
1. **Write pre-test**. Commit: `G4.STRING.1: add string function pre-test`
2. **Fix SUBSTR edge cases** — negative offsets, UTF-8 boundaries.
   Commit: `G4.STRING.2: fix SUBSTR negative offset and UTF-8`
3. **Fix TRIM with chars** — TRIM(str, chars) custom trim set.
   Commit: `G4.STRING.3: fix TRIM with custom characters`
4. **Run TCL tests**. Commit: `G4.STRING.N: string function TCL tests green`

---

## G4.DATE — Date/Time Functions

### Goal
```
Objective: date(), time(), datetime(), julianday(), strftime(), unixepoch(),
date arithmetic (modifiers), CURRENT_TIME/DATE/TIMESTAMP.
Completion criterion: testgen date, timediff PASS.
Verify: go test -tags testgen ./testgen/date/ ./testgen/timediff/ -count=1 && go test -run TestP4Date -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p4_date_test.go`
- date('2023-01-15') → '2023-01-15'
- date('now') — current date (mock or skip exact)
- date('2023-01-15','+1 day') → '2023-01-16'
- date modifiers: '+N days', '-N months', 'start of year', 'utc', 'localtime'
- strftime('%Y-%m-%d', '2023-01-15')
- julianday() — Julian day number
- unixepoch() — Unix timestamp
- time(), datetime()
- timediff('2023-01-01','2023-12-31')
- CURRENT_DATE, CURRENT_TIME, CURRENT_TIMESTAMP

### Steps
1. **Write pre-test**. Commit: `G4.DATE.1: add date/time pre-test`
2. **Implement date modifiers** — +/-days/months/years, start of, weekday.
   SQLite ref: `src/date.c` (parseYyyyMmDd, computeYmd, etc.).
   Commit: `G4.DATE.2: implement date/time modifiers`
3. **Fix strftime** — all format specifiers (%Y %m %d %H %M %S %j %w %f %s).
   Commit: `G4.DATE.3: implement strftime format specifiers`
4. **Run TCL tests**. Commit: `G4.DATE.N: date/time TCL tests green`

---

## G4.NUMERIC — Numeric Functions & Precision

### Goal
```
Objective: ABS, ROUND, SIGN, MIN/MAX (scalar), POW, SQRT, numeric precision,
float formatting.
Completion criterion: testgen round, decimal, percentile, ieee, nan, atof, fpconv PASS.
Verify: go test -tags testgen ./testgen/round/ ./testgen/decimal/ ./testgen/percentile/ ./testgen/ieee/ ./testgen/nan/ ./testgen/atof/ ./testgen/fpconv/ -count=1 && go test -run TestP4Numeric -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p4_numeric_test.go`.
   Commit: `G4.NUMERIC.1: add numeric function pre-test`
2. **Fix ROUND** — banker's rounding, precision argument, NULL.
   Commit: `G4.NUMERIC.2: fix ROUND precision and rounding mode`
3. **Fix float formatting** — REAL output matches SQLite (whole→X.0).
   Commit: `G4.NUMERIC.3: fix REAL float output formatting`
4. **Implement decimal type** — if needed for decimal()/percentile().
   Commit: `G4.NUMERIC.4: implement decimal arithmetic`
5. **Run TCL tests**. Commit: `G4.NUMERIC.N: numeric TCL tests green`

---

## G4.PRINTF — Printf

### Goal
```
Objective: printf()/format() with SQLite format specifiers — %d, %s, %f, %g, %x,
%o, %c, %q, %Q, %w, width, precision, flags.
Completion criterion: testgen printf PASS.
Verify: go test -tags testgen ./testgen/printf/ -count=1 && go test -run TestP4Printf -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p4_printf_test.go`
- printf('%d', 42) → '42'
- printf('%5d', 42) → '   42' (width)
- printf('%-5d|', 42) → '42   |' (left-align)
- printf('%05d', 42) → '00042' (zero-pad)
- printf('%.2f', 3.14159) → '3.14'
- printf('%s', 'hello')
- printf('%q', "it's") → "'it''s'" (SQL quote)
- printf('%Q', "it's") → "'it''s'" (with NULL handling)
- printf('%w', 'a\"b') → escape for LIKE
- printf('%x', 255) → 'ff'
- Multiple args: printf('%s=%d', 'x', 5)

### Steps
1. **Write pre-test**. Commit: `G4.PRINTF.1: add printf pre-test`
2. **Implement format specifiers** — SQLite ref: `src/printf.c` (sqlite3_str).
   This is a large function; implement incrementally by specifier type.
   Commit: `G4.PRINTF.2: implement printf integer specifiers (%d %x %o)`
3. **Implement %f %g %e** — floating point formatting.
   Commit: `G4.PRINTF.3: implement printf float specifiers`
4. **Implement %q %Q %w** — SQLite-specific quoting specifiers.
   Commit: `G4.PRINTF.4: implement printf SQL quoting specifiers`
5. **Fix transpiler** — `sqlite3_mprintf_*` TCL commands need harness support.
   Commit: `G4.PRINTF.5: implement sqlite3_mprintf TCL command in harness`
6. **Run TCL tests**. Commit: `G4.PRINTF.N: printf TCL tests green`

---

## G4.LIKE — LIKE / GLOB / Pattern Matching

### Goal
```
Objective: LIKE (% _ wildcards), GLOB (* ? [ ] wildcards), ESCAPE clause,
case sensitivity, REGEXP operator.
Completion criterion: testgen like, regexp PASS.
Verify: go test -tags testgen ./testgen/like/ ./testgen/regexp/ -count=1 && go test -run TestP4Like -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p4_like_test.go` — LIKE/GLOB/ESCAPE/REGEXP.
   Commit: `G4.LIKE.1: add LIKE/GLOB pre-test`
2. **Fix LIKE case sensitivity** — ASCII case-insensitive by default.
   Commit: `G4.LIKE.2: fix LIKE case-insensitive matching`
3. **Fix GLOB character classes** — [a-z], [!a-z], * ? wildcards.
   Commit: `G4.LIKE.3: fix GLOB pattern matching`
4. **Fix ESCAPE** — custom escape character in LIKE.
   Commit: `G4.LIKE.4: implement LIKE ESCAPE clause`
5. **Run TCL tests**. Commit: `G4.LIKE.N: LIKE/GLOB TCL tests green`
