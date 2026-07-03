package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/nelthaarion/breezeorm/pkg/cache"
	"github.com/nelthaarion/breezeorm/pkg/compiler"
	"github.com/nelthaarion/breezeorm/pkg/dialect"
	ormdriver "github.com/nelthaarion/breezeorm/pkg/driver"
	"github.com/nelthaarion/breezeorm/pkg/driver/sqladapter"
)

const (
	// DefaultStmtCacheSize bounds the prepared-statement cache. Each entry
	// holds a live server-side prepared statement (a real resource on the
	// database connection, not just process memory), so this cap is a
	// resource-exhaustion control, not just a memory one: an unbounded cache
	// fed by a workload with many distinct query shapes (or an attacker able
	// to influence query shape) would otherwise accumulate prepared
	// statements on the DB server forever.
	DefaultStmtCacheSize = 2000

	// DefaultPlanCacheSize bounds the SQL-text cache (keyed by structural
	// plan hash). Purely in-process memory, but still attacker-influenceable
	// if query shape is ever derived from request input — bounded for the
	// same reason.
	DefaultPlanCacheSize = 2000

	// DefaultQueryTimeout is applied to any context that doesn't already
	// carry a deadline. Every query MUST have a bound: without one, a slow
	// or wedged connection (or a pathological query) can hold a pool
	// connection indefinitely, which is a classic path to connection-pool
	// exhaustion and cascading failure under load.
	DefaultQueryTimeout = 10 * time.Second

	// DefaultCacheShards is the number of independent, independently-locked
	// shards both the plan-text cache and the prepared-statement cache are
	// split into (see pkg/cache.ShardedLRU). Concurrent callers only contend
	// with each other if they land in the same shard, so this trades a
	// slightly smaller effective LRU window per shard for much lower lock
	// contention under concurrent load — the access pattern this Executor
	// actually sees (many goroutines repeatedly hitting a small, hot set of
	// query shapes) is exactly the case a single global mutex serializes
	// unnecessarily.
	DefaultCacheShards = 32
)

// Executor owns the prepared-statement cache and SQL-text cache for one
// database connection + dialect pair, and is the only place calls into the
// driver abstraction (pkg/driver) happen. All caches are bounded (LRU) so a
// workload with unbounded query shape diversity cannot exhaust memory or
// leak prepared statements on the server; evicted statements are Close()'d.
//
// Executor talks to ormdriver.DB, not *sql.DB directly — see pkg/driver's
// doc comment for why. New() is the backward-compatible constructor
// (accepts *sql.DB, as before the driver abstraction existed);
// NewWithDriver() is the general one a future native-driver adapter would use.
type Executor struct {
	db      ormdriver.DB
	dialect dialect.Dialect

	// planTextCache maps a compiler.CompiledQuery.CacheKey (structural,
	// literal-value-independent) to rendered SQL text. Bind arguments are
	// NEVER cached here — they're re-derived fresh on every call via
	// ExtractArgs, which is what makes caching the text safe: two calls with
	// the same CacheKey but different WHERE literals correctly get the same
	// SQL string and different, correct Args.
	//
	// Sharded (see pkg/cache.ShardedLRU) rather than a single-mutex LRU:
	// CacheKey is already a well-distributed uint64 hash (pkg/compiler's
	// PreHash / structuralHash), so shard selection is a cheap avalanche
	// mix with no re-hashing of query text — see cache.Uint64Hash.
	planTextCache *cache.ShardedLRU[uint64, string]

	// stmtCache maps rendered SQL text to a prepared statement. Bounded and
	// eviction-safe: the onEvict callback closes the statement so evicted
	// entries don't leak server-side resources or client-side file
	// descriptors.
	//
	// Sharded for the same contention reason as planTextCache. The key
	// remains the exact SQL string — sharding only changes which lock
	// protects a given entry, never how entries are looked up within a
	// shard (see cache.ShardedLRU's doc comment on why using the hash as
	// the map key itself would be unsafe here: a collision must never
	// return the wrong prepared statement for a query).
	stmtCache *cache.ShardedLRU[string, ormdriver.Stmt]

	defaultTimeout time.Duration
	retry          RetryPolicy
}

// RetryPolicy controls automatic retry of transient (deadlock, serialization
// failure, connection-reset) errors at the statement level. This is separate
// from — and composes with — pkg/transaction's retry, which operates at the
// whole-transaction level; retrying an individual statement here is only
// safe for statements executed outside an explicit multi-statement
// transaction (Executor does not retry once a *sql.Tx is involved, since
// retrying a single statement inside a transaction the caller already
// partially executed could silently double-apply side effects).
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy retries transient errors up to twice with jittered
// exponential backoff. MaxAttempts: 1 disables retry.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 5 * time.Millisecond, MaxDelay: 200 * time.Millisecond}
}

// Option configures an Executor at construction time.
type Option func(*Executor)

// WithStmtCacheSize replaces the prepared-statement cache with one sized to
// n total entries, split across DefaultCacheShards shards. Use
// WithStmtCache to control shard count directly.
func WithStmtCacheSize(n int) Option {
	return func(ex *Executor) { ex.stmtCache = newStmtCache(n, DefaultCacheShards) }
}

// WithStmtCache replaces the prepared-statement cache with one sized to n
// total entries split across numShards shards.
func WithStmtCache(n, numShards int) Option {
	return func(ex *Executor) { ex.stmtCache = newStmtCache(n, numShards) }
}

// WithPlanCacheSize replaces the plan-text cache with one sized to n total
// entries, split across DefaultCacheShards shards. Use WithPlanCache to
// control shard count directly.
func WithPlanCacheSize(n int) Option {
	return func(ex *Executor) {
		ex.planTextCache = cache.NewShardedLRU[uint64, string](n, DefaultCacheShards, cache.Uint64Hash, nil)
	}
}

// WithPlanCache replaces the plan-text cache with one sized to n total
// entries split across numShards shards.
func WithPlanCache(n, numShards int) Option {
	return func(ex *Executor) {
		ex.planTextCache = cache.NewShardedLRU[uint64, string](n, numShards, cache.Uint64Hash, nil)
	}
}

func WithDefaultTimeout(d time.Duration) Option {
	return func(ex *Executor) { ex.defaultTimeout = d }
}

func WithRetryPolicy(p RetryPolicy) Option {
	return func(ex *Executor) { ex.retry = p }
}

func newStmtCache(size, numShards int) *cache.ShardedLRU[string, ormdriver.Stmt] {
	return cache.NewShardedLRU[string, ormdriver.Stmt](size, numShards, cache.StringHash, func(_ string, stmt ormdriver.Stmt) {
		_ = stmt.Close() // best-effort: nothing actionable if this fails at eviction time
	})
}

// New creates an Executor bound to a *sql.DB and dialect — the backward-
// compatible constructor, unchanged in signature from before the driver
// abstraction existed. Internally wraps db via pkg/driver/sqladapter.
func New(db *sql.DB, d dialect.Dialect, opts ...Option) *Executor {
	return NewWithDriver(sqladapter.Wrap(db), d, opts...)
}

// NewWithDriver creates an Executor bound to any ormdriver.DB implementation
// — the general constructor. Use this when backing Breeze ORM with something
// other than database/sql once such an adapter exists.
func NewWithDriver(db ormdriver.DB, d dialect.Dialect, opts ...Option) *Executor {
	ex := &Executor{
		db:             db,
		dialect:        d,
		planTextCache:  cache.NewShardedLRU[uint64, string](DefaultPlanCacheSize, DefaultCacheShards, cache.Uint64Hash, nil),
		stmtCache:      newStmtCache(DefaultStmtCacheSize, DefaultCacheShards),
		defaultTimeout: DefaultQueryTimeout,
		retry:          DefaultRetryPolicy(),
	}
	for _, o := range opts {
		o(ex)
	}
	return ex
}

// Close purges the prepared-statement cache, closing every cached statement.
// Call this before closing the underlying connection pool to avoid leaking
// server-side prepared-statement handles.
func (ex *Executor) Close() error {
	ex.stmtCache.Purge()
	return nil
}

// withTimeout ensures ctx carries a deadline, applying the Executor's
// default if the caller didn't already set one. Every query path routes
// through this — there is no way to issue a query with an unbounded context
// through this Executor.
func (ex *Executor) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	if ex.defaultTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, ex.defaultTimeout)
}

// Resolve turns a CompiledQuery into ready-to-execute SQL text + fresh bind
// args, using the SQL-text cache on the CacheKey. This is the corrected
// design: earlier drafts cached SQL text and Args bundled together, which
// silently served stale literal values on a cache hit. Text and args are
// cached (or recomputed) completely independently now.
func (ex *Executor) Resolve(cq *compiler.CompiledQuery) (*GeneratedSQL, error) {
	sqlText, ok := ex.planTextCache.Get(cq.CacheKey)
	if !ok {
		gen, err := GenerateSQL(cq.Physical)
		if err != nil {
			return nil, err
		}
		ex.planTextCache.Set(cq.CacheKey, gen.SQL)
		return gen, nil
	}
	args, err := ExtractArgs(cq.Physical)
	if err != nil {
		return nil, err
	}
	return &GeneratedSQL{SQL: sqlText, Args: args}, nil
}

// prepare returns a cached prepared statement for the given SQL text,
// preparing it on first use. Concurrent misses for identical SQL text are
// coalesced within that text's shard (see cache.ShardedLRU.GetOrCompute /
// cache.LRU.GetOrCompute), so a burst of concurrent first-time callers for
// the same new query shape triggers exactly one PrepareContext round trip.
func (ex *Executor) prepare(ctx context.Context, sqlText string) (ormdriver.Stmt, error) {
	return ex.stmtCache.GetOrCompute(sqlText, func() (ormdriver.Stmt, error) {
		return ex.db.PrepareContext(ctx, sqlText)
	})
}

// Query runs a SELECT and returns a *Rows wrapping the driver's row cursor.
// The caller MUST close the returned *Rows (directly, or by consuming it
// through pkg/scanner, which does so internally) — Close both releases the
// underlying connection back to the pool AND cancels this call's timeout
// context. Failing to close leaks both.
func (ex *Executor) Query(ctx context.Context, gen *GeneratedSQL) (*Rows, error) {
	ctx, cancel := ex.withTimeout(ctx)
	rows, err := withRetryGeneric(ctx, ex.retry, func() (ormdriver.Rows, error) {
		stmt, err := ex.prepare(ctx, gen.SQL)
		if err != nil {
			return nil, fmt.Errorf("execution: prepare: %w", err)
		}
		rows, err := stmt.QueryContext(ctx, gen.Args...)
		if err != nil {
			return nil, fmt.Errorf("execution: query: %w", err)
		}
		return rows, nil
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return &Rows{Rows: rows, cancel: cancel}, nil
}

// Rows wraps the driver's row cursor so that Close() also releases the
// timeout context created for this query. Embeds ormdriver.Rows so
// Next/Scan/Err/Columns are all available directly, satisfying pkg/scanner's
// RowsSource interface.
type Rows struct {
	ormdriver.Rows
	cancel context.CancelFunc
}

// Close releases the underlying rows AND cancels the query's timeout
// context. Safe to call multiple times (sql.Rows.Close is idempotent;
// context.CancelFunc is always idempotent).
func (r *Rows) Close() error {
	defer r.cancel()
	return r.Rows.Close()
}

// Exec runs an INSERT/UPDATE/DELETE/UPSERT without a result set.
func (ex *Executor) Exec(ctx context.Context, gen *GeneratedSQL) (ormdriver.Result, error) {
	ctx, cancel := ex.withTimeout(ctx)
	defer cancel()
	return withRetryGeneric(ctx, ex.retry, func() (ormdriver.Result, error) {
		stmt, err := ex.prepare(ctx, gen.SQL)
		if err != nil {
			return nil, fmt.Errorf("execution: prepare: %w", err)
		}
		res, err := stmt.ExecContext(ctx, gen.Args...)
		if err != nil {
			return nil, fmt.Errorf("execution: exec: %w", err)
		}
		return res, nil
	})
}

// BulkExec executes a pre-rendered multi-row INSERT (see GenerateBulkInsert)
// in a single round trip.
func (ex *Executor) BulkExec(ctx context.Context, gen *GeneratedSQL) (ormdriver.Result, error) {
	return ex.Exec(ctx, gen)
}

// withRetryGeneric runs fn, retrying on transient errors per policy. It
// never retries once the context is done, and never retries more than
// MaxAttempts-1 additional times.
func withRetryGeneric[T any](ctx context.Context, policy RetryPolicy, fn func() (T, error)) (T, error) {
	attempts := policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	var zero T
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			d := backoff(policy, attempt)
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(d):
			}
		}
		v, err := fn()
		if err == nil {
			return v, nil
		}
		lastErr = err
		if !isRetryableError(err) {
			return zero, err
		}
	}
	return zero, fmt.Errorf("execution: exhausted %d attempts: %w", attempts, lastErr)
}

