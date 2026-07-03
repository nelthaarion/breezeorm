# Breeze ORM — Performance Fix Changelog

This document describes the performance fixes applied to Breeze ORM based on a
code-and-profile review of the `benchmark/` suite. Each fix is annotated with
the file(s) touched, the root cause it addresses, and the expected impact.

The fixes are purely additive to the hot path — no public API changed, no
behavioral semantics changed, and every fallback path preserves the exact
behavior of the original code for any case the fast path doesn't cover.

---

## Summary

| # | Fix | Path affected | Impact |
|---|---|---|---|
| 1 | Inline type switch in `ScanOne` and `ScanRow` | FindByID, Cursor reads | Removes per-column indirect call on the two scan paths that were missed |
| 2 | Hoist `byName` map out of `rowValues` in `CreateBatch` | Bulk insert | Eliminates N throwaway maps + N×(field count) reflect calls per batch |
| 3 | Use precomputed `Offset` in `structAssignments` | Insert, Update | Replaces `reflect.FieldByIndex` walk with single pointer add per column |
| 4 | Wire up `FastScan` registry in `Find` and `First` | All reads (with codegen) | Enables the zero-dispatch generated-scanner path end-to-end |
| 5 | `compiledCache.GetOrCompute` (coalesce concurrent misses) | All (concurrent load) | Prevents N× redundant compiles on a cold cache for a hot new shape |
| 7 | Skip `withRetryGeneric` closure when `MaxAttempts <= 1` | All queries | Removes 1 closure allocation per query when retry is off |
| 9 | Lock-free `metadata.Compile` via `atomic.Pointer` | All (concurrent load) | Removes RLock contention from every `orm.Model[T](db)` call |

Fixes #6 (avoid `Limit(1)` clone in `First`), #8 (pool `[]any` args), #10
(caller-side context deadlines), #11 (native pgx driver), and #12 (codegen
tool) were intentionally NOT applied in this pass — #6 and #8 are more
invasive API changes for marginal gain, and #10/#11/#12 are larger
architectural efforts documented in the main README. The applied fixes are
exactly the ones that are (a) safe, (b) self-contained, and (c) yield real
measurable improvement without changing any public signature.

---

## Fix 1: Inline type switch in `ScanOne` and `ScanRow`

**File:** `pkg/scanner/scanner.go`

**Root cause:** `ScanAllHint` already had a fully inlined `switch a.Kind`
that addressed each column's destination field via a direct `(*int64)(fp)` /
`(*string)(fp)` / etc. cast — a free pointer-shaped-value-into-`any` store.
But `ScanOne` (the FindByID/`First()` path) and `ScanRow` (the streaming
`Cursor` path) were never updated to match; both still called
`a.assign(fieldPtr)`, an **indirect function-pointer call** the Go compiler
cannot inline. Every column, every row, paid for a func-value load +
indirect branch on those two paths — even though the README claimed the
optimization covered the scan path broadly.

**Fix:** Copied the exact inline switch from `ScanAllHint` into both
`ScanOne` and `ScanRow`. The `default` (kindOther) case still falls back to
`a.assign(fp)` so exotic/custom types are unchanged.

**Expected impact:** Removes one indirect call per column per row on the
FindByID and Cursor paths. For the 5-column `benchUser` model (all
fast-path kinds), that's 5 indirect calls eliminated per FindByID. Small
in absolute ns (the 16µs is dominated by pgx + Postgres round-trip), but
free, and removes an obvious inconsistency.

---

## Fix 2: Hoist `byName` map out of `rowValues` in `CreateBatch`

**File:** `pkg/orm/query.go`

**Root cause:** The old `rowValues` built a `map[string][]int` of every
column's `FieldIndex` **on every single row** of a `CreateBatch` call:

```go
func rowValues[T any](tbl *metadata.Table, model *T, cols []string) []any {
    v := reflect.ValueOf(model).Elem()
    byName := make(map[string][]int, len(tbl.Columns))  // ← EVERY ROW
    for _, c := range tbl.Columns { byName[c.Name] = c.FieldIndex }
    ...
}
```

For `MaxBulkInsertRows = 5000`, that's **5000 throwaway maps** plus
5000×(field count) `reflect.FieldByIndex` calls — all computing the same
answer, since the map depends only on `tbl` which is fixed for the whole
batch.

**Fix:** `batchColumns` now returns a parallel `[]*metadata.Column` slice
(computed once per batch). `rowValues` takes that slice and addresses each
field via its precomputed `Offset` — a single `uintptr` add per column,
matching what the scanner already does. No map, no `FieldByIndex` walk.

**Expected impact:** For a 5000-row batch insert of a 4-column model:
eliminates 5000 map allocations + 20,000 `reflect.FieldByIndex` calls,
replaced with 20,000 pointer adds. Large for batch-heavy workloads;
negligible (single-row) for the benchmark's `Create` path.

---

## Fix 3: Use precomputed `Offset` in `structAssignments`

**File:** `pkg/orm/query.go`

**Root cause:** `structAssignments` (used by single-row `Create` and
`UpdateAll`) called `v.FieldByIndex(c.FieldIndex)` per column, which
re-traverses the embedded-struct chain on every call. The `Offset` field
was already precomputed at `metadata.Compile` time but never used on the
write path — only the read path (scanner) used it.

**Fix:** Same pattern as the scanner: get the base pointer via
`v.UnsafeAddr()`, then `unsafe.Pointer(uintptr(dest) + c.Offset)` per
column, then `reflect.NewAt(c.Type, ptr).Elem().Interface()` to materialize
the value. Still one `reflect.NewAt` per column (a future codegen pass
would replace this with direct `*(*T)(ptr)` casts), but the
`FieldByIndex` walk is gone.

**Expected impact:** Moderate for single-row Insert/Update. Part of why
Insert (66µs) is still ~6% slower than raw prepared (62µs) — the
value-extraction side still had reflect overhead. This narrows that gap.

---

## Fix 4: Wire up `FastScan` registry in `Find` and `First`

**Files:** `pkg/scanner/scanner.go`, `pkg/orm/query.go`

**Root cause:** The codebase already had a complete FastScan infrastructure
that was simply never called from the public API:

- `pkg/scanner/registry.go` — a `fastScanRegistry` (`sync.Map`) with
  `RegisterFastScan[T]` / `LookupFastScan[T]`
- `pkg/scanner/gen/user_scan.go` — a hand-written example of a generated
  scanner: `func scanUser(rows, dest *User) error { return rows.Scan(&dest.ID, ...) }`
- `pkg/scanner/scanner.go` — `ScanAllHintFast[T]`, a variant that takes a
  `FastScanFunc[T]` and skips the entire `Assignments` loop + switch +
  `targetsPool`

But `pkg/orm/query.go`'s `Find` and `First` always called `ScanAllHint` /
`ScanOne` — neither ever checked `LookupFastScan`. The infrastructure sat
unused.

**Fix:**
1. Added `ScanOneFast[T]` (the single-row counterpart of
   `ScanAllHintFast`) to `pkg/scanner/scanner.go`.
2. `Find` now checks `scanner.LookupFastScan[T](cq.CacheKey)` first; on a
   hit, dispatches through `ScanAllHintFast` (no Plan, no Assignments
   loop, no targetsPool — just the generated `rows.Scan(&dest.Field, ...)`).
3. `First` does the same with `ScanOneFast`.
4. Both fall back to the existing `ScanAllHint` / `ScanOne` paths when no
   generated scanner is registered, so un-generated models are unchanged.

**Expected impact:** This is the documented "biggest remaining chunk." For
any model with a registered FastScan function, the per-row scan becomes
literally `rows.Scan(&field, &field, ...)` with zero dispatch — matching
hand-written sqlx. This is the single highest-leverage fix for closing the
read-path gap to sqlx/Bun. (The actual speedup requires running the
codegen tool to register FastScan functions, which is the separately
documented `cmd/breezeorm-gen` effort; the wiring is now in place.)

---

## Fix 5: `compiledCache.GetOrCompute` (coalesce concurrent misses)

**File:** `pkg/orm/query.go`

**Root cause:** `compileCached` used a check-then-`Set` pattern:

```go
if cq, ok := db.compiledCache.Get(key); ok { return cq, nil }
cq, err := compiler.Compile(...)  // ← N goroutines run this for a cold key
db.compiledCache.Set(key, cq)
```

The `stmtCache` already used `GetOrCompute` (which coalesces concurrent
misses for the same key via an `inflight` map — see `pkg/cache/lru.go`).
But `compiledCache` did not. Under concurrent first-time callers for a new
query shape, N goroutines each independently ran the full
`planner.Build → optimizer → PlanPhysical → structuralHash` pipeline, and
only one result survived the `Set`.

**Fix:**

```go
return db.compiledCache.GetOrCompute(key, func() (*compiler.CompiledQuery, error) {
    return compiler.Compile(ctx, b, db.dialect, db.passes, db.plugins)
})
```

**Expected impact:** None for the single-goroutine benchmark, but
meaningful under concurrent production load — prevents N× redundant
compiles on a cold cache for a hot new query shape.

---

## Fix 7: Skip `withRetryGeneric` closure when `MaxAttempts <= 1`

**File:** `pkg/execution/executor.go`

**Root cause:** Every `Query` and `Exec` call wrapped its work in a
closure passed to `withRetryGeneric`. The closure captures `ctx`, `ex`,
`gen` — and because `withRetryGeneric` takes a `func() (T, error)`
parameter, the closure is forced onto the heap (passed as an interface
value). That's **1 heap allocation per query** that's pure overhead when
retry is effectively off (the common case) or when the first attempt
succeeds (almost always).

**Fix:** Added a fast path at the top of `Query` and `Exec`: when
`ex.retry.MaxAttempts <= 1`, run `prepare + query/exec` inline with no
closure. The retry path is preserved unchanged for `MaxAttempts > 1`.

**Expected impact:** 1 fewer allocation per query on the default
configuration (`DefaultRetryPolicy().MaxAttempts == 3`, so this fast path
doesn't trigger by default — but callers who set
`WithRetryPolicy(RetryPolicy{MaxAttempts: 1})` for latency-sensitive
services get the win). To get the win by default, a future change could
restructure `withRetryGeneric` to not require a closure (e.g., take the
work as an interface with a `Run() (T, error)` method), but that's a
larger refactor.

---

## Fix 9: Lock-free `metadata.Compile` via `atomic.Pointer`

**File:** `pkg/metadata/metadata.go`

**Root cause:** The metadata registry used `sync.RWMutex`:

```go
type registry struct {
    mu      sync.RWMutex
    entries map[reflect.Type]*entry
}
```

`getOrCreateEntry` took an `RLock` on every call. The README called this
"lock-free" but it was actually RLock-based. Under heavy concurrent load
with many goroutines doing `orm.Model[T](db)` (which happens on every
fresh query chain), this was a real read-lock contention point — even
though the map is written once per type and never mutated after.

**Fix:** Replaced with `atomic.Pointer[map[reflect.Type]*entry]` +
copy-on-write writes, the same pattern `pkg/cache.Cache` already uses:

```go
type registry struct {
    snapshot atomic.Pointer[map[reflect.Type]*entry]
    writeMu  sync.Mutex
}
```

Reads are now a single atomic pointer load + map read — no mutex at all.
Writes serialize behind `writeMu`, build a new map, and atomically swap
the pointer.

**Expected impact:** Small but real under concurrent load. The map is
written once per model type (finite, program-defined set), so the
copy-on-write cost is negligible; the read-path lock acquisition is gone
entirely.

---

## How to verify

```bash
# Run the existing test suite — all tests should still pass, since no
# public API or behavioral semantics changed.
go test ./...

# Race-clean (the original README claims race-cleanliness; these fixes
# preserve it — atomic.Pointer + copy-on-write is race-safe by design).
go test -race ./pkg/cache/... ./pkg/metadata/... ./pkg/scanner/...

# Re-run the benchmark to measure the impact.
cd benchmark
go test -run NONE -bench . -benchmem -benchtime=2s .
```

## Files changed

```
pkg/scanner/scanner.go       — Fix 1 (ScanOne, ScanRow inline switch) + Fix 4 (ScanOneFast)
pkg/orm/query.go             — Fix 2 (rowValues), Fix 3 (structAssignments), Fix 4 (Find/First dispatch), Fix 5 (compileCached GetOrCompute)
pkg/execution/executor.go    — Fix 7 (Query/Exec fast path)
pkg/metadata/metadata.go     — Fix 9 (atomic.Pointer registry)
```

No new files, no deleted files, no public API changes.

---

# Security Fix Changelog

