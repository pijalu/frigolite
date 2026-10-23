# ANALYZE Corrupt Statistics Schema Validation Plan

## Reproduction / source contract

Canonical `../sqlite/test/analyze.test:351-362` disables defensive mode, rewrites `sqlite_master.sql` for `sqlite_stat1` to `nonsense` under `PRAGMA writable_schema=on`, closes/reopens `test.db`, then expects `ANALYZE` to fail with `malformed database schema (sqlite_stat1)`. Regenerated `testdata/analyze.json` now retains this order. Exact family reports `analyze-5.99` expected error but got success. `validateLoadedSchema` validates CREATE rows during statement preflight, but `ANALYZE` may bypass schema validation or fail to reload after open.

## Steps

1. Add native disk-backed regression matching schema creation, ANALYZE to materialize sqlite_stat1, writable-schema SQL update, close/reopen, and `ANALYZE` error.
2. Run native test RED and localize dispatch/preflight/schema-load path versus `../sqlite/src/analyze.c` and `prepare.c`.
3. Implement smallest SQLite-faithful correction, preserving writable_schema same-connection stale-schema behavior.
4. Run native regression, exact analyze suite (document separate remaining subfamilies), then refreshed full suite with complete retained evidence.