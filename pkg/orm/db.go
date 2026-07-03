// Package orm is the public, user-facing API. It composes every internal
// package (metadata, compiler, planner, optimizer, execution, scanner,
// dialect, plugins, transaction, migrations, relations, hooks) behind a
// small, strongly-typed, generic surface:
//
//      db := orm.Open(sqlDB, dialect.Postgres{})
//      users, err := orm.Model[User](db).Where(...).OrderBy(...).Limit(20).Find(ctx)
package orm

import (
        "context"
        "database/sql"
        "fmt"

        "github.com/nelthaarion/breezeorm/pkg/cache"
        "github.com/nelthaarion/breezeorm/pkg/compiler"
        "github.com/nelthaarion/breezeorm/pkg/dialect"
        "github.com/nelthaarion/breezeorm/pkg/execution"
        "github.com/nelthaarion/breezeorm/pkg/optimizer"
        "github.com/nelthaarion/breezeorm/pkg/plugins"
        "github.com/nelthaarion/breezeorm/pkg/scanner"
)

// DefaultCompiledQueryCacheSize bounds DB.compiledCache. See the CAVEAT on
// compiler.PreHash before making this cache request/context-sensitive.
const DefaultCompiledQueryCacheSize = 2000

// DefaultScanPlanCacheSize bounds DB.scanPlanCache.
const DefaultScanPlanCacheSize = 2000

// DB is the top-level handle you get from Open(). It's safe for concurrent
// use — *sql.DB is already a connection pool, and every cache attached here
// is a bounded, thread-safe LRU.
type DB struct {
        sqlDB    *sql.DB
        dialect  dialect.Dialect
        executor *execution.Executor
        passes   []optimizer.Pass
        plugins  plugins.Chain
        execOpts []execution.Option

        // compiledCache holds the full output of compiler.Compile (the logical
        // plan, the optimized plan, and the physical plan) keyed by PreHash.
        // PreHash is computed straight from the builder, before the planner or
        // optimizer even run.
        //
        // Why this matters: without it, every Find/Create/Update/Delete call
        // re-ran the entire compile pipeline even for a query shape seen a
        // thousand times. Only the SQL-text and prepared-statement caches
        // downstream were warm. This is what makes "compile once, execute
        // forever" actually true at this layer.
        //
        // It's sharded (see pkg/cache.ShardedLRU) because this Get happens on
        // every single call from every goroutine. Under concurrent load, a
        // single-mutex LRU here would serialize unrelated goroutines issuing
        // completely different query shapes against each other for no reason.
        // PreHash is already a well-distributed uint64, so shard selection is
        // just a cheap avalanche mix — no re-hashing of query text.
        compiledCache *cache.ShardedLRU[uint64, *compiler.CompiledQuery]

        // scanPlanCache holds *scanner.Plan — the mapping of result columns to
        // struct fields via precomputed offsets — keyed by (table, column list).
        //
        // This was the actual root cause of Breeze ORM trailing GORM/Bun/sqlx
        // on reads in the original benchmark: scanner.Compile was called fresh
        // on every single Find. But for a fixed query shape, the result column
        // list never changes, so the Plan it produces never changes either.
        // Same "compile once" principle, one layer down the pipeline. Sharded
        // for the same contention reason.
        scanPlanCache *cache.ShardedLRU[uint64, *scanner.Plan]
}

// Option configures a DB at Open time.
type Option func(*DB)

// WithPlugins registers plugins, run in the given order for every query.
func WithPlugins(pl ...plugins.Plugin) Option {
        return func(db *DB) { db.plugins = append(db.plugins, pl...) }
}

// WithOptimizerPasses overrides the default optimizer pipeline.
func WithOptimizerPasses(passes ...optimizer.Pass) Option {
        return func(db *DB) { db.passes = passes }
}

// WithExecutorOptions passes through low-level Executor tuning: bounded
// cache sizes (execution.WithStmtCacheSize / WithPlanCacheSize), the default
// per-query timeout (execution.WithDefaultTimeout), and retry policy
// (execution.WithRetryPolicy). Production deployments under adversarial or
// bursty load should size these deliberately rather than accept defaults
// blindly — e.g. a service with thousands of distinct dynamically-generated
// query shapes may want a larger stmt cache; a latency-sensitive service may
// want a tighter default timeout than the library default.
func WithExecutorOptions(opts ...execution.Option) Option {
        return func(db *DB) { db.execOpts = append(db.execOpts, opts...) }
}

// WithCompiledQueryCacheSize overrides the bound on the CompiledQuery cache
// (default DefaultCompiledQueryCacheSize). Size for the number of distinct
// query *shapes* your application issues, not the number of requests.
func WithCompiledQueryCacheSize(n int) Option {
        return func(db *DB) {
                db.compiledCache = cache.NewShardedLRU[uint64, *compiler.CompiledQuery](n, execution.DefaultCacheShards, cache.Uint64Hash, nil)
        }
}

// WithScanPlanCacheSize overrides the bound on the scan-plan cache (default
// DefaultScanPlanCacheSize).
func WithScanPlanCacheSize(n int) Option {
        return func(db *DB) {
                db.scanPlanCache = cache.NewShardedLRU[uint64, *scanner.Plan](n, execution.DefaultCacheShards, cache.Uint64Hash, nil)
        }
}

// Open wraps an already-configured *sql.DB (the caller chooses and imports
// the driver — this package intentionally has zero database driver
// dependencies) with the given dialect.
func Open(sqlDB *sql.DB, d dialect.Dialect, opts ...Option) *DB {
        db := &DB{
                sqlDB:   sqlDB,
                dialect: d,
                passes:  optimizer.DefaultPipeline(),
                compiledCache: cache.NewShardedLRU[uint64, *compiler.CompiledQuery](
                        DefaultCompiledQueryCacheSize, execution.DefaultCacheShards, cache.Uint64Hash, nil),
                scanPlanCache: cache.NewShardedLRU[uint64, *scanner.Plan](
                        DefaultScanPlanCacheSize, execution.DefaultCacheShards, cache.Uint64Hash, nil),
        }
        for _, o := range opts {
                o(db)
        }
        db.executor = execution.New(sqlDB, d, db.execOpts...)
        return db
}

// Ping verifies the underlying connection is reachable.
func (db *DB) Ping(ctx context.Context) error {
        if err := db.sqlDB.PingContext(ctx); err != nil {
                return fmt.Errorf("orm: ping: %w", err)
        }
        return nil
}

// SQLDB exposes the underlying *sql.DB for cases the ORM doesn't cover yet
// (raw SQL escape hatch, driver-specific tuning).
func (db *DB) SQLDB() *sql.DB { return db.sqlDB }

// Dialect returns the configured dialect.
func (db *DB) Dialect() dialect.Dialect { return db.dialect }

// Close purges the prepared-statement cache (explicitly closing every cached
// *sql.Stmt) and then closes the underlying connection pool. Purging first —
// rather than relying on sql.DB.Close() to implicitly invalidate statements —
// ensures statement-close errors aren't silently swallowed by pool teardown
// and that server-side prepared-statement handles are released deterministically.
func (db *DB) Close() error {
        _ = db.executor.Close()
        return db.sqlDB.Close()
}

// Stats exposes plan/statement cache hit-rate counters.
func (db *DB) Stats() (planHits, planMisses, stmtHits, stmtMisses int64) {
        return db.executor.Stats()
}
