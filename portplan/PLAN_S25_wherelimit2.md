# PLAN S25 — wherelimit2 (4 leaves: 5.4, 5.5, 6.1, 6.2)

Agent unit brief. Dedicated git worktree off the current feature/perf
head. File territory: internal/parse (DELETE/UPDATE ... ORDER BY LIMIT),
internal/execdml, internal/exectrigger, the harness. Fully disjoint from
S23/S24's planner files.

## The cases (read ../sqlite/test/wherelimit2.test sections 5-6 first)

- **5.4/5.5**: DELETE/UPDATE with ORDER BY/LIMIT/OFFSET over the view
  "v w" via INSTEAD OF triggers, logging through a `db func log` UDF
  into a runtime list (`proc log {args} { lappend ::log ... }`). The
  harness shows `runtime variable mismatch got [""]` — the trigger's
  SELECT log(...) side effects never reach the runtime variable. The
  lessons' callback-interop section covers sibling shapes
  (alterlegacy/altertab's variadic trigger UDF) — check how those were
  wired (the converter's db func handling + the trigger body's
  user-function calls writing runtime vars).
- **6.1**: `WITH t2 AS MATERIALIZED (VALUES(5)) DELETE FROM t2 ORDER BY
  rank()OVER() LIMIT 2;` — our parser errors `near "ORDER"`; read the C
  grammar (parse.y: DELETE FROM with ORDER BY/LIMIT) and what error TEXT
  C produces for a CTE-named target ("view t2 may not be modified"? run
  the 3.54 oracle: the error text is the contract).
- **6.2**: `SELECT * FROM t2;` got [1 2 3 5 8 13] want [3 5 8 13] — a
  cascade of 6.1's state (the fixture's t2 differs after the failed
  6.1): verify after 6.1 lands.

## Verify

- The 3.54 oracle for 6.1's exact error text and 5.x's row sets.
- The family diff (python); wherelimit2 strictly better, zero new.
- Records: append to .agents/lessons_learned.md; do not edit TODO.md;
  keep this file's status lines current.
