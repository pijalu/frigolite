# trustschema1 fixture and registration contract

## Evidence
- `FRIGOLITE_TEST=trustschema1 go test . -run '^TestSQLiteSuite$' -count=1` produced 37 failing leaves.
- `../sqlite/test/trustschema1.test:20-30` defines TCL UDF f1 and registers f1/f2/f3 with distinct innocuous, deterministic and directonly flags before creating t1.
- Current JSON lacks those registrations and its initial test body does not reliably carry setup; errors cascade as no such function/table rather than the expected trusted-schema result.

## Plan before edits
1. Trace tclconvert support for `db function` and the public `RegisterFunction` API, including security flags; inspect SQLite `trusted_schema` source contract.
2. Determine whether JSON can faithfully express function registration lifecycle and flags. Add failing converter/harness tests before implementation.
3. Port TCL function callbacks and per-connection security metadata only if existing engine registration surface supports it; otherwise add focused engine-facing design, not fake functions or skips.
4. Regenerate only trustschema1 after converter contract is supported; run isolated fixture and source-derived native security tests.
