# trustschema1 Tcl function-registration fidelity

## Evidence
- Latest profile: trustschema1 has 36 failing leaves, many `no such function: f1/f2/f3` and cascaded missing tables.
- `../sqlite/test/trustschema1.test:22-30` defines Tcl proc `f1 {x} {return $x}`, then calls `db function f1 -innocuous -deterministic f1`, `db function f2 -deterministic f1`, and `db function f3 -directonly -deterministic f1` before creating t1.
- `testdata/trustschema1.json` starts at case 1.110; source-faithful conversion in `/tmp/trust-regenerated/trustschema1.json` retains ordered case 1.100 SQL but omits Tcl db-function registrations. Engine/public API already supports `DB.RegisterFunctionFlags`.

## Plan
1. Add interpreter regression that captures `db function` registration marker with name and SQLite safety flags, preserving placement within test.
2. Add JSON schema propagation and harness handler that registers supported Tcl identity proc callback on the selected DB connection, mapping deterministic/innocuous/directonly flags faithfully.
3. Regenerate only trustschema1 fixture to restore ordered setup case and marker; do not rewrite unrelated fixtures.
4. Verify converter tests, exact trustschema1 family, then compare leaf failures to baseline. Add setup reset/reopen coverage if source uses it.

## Results
- `db function` markers carry Tcl proc args/body, name, arity, and deterministic/innocuous/directonly flags. Harness supports the faithfully captured one-argument identity proc used by this source and calls `DB.RegisterFunctionFlags` on current connection.
- Regenerated only `testdata/trustschema1.json`; ordered `1.100` now contains function registrations before CREATE TABLE.
- Converter regression and all `trustschema1` cases pass; previous 36 failing leaves are resolved.
