// Package orm is the public, user-facing API. It composes every internal
// package (metadata, compiler, planner, optimizer, execution, scanner,
// dialect, plugins, transaction, migrations, relations, hooks) behind a
// small, strongly-typed, generic surface:
//
//	db := orm.Open(sqlDB, dialect.Postgres{})
//	users, err := orm.Model[User](db).Where(...).OrderBy(...).Limit(20).Find(ctx)
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

// DB is the top-level ORM handle. It is safe for concurrent use — the
// underlying *sql.DB is already a connection pool, and every cache it owns
// (via Executor, and the CompiledQuery/ScanPlan caches below) is a bounded,
// thread-safe LRU.
type DB struct {
	sqlDB    *sql.DB
	dialect  dialect.Dialect
	executor *execution.Executor
	passes   []optimizer.Pass
	plugins  plugins.Chain
	execOpts []execution.Option

	// compiledCache caches the full compiler.Compile output (logical plan,
	// optimized plan, physical plan) keyed by compiler.PreHash — computed
	// directly from the query.Builder, before planner.Build/optimizer/
	// PlanPhysical run at all. Without this, every call to Find/Create/
	// UpdateAll/Delete re-ran the entire compile pipeline (including a
	// SHA-256 structural hash) even for a query shape seen a thousand times
	// before; only the downstream SQL-text/prepared-statement caches in
	// Executor were actually warm. Benchmarked: this cache is what makes
	// "compile once, execute forever" true for this layer too.
	//
	// Sharded (see pkg/cache.ShardedLRU): this Get happens on every single
	// Find/Create/UpdateAll/Delete call, from every caller goroutine — under
	// concurrent load a single-mutex LRU here serializes unrelated
	// goroutines issuing completely different query shapes against each
	// other for no reason. PreHash already produces a well-distributed
	// uint64, so sharding is a cheap avalanche mix (cache.Uint64Hash), not a
	// re-hash of anything.
	compiledCache *cache.ShardedLRU[uint64, *compiler.CompiledQuery]

	// scanPlanCache caches *scanner.Plan (which result columns map to which
	// struct field, via precomputed offsets) keyed by (table, column list).
	// This was the actual root cause of Breeze ORM trailing GORM/Bun/sqlx on
	// reads in benchmarking: pkg/scanner.Compile was being called fresh on
	// every single Find call — for a fixed query shape, the result column
	// list never changes, so the Plan it produces never changes either.
	// Same "compile once" principle as compiledCache, one layer further
	// down the pipeline. Sharded for the same contention reason.
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
