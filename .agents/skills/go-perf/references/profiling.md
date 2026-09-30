# Profiling & benchmarking workflow

Commands for the measure → profile → fix → verify loop. All of these have been proven on real workloads; prefer them over ad-hoc timing.

## 1. Benchmarks

```bash
go test -bench=. -benchmem ./pkg/...          # standard benchmarks
go test -bench=BenchmarkFoo -benchmem -count=5 ./pkg/  # multiple rounds
benchstat old.txt new.txt                     # statistical comparison (golang.org/x/perf/cmd/benchstat)
```

- Always record **ns/op, B/op, allocs/op** — allocs/op is the GC-pressure signal.
- <5% deltas may be noise: use `-count` ≥ 3 and `benchstat`, or run interleaved A/B (alternate old/new builds per round) to cancel machine drift.
- For workloads `go test -bench` can't express (long-lived state, cross-statement effects), write a standalone probe program that reproduces the workload and prints per-op time + `runtime.MemStats` deltas (HeapAlloc, TotalAlloc, NumGC, PauseNs). Keep probes out of the project (e.g. under /tmp with a `replace` directive).

## 2. CPU profile

```bash
go test -cpuprofile=cpu.out -bench=. ./pkg/
go tool pprof -top -cum -nodecount=30 binary cpu.out      # sorted by cumulative time
go tool pprof -http=:7070 cpu.out                          # web UI / flame graph
```

For long-running programs or services:

```go
import _ "net/http/pprof"
go http.ListenAndServe("localhost:6060", nil)              // separate port, localhost only
```

```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30   # CPU under load
go tool pprof http://localhost:6060/debug/pprof/heap                 # retained memory
```

Profile under **representative load**, never idle. Keep the pprof endpoint on a separate localhost port.

## 3. Reading a CPU profile — the GC diagnosis

Work down the `-top -cum` list and classify:

- `runtime.gcDrain`, `runtime.scanobject`, `runtime.mallocgc`, `runtime.madvise`, `runtime.kevent/pthread_cond_wait` = **GC/scheduler coordination**. If these dominate (app frames < 50%), the real problem is allocation volume, not code logic. Go straight to the alloc profile (§4).
- `syscall.syscall` = I/O — buffer or batch.
- `runtime.lock2`, `semacquire` = lock contention.
- App frames = actual logic; optimize the top one.

**Quantify the GC share:** re-run the benchmark with `GOGC=off`. The wall-time delta vs normal GOGC is the GC tax; CPU-time delta is usually larger (GC runs on multiple cores). If GC tax is large, hunt allocations (§4) before touching logic. (`GOGC=off` is diagnostic here — it may balloon memory; never ship it without GOMEMLIMIT.)

## 4. Allocation profile — where the bytes come from

```bash
go test -memprofile=mem.out -bench=. ./pkg/
go tool pprof -top -sample_index=alloc_space -nodecount=20 binary mem.out   # allocation VOLUME (GC pressure)
go tool pprof -top -sample_index=alloc_objects binary mem.out               # allocation COUNT (latency spikes)
go tool pprof -top -sample_index=inuse_space binary mem.out                 # retained memory (leaks)
go tool pprof -list 'FuncName' -sample_index=alloc_space binary mem.out     # line-level view of one function
go tool pprof -top -cum -focus 'PackageName' ...                            # restrict to your frames, exclude runtime noise
```

- `alloc_space` answers "what feeds the GC" — start here.
- `inuse_space` answers "what is retained" — use for leaks (compare before/after a workload).
- Cumulative profiles include ALL prior phases in the same process; for per-phase attribution use separate runs (or a probe that snapshots MemStats between phases).

## 5. The allocator hit list (what usually shows up)

Innocent-looking hot-path constructs that allocate:

- `fmt.Sprintf("%v", x)` — formats + allocates even for trivial values. Use typed comparisons/serialization (`strconv.FormatInt`, `AppendInt` into reusable buffers). Also: `%v` equality-comparison of values (`Sprintf(a) != Sprintf(b)`) is both slow and fragile — compare typed values.
- `strings.ToUpper/ToLower/Replace/TrimSpace` — full-text copies. For keyword gates, use allocation-free ASCII fold scans (`indexFoldASCII`-style loops, or compare with `strings.EqualFold` on short bounded prefixes).
- `append` growth in loops — preallocate (see patterns.md §2).
- Interface boxing — `runtime.convT*` frames; box pointers, prefer concrete types (patterns.md §3).
- `map[string]X` built per call with identical inputs — memoize with a validity guard (schema/version fingerprint, generation counter) and add an invalidation test that does the DDL/invalidating action mid-loop.
- Regex matching on hot paths — even non-allocating `FindStringIndex` costs; precompile or replace with byte scans.

## 6. Escape analysis

```bash
go build -gcflags='-m -m' ./pkg/ 2>&1 | grep -E "escapes|moved to heap"
```

Look for hot-path values that escape unnecessarily (returning pointers to locals, closures, boxing). See patterns.md §6 for triggers and when escaping is fine.

## 7. GC tuning (GOGC / GOMEMLIMIT)

Only after profiling shows GC as the bottleneck. Change incrementally, verify with the same probe.

| Knob | Meaning | When |
|---|---|---|
| `GOGC=100` (default) | collect when heap grows 100% since last GC | leave alone unless GC-dominated profiles |
| `GOGC=N` (N>100) | allow more garbage between collections | sustained-throughput services with memory headroom (Uber/Cloudflare saved real cores this way) |
| `GOGC=off` | disable GC | short-lived CLI tools; or paired with GOMEMLIMIT below |
| `GOMEMLIMIT=X` | soft heap cap; GC turns aggressive near the limit | containers: set ~20% below the container limit (512MiB limit → 400MiB GOMEMLIMIT) |
| `GOMEMLIMIT=X GOGC=off` | grow freely, full GC at threshold | fixed/predictable memory environments; backfires on allocation-heavy apps — benchmark |

Monitor with `runtime.ReadMemStats` (HeapAlloc, TotalAlloc, NumGC, PauseNs) or pprof. `debug.SetMemoryLimit`/`debug.SetGCPercent` set these programmatically.

## 8. Correctness guardrails for performance fixes

- One optimization per commit, before/after numbers in the message.
- Reuse/pooling bugs are stale-state bugs: `Reset()` discipline, no state leaked across uses, nested/reentrant callers get their own slots (depth-indexed scratch, not a single shared buffer).
- Caches keyed on derived data need an invalidation guard test — perform the invalidating action (DDL, config change) mid-loop and assert fresh results.
- Run the package's full test suite (and downstream suites for shared code) after every win. A perf fix that changes behavior is reverted, not traded.