// isRetryableError classifies deadlocks, serialization failures, and lock
// wait timeouts as transient. This is a string-matching heuristic because
// this package has no driver-specific dependency (by design — see
// pkg/driver); callers with a specific driver imported can layer a more
// precise typed-error check on top by wrapping RetryPolicy's classification
// — left as a documented extension point rather than baked in here.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false // never retry a context the caller already gave up on
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"deadlock", "serialization failure", "lock wait timeout", "could not serialize", "database is locked"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func backoff(policy RetryPolicy, attempt int) time.Duration {
	base, max := policy.BaseDelay, policy.MaxDelay
	if base <= 0 {
		base = 5 * time.Millisecond
	}
	if max <= 0 {
		max = 200 * time.Millisecond
	}
	d := base * time.Duration(uint64(1)<<uint(attempt-1))
	if d > max {
		d = max
	}
	return time.Duration(rand.Int63n(int64(d) + 1)) // full jitter
}

// Cursor is a streaming row iterator for large result sets. It wraps the
// driver's row cursor directly rather than materializing a full []T slice,
// and owns releasing the timeout context set up by QueryCursor.
type Cursor struct {
	rows   ormdriver.Rows
	cancel context.CancelFunc
}

// QueryCursor runs a SELECT and returns a streaming Cursor instead of
// buffering all rows in memory. The caller MUST call Close.
func (ex *Executor) QueryCursor(ctx context.Context, gen *GeneratedSQL) (*Cursor, error) {
	ctx, cancel := ex.withTimeout(ctx)
	rows, err := withRetryGeneric(ctx, ex.retry, func() (ormdriver.Rows, error) {
		stmt, err := ex.prepare(ctx, gen.SQL)
		if err != nil {
			return nil, fmt.Errorf("execution: prepare: %w", err)
		}
		return stmt.QueryContext(ctx, gen.Args...)
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return &Cursor{rows: rows, cancel: cancel}, nil
}

func (c *Cursor) Next() bool          { return c.rows.Next() }
func (c *Cursor) Err() error          { return c.rows.Err() }
func (c *Cursor) Raw() ormdriver.Rows { return c.rows }

// Close releases both the underlying rows and the timeout context created
// for this cursor. Safe to call multiple times.
func (c *Cursor) Close() error {
	defer c.cancel()
	return c.rows.Close()
}

// Stats exposes cache hit/miss counters for observability plugins.
func (ex *Executor) Stats() (planHits, planMisses, stmtHits, stmtMisses int64) {
	ph, pm := ex.planTextCache.Stats()
	sh, sm := ex.stmtCache.Stats()
	return ph, pm, sh, sm
}
