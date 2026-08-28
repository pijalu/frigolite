# Unit Conformance Layer (UCL) — Method Document

> **Status**: AUTHORITATIVE method for ALL remaining work (P5 gaps → P9).
> Supplement to `PORTPLAN.md` §5e/§6; adopted after the 2026-08 approach
> review of the FTS4 merge-writer stall.
> **Scope**: every goal whose completion is verified by tests must build its
> UCL instrument FIRST. No exceptions for "too big" or "mostly SQL" topics.

---

## 1. Why this exists (incident record)

The FTS4 incremental-merge work stalled for days on a single assertion
(`testgen/fts4growth` 7.7: `sum(length(block))` 634257 vs oracle 635247).
Post-mortem found three root causes:

1. **Wrong debugging granularity.** The assertion is a scalar byte-sum with
   zero locality: each iteration was full e2e run → hex dump → guess. Nothing
   pointed at *which* block/node/byte diverged.
2. **Re-derivation instead of port.** `internal/fts/incrwriter.go` (432
   lines) re-derives the ~2000-line writer subsystem of
   `ext/fts3/fts3_write.c` (`fts3IncrmergeInit/Load/Append/Flush/Push`,
   `aNodeWriter` hierarchy, `fts3TruncateNode`, `fts3PromoteSegments`,
   hint blob). Gaps were patched from observed behavior until
   `.agents/lessons_learned.md` §62 recorded an explicit "EMPIRICAL
   convergence" heuristic — a direct violation of PORTPLAN principle 10.
3. **No circuit breaker.** Six near-duplicate goals accumulated in the goa
   queue, all targeting the same assertion. Queue growth was masking the
   approach failure.

The same failure mode threatens every remaining topic (WAL frame layout,
JSONB binary, pager/ptrmap, planner EQP strings): any seam where correctness
is "match SQLite's observable output" degrades into guess-loops when the
only feedback is a scalar e2e mismatch. UCL is the standing countermeasure.

## 2. What SQLite actually ships (question 1: dedicated UT?)

**SQLite has NO xUnit-style unit tests.** Verified inventory:

| Artifact | Kind | Portable to Frigolite? |
|---|---|---|
| `test/*.test` (1174 files) | e2e TCL through the C API | already transpiled (testgen) |
| `src/test_*.c` | TCL bindings exposing internal C APIs | no (C/TCL-only surface) |
| `ext/fts3/fts3_test.c` | TCL bindings (near-match, tokenizer) | no |
| `ext/fts3/tool/fts3view.c` | **structure inspector**: decodes `%_segdir` rows, `%_segments` blocks (leaf/interior node trees), doclists, vocabulary | **YES — pure decode logic** |
| `ext/fts3/tool/fts3cov.sh` | gcov coverage harness | methodology only |
| `test/dbfuzz2*, ext/misc/*` | fuzz/property drivers | methodology (differential vs oracle) |

Conclusion: there is no dedicated UT suite to port. What IS portable is
SQLite's **observability tooling** — structure decoders — plus the oracle
CLI (`/usr/bin/sqlite3` 3.51.0) as a behavioral ground-truth generator, plus
the C source itself as the anchor for unit-level expectations.

## 3. UCL instruments

### U0 — Universal adoption (the rule)
Every remaining goal (PORTPLAN §5a queue) MUST include, **before engine
edits on its seam**, a UCL tranche:
1. scenario(s) for the seam (smallest that exhibits the behavior → full),
2. committed oracle fixture(s) generated from `/usr/bin/sqlite3`,
3. localized unit tests (first-divergence output, or exact golden values)
   whose expectations come ONLY from U1 sources,
4. for byte-layout seams: a decoder ported from the corresponding SQLite
   C tooling/source.

A goal whose sub-plan lacks the UCL tranche is not ready to be created as a
goa goal (PORTPLAN §5b contract item 0).

### U1 — Expectation sourcing (no code bias)
Golden expectations may be derived ONLY from:
- `/usr/bin/sqlite3` (oracle CLI) output on a committed scenario, or
- direct reading of SQLite C source (`../sqlite/src/`, `../sqlite/ext/`),
  with the C function/line cited in the test.

 NEVER record frigolite's current output as the expected value. If a
 frigolite result is used to *shape* a test, the test is biased and will
 enshrine a bug.

### U2 — Golden fixture pipeline
- A **scenario** is a JSON file: `{name, pragmas, sql: [...], dumpAfter}`.
- `tools/orafixture` (generator; generic, all topics) runs the scenario
  under the oracle CLI and commits the resulting database/journal/WAL files
  as golden fixtures next to the scenario. Generation is deterministic
  (fixed SQL, fixed pragmas, no timestamps).
- The conformance test replays the same scenario on frigolite and compares
  observable state against the fixture. Oracle-produced files are standard
  SQLite artifacts, so frigolite reads them through its own storage layer —
  exercising the reader too.

### U3 — Structure decoders (port of SQLite tooling)
For each byte-layout seam, port the corresponding SQLite decoder into the
owning `internal/` package (FTS: `internal/fts/segview.go` from
`fts3view.c`; WAL: frame/header decoder per `src/wal.c`; pager: header/
ptrmap per `src/pager.c` format docs). Each Go function carries a comment
mapping it to its C origin. Decoders are written from the C source, NOT
from frigolite's output. Thin CLIs under `tools/` (e.g. `tools/ftsview`)
expose them for interactive debugging.

