# gencol1 source-fixture audit

## Evidence
- `FRIGOLITE_TEST=gencol1 go test . -run '^TestSQLiteSuite$' -count=1` reports 37 failing leaves; many report `table already exists` or stale FK mismatch.
- `../sqlite/test/gencol1.test:163-164` explicitly closes and reopens the primary connection before `gencol1-4.100`; source also has multiple `reset_db` and `sqlite3 db :memory:` lifecycle boundaries.
- Existing `testdata/gencol1.json` has reset markers later but no lifecycle boundary before 4.100.

## Work completed / evidence
1. Regenerated only gencol1. Converter now represents ordered close/reset lifecycle around source `sqlite3 db :memory:` and `reset_db` boundaries.
2. Isolated failures reduced from 37 to 14 (0.80s); duplicate-table cascades are gone.
3. Remaining clusters: generated-column NULL/display mismatches (2.1/2.2/2.3/2.4/2.5/2.6.150, 17.20); REPLACE FK-vs-UNIQUE error precedence (4.110); missing/incorrect lifecycle or writable_schema behavior (15.20); PRAGMA table_xinfo type formatting (21.1); indexed generated-column ordering (23.2); EXPLAIN opcode (23.4); RAISE(IGNORE) in generated column accepted (23.5). These need separate source-backed fixes.
