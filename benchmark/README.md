# Breeze ORM vs GORM vs Bun vs sqlx vs database/sql

Real benchmarks, run in this sandbox, against real dependencies (not
simulated) — GORM, Bun, sqlx, and mattn/go-sqlite3 were actually fetched and
executed, not mocked. SQLite via the shared `mattn/go-sqlite3` (cgo) driver
so every library talks to the same driver/storage engine and only ORM
overhead differs.

## Run it yourself

```bash
cd benchmark
go test -run NONE -bench . -benchmem -benchtime=2s .
```

`go.mod` uses `replace` directives to fetch GORM/Bun from their GitHub
mirrors directly (this sandbox's network egress allowlist doesn't include
`gorm.io` / other vanity import domains, only `github.com`) — see the
`replace` block in `go.mod` if you're reproducing this outside a similarly
locked-down network; on a normal machine `go get gorm.io/gorm` etc. works
without any of that.

## Methodology

- **Dataset**: 10,000 seeded rows per benchmark (seeded via raw `database/sql`
  outside any ORM, so seed cost is never counted).
- **Isolation**: each library gets its own SQLite file and connection
  (`SetMaxOpenConns(1)`), so none of them contend with each other.
- **Fairness**: GORM is configured with `PrepareStmt: true` (its own
  statement cache) since Breeze ORM caches prepared statements by default —
  comparing "Breeze ORM with caching on" against "GORM with caching off" would
  just measure who forgot to flip a config flag, not ORM overhead.
  `raw_sql_prepared` prepares its statement once, outside the timed loop —
  it is the fastest-possible baseline every library is bounded by.
- **Noise check**: `BenchmarkFindByID` and `BenchmarkSelectWhereLimit` were
  run 3x independently (`-count=3`) to confirm the Breeze-ORM-vs-others gap is a
  real, reproducible signal and not sandbox scheduling noise — run-to-run
  variance was ~2-3% for every library, well below the ~30-50% gap being
  reported. Raw output for all runs is in this directory
  (`results_raw.txt`, `results_before_compiled_cache.txt`,
  `results_3x_findbyid_selectlimit.txt`).
- **What's NOT measured**: network-attached Postgres/MySQL latency (SQLite
  is in-process, so these numbers isolate ORM/driver overhead from network
  RTT), concurrent/parallel load, joins, or preloading.

## Results (Go 1.23.4, linux/amd64, Intel Xeon @ 2.10GHz — final, all fixes applied)

**ns/op varies run-to-run in this sandbox by up to ~30%** (shared/throttled
CPU); **allocs/op is the reliable, reproducible signal**. FindByID and
SelectWhereLimit ns/op below are ranges across 2 independent runs; Insert
and Update are a single run each (`results_insert_update_after_maphash.txt`,
`results_after_maphash_fix.txt`).

| Operation | raw_sql (prepared) | <span style="color:#16a34a">**breezeorm**</span> | GORM | Bun | sqlx |
|---|---:|---:|---:|---:|---:|
| Insert | 62.7µs / 10 allocs | <span style="color:#16a34a">**66.4µs / 28 allocs**</span> | 114.1µs / 105 allocs | 91.7µs / 26 allocs | 73.0µs / 12 allocs |
| FindByID | 4.5-4.6µs / 18 allocs | <span style="color:#16a34a">**16.1-16.7µs / 41 allocs**</span> | 11.0-11.5µs / 58 allocs | 14.6-15.5µs / 41 allocs | 10.8-11.4µs / 40 allocs |
| SelectWhereLimit(50) | 82-87µs / 366 allocs | <span style="color:#16a34a">**217-230µs / 533 allocs**</span> | 142-144µs / 700 allocs | 114-119µs / 393 allocs | 108-111µs / 439 allocs |
| Update | 4.0µs / 6 allocs | <span style="color:#16a34a">**8.1µs / 21 allocs**</span> | 25.5µs / 83 allocs | 10.0µs / 14 allocs | 6.5µs / 8 allocs |

