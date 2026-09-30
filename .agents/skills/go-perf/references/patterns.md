# Technique catalog (from goperf.dev, Part 1: Common Performance Patterns)

Each entry: what to do, why it works, when NOT to, and the site's benchmark magnitudes (illustrative — re-measure on your workload).

## 1. Object pooling (sync.Pool)

**Do:** reuse short-lived, resettable objects.

```go
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
buf := bufPool.Get().(*bytes.Buffer)
buf.Reset()
buf.Write(payload)
bufPool.Put(buf)
```

**Why:** avoids per-call heap allocations → less allocator work and GC pressure. Site benchmark (4KB write): 864 ns/op, 4096 B/op, 1 alloc → 42 ns/op, 0 B/op, 0 allocs (~20×).

**Use when:** objects are short-lived and reusable (buffers, scratch memory, request state); allocation/GC churn is measurable; lifecycle is local and a `Reset()` makes reuse safe.

**Avoid when:** objects are long-lived or shared across goroutines (pools don't coordinate ownership); reuse rate is low (idle pooled objects waste memory); lifecycle tracking matters more than allocation speed.

**Caveats:** benefit only shows under sustained load; objects come back in whatever state they were returned — always `Reset()` before use. "Not a silver bullet."

**Variants seen in real codebases:** per-statement scratch slots (nest-depth indexed buffers with a `defer` unwind) when a plain pool can't express nested reentrancy; clone-on-retain when a pooled scratch value must be kept past the next `Get()`.

## 2. Memory preallocation

**Slices:** `make([]T, 0, expectedSize)` when appending; `make([]T, n)` + index-assign when fully populated (also avoids bounds checks). Growth doubles capacity up to ~1024 elements, then ~25% steps — more allocation rounds for large unpreallocated slices.

**Maps:** `make(map[K]V, sizeHint)` avoids rehash/resize cycles (size hints since Go 1.11).

**Site benchmark** (append 10,000 ints): 28,539 ns/op, 357,626 B/op, 19 allocs → 7,093 ns/op, 81,920 B/op, 1 alloc (~4× faster, ~19× fewer allocs).

**Don't** when size is wildly unpredictable (over-allocation wastes memory, under-allocation still reallocates); don't prealloc as premature optimization — profile first.

## 3. Avoiding interface boxing

Boxing wraps a concrete value in a two-word structure (type descriptor + data pointer). **Since Go 1.4 all boxing allocates**, even small values (golang/go#12128).

- **Box pointers, not large struct values:** `shapes = append(shapes, &s)`. Site benchmark, 1000 boxed 4KB structs: ~404,649 → ~340,549 ns/op (~19% faster).
- **Pass pointers to interface-accepting functions:** boxing happens at the call site; pointers avoid the struct copy (~11% faster, 4096 B/op both).
- **Call concrete types directly** in hot paths when the type is known — removes indirection and allocation.
- **Typed containers over `[]interface{}`:** generics or concrete slices eliminate per-element boxing.
- **Profile for boxing:** allocations via `runtime.convT*` frames in pprof indicate boxing.

**Boxing is fine when:** abstraction/API design matters more (negligible cost off hot paths), values are small, short-lived, or genuine runtime polymorphism over heterogeneous types is needed.

## 4. Zero-copy

- **Slice instead of copy:** `buffer[128:256]` is a header over the same array — site benchmark: copying 64KB = 4,246 ns/op, 65,536 B/op vs re-slicing 0.592 ns/op, 0 B/op. **Behavior differs** (deep copy vs shared view) — ownership must be explicit; concurrent read/write over shared arrays causes subtle bugs.
- **`io.CopyBuffer`** streams through one reusable buffer instead of per-read allocation.
- **mmap:** `x/exp/mmap` + `ReadAt` removes syscalls but still copies (~25% faster than os.ReadAt in the site benchmark). True zero-copy = `unix.Mmap` + consume mapped pages directly: ~2× faster for memory-bound work (XXHash over 4MB), little gain when compute-bound (SHA256).
- Production users: fasthttp, gRPC-Go buffer pools, MinIO, Protobuf direct decoding, Badger/InfluxDB mmap.

## 5. Struct field alignment

- **Order fields largest → smallest; group same-size fields; don't alternate sizes.** Site benchmark (10M instances): 240MB → 160MB and slightly faster just from reordering.
- **Pad against false sharing** in concurrently-written counters: `_ [56]byte` between fields kept on separate cache lines (~3.8% faster on a 2-goroutine increment benchmark, varies 3–6%).
- Run the `fieldalignment` linter (`golang.org/x/tools/go/analysis/passes/fieldalignment`).
- Costs nothing, changes no logic — free to apply on hot paths and large volumes.

## 6. Stack allocations & escape analysis

- The compiler heap-allocates variables that escape. Diagnose: `go build -gcflags="-m"` ("moved to heap: x", "can inline").
- **Escape triggers:** returning pointers to locals, closures capturing variables, interface conversions (boxing), storing `&x` in globals/struct fields, very large composite literals.
- **Prefer value returns** for small structs (`printUser(u User)` not `*User`) in tight loops / latency-sensitive paths.
- **Verify, don't assume:** a returned pointer that provably doesn't outlive its scope may stay on the stack. To measure real heap cost, force escape via a package-level `sink` (site benchmark: ~0.26 ns/op stack vs ~10.55 ns/op, 24 B/op, 1 alloc with a true escape — ~40×).
- **Letting values escape is fine when:** constructors return pointers (idiomatic), the value genuinely outlives the call, allocations are small/infrequent, or avoiding escape would make code awkward.

## 7. GC-aware memory efficiency

- Go's GC: concurrent, non-generational, tri-color mark-sweep; STW pauses typically <100µs.
- **Allocation-pressure techniques** (preferred over tuning): stack allocation (above), sync.Pool (above), batch allocations (`make([]User, 0, 1000)` instead of 1000 small ones), `weak` references (Go 1.24 `weak.Make`) for caches/dedup that must not keep targets alive — always nil-check `.Value()`.
- GC **tuning** (GOGC/GOMEMLIMIT): see `references/profiling.md` §"GC tuning". Leave knobs alone unless profiling shows GC as the bottleneck; then change incrementally and verify. `GOGC=off` only for special cases (short-lived CLI tools) or paired with GOMEMLIMIT in fixed-memory environments.

## 8. Buffered I/O and batching

- Wrap hot read/write loops in `bufio.Reader/Writer`; flush deliberately. Syscall overhead shows in CPU profiles as `syscall.syscall` — if it dominates, buffer more, or batch payloads.
- **Batch operations:** amortize per-operation fixed costs (syscalls, round trips, transactions) over groups. The right batch size is workload-dependent — measure latency vs throughput.

## 9. Lazy initialization

- Defer expensive construction until first use (lazy singletons via `sync.Once`), keeping startup fast and avoiding work for unused features. Watch thread safety (`sync.Once`, not check-then-set) and don't lazily build things that are *always* used (just moves the cost and adds a check).

## 10. Atomics & synchronization

- Prefer `atomic.Int64/Bool/Pointer` over mutexes for single-word state; mutexes for compound invariants.
- Atomics avoid lock contention but don't compose — a mutex guarding three fields is correct where three atomics are not.
- Contention shows in CPU profiles as `runtime.lock2`/`semacquire`; false sharing shows as unexplained cache misses (pad — see §5).

## 11. Immutable data sharing

- Immutable structures can be shared across goroutines without locks; readers get a consistent snapshot. Build-then-publish (never mutate after publish); for updates, copy-on-write. Pairs with the COW discipline used in template/AST caching.

## 12. Worker pools

- Bound goroutines with a worker pool (fixed workers + job channel) instead of unbounded `go` per task — caps memory and scheduler churn. Worker count ≈ GOMAXPROCS for CPU-bound, higher for I/O-bound (measure).

## 13. Compiler flags & build-level

- `go build -gcflags="-m"` (escape/inline analysis); `-ldflags="-s -w"` shrinks binaries (not speed); PGO (`go build -pgo=auto` with a profile) lets the compiler inline/profile-guide hot paths — collect the profile from representative production load.
- Inlining: small hot functions benefit; the `-m` output shows what inlined and why not.

## 14. Networking patterns (when the workload is networked)

goperf.dev Part 2 covers: benchmark/load-test first, pprof with GC endpoints, net/http internals, 10k+ concurrent connections, GOMAXPROCS/epoll-level tuning, resilient connection handling, long-lived-connection memory management, TCP vs HTTP/2 vs gRPC vs QUIC, socket options, DNS tuning, TLS. Read the relevant page at https://goperf.dev/02-networking/ before optimizing a networked service — the same profile-first loop applies.
