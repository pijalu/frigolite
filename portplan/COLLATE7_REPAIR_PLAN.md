# collate7 source-faithful lifecycle repair plan

## Reproduction and source contract
- Reproduced with `FRIGOLITE_TEST=collate7 go test . -run '^TestSQLiteSuite$' -count=1`: 2.1 fails at CREATE TABLE with `no such collation sequence: CASELESS`; 2.2 and 2.4 cascade from missing `abc16`.
- `../sqlite/test/collate7.test` lifecycle: initialize `::caseless_del`; define `caseless_cmp` (`string compare -nocase`); register collation with destructor, assert counter 0; delete and assert 1; assert absent collation CREATE error; register inline destructor, close DB and assert 2; forcedelete/reopen file DB, register again; execute UTF-16 pragma/table setup, assert 2; query and assert 2; delete and assert 3; query error for absent collation.
- UTF-16 capability stays unchanged. Initial failure is absent Tcl registration and not evidence supporting UTF-16 enablement.

## Ordered implementation plan
1. Audit fixture model/step executor and converter command handling for `register_collation`, `delete_collation`, Tcl procedure/global serialization, and runtime callback dispatch. Use dedicated typed ordered steps; preserve connection handle, collation name, proc/destructor identity, and registration/deletion order. No unconditional DB-open registration.
2. Add fixture-scoped callback runtime: invoke the serialized Tcl comparator against both strings with Tcl `string compare -nocase` semantics; invoke destructor (`incr ::caseless_del`) on explicit deletion, replacement/close paths matching SQLite semantics; update fixture-global vars visible to later `do_test` result evaluation. Avoid package-global state leakage.
3. Add converter regressions pinning ordered markers, proc/global metadata, inline and `$cmd` destructor forms, and deletion/close positions; add native engine/harness regression exercising compare, explicit deletion, failed missing-collation use, close destructor, and global counter values.
4. Regenerate only `testdata/collate7.json` from authoritative source via normal converter; inspect diff for all lifecycle assertions and state transitions. Preserve workspace edits elsewhere.
5. Run focused converter tests, native regression, full collate7 family, attach2 family, and `git diff --check`. Retain/document broader suite failure evidence without claiming all-suite green.

## Verified progress / remaining family evidence
- Implemented typed ordered registration/deletion markers with comparator body, proc args, destructor script, Tcl global snapshot, and runtime global assertions; harness invokes `string compare -nocase`/`string compare` callbacks, explicit delete destructor, and close/reset cleanup in fixture scope.
- Regenerated only `testdata/collate7.json` from `../sqlite/test/collate7.test`; it now includes lifecycle/assertion steps 1.1–1.4 and 2.1–2.4, plus forcedelete/reopen ordering.
- `FRIGOLITE_TEST=collate7 go test . -run '^TestSQLiteSuite$' -count=1`, focused converter/native tests, `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1`, and `git diff --check` passed before final cleanup-hook adjustment; rerun final checks.
- Broader `FRIGOLITE_TEST=collate go test . -run '^TestSQLiteSuite$' -count=1` remains red: unsupported collate3 skip notice; collate9 lacks `reverse sort` registration and subsequent table/index failures; collateA has cross-test `t1 already exists`. These are separate fixtures and do not invalidate collate7 result. Do not claim collate-family or all-suite green.

## Findings and scope
- Existing `tools/tcl2go/processcmdextra.go` has `processCreateCollation` / `processDeleteCollation`, but emits direct Go registrations and compile-time counter increments, while unsupported comparator callback names are dropped. Existing collector `collectCollateDtorVars` only recognizes simple counter proc values. This is not source-faithful for runtime/global lifecycle and currently collate7 JSON omits the calls entirely.
- Existing `tools/tcl2go/processdb_part2.go` `db collate` support is a separate interface and cannot stand in for `sqlite3_create_collation_v2` ordered API calls.
- `tools/status` is a directory, not an executable; prior `sh tools/status` exited 126 and `sh tools/status` exit 0 is not a supported reporting interface. Consult its contents/script entrypoints before using later.
