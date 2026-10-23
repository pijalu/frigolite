# SQLite soundex support for func-20

## Evidence
- `func.test:990-1014` runtime-probes `soundex`, then loops expected name/Soundex pairs and captures `SELECT soundex($name)`.
- Prior JSON emitted malformed `soundex(euler)` because `$name` was lost; source-faithful regeneration now emits `soundex('euler')` (confirmed in `/tmp/func-regenerated/func.json`, and regenerated only `testdata/func.json`).
- Exact `func-20.1` now exposes genuine engine gap: `no such function: soundex`.
- SQLite `src/func.c:1762-1816` defines exact optional `SQLITE_SOUNDEX` behavior: first ASCII alpha uppercase; classes [BFPV=1, CGJKQSXZ=2, DT=3, L=4, MN=5, R=6]; suppress adjacent equal classes, vowels/non-class reset, pad to four, no alpha/NULL -> `?000`.

## Plan
1. Add failing direct function tests for all SQLite source vectors plus NULL/non-alpha/duplicate-class reset cases.
2. Implement exact source mapping and register scalar `SOUNDEX` with one argument.
3. Validate package tests and exact regenerated `func-20.1` / family outcomes.
4. Keep optional compile-flag question explicit; this pure-Go test harness includes tests only when oracle exposes the feature, so providing behavior matches converted source conditional.

## Results
- Direct SQLite vector test passes, including the func.test name corpus, NULL, no-alpha, duplicate-code suppression and reset across punctuation.
- `go test ./tools/tclconvert/... ./internal/function -count=1` passes.
- `FRIGOLITE_TEST=func go test . -run '^TestSQLiteSuite$/^func$/^func-20' -count=1` passes all 16 source cases.
- Regenerated single-file fixture was compared to existing diff; only 16 source SQL literals retained (plus previously added func-4.39 marker), avoiding unrelated corpus rewrites.
