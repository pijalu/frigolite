# Plan — S59: printf `%j/%J` JSON quoting (2026-11-25)

Mode: debug — 14 raw-byte leaf failures (`printf-20.9`–`20.16`,
`20.23`–`20.27` + family) are reproduced; localize to the printf verb
dispatch and port SQLite's `%j/%J` conversion.

## Evidence

- Focused `/tmp/t24_printf_focus0.log`: leaves = `printf`,
  `printf-20.9`..`20.16`, `printf-20.23`..`20.27`; all other `printf` and
  `printf2` cases pass.
- `internal/function/function_printf.go:207` sends `'J'` and `'j'` to
  `sqliteFormatString`, which currently treats them as plain `%s`. SQLite
  3.54 added `etESCAPE_j`/`etESCAPE_J` (`src/printf.c:37-38, 891-966` in
  version-3.54.0), a distinct JSON-string conversion.
- Canonical C rules (`printf.c` + `printf.html`): `%J` emits a JSON string
  literal (`null` for NULL); `%j` emits the inner escaped text (empty for
  NULL); precision truncates the ARGUMENT (bytes by default, codepoints with
  `!`); `"`/`\\`/control bytes are escaped (`\b\t\n\f\r` or `\u00XX`);
  width pads the rendered value.
- Oracle caveat: `/usr/bin/sqlite3` 3.54 in this environment returns NULL for
  `%j/%J` (its build lacks the feature despite reporting 3.54). A canonical
  SQLite 3.55-dev build from `sqlite.org/src/tarball/...version-3.54.0`
  produced the raw outputs used below; fixture expectations are Tcl-canonical
  and the harness's `cleanExpected` strips one Tcl list layer.
- Fixture nuance: `printf-20.20` expects `<いち,   >` (three spaces). The
  built oracle's raw output is `<いち, >` (one space); the durable fixture is
  the gate, so this slice implements the fixture contract: width is measured
  in codepoints when `!` OR an explicit precision is present, otherwise in
  bytes. Record this as a documented fixture/Build discrepancy.

## Change (one coherent slice)

1. **Pin first (RED).** Add `frigolite_printf_json_t24_pin_test.go` driving
   `frigolite.Open/Query` directly. Cover all `printf-20.1`..`20.27` SQL and
   expected raw strings (including control-byte hex from 20.9, NULLs from
   20.10, width/precision `!` shapes, escaped quote/backslash/control bytes,
   and precision 0). Run before engine edits and record the exact failure.

2. **Port the conversion.** Add `internal/function/printf_json.go` with
   `sqliteFormatJSONString(verb byte, leftJustify, alt2 bool, width, precision
   int, val interface{}, missing bool) string`:
   - `verb == 'J'` uses `null` for NULL/missing and surrounds non-NULL with
     `"..."`; `verb == 'j'` uses empty text for NULL/missing and no quotes.
   - Determine input bytes from `toString(val)`; with precision >= 0 take
     that many bytes normally, or that many UTF-8 codepoints (extend across
     the final codepoint's continuation bytes) when `alt2` is set.
   - Escape bytes <= 0x1f, `"`, and `\\`: emit preceding literal chunks
     unchanged; map 0x08/09/0a/0c/0d to `b/t/n/f/r`; all other controls use
     lowercase `\u00xx`. Do not escape normal UTF-8 bytes.
   - Width: rendered length is bytes by default, codepoints when `alt2` or
     explicit precision; leftJustify appends spaces, default prepends them.
   - Keep `function_printf.go` dispatch small: case `'J','j'` calls the new
     helper instead of `sqliteFormatString`; remove them from the `%s` case.
   - No harness/converter/fixture edits; `cleanExpected` already strips the
     Tcl list layer and preserves the expected escaped text.

3. **Verify.**
   - Pin RED then GREEN.
   - `FRIGOLITE_TEST=printf go test -run '^TestSQLiteSuite$' -count=1 -v`
     0 leaves; `FRIGOLITE_TEST=printf2` stays 0.
   - `go test ./internal/... -count=1` green.
   - `tools/quality_gate.sh` normalized staticcheck/gocognit/gocyclo NEW=0 vs
     `/tmp/t23_quality2.log` (overall exit 1 remains pre-existing file size).
   - Full gate detached (`-timeout 30m`, status file); raw-byte leaf-name diff
     vs `/tmp/t23_full_gate.log` (1253) and durable `/tmp/t10_full2.log`:
     NEW=0 except documented flakes; 14 target leaves GONE.
   - Converter failure-name set unchanged vs `/tmp/head_tclconvert_all.log`.

4. **Land.** Commit code, then plan/handoff/TODO/lessons commit. Update goal
   todo: close t24, add the next unit (`quote` double-quoted identifier
   rejection).

## Carded / risks

- Do not route `%j/%J` through `escapeSQLString` or `json_quote`: JSON
  string escaping differs from SQL quote/backslash rules and NULL is `null`
  only for `%J`.
- `!` precision conversion must skip UTF-8 continuation bytes without
  splitting a codepoint; use byte scanning, not `[]rune` truncation.
- The 20.20 width discrepancy is fixture-driven and must be recorded in the
  handoff; do not silently change the fixture to match a different build.
- Full gates are detached; compare raw fail sets, never counts or line
  greps.
