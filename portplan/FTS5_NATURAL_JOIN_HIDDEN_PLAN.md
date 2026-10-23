# FTS5 NATURAL JOIN hidden-column regression plan

## Reproduction/source contract
- Full suite after JOIN repairs: `/tmp/frigolite-full-after-joinfix.log`, exit 1 at 370.040s. First independent native failures: `TestFTS5ResumePinLeftJoinUnusableMatch` and `TestFTS5ResumePinNaturalJoinHiddenCols`.
- Exact repro: `go test . -run '^TestFTS5ResumePinNaturalJoinHiddenCols$' -count=1`; both orderings return `b 1 {}` instead of `b world {}`.
- Native regression setup and SQL are `frigolite_fts5_resume_pin_test.go:746-775`; SQLite source `../sqlite/test/fts5misc.test` cases 12.1/12.2. SQLite hidden FTS5 `rank` and table-name columns must be excluded from NATURAL join merge; NATURAL matches on visible column `a` only.
- Root cause confirmed: hidden FTS5 definitions (`ft`, `rank`) occupied positions in materialized source rows, while output-star vectors are visible-only; reverse-order joins consumed positional vector as if hidden slots were visible. `buildMaterializedRowMaps` now stores visible-only vectors; index-aware right output maps source definition positions to visible-vector positions. In `applyJoin`, right vectors move to a dedicated key and are removed from generic positional slot to avoid mistaking right vector for left vector.
- Verified: both native FTS5 resume tests pass; `FRIGOLITE_TEST=joinH`, `selectD`, and `attach2` suites pass; broader `TestJoin|TestP6_|TestFTS5ResumePin`, `internal/execquery`, and `git diff --check` pass.
## Plan and implementation
1. Traced definitions: hidden FTS5 columns present in rightDefs; FTS5 rows contain user columns then hidden table-name and rank slots.
2. Added exact regression through existing native test `TestFTS5ResumePinNaturalJoinHiddenCols` (also direct suite cases 12.1/12.2); it caught reverse-order hidden-slot alignment after first repair.
3. Implemented visible-only positional vectors in `buildMaterializedRowMaps`, source-index-to-visible-vector mapping, and separate right-side vector key consumption in `applyJoin`.
4. Verified native FTS5 cases, JoinH, selectD, attach2, broad JOIN/P6/FTS5 native tests, execquery, and diff check (commands above).
