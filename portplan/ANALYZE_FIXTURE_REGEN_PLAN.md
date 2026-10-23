# Analyze Fixture Regeneration Plan

## Evidence

Latest serialized `go test ./... -count=1` failed (376 seconds), with first failing family `analyze`. Failure output shows `analyze-3.10` converted Tcl `$name` interpolations as empty identifiers, while canonical `../sqlite/test/analyze.test:210-220` contains an explicit table name `[silly " name]`. Subsequent source section `analyze-4.0` uses `db2` lifecycle and closes/reopens `test.db`; fixture may also omit lifecycle setup. This points to stale/incomplete conversion, not yet an engine diagnosis.

## Steps

1. Regenerate only `testdata/analyze.json` from authoritative SQLite source with tclconvert.
2. Review exact JSON diff, especially variable substitution, named-connection lifecycle, and expectations; preserve all unrelated workspace files.
3. Run exact analyze family. For any residual engine-visible mismatch, inspect SQLite source and add native regression before the smallest source-faithful fix.


## Named-connection converter repair

`analyze-4.0` source opens `db2`, executes DDL/DML through `db2 eval`, closes the main connection, then reopens it. Regenerated JSON captured `open_named_db` but silently dropped SQL executed through the Tcl variable command `db2`, so the shared disk DB never receives `t4`.

1. Extend Tcl converter dispatch for a known named SQLite connection command (`db2 eval`/`db2 close`) to retain connection name in emitted step database field; do not treat arbitrary unknown Tcl commands as SQL.
2. Native converter regression should assert the `db2 eval` DDL step targets database `db2`.
3. Run converter tests, regenerate only analyze.json, verify t4 DDL step database identity, then exact analyze family.