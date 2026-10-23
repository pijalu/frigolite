# alterqf Rename DQS Engine Plan

## Evidence
SQLite `alter.c` `renameQuotefixFunc` reparses each schema SQL and resolves tokens against current schema. For rename two→four, DQS normalization must run against the OLD table columns: existing `"four"` is unresolved and becomes `'four'`, while `"two"` remains an identifier for subsequent rename. Applying quote-fix after replacing the column list incorrectly treats old literal `"four"` as a new column. Existing Go path mutates `ColCache` before table/index/trigger rewrite.

## Exact work
1. Add native RED test covering DQS table CHECK, index expression/predicate, and trigger SQL for source test case.
2. Before changing column definitions/cache, normalize each affected schema object with `quoteFixWithSchema` under old schema; then perform token-based rename.
3. Verify rewrite ordering and preserve rollback on failed rename; run native and execddl tests.
4. Restore only speculative fixture edits, preserve untouched worktree/root artifact.

## Verification
Native focused regression, exact alterqf suite, `git diff --check`, then full suite with captured elapsed/stdout/stderr.