### U4 — Localization requirement
A parity failure is only acceptable if the test output names the **first
divergence**: file/page/block id, structure height, byte offset, decoded
context (e.g., `block 2155 h=1 off=37: separator term "xyz" vs expected
"xyzz"`). A scalar mismatch (sum/len/count/exit-code) is not a sufficient
failure report for parity work. If an existing e2e assertion has zero
locality, the UCL scenario for that seam must be built BEFORE further
engine edits.

### U5 — Circuit breaker (stall detection) — global
Trigger (any one):
- two consecutive sessions end without closing the same assertion, or
- a second goa goal gets queued for the same failure, or
- the failure is byte/structure parity with zero locality.

Action: STOP engine edits on that seam. Build or extend the UCL instrument
(decoder + scenario + fixture) until the failure is localized; resume edits
only under first-divergence supervision. Queuing another same-target goal is
never the correct response.

### U6 — Non-parity topics still need UCL
Topics without byte layouts (planner text, error strings, API semantics)
use the same pipeline in golden-value form: oracle CLI output (EXPLAIN
QUERY PLAN, error messages, PRAGMA results, script transcripts) committed
as fixtures; unit tests replay on frigolite and diff. The unit of
comparison is chosen so a mismatch names the first divergent line/field.

## 4. Scenario format (generic)

```json
{
  "name": "fts4growth-x6",
  "pragmas": {"page_size": 1024},
  "sql": ["CREATE VIRTUAL TABLE x6 USING fts4;", "..."],
  "dumpAfter": "last"
}
```

Scenarios start tiny (seconds to run, one behavior each) and progress to
the full failing sequence. Small scenarios keep the fix loop fast and pin
individual behaviors; the big one proves final parity.

## 5. Per-topic instrument matrix (ALL remaining topics)

| Topic (goal) | Parity surface | Instrument | Oracle fixture content |
|---|---|---|---|
| FTS3/4 incr-merge writer (`P6.FTS-WPORT`) | `%_segments` block bytes, `%_segdir` rows | `internal/fts/segview.go` (from fts3view.c) + `tools/ftsview` | DB after merge scenarios |
| FTS3/4 matchinfo/snippet (`P6.FTS-D/E` regressions) | aux-function blobs/text | golden values | query outputs |
| FTS4 automerge distribution (`P6.FTS-G`, fts4merge4) | segdir level layout | segview + segdir dumps | DB after automerge loops |
| FTS3/4 misc (`P6.FTS-H`) | integrity/offsets/sort | golden values | query outputs |
| JSON1/JSONB (`P6.JSON`) | `jsonb()` binary, all function results | `internal/function` golden tests anchored on `src/json.c`/`jsonb.c` | `SELECT jsonb(...)`, `->`/`->>` matrices |
| Virtual tables (`P6.VTAB`) | module behavior, EQP | golden transcripts per module | query transcripts |
| Backup (`P5.BACKUP` residue) | page-level copy semantics | oracle backup DBs | src.db + backup.db pairs |
| Autoindex / planner (`P7.*`) | EXPLAIN QUERY PLAN text, index choice | golden EQP fixtures | `.eqp` transcripts |
| WAL (`P7.WAL-*`) | WAL header/frame bytes, checkpoint outcomes | `internal/wal` frame decoder per `src/wal.c` | `-wal`/`-shm` files from oracle |
| Locks (`P7.LOCK-*`) | observable busy/locking outcomes | multi-connection scenario transcripts | CLI scripts |
| Journal modes (`P7.WAL-E`) | journal file bytes/behavior | journal decoder | `-journal` files |
| Snapshot (`P7.SNAPSHOT`) | read-mark observable behavior | scenario transcripts | CLI scripts |
| Pager/format (`P8.PAGER`) | header, ptrmap, freelist layout | hexdump fixtures per file-format doc | DBs at forced states |
| Corruption (`P8.CORRUPT`) | detection points + messages | corrupted-DB corpus + golden errors | byte-patched DBs |
| Encoding/URI (`P8.ENCODING`) | UTF-16 DBs, URI handling | encoding fixtures | oracle UTF-16 DBs |
| Incr/auto vacuum (`P8.INCRVACUUM`) | page maps | hexdump/ptrmap fixtures | DBs post-vacuum |
| VACUUM/RECOVER/ROLLBACK (`P8.*`) | file bytes, semantics | fixtures + transcripts | oracle DBs |
| Pragmas (`P8.PRAGMA`) | pragma outputs/limits | golden values | transcripts |
| Perf closeout (`P9.PERF`) | functional assertions | existing practice + oracle | transcripts |

Rule: when in doubt, a topic gets BOTH a decoder (if bytes) and golden
transcripts (for messages/EQP). Building the instrument is part of the
goal's first micro-task, not an optional extra.

## 6. Rules of engagement

1. Engine edits on a broken seam resume only after the failure is localized
   by a UCL instrument (U0/U4).
2. A heuristic that passes a test but diverges from the C dispatch
   (cf. lesson §62) is a **defect**, not a fix — PORTPLAN principles 10/12.
3. Fixtures are committed; regeneration must be deterministic and must be
   re-runnable via `tools/orafixture` only.
4. Decoder/tool code follows the quality gates like all production code;
   split files if the 500-line soft limit is approached.
5. Oracle = `/usr/bin/sqlite3` (currently 3.51.0). Version is recorded in
   each fixture directory (`ORACLE_VERSION`) so upgrades are visible.
6. testgen packages remain the e2e safety net; UCL tests are the debugging
   and localization layer. Green requires both.
