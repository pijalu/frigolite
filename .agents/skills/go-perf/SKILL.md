---
name: go-perf
description: Go performance optimization: pprof profiling (CPU/heap/allocs), allocation hunting, escape analysis, sync.Pool, preallocation, interface boxing, zero-copy, struct alignment, GC tuning (GOGC/GOMEMLIMIT), benchmarks. Use whenever the user says Go code is slow, wants profiling/profiling results interpreted, mentions memory usage, GC pressure, allocations, pprof, flame graphs, go test -bench, or optimization of Go programs — even if they don't say "performance".
---

# Go Performance (goperf.dev techniques)

Fix performance by measurement, never by intuition. The loop is always:
**benchmark → profile → fix the top allocator/CPU frame → re-benchmark → repeat until the numbers move or the floor is real.**

## The loop

1. **Establish a baseline benchmark.** Write a `go test -bench` benchmark or a standalone probe program that reproduces the workload. Record ns/op, B/op, allocs/op. A fix without a before-number is a guess.
2. **Profile under representative load** (not idle). CPU profile shows where time goes; the alloc profile shows what feeds the GC. Read `references/profiling.md` for the exact commands and how to read the output (including the "GC dominates the CPU profile" diagnosis).
3. **Fix the top frame only**, using the pattern tables in `references/patterns.md`. One fix per commit so each number is attributable.
4. **Re-benchmark with the same probe.** Keep the win or revert it. Small wins (<5%) may be noise — run the benchmark 3+ times or use `benchstat`.
5. **Verify correctness** — run the package tests. Allocation/pooling changes are where reuse bugs live (stale state across uses).

## Choosing the technique

Symptom → pattern (details, code, and benchmark numbers in `references/patterns.md`):

| Symptom (profile says) | Technique |
|---|---|
| `runtime.mallocgc`, `gcDrain`, `madvise` dominate CPU; CPU/wall > 1.2 (multi-core GC) | Allocation hunting: find the top frame in `pprof -sample_index=alloc_space`, then apply pooling/prealloc/boxing fixes below |
| One allocation-heavy call repeated per item in a loop | sync.Pool (short-lived, resettable objects only — always `Reset()`); batch allocations into one slice |
| `append` in a loop over known-size input | Preallocate: `make([]T, 0, n)`; index-assign for fully-populated slices |
| `runtime.convT*` in alloc profiles | Interface boxing: box pointers not values, prefer concrete types/typed containers in hot paths |
| `fmt.Sprintf`/`strings.ToUpper`/`strings.Replace` on hot paths | Zero-copy / allocation-free rewrites: slicing instead of copying, ASCII fold scans, `strconv` appends into reusable buffers |
| Large structs copied into interfaces or by value | Pass pointers; order struct fields largest→smallest (run `fieldalignment`); pad for false sharing in concurrent counters |
| GC pauses/latency spikes, OOM in containers | GOGC/GOMEMLIMIT tuning (see `references/profiling.md` §GC tuning) — profile first, change incrementally |
| Hot function called with pointers everywhere, small short-lived values | Check escape analysis: `go build -gcflags="-m"` — returned pointers that don't escape may still be stack-allocated; prefer value returns for small structs |

Rules of thumb that generalize:

- **Every allocation is GC pressure.** Since Go 1.4 *all* interface boxing allocates, even small values. Innocent-looking lines (`fmt.Sprintf("%v", x)`, `strings.ToUpper(s)`, `append` growth) are the usual culprits — check the alloc profile before rewriting logic.
- **Reuse beats re-allocate, but only for short-lived, resettable objects.** Long-lived or shared objects don't belong in pools; low reuse rate wastes memory.
- **The compiler is smarter than intuition.** A returned pointer is not automatically a heap allocation. Verify with `-gcflags="-m"` and benchmarks (force escape with a global sink when measuring).
- **Synthetic numbers don't transfer.** goperf.dev's benchmarks are illustrative magnitudes; always re-measure on the real workload.

## Repo-specific hooks

If this repository has an established benchmark/probe layout, reuse it. In frigolite: scratch probe programs under `/tmp` (never project dependencies), the JSON-driven CRUD harness under `/tmp/perf` (`frigo`/`ssql` + `probe*`), and the repo quality gates (`tools/quality_gate.sh`, gocognit/gocyclo/staticcheck) must pass for every fix. Performance fixes here also need the standard test validation (targeted suites + census) — a perf fix that breaks correctness is reverted.

## References

- `references/patterns.md` — full technique catalog from goperf.dev Part 1 (pooling, prealloc, boxing, zero-copy, alignment, escape analysis, GC, plus buffered I/O, batching, lazy init, atomics, immutable data) with code patterns, when-NOT-to guidance, and benchmark magnitudes.
- `references/profiling.md` — pprof workflows (in-process and net/http endpoints), CPU vs heap vs alloc profiles, reading the output, GC diagnosis workflow, GOGC/GOMEMLIMIT tuning, benchstat.
