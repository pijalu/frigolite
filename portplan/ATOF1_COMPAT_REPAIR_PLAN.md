# atof1 compatibility repair plan

## Evidence
- Fresh full suite: `go test ./... -count=1`, exit 1, elapsed 377s. Status/elapsed retained at `portplan/FULL_SUITE_CURRENT_REPAIR.status/.summary`; complete output kept outside the repository.
- Focused family reproduced exact three failures. Native RED identified `IS TRUE` specifically: stored integer 3 -> `substr(3,',')` returns text "3", which is true per SQLite truthiness. SQLite AST contract from `expr.c:5130-5135`: IS maps to EQ/NULLEQ, but `IS TRUE/FALSE` compiler special-cases booleans via `OP_IsTrue` (`expr.c:5264`), not numeric type equality.
- Earliest current family: `atof1`. Failures 2.10 / 2.20: `substr(a,',') is true` over UTF16be-encoded blob/text produces false for both rows; expected true then false. Failure 2.40: `SELECT randomblob(0) - 1` yields `-1.0`, expected integer `-1`.

## Validation and next steps
- Native combined regression and exact family `FRIGOLITE_TEST=atof1 go test . -run '^TestSQLiteSuite$' -count=1` pass. Existing fixture unchanged; no expectation or test skipped/weakened.
- Full-suite `go test -json ./... -count=1` is active; raw JSON at `portplan/FULL_SUITE_AFTER_ATOF1.json`, stderr/status/elapsed and rendered text evidence to follow on completion. Earliest remaining failure awaits this run.
- `git diff --check` passes. `test.db2` hash unchanged.

## Risks
- The existing database encoding paths are context-dependent; avoid broad text conversion changes.
- Randomblob(0) must preserve SQLite empty-blob storage class semantics rather than special-case arithmetic SQL.