The following security fixes were applied after a read-only security audit.
Each fix is annotated with severity, root cause, and the performance impact
verification (all fixes are zero-cost or strictly faster on the hot path).

## H1: SQL injection in migrations — validate `versionTable`

**File:** `pkg/migrations/migration.go`
**Severity:** HIGH
**Perf impact:** Zero — validation runs once at `New()`; every method checks
`initErr` (a single nil-check) before doing any work.

The `versionTable` field was interpolated into raw DDL/DML via `fmt.Sprintf`
with no validation — a caller passing `"foo; DROP TABLE users; --"` would
execute that. Now `New()` validates the name against the same alphanumeric +
`_` + `.` allowlist every `dialect.ValidateIdentifier` uses, and every method
returns the stored `initErr` immediately if validation failed.

## H2: Data race in `Metrics` plugin — atomic counters

**File:** `pkg/plugins/builtin.go`
**Severity:** HIGH
**Perf impact:** ~1ns per `Add` — negligible next to any DB round trip.

`Metrics.QueryCount++`, `ErrorCount++`, and `TotalDuration +=` were
non-atomic operations called concurrently from multiple goroutines (every
`Query`/`Exec` fires `AfterExecute`). Changed to `atomic.Int64` fields with
`.Add(1)` — race-clean, negligible cost.

## M1: Regex caching in validation — `sync.Map` cache

**File:** `pkg/validation/validation.go`
**Severity:** MEDIUM
**Perf impact:** Strictly faster — `sync.Map.Load` (~10ns) replaces
`regexp.Compile` (~5-20µs) on every `Validate()` call. 500-2000x speedup.

Added `regexCache sync.Map` that caches compiled regex patterns by string.
`compileRegex(pattern)` returns the cached `*regexp.Regexp` on hit, compiles
+ stores on miss. Thread-safe; no downside.

## M2: Wire up `BeforeExecute`/`AfterExecute` plugins + lifecycle hooks

**Files:** `pkg/orm/query.go`, `pkg/plugins/plugin.go`
**Severity:** MEDIUM (observability gap — Auditing/Tracing silently did nothing)
**Perf impact:** Zero when no plugins are registered (single `len()==0`
check); `time.Now()` only called when plugins are present.

`Chain.RunBeforeExecute`/`RunAfterExecute` were defined but never called.
Now wired into:
- `queryAndPlanBuilder` (Find, First) — plugin hooks around `executor.Query`
- `Count` — plugin hooks around `executor.Query`
- `execWithPlugins` (new helper on `DB`) — wraps `executor.Exec` for
  `Create`, `UpdateAll`, `Delete`, `CreateBatch`

All guarded by `if len(db.plugins) > 0` — when no plugins are registered
(the benchmark default), the only cost is a single slice-length check.

Lifecycle hooks (`RunBeforeCreate`/`RunAfterCreate`) wired into `Create`.
Type assertion (~2ns) returns nil immediately when the model doesn't
implement the hook interface — no perf impact on models without hooks.

## M3: Cache-poisoning guard for request-scoped plugins

**Files:** `pkg/plugins/plugin.go`, `pkg/plugins/builtin.go`, `pkg/orm/query.go`
**Severity:** MEDIUM (cross-tenant data leak when MultiTenancy becomes functional)
**Perf impact:** Zero for empty chain (`len()==0` → returns true immediately);
one type assertion per plugin when chain is non-empty.

Added `RequestScopedPlugin` optional interface + `Chain.IsCacheSafe()`.
`MultiTenancy` implements `IsRequestScoped() bool { return true }`.
`compileCached` now checks `db.plugins.IsCacheSafe()` — if any plugin is
request-scoped, the plan cache is bypassed entirely (every call gets a fresh
compile). This prevents a cached plan baked with one tenant's predicate from
being served to a different tenant.

Cache-safe plugins (Metrics, Tracing, Auditing, SoftDelete) don't implement
`IsRequestScoped` and continue to use the cache normally.

## L6: Align retryable-error classification between executor and transaction

**File:** `pkg/transaction/transaction.go`
**Severity:** LOW (availability — missed retry on SQLite)
**Perf impact:** Zero — one additional string comparison per error.

Added `"database is locked"` to `defaultIsRetryable`'s needle list, matching
`executor.go`'s `isRetryableError`. Previously, a transient SQLite lock
surfaced as a hard transaction failure instead of being retried.
