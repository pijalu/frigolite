# Handoff — index-key comparison allocations (2026-11-25)

Consumes audit P1/P2. Profile-driven performance fix, behavior preserved.

## Evidence — measure first

Paired audit baseline at HEAD `32ba3ee82` (`/tmp/perf_base_*.log`,
`/tmp/perfprof/cpu.prof`, `/tmp/perfprof/mem.prof`):

- CPU top: `syscall.rawsyscalln` 39.5%, `runtime.kevent` 18.5%, i.e. GC/scheduler
  coordination, not engine logic. The actionable profile is
  `-sample_index=alloc_objects`: `storage.DecodeRecord` 26.99% flat / 40.48%
  cumulative of 18.34M allocated objects; `decodeValue` 18.8%,
  `decodeBigEndianInt` 8.3%.
- Callers: `btree.RecordPayloadCompare` → 7,413,735 `DecodeRecord` calls
  (99.87% of that function's allocations). Every index comparison fully
  decoded both payloads into a fresh `Record` + `[]interface{}`.

## Change

- `internal/storage/record.go`: `DecodeRecordPrefix(data, out)` walks the
  record header with separate type/data cursors, validates every field's
  bounds exactly like `DecodeRecord`, decodes only the first `len(out)`
  fields into caller-owned storage, and returns the exact field count.
- `internal/btree/btree_keyinfo.go`: `RecordPayloadCompare` uses two
  `[16]interface{}` stack arrays with the prefix decoder; wide records
  (>16 fields) and decode errors fall back to the original full-decode
  path, preserving the field-count tiebreak and corrupt-record behavior.

A first attempt using `DecodeRecordColumns` REGRESSED (its `decodeColumnAt`
scans the whole requested-column list per field); it was replaced, not kept.

## Measurements (paired, interleaved, `reps=3`, 8 pairs)

Frozen baseline worktree `/tmp/frigo-base` (HEAD), optimized driver
`/tmp/perfprof/perfprof`, same flags `-index -point-ops 200`. Median ops/s:

| Phase | baseline | optimized | ratio | paired min–max |
|---|---:|---:|---:|---|
| `insert` | 174,899 | 201,044 | **1.149×** | 1.123–1.176 |
| `indexed-delete` | 132,125 | 143,858 | **1.089×** | 1.030–1.101 |
| `point-delete` | 208,436 | 232,841 | **1.117×** | 0.991–1.166 |
| `indexed-select` | 166,418 | 170,606 | 1.025× | 0.851–1.226 |
| `indexed-update` | 130,642 | 130,966 | 1.002× | 0.919–1.039 |
| `point-select` | 537,870 | 531,355 | 0.988× | 0.781–1.075 |
| `count-star` | 1,467,330,110 | 1,380,312,042 | 0.941× | 0.904–1.021 |
| `scan` | 19,329,130 | 19,308,485 | 0.999× | 0.982–1.012 |

`count-star`, `point-select`, `point-update` and `scan` do not touch the
index comparator and their per-pair ranges straddle 1.0 — run-to-run GC
noise, not a causal regression. Allocations drop 18.34M → 13.39M (-27%)
on the same workload.

## Equivalence

- `go test -count=1 ./internal/btree/ ./internal/storage/` PASS.
- Focused harness (family-substring diff vs t30): `index`/`where`/`delete`/
  `update`/`select`/`insert`/`btree` — NEW=0; `select` GONE=3 and `insert`
  GONE=2 from earlier correctness commits.
- Byte-for-byte file comparison is NOT a valid gate: two runs of the SAME
  binary produce different index page layouts (nondeterministic map order).
  Both files pass `PRAGMA integrity_check` and return identical row sets.
- Full 12-shard harness at this change (run dir
  `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.oL1npl`,
  12/12 logs+status, no kills): leaf FAIL set 1015 vs t30 1028, **NEW=0**,
  GONE=13 (unchanged from the pre-optimization gate). Family 190 vs 192,
  NEW=0.
- `go test -run TestSOLID_ ./...`: rc=0, 27 packages ok. Cross-engine matrix
  and `TestS66SQLiteFileCompatibility` PASS.

## Docs

- `TODO.md` PERF item updated with the measured gain and remaining gap.
- `.agents/lessons_learned.md`: allocation-profile + paired-measurement
  section.
