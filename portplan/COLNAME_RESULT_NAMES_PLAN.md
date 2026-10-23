# colname full-suite failure plan

## Evidence
- After repairing collate9, collateA stale fixture, and colmeta stale fixture, root suite's next failing family is colname.
- `FRIGOLITE_TEST=colname go test . -run '^TestSQLiteSuite$' -count=1` passes setup only after source regeneration but fails named result assertions across qualified refs, stars, and views. Example source `../sqlite/test/colname.test:123-132`: with `short_column_names=OFF` and `full_column_names=OFF`, `SELECT Tabc.a, tAbc.b, taBc.c FROM tabc` must retain original table/column spelling; result currently strips qualifier. Other failures indicate qualified-star and view output metadata too.
- `testdata/colname.json` was stale; regenerated from authoritative source. Remaining failures are engine column-name metadata semantics, not attach2 behavior.

## Bounded plan
1. Add failing native SQL regression for pragma state OFF/OFF and qualified reference naming; inspect `SelectContext` and result-column naming path.
2. Add short-column-name state access to SELECT context if needed; implement only source-confirmed qualified-reference semantics, preserving full-column precedence, aliases, expressions, and default short names.

## Progress
- Native regression RED before implementation: `Query.Columns` returned `a,b,c` rather than `Tabc.a,tAbc.b,taBc.c` under OFF/OFF.
- Added `ShortColumnNames()` to SELECT context and preserve explicit table/column spelling when short names are disabled and full names are disabled. Focused native regression passes.
- Source-faithful Tcl `execsql2` semantics added: each callback stores columns in Tcl array `data($name)`, so duplicate labels resolve to the final column value; harness helper regression passes. This removes some duplicate-value mismatches but does not repair SQLite output-column alias uniquification.
- Exact `FRIGOLITE_TEST=colname` remains red in view output labels (`a:1`, view-qualified names), duplicate-name handling in engine rows, full-column star qualification, and sqlite_master alias behavior. This is a broad engine/result-metadata implementation gap, not an attach2 UTF-16/path failure.
- Fresh root suite `/tmp/attach2-goal-root-suite-final.log`: exit 1 after 153s; 293 failing harness families, first `colname`; collate9/collateA/colmeta absent from failing set after repairs. Do not claim suite green.
- Final requested `go test ./...` exceeded the 300s tool timeout (output buffering left the redirected log empty); `go test ./... -run '^$'` passes compilation across packages, while root integration run above remains red. Focused attach2 and native UTF-16 gating/path/mismatch tests pass.
3. Run new native regression, relevant colname subcases/family, then `git diff --check`.
4. Record residual colname subcases and next full-suite family. Full-suite includes many unrelated existing failures; do not weaken expectations or claim green.
