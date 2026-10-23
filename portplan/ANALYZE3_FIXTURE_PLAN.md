# Analyze3 Fixture Repair Plan

## Evidence

After named-connection repair and schema-validation fix, exact base `FRIGOLITE_TEST=analyze go test . -run '^TestSQLiteSuite$' -count=1` passes. Broader substring selection also executes `analyze3` and reports empty `$l/$u` substitutions in braced `do_eqp_test`, malformed empty insert values, and transaction cascade failures. Canonical `../sqlite/test/analyze3.test:138-155` explicitly unsets then sets Tcl variables used inside test SQL, and file contains ordered transaction setup plus named helpers. This is converter/harness divergence, not query-engine failure.

## Steps

1. Regenerate only `testdata/analyze3.json` from canonical source.
2. Inspect parameter values/bind markers, procedure/loop expansion, transaction boundaries, and expected results; do not alter expected rows.
3. Run exact analyze3 family. If missing behavior persists, isolate converter test first, then native engine behavior.
4. Record durable converter lesson and retain next suite inventory after exact family passes.