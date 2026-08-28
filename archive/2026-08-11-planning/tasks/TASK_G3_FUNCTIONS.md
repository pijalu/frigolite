# TASK G3 — SQL Functions (string, numeric, datetime, printf)

> **Phase**: G3 (depends on G2 core goals green)
> **Goal IDs**: G3.STRING, G3.NUMERIC, G3.DATETIME, G3.PRINTF
> **Read first**: `PORTPLAN.md` §0 (principle #4: prefer Go stdlib),
> **`portplan/DESIGN.md` §E (functions: date/time, printf, string/numeric)**,
> `portplan/GUIDELINES.md`.
> **Status**: 🟡 stabilizing — STAB-1..5 COMPLETE (STAB-4: like/printf/func5/round;
> STAB-5: existsexpr/orderby/vtab/vtabI/swarmvtabfault/misc/tkt/tkt_80e031a00).
> Formal G3 goals not started.

---

## Stabilization Tracking (STAB)

The FUNCTIONS-family testgen packages are being stabilized before formal G3
goals start. Each STAB goal fixes a cluster of FAIL packages via the triage
rule (pure-Go repro first).

### STAB-4: Fix func5, like, printf, round (COMPLETE)

Root-causing each via pure-Go repro (`frigolite.Open/Exec/Query`):

| Package | Root cause | Category | Fix location | Status |
|---------|-----------|----------|-------------|--------|
| **like** | `compareSkippedAffinity` in `internal/util/compare.go` returned `0` (equal) for TEXT-vs-BLOB with NONE affinity instead of SQLite type ordering (`TEXT < BLOB`). The `value` package's equivalent had this correct. | Engine bug | `internal/util/compare.go` line ~241: `return 0` → `return int(ta) - int(tb)` | ✅ FIXED & verified (S4.1) |
| **printf** | printf-20.* tests in `skipTests` use `->>` operator (JSON, parse error). `emitSkippedTestSideEffects` in `tools/tcl2go/flow.go` runs SQL for "side effects" AND checks errors via `t.Errorf("exec error (skipped test side effects)")`. Pure SELECTs — no side effects. | Transpiler bug | `tools/tcl2go/flow.go`: suppress error check in `emitSkippedTestSideEffects` (lines 162-164, 186-188) | ✅ FIXED & verified (S4.2) |
| **func5** | func5-2.2/2.3 in `skipTestsMore` use counter1/counter2 (C-API `sqlite3_create_function`). Same `emitSkippedTestSideEffects` issue — SQL errors but error check still runs. | Transpiler bug | Same as printf (same `emitSkippedTestSideEffects` fix) | ✅ FIXED & verified (S4.2) |
| **round** | round() engine func CORRECT (verified vs sqlite3). Transpiler bugs: (1) `rand()` → constants, (2) `[string trimright $r 0]` → literal `"$r 0"`, (3) `[db one SQL]` → `tclExecSQL` (doesn't set `_res`), (4) do_test body `set x [db one ...]` (3 args) not matched by `bodyEndsWithSetVar` (checks `len==2`) → falls to `emitErrorResultCheck` → `_res` nil → PANIC. Original TCL: `ori/sqlite/test/round1.test`. | Transpiler bug | Fix `bodyEndsWithSetVar` to handle 3-arg `set x [cmd]`; or guard nil `_res` | ✅ FIXED & verified (S4.2) |

### STAB-5: Fix the remaining 8 FAIL clusters across JOIN/ORDER/VTAB/OTHER (COMPLETE)

| Package | Root cause | Category | Fix location | Status |
|---------|-----------|----------|-------------|--------|
| **existsexpr** | EXPLAIN QUERY PLAN did not render SQLite's EXISTS-to-JOIN loops (`SEARCH t EXISTS USING idx (col=?)` / `SCAN t EXISTS`) for top-level WHERE EXISTS conjuncts; they were shown as SCALAR SUBQUERY nodes. | Engine (EQP) | `internal/execquery/explain_plan.go`: `existsJoinNode`/`existsJoinSearchColumn`/`planWhereSubqueries` | ✅ FIXED & verified |
| **orderby** | orderby6 `[db eval $sql1]` — a bare TCL `$var` expected value was not recognized as a variable reference, so the transpiler compared against the literal `$sql1`. | Transpiler bug | `tools/tcl2go/expected.go`: `isBareTCLVarRef` | ✅ FIXED & verified |
| **vtab** | (1) echo module `varchar(32)` arg mangled the stored CREATE VIRTUAL TABLE SQL (grammar stops vtabarg accumulation at `(`), corrupting schema round-trip → `no such column: a`. (2) `t4 MATCH 'b'` table-name MATCH rejected (`no such column: t4`) because FTS colDefs lacked the implicit table-name/docid hidden columns. | Engine bug | verbatim vtab RawSQL storage (`ast.go`, `parser.go`, `ddl_trigger.go` `vtabSQL`, `ddl.go`/`ddl_drop.go` strip helpers); FTS hidden columns (`schema_parse.go`); FTS3 column-type stripping (`fts3.go`); insert count excludes hidden columns (`execdml`) | ✅ FIXED & verified |
| **vtabI** | `all_col_list` procs (`lappend` in a `for` loop) not transpiled → missing columns. | Transpiler bug | `tools/tcl2go/collect.go`: `rangeListProcValue` | ✅ FIXED & verified (S5.3) |
| **swarmvtabfault** | dependent on vtab/vtabI gaps; now green. | — | — | ✅ FIXED & verified |
| **misc** | IN subquery result when both a match and a NULL are present (`found` discarded by `else if`). | Engine bug | `internal/execexpr/expression_eval.go`/`expression_inlist.go`: `inListScanItems`/`evalInListSubqueryItem` | ✅ FIXED & verified (S5.1) |
| **tkt** | RAISE(IGNORE) in BEFORE triggers did not abort the statement cleanly. | Engine bug | trigger execution | ✅ FIXED & verified (S5.2) |
| **tkt_80e031a00** | result mismatch ([{}] vs [1]/[0]) — correlated EXISTS boolean. | Engine bug | EXISTS evaluation | ✅ FIXED & verified |

Verify command for STAB-5 (all green):

```bash
go build ./... && go test -run TestSOLID_ -count=1 . && ./tools/quality_gate.sh && \
go test -tags testgen -count=1 -timeout 180s ./testgen/existsexpr/ ./testgen/orderby/ ./testgen/vtab/ \
  ./testgen/vtabI/ ./testgen/swarmvtabfault/ ./testgen/misc/ ./testgen/tkt/ ./testgen/tkt_80e031a00/ && \
go test -tags testgen -count=1 -timeout 120s ./testgen/check/ ./testgen/fkey/ ./testgen/subquery/ ./testgen/rowvalue/
```

**rowvalue** (G0 FIX-4-FAILS guard) was also fixed in STAB-5: `(1,NULL) IN
(SELECT 1,NULL)` returned 1 instead of NULL — `subqueryRowMatch` in
`internal/execexpr/expression_inlist.go` treated a NULL-element row as a
match. Fixed to SQLite row-value semantics (NULL at an undecided position is
not a match; a later differing element decides FALSE).

Commit prefix: `S5.<step>` (S5.1–S5.9).

**Applied fixes (S4.1/S4.2) — recorded for reference:**

- **like**: `internal/util/compare.go` `compareSkippedAffinity` `return 0` →
  `return int(ta) - int(tb)` (S4.1).
- **printf/func5**: `tools/tcl2go/flow.go` `emitSkippedTestSideEffects` and
  `emitSkippedDoTestSideEffects` now tolerate unsupported-feature exec errors
  (`_ = _res.Error`) while keeping the `db.Exec` side-effect call (S4.2).
- **round**: `tools/tcl2go/flow.go` `bodyEndsWithSetVar` matches 3-arg
  `set x [cmd]` bodies (round1's `set x [db one ...]`), plus cmdExprString
  trim/trimleft/trimright support (S4.2).
- round() engine function verified correct vs sqlite3
  (`round(0.4,1)`→`0.4`, `round(1.5)`→`2`, `round(NULL)`→NULL,
  `round('abc')`→`0.0`).

**Evidence notes**
- Original TCL: `ori/sqlite/test/round1.test` (42 lines).
- printf-20.* skip reasons: `tools/tcl2go/skiptests.go` lines 619-648.
- func5-2.2/2.3 skip reasons: `tools/tcl2go/skiptests2.go` lines 15-20.

---

## Objective

Make every built-in scalar/aggregate function match SQLite exactly. Use the Go
standard library as the primary building block (`strings`, `strconv`, `math`,
`time`, `regexp`, `fmt`). Reference SQLite `src/func.c`, `src/date.c`,
`src/printf.c`.

**Triage rule**: pure-Go pre-test (`frigolite_p3_*.go`) + oracle first.

---

## Goal G3.STRING — String functions

**Scope**: `instr`, `substr`/`substring`, `like`/`glob`, `quote`, `hex`/`unhex`,
`trim`/`ltrim`/`rtrim`, `replace`, `char`, `length`, `lower`/`upper`, `printf`
(string subset), `regexp`/`regexpi`, `hexlit`.

**Key areas**: `internal/function/function.go`. stdlib: `strings`, `strconv`,
`regexp` (RE2).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/instr/ ./testgen/substr/ ./testgen/like/ ./testgen/quote/ \
  ./testgen/trim/ ./testgen/hexlit/ ./testgen/regexp/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP3String' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. SUBSTR (1-based, negative offset, BLOB), INSTR, REPLACE, TRIM (chars/both),
   CHAR, LENGTH (chars vs bytes for BLOB), HEX/UNHEX.
3. LIKE/GLOB with ESCAPE; case-folding (ASCII vs Unicode via `unicode`/`strings`).
4. REGEXP/REGEXPI via stdlib `regexp` (RE2); LIKE→GLOB edge cases.
5. QUOTE for all types (int/float/text/blob/NULL; Inf/NaN).
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G3.NUMERIC — Numeric functions

**Scope**: `round`, `nan`, `zeroblob`, `unhex`, `percentile`, `decimal`,
`ieee754`, `abs`, `sign`, `mod`, `math` (acos/asin/…/trunc).

**Key areas**: `internal/function/`. stdlib: `math`, `math/rand`, `strconv`.
Reference SQLite `src/func.c` (math funcs), `ext/misc/ieee754.c`,
`ext/misc/decimal.c`, `ext/misc/percentile.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/round/ ./testgen/nan/ ./testgen/zeroblob/ ./testgen/percentile/ \
  ./testgen/decimal/ ./testgen/ieee754/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP3Numeric' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. ROUND (digits, negative digits, float edge cases), ABS, SIGN, MOD, random*.
3. Full math function set (sqrt, ln, log, exp, trig) via stdlib `math`; NaN/Inf.
4. zeroblob/unhex; IEEE754 inspection (ext/misc/ieee754.c port via stdlib
   `math`/`strconv`); decimal arithmetic (ext/misc/decimal.c).
5. percentile aggregate (ext/misc/percentile.c — median/quartile).
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G3.DATETIME — Date/time functions

**Scope**: `date`, `time`, `datetime`, `julianday`, `strftime`, `timediff`,
`unixepoch`, modifiers (`'+1 day'`, `'start of month'`, …).

**Key areas**: `internal/function/`. **stdlib `time`** + Julian-day arithmetic.
Reference SQLite `src/date.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/date/ ./testgen/time/ ./testgen/timediff/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP3Date' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. Julian-day ↔ civil-date conversions; sub-second precision; timezone handling.
3. All modifiers (NNN days/hours/minutes/seconds/months/years, start of *, utc,
   localtime, weekday, unixepoch, auto, substring).
4. strftime format specifiers; date/time/datetime/julianday/unixepoch/timediff.
5. Use stdlib `time` for the civil-calendar math (portable, no CGO).
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G3.PRINTF — printf / format functions

**Scope**: `printf`, `format`, `func2`–`func9`.

**Key areas**: `internal/function/`. Reference SQLite `src/printf.c`
(`%!.Ng`, `%w`, `%Q`, `%q`, `*` width/precision, alternate forms).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/printf/ ./testgen/func2/ ./testgen/func3/ ./testgen/func4/ \
  ./testgen/func5/ ./testgen/func6/ ./testgen/func7/ ./testgen/func8/ \
  ./testgen/func9/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP3Printf' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. Port `src/printf.c` formatting: `%d %i %u %f %e %g %s %c %x %X %o %p %n`,
   width/precision/flags, `%!g` alternate, `%w` (URL-encode), `%Q`/`%q` (quote).
3. Float formatting must match SQLite's `%!.15g`-derived output exactly.
4. NULL handling in format args; too-few/too-many args behavior.
5. Per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All four goals green; pre-tests pass; quality + SOLID pass; no G1/G2 regression.
- `PORTPLAN.md` §5 G3 rows → 🟢.