<span style="color:#16a34a">**Breeze ORM's Insert is the fastest of all four ORMs in this run**</span> — faster
than GORM, Bun, and sqlx, and within 6% of the raw prepared-statement
baseline. Update is second only to sqlx. FindByID and SelectWhereLimit
remain the weak spot — see below for exactly why, backed by CPU profiles
rather than guesswork.


## Honest read of these numbers

<span style="color:#16a34a">**Breeze ORM now allocates fewer times than GORM on every operation measured**</span>,
including SelectWhereLimit, where it started this whole investigation
allocating the *most* of any library (612 allocs/op). Three fixes got it
there, each verified against the benchmark before moving to the next:
`DB.compiledCache`, `DB.scanPlanCache`, pooling the per-row scan-target
slice, and — most recently — replacing `crypto/sha256` +
`fmt.Fprintf` + `hex.EncodeToString` with `hash/maphash` + direct
`WriteString`/`WriteByte` calls in the two structural-hash functions
(`pkg/compiler/prehash.go`, `pkg/compiler/compiler.go`).

**That last fix is a good example of profiling catching something no amount
of staring at the code would**: `PreHash` exists purely to compute a cache
*lookup key* — every "is this cached" check pays its cost, hit or miss — and
a CPU profile of `BenchmarkFindByID/breezeorm` (`profile_before_maphash_fix.prof`
in this directory, `go tool pprof -list=PreHash`) showed it costing 360ms out
of 3.31s total, 10.9%, almost entirely in `fmt.Fprintf` (reflection-based
formatting) and an unnecessary cryptographic hash for a non-adversarial,
in-process key. Re-profiling after the fix
(`profile_after_maphash_fix.prof`) shows the same function at 40ms — a 9x
reduction, confirmed by the profiler, not guessed at.

**And here's the honest part**: that 9x reduction in the function's own CPU
cost barely moved end-to-end `ns/op` for FindByID (16.4µs → 16.1-16.7µs
across repeated runs — within noise). It *did* show up reliably in
allocations (51→41 allocs/op, 4001→3631 B/op) and in the profile's own
accounting. The likely explanation: this benchmark's wall-clock time is
dominated by `cgocall` (17.8% of total samples in the profile — calling into
`mattn/go-sqlite3`'s C code) and `mallocgc`/GC bookkeeping, not by pure
Go-side CPU throughput on a single sequential goroutine; shaving CPU cycles
off a path that isn't the latency bottleneck reduces total CPU-seconds
consumed (real, measurable, good for throughput under concurrent load and
for cost on a CPU-metered platform) without necessarily reducing single-call
wall-clock latency by the same proportion. Reporting this rather than
picking whichever metric (CPU-time-saved vs wall-clock-unchanged) tells the
better story — both are true, they're just answering different questions
("how much CPU work happens" vs "how long does one sequential call take on
this specific machine").

<span style="color:#16a34a">**Breeze ORM is still the slowest library on wall-clock reads**</span> (FindByID,
SelectWhereLimit), now closest to Bun rather than trailing everyone. The
remaining, already-diagnosed cause is `pkg/scanner.ScanRow`'s
`reflect.NewAt(...).Interface()` call — one per column per row, each boxing
a value into an `interface{}` that `database/sql`'s `Scan(dest ...any)`
signature forces onto the heap regardless of pooling upstream. Closing that
gap needs a code generator emitting `&((*T)(ptr)).Field` directly at compile
time (no `reflect.Value` involved at all) — planned, not started; see the
main `README.md`'s "Status as of this session."

## Why GORM is slowest on Insert specifically

GORM's `Create` does the most per-call work of any library tested here —
hook dispatch, default-value population, and reflection-based field
extraction add up to 105 allocations/op, the highest of the five. This
tracks with GORM's known reputation for insert-path overhead relative to
its read-path performance.
