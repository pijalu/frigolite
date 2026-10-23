# Altertab Fixture Runtime Support Plan

## Evidence

Canonical regeneration of `testdata/altertab.json` restores ordered source markers absent from the stale fixture. Focused `FRIGOLITE_TEST=altertab go test . -run '^TestSQLiteSuite$' -count=1` now reaches `altertab-4.1` and `altertab-10.2`, then fails because the harness rejects source-level Tcl procedures `squish` and `trigger` as unsupported. SQLite `altertab.test` defines `squish` as whitespace normalization (`proc squish {a} ... regsub ...`) before 4.2; section 10.2 defines callback `trigger` used by later TEMP-trigger tests. Existing `alterlegacy` fixture implements equivalent semantics, but only registered for that fixture family.

## Plan

1. Inspect the source procedures and existing harness semantic implementations; verify exact arity and callback effects.
2. Add minimal fixture-family-scoped support for altertab's two source procedures, reusing equivalent existing callback semantics where correct; avoid broadening accepted Tcl bodies globally.
3. Add/adjust native harness regression as needed to pin conversion markers and behavior.
4. Run exact altertab family, inspect any next failure against SQLite source, then run full suite with retained elapsed/stdout/stderr/status evidence.
