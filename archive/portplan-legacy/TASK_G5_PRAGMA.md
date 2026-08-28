# TASK G5.PRAGMA — PRAGMA statements

> **Phase**: G5 (advanced SQL).
> **Goal**: G5.PRAGMA.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.CREATE; G3 (schema pragmas).
> **Current state: PARTIAL** — 25+ pragmas exist; gaps remain.

## Objective
Pragmas that affect *correctness* match SQLite: `table_info`/`table_xinfo`/
`pragma_table_info`, `index_info`/`index_list`/`index_xinfo`, `foreign_key_list`/
`foreign_key_check`/`foreign_keys` (ON/OFF), `collation_list`, `database_list`,
`compile_options`, `integrity_check`/`quick_check`, `encoding`, `journal_mode`
(rollback modes only — WAL is deferred), `page_size`/`cache_size`/`auto_vacuum`
(structural), `user_version`/`application_id`, `writable_schema`, `case_sensitive_like`,
`recursive_triggers`, `defer_foreign_keys`, `reverse_unordered_select`, and the
function-style table-valued pragmas (`pragma_table_info(t)`).

> **Scope note:** Pragmas that toggle *engine features* (foreign_keys,
> recursive_triggers, case_sensitive_like) must actually change behavior — not
> just be accepted. WAL-mode pragmas return the current mode but can't switch to
> WAL (deferred — document).

## Scope — testgen packages
`pragma`, `pragmafault` (fault → triage/N-A the fault parts).

## Pre-test file
`frigolite_p5_pragma_test.go` — `TestP5Pragma_*`. Cases vs oracle:
- table_info/table_xinfo: column order, types, notnull, dflt_value, pk, hidden.
- index_info/index_list/index_xinfo.
- foreign_key_list/foreign_key_check/foreign_keys toggle.
- collation_list; database_list; compile_options.
- integrity_check/quick_check on clean + manually-damaged DB.
- user_version/application_id set+get round-trip.
- case_sensitive_like toggle changes LIKE behavior.
- recursive_triggers toggle changes trigger recursion.
- Function-style: `SELECT * FROM pragma_table_info('t')`.

## SQLite source references
- `src/pragma.c` — every pragma handler. **This is the spec.**
- `internal/exec/pragma.go` + `pragma_table.go`.

## Steps
- [ ] **G5.PRAGMA.1** Baseline `pragma` package; record results. Commit:
      `G5.PRAGMA.1: pragma baseline`.
- [ ] **G5.PRAGMA.2** Pre-test suite. Commit: `G5.PRAGMA.2: pragma pre-test suite`.
- [ ] **G5.PRAGMA.3** Feature-toggling pragmas (foreign_keys, recursive_triggers,
      case_sensitive_like) actually change behavior. Commit: `G5.PRAGMA.3: behavior pragmas`.
- [ ] **G5.PRAGMA.4** Table-valued pragma functions (pragma_table_info etc.).
      Commit: `G5.PRAGMA.4: pragma table-functions`.
- [ ] **G5.PRAGMA.5** integrity_check/quick_check. Commit: `G5.PRAGMA.5: integrity check`.
- [ ] **G5.PRAGMA.6** Triage pragmafail → N-A fault parts. pragma green.
      Commit: `G5.PRAGMA.6: pragma TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/pragma/ && \
go test -run 'TestP5Pragma' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Pragmas affecting correctness match SQLite: table_info/xinfo, index_info/list/xinfo, foreign_key_list/check/foreign_keys toggle, collation_list, database_list, compile_options, integrity_check/quick_check, encoding, journal_mode (rollback only), page_size/cache_size/auto_vacuum, user_version/application_id, case_sensitive_like + recursive_triggers behavior toggles, table-valued pragma functions. Spec is src/pragma.c. See portplan/TASK_G5_PRAGMA.md." \
  completionCriterion "testgen pragma PASS and TestP5Pragma pre-tests PASS; behavior-toggling pragmas actually change behavior." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/pragma/ && go test -run TestP5Pragma -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G5.PRAGMA. 25+ pragmas exist. Spec is src/pragma.c; code in internal/exec/pragma.go + pragma_table.go.
Behavior pragmas (foreign_keys, recursive_triggers, case_sensitive_like) must change behavior, not just be accepted.
Decisions: WAL-mode pragmas accepted but can't switch to WAL (deferred) — document.
Next: baseline, pre-tests, wire behavior toggles, then table-valued pragmas + integrity_check.
Risks: integrity_check must catch real corruption (coordinate storage layer); pragma argument parsing edge cases.
Carried limits: verifyCommand above.
```
