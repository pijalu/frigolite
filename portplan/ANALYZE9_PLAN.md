# analyze9 Fixture Regeneration Plan

## Evidence
- Isolated `FRIGOLITE_TEST=analyze9 go test . -run '^TestSQLiteSuite$' -count=1` fails 34 cases. Early cases lack `test_decode`, `s`, `lindex`, `lrange`; later sections reuse `t1` without reset. Source `../sqlite/test/analyze9.test` registers TCL functions/procs at lines 25–38, 112, 159–160 and invokes `reset_db` before section 2 (line 82).
- Existing fixture has 70 unordered groups with section labels stripped (`1.0`, `2.1`), no reset markers. Fresh converter output has 176 ordered groups and explicit `__RESET_DB__`, preserving expanded loop-generated tests; therefore stale fixture is primary mismatch source.

## Plan before edits
1. Check harness support for analyze9's TCL UDF registration and sample decoding; do not attribute missing functions to STAT4 until setup is faithfully represented.
2. Regenerate only `testdata/analyze9.json` using current converter; inspect ordered test/reset boundaries and test counts.
3. Add/extend generic typed UDF support only where source setup requires it, with focused converter/harness test.
4. Run isolated analyze9, then re-assess remaining source-backed engine failures. No skipped/rewritten expectations.

## Partial implementation/evidence
- Current converter regenerated only analyze9.json: source groups 70→176, marked ordered, added reset boundaries, expanded loops.
- Added source-faithful harness UDFs for SQLite `test_decode(record)` (`test_func.c:510-599`), `s`, and Tcl `lindex`/`lrange`; added integer division and named SQL-parameter binding for braced `execsql` (Tclsqlite binding semantics). RED tests proved both converter defects, then passed after fixes.
- Added variadic Tcl proc `args` capture, array `set`, `info exists`, and binding for Tcl array references in braced SQL. RED regressions cover proc array+info lookup, variadic args, integer division, and SQL parameter binding.
- Regenerated analyze9 fixture; `analyze9-4.1` now captures 292 steps, including 290 source-driven filler inserts. Isolated analyze9 failures fell 34→20 (then 25→20 after added UDF/init fixes); STAT4 `4.2`–`4.6` now pass. Remaining 20 failures include 17.1.2/19.2 error semantics and 20.x proc UDF r/sample setup; next inspect Tcl source/proc registration. No tests skipped or expectations altered.
- Added authorization action `SQLITE_ANALYZE`, applied from `execAnalyze`; schema-qualified ANALYZE resolves target existence against named schema. Converter maps source authproc denying ANALYZE to typed `auth_analyze_deny` marker; JSON serializer now retains it.
- Isolated `FRIGOLITE_TEST=analyze9 go test . -run '^TestSQLiteSuite$' -count=1` passes all 176 generated ordered groups after authorizer marker fix. Focused converter/auth/exec packages pass, avtrans and aggnested native pins pass. Full suite pending rerun.
