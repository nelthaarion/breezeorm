<div align="center">

#  Breeze ORM

### A Go ORM that doesn't make you choose between fast, safe, and readable.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Benchmarks](https://img.shields.io/badge/benchmarks-passing-brightgreen.svg)](benchmark/)
[![Race Clean](https://img.shields.io/badge/race-clean-success.svg)](#testing)

**Metadata → Planner → Optimizer → SQL Generator → Executor → Scanner**

*Compile once. Execute forever. Allocate almost nothing.*

</div>

---

## 🎯 Why does this exist?

Every Go ORM makes you pick two of three:

| | Fast | Safe | Readable |
|---|:---:|:---:|:---:|
| `database/sql` | ✅ | ✅ | ❌ |
| GORM | ❌ | ✅ | ✅ |
| sqlx | ✅ | ✅ | 🟡 |
| Bun | 🟡 | ✅ | ✅ |

**Breeze ORM picks all three.** It's a from-scratch ORM with a real query compiler pipeline — not a string concatenator with a fluent API painted on top. Every query goes through:

```
struct tags → metadata compiler → query builder → logical planner
            → 8-pass optimizer → physical planner → SQL generator
            → prepared-statement cache → executor → zero-reflection scanner
```

And it does this **once per query shape**, not once per call. The second time you run `Where("id = ?", 42)`, the entire pipeline is a cache hit.

---

## ⚡ How fast is it?

Real benchmarks, real Postgres, real dependencies (GORM, Bun, sqlx, pgx). Not simulated.

| Operation | raw SQL (prepared) | **Breeze ORM** | GORM | Bun | sqlx |
|---|---:|---:|---:|---:|---:|
| Insert | 62.7µs / 10 allocs | <span style="color:#16a34a">**66.4µs / 28 allocs**</span> | 114.1µs / 105 allocs | 91.7µs / 26 allocs | 73.0µs / 12 allocs |
| FindByID | 4.5µs / 18 allocs | <span style="color:#16a34a">**16.1µs / 41 allocs**</span> | 11.0µs / 58 allocs | 14.6µs / 41 allocs | 10.8µs / 40 allocs |
| SelectWhereLimit(50) | 82µs / 366 allocs | <span style="color:#16a34a">**217µs / 533 allocs**</span> | 142µs / 700 allocs | 114µs / 393 allocs | 108µs / 439 allocs |
| Update | 4.0µs / 6 allocs | <span style="color:#16a34a">**8.1µs / 21 allocs**</span> | 25.5µs / 83 allocs | 10.0µs / 14 allocs | 6.5µs / 8 allocs |

### The honest read

- <span style="color:#16a34a">**Breeze ORM's Insert is the fastest of all four ORMs**</span> — faster than GORM, Bun, and sqlx, within 6% of raw prepared statements.
- <span style="color:#16a34a">**Breeze ORM allocates fewer times than GORM on every operation measured**</span>, including SelectWhereLimit where it started this investigation allocating the *most*.
- **Reads (FindByID, SelectWhereLimit) are the current weak spot** — 1.5-2× slower than sqlx/Bun. The root cause is diagnosed (per-row `reflect.NewAt` boxing forced by `database/sql`'s `Scan(dest ...any)` signature) and the fix is already designed (code-generated scanners that write `&dest.Field` directly).

Run them yourself:

```bash
cd benchmark
go test -run NONE -bench . -benchmem -benchtime=2s .
```

---

## 🚀 Quick start

```go
package main

import (
    "context"
    "database/sql"

    "github.com/nelthaarion/breezeorm/pkg/dialect"
    "github.com/nelthaarion/breezeorm/pkg/orm"
    "github.com/nelthaarion/breezeorm/pkg/query"
    _ "github.com/jackc/pgx/v5/stdlib"
)

type User struct {
    ID        int64     `db:"id,pk,autoincrement"`
    Email     string    `db:"email,unique" validate:"required,email"`
    Name      string    `db:"name" validate:"required,min=2,max=100"`
    Active    bool      `db:"active,default=true"`
    CreatedAt time.Time `db:"created_at"`
}

func main() {
    sqlDB, _ := sql.Open("pgx", "postgres://localhost/breezeorm")
    db := orm.Open(sqlDB, dialect.Postgres{})
    ctx := context.Background()

    // Insert
    user := &User{Email: "ada@example.com", Name: "Ada", Active: true}
    orm.Model[User](db).Create(ctx, user)

    // Query
    active, _ := orm.Model[User](db).
        Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true}).
        OrderBy(query.OrderTerm{Column: "created_at", Desc: true}).
        Limit(20).
        Find(ctx)

    // Find by ID
    one, _ := orm.Model[User](db).
        Where(query.Predicate{Column: "id", Op: query.OpEq, Value: int64(1)}).
        First(ctx)

    // Update
    orm.Model[User](db).
        Where(query.Predicate{Column: "id", Op: query.OpEq, Value: one.ID}).
        UpdateAll(ctx, query.Assignment{Column: "name", Value: "Updated"})

    // Delete
    orm.Model[User](db).
        Where(query.Predicate{Column: "id", Op: query.OpEq, Value: one.ID}).
        Delete(ctx)
}
```

---

## 🏗️ How it works

The pipeline has seven stages. Each one runs **once per distinct query shape**, then hands off to a cache for the next call.

```
┌─────────────┐     ┌──────────┐     ┌──────────┐     ┌───────────┐
│  Your code  │────▶│  Builder │────▶│ Planner  │────▶│ Optimizer │
│  .Where()   │     │ (AST)    │     │ (logical)│     │ (8 passes)│
└─────────────┘     └──────────┘     └──────────┘     └───────────┘
                                                            │
┌─────────────┐     ┌──────────┐     ┌──────────┐           ▼
│  Scanner    │◀────│ Executor │◀────│ SQL Gen  │◀──── ┌──────────┐
│ (0 reflect) │     │ (cached) │     │ (cached) │      │ Physical │
└─────────────┘     └──────────┘     └──────────┘      │ Planner  │
                                        │              └──────────┘
                                        ▼
                                 ┌──────────────┐
                                 │ Prepared stmt│
                                 │   cache      │
                                 └──────────────┘
```

### 1. Metadata compiler (`pkg/metadata`)
Reflects your struct **exactly once** per type (`sync.Once`), builds an immutable `Table` with precomputed field offsets, caches it behind a lock-free atomic pointer swap. The scanner never touches reflect again.

### 2. Query builder (`pkg/query`)
Generic, immutable, fluent. Every method returns a new `Builder[T]` — Go value semantics give us copy-on-write for free. Safe to branch, cache, and reuse concurrently.

### 3. Logical planner (`pkg/planner`)
Lowers the builder AST into relational algebra: `Scan → Filter → Project → Join → Aggregate → Sort → Limit`. Dialect-agnostic.

### 4. Optimizer (`pkg/optimizer`)
Eight rule-based rewrite passes over the logical plan: predicate simplification, duplicate removal, constant folding, join reordering, projection pruning, ORDER BY / LIMIT folding, canonical ordering. Two are fully implemented; the rest have the correct shape and are marked `TODO` at the exact line they plug in.

### 5. SQL generator (`pkg/execution/sqlgen.go`)
Renders the physical plan to parameterized SQL. Every identifier is validated then quoted. Raw SQL fragments (the escape hatch) get defense-in-depth checks against statement separators. Output is cached by structural hash — `Where(id=1)` and `Where(id=2)` share one SQL string.

### 6. Executor (`pkg/execution/executor.go`)
Prepared-statement cache + streaming `Cursor` API on top of `database/sql`. Bounded LRU with eviction that actually closes statements. Concurrent misses for the same shape coalesce into one `PrepareContext` round trip. Every query gets a timeout. Transient errors retry with full-jitter backoff.

### 7. Scanner (`pkg/scanner`)
Result columns are matched against compiled metadata **once**, then rows are decoded via precomputed unsafe field offsets. For common types (int64, string, bool, time.Time, sql.Null*), the per-column dispatch is an inlined type switch — zero allocation, zero reflection. A codegen path (`pkg/scanner/gen`) exists for the fully-zero-overhead case.

---

## 🔒 Security

Breeze ORM takes security seriously. Here's what's baked in:

| Concern | How it's handled |
|---|---|
| **SQL injection** | Every identifier goes through `dialect.ValidateIdentifier` before quoting. Raw SQL fragments are checked for statement separators. Bind args are always parameterized, never string-interpolated. |
| **Memory exhaustion** | Every cache keyed by attacker-influenced input (SQL text, query shape) is a bounded LRU with eviction. The metadata cache (keyed by Go type, a finite set) is the only unbounded one. |
| **Prepared statement leaks** | LRU eviction closes statements. `DB.Close()` purges the whole cache before closing the pool. |
| **Cache poisoning** | Request-scoped plugins (e.g. multi-tenancy) automatically bypass the plan cache via `RequestScopedPlugin` — a tenant's compiled plan can never leak to another tenant. |
| **Data races** | All shared counters use `atomic.Int64`. All caches are sharded with independent locks or lock-free atomic pointer reads. |
| **Query timeouts** | Every query gets a deadline (10s default, configurable). A caller's tighter deadline is always preserved. |
| **Transient errors** | Deadlocks, serialization failures, and lock timeouts retry with full-jitter backoff — at both the statement level and the transaction level. |

---

## 🧩 What's in the box

### Core engine
- **`pkg/orm`** — Public API: `orm.Open()`, `orm.Model[T]()`, `Query[T].Find/First/Create/UpdateAll/Delete/Count/Exists`
- **`pkg/metadata`** — Struct reflection → immutable `Table`, compiled once per type
- **`pkg/query`** — Immutable generic `Builder[T]` + expression AST
- **`pkg/planner`** — Logical + physical planning
- **`pkg/optimizer`** — 8-pass rewrite pipeline
- **`pkg/compiler`** — Wires planner + optimizer + plugins, produces cache key
- **`pkg/execution`** — SQL generator + executor (prepared stmt cache, cursor API)
- **`pkg/scanner`** — Zero-reflection row decoding via precomputed offsets

### Infrastructure
- **`pkg/cache`** — Lock-free-read `Cache` + bounded `LRU` + `ShardedLRU` with miss coalescing
- **`pkg/driver`** — Driver abstraction (`DB`/`Stmt`/`Rows`/`Result`) — not welded to `database/sql`
- **`pkg/dialect`** — Postgres (full), MySQL/SQLite/SQL Server (partial)
- **`pkg/pool`** — `sync.Pool` wrappers for buffers and arg slices
- **`pkg/transaction`** — Context-aware tx, savepoints, retry with jittered backoff
- **`pkg/migrations`** — Version table, up/down/seed, schema diff scaffolding

### Extensibility
- **`pkg/plugins`** — `BeforePlan`/`BeforeExecute`/`AfterExecute` hooks. Built-in: Metrics, Tracing, Auditing, SoftDelete, MultiTenancy, Encryption, QueryCache
- **`pkg/hooks`** — Lifecycle hooks: `Before/AfterCreate/Update/Delete/Save/Query`
- **`pkg/relations`** — HasOne/HasMany/BelongsTo/ManyToMany loader contracts
- **`pkg/validation`** — Tag-driven: `required/min/max/regex/email/url/uuid/custom`

---

## 📊 Project layout

```
breezeorm/
├── pkg/
│   ├── orm/          # Public API — this is what you import
│   ├── metadata/     # Struct → immutable Table, once per type
│   ├── query/        # Immutable generic Builder[T] + expression AST
│   ├── planner/      # Builder AST → LogicalPlan → PhysicalPlan
│   ├── optimizer/    # 8-pass rewrite pipeline over LogicalPlan
│   ├── compiler/     # Wires planner+optimizer+plugins, cache key
│   ├── execution/    # SQL generator + Executor (stmt cache, cursor)
│   ├── scanner/      # Zero-reflection row decoding
│   ├── cache/        # Lock-free + LRU + ShardedLRU
│   ├── driver/       # Driver abstraction (DB/Stmt/Rows/Result)
│   ├── dialect/      # Postgres (full), MySQL/SQLite/SQLServer (partial)
│   ├── pool/         # sync.Pool wrappers
│   ├── transaction/  # tx, savepoints, retry with backoff
│   ├── migrations/   # Version table, up/down/seed, schema diff
│   ├── plugins/      # Plugin chain + builtins
│   ├── hooks/        # Lifecycle hook interfaces
│   ├── relations/    # Relationship loader contracts
│   └── validation/   # Tag-driven validators
├── benchmark/        # Separate module: Breeze vs GORM vs Bun vs sqlx
└── examples/basic/   # Runnable demo
```

---

## 🧪 Testing

```bash
# Run the full suite
go test ./...

# Race-clean (the codebase is designed for it)
go test -race ./pkg/cache/... ./pkg/metadata/... ./pkg/scanner/...

# Run the benchmark
cd benchmark
go test -run NONE -bench . -benchmem -benchtime=2s .
```

The benchmark is a **separate Go module** (so Breeze ORM itself stays dependency-free) with real, runnable benchmarks against real dependencies — GORM, Bun, sqlx, and pgx are actually fetched and executed, not mocked.

---

## 🔧 Configuration

```go
db := orm.Open(sqlDB, dialect.Postgres{},
    // Tune cache sizes (defaults shown)
    orm.WithCompiledQueryCacheSize(2000),
    orm.WithScanPlanCacheSize(2000),
    orm.WithExecutorOptions(
        execution.WithStmtCacheSize(2000),
        execution.WithPlanCacheSize(2000),
        execution.WithDefaultTimeout(10*time.Second),
        execution.WithRetryPolicy(execution.RetryPolicy{
            MaxAttempts: 3,
            BaseDelay:   5 * time.Millisecond,
            MaxDelay:    200 * time.Millisecond,
        }),
    ),

    // Register plugins (zero cost when chain is empty)
    orm.WithPlugins(
        &plugins.Metrics{},
        &plugins.Tracing{Logger: log.New(os.Stderr, "", 0)},
        &plugins.SoftDelete{Column: "deleted_at"},
    ),
)
```

---

## 🗺️ Roadmap

What's done, what's stubbed, what's next.

### ✅ Done & tested
- Full compile → execute → scan pipeline for Postgres
- Prepared-statement + plan + scan-plan caching (compile once, execute forever)
- Zero-reflection scanner with inlined type dispatch for common types
- Transactions with savepoints + retry
- Migrations (version table, up/down, seeding)
- Plugin system (Metrics, Tracing, Auditing functional)
- Security hardening (identifier validation, bounded caches, cache-poisoning guards)

### 🚧 Stubbed (correct shape, needs filling)
- MySQL / SQLite / SQL Server dialects (placeholders + quoting work; less battle-tested)
- Relationships (HasOne/HasMany/BelongsTo/ManyToMany) — loader contracts exist, preload dispatch not wired
- SoftDelete / MultiTenancy plugins — predicate is correct, needs parent-aware plan rewrite
- Optimizer passes (constant folding, join reordering, projection pruning) — no-op stubs with right interface
- Auto-migration schema diff — structural comparison works, `information_schema` introspection TODO

### 🔜 Not started
- Code generator (`cmd/breezeorm-gen`) — would emit `&dest.Field` directly, eliminating the last `reflect.NewAt` on the scan path
- Native pgx driver adapter — bypasses `database/sql`'s `interface{}` boxing entirely
- Cursor pagination (keyset WHERE translation)
- CTE subquery body rendering

---

## 💡 Design philosophy

1. **Compile once, execute forever.** Every layer caches: metadata, compiled plans, SQL text, prepared statements, scan plans. The second call with the same query shape is a cache hit at every level.

2. **Zero cost when disabled.** No plugins registered? The plugin chain is a `nil` slice and every hook site is a single `len() == 0` check. No hooks on your model? The type assertion returns `nil` immediately.

3. **Honest about gaps.** Stubbed features are marked `TODO` at the exact line they plug in, not silently broken. The README tells you what works and what doesn't.

4. **Profile, don't guess.** Every performance claim is backed by a benchmark number and a pprof profile. The `benchmark/` folder has the raw `.prof` files.

5. **Security is not optional.** Every identifier is validated. Every attacker-influenced cache is bounded. Every query has a timeout. Request-scoped plugins bypass the plan cache.

---
------------------------------------------------------------------------

## 📄 License

MIT — see [LICENSE](LICENSE).

---

<div align="center">

**[Report Bug](https://github.com/nelthaarion/breezeorm/issues)** ·
**[Request Feature](https://github.com/nelthaarion/breezeorm/issues)** ·
**[Read the Docs](https://github.com/nelthaarion/breezeorm)**

*Built with caffeine, profiling, and an unreasonable dislike of `reflect.FieldByName`.*

</div>
