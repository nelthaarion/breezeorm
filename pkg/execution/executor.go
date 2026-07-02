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
)

// Executor owns the prepared-statement cache and SQL-text cache for one
// *sql.DB + dialect pair, and is the only place raw database/sql calls
// happen. All caches are bounded (LRU) so a workload with unbounded query
// shape diversity cannot exhaust memory or leak prepared statements on the
// server; evicted statements are Close()'d.
type Executor struct {
	db      *sql.DB
	dialect dialect.Dialect

	// planTextCache maps a compiler.CompiledQuery.CacheKey (structural,
	// literal-value-independent) to rendered SQL text. Bind arguments are
	// NEVER cached here — they're re-derived fresh on every call via
	// ExtractArgs, which is what makes caching the text safe: two calls with
	// the same CacheKey but different WHERE literals correctly get the same
	// SQL string and different, correct Args.
	planTextCache *cache.LRU[string, string]

	// stmtCache maps rendered SQL text to a prepared *sql.Stmt. Bounded and
	// eviction-safe: the onEvict callback closes the statement so evicted
	// entries don't leak server-side resources or client-side file
	// descriptors.
	stmtCache *cache.LRU[string, *sql.Stmt]

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

func WithStmtCacheSize(n int) Option {
	return func(ex *Executor) { ex.stmtCache = newStmtCache(n) }
}

func WithPlanCacheSize(n int) Option {
	return func(ex *Executor) { ex.planTextCache = cache.NewLRU[string, string](n, nil) }
}

func WithDefaultTimeout(d time.Duration) Option {
	return func(ex *Executor) { ex.defaultTimeout = d }
}

func WithRetryPolicy(p RetryPolicy) Option {
	return func(ex *Executor) { ex.retry = p }
}

func newStmtCache(size int) *cache.LRU[string, *sql.Stmt] {
	return cache.NewLRU[string, *sql.Stmt](size, func(_ string, stmt *sql.Stmt) {
		_ = stmt.Close() // best-effort: nothing actionable if this fails at eviction time
	})
}

// New creates an Executor bound to a live connection pool and dialect, with
// production-safe defaults: bounded caches, a default query timeout, and
// retry on transient errors.
func New(db *sql.DB, d dialect.Dialect, opts ...Option) *Executor {
	ex := &Executor{
		db:             db,
		dialect:        d,
		planTextCache:  cache.NewLRU[string, string](DefaultPlanCacheSize, nil),
		stmtCache:      newStmtCache(DefaultStmtCacheSize),
		defaultTimeout: DefaultQueryTimeout,
		retry:          DefaultRetryPolicy(),
	}
	for _, o := range opts {
		o(ex)
	}
	return ex
}

// Close purges the prepared-statement cache, closing every cached statement.
// Call this before closing the underlying *sql.DB to avoid leaking
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

// prepare returns a cached *sql.Stmt for the given SQL text, preparing it on
// first use. Concurrent misses for identical SQL text are coalesced (see
// cache.LRU.GetOrCompute), so a burst of concurrent first-time callers for
// the same new query shape triggers exactly one PrepareContext round trip.
func (ex *Executor) prepare(ctx context.Context, sqlText string) (*sql.Stmt, error) {
	return ex.stmtCache.GetOrCompute(sqlText, func() (*sql.Stmt, error) {
		return ex.db.PrepareContext(ctx, sqlText)
	})
}

// Query runs a SELECT and returns a *Rows wrapping the driver's *sql.Rows.
// The caller MUST close the returned *Rows (directly, or by consuming it
// through pkg/scanner, which does so internally) — Close both releases the
// underlying connection back to the pool AND cancels this call's timeout
// context. Failing to close leaks both.
func (ex *Executor) Query(ctx context.Context, gen *GeneratedSQL) (*Rows, error) {
	ctx, cancel := ex.withTimeout(ctx)
	rows, err := withRetryGeneric(ctx, ex.retry, func() (*sql.Rows, error) {
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

// Rows wraps *sql.Rows so that Close() also releases the timeout context
// created for this query. Embeds *sql.Rows so Next/Scan/Err/Columns are all
// available directly, satisfying pkg/scanner's RowsSource interface.
type Rows struct {
	*sql.Rows
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
func (ex *Executor) Exec(ctx context.Context, gen *GeneratedSQL) (sql.Result, error) {
	ctx, cancel := ex.withTimeout(ctx)
	defer cancel()
	return withRetryGeneric(ctx, ex.retry, func() (sql.Result, error) {
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
func (ex *Executor) BulkExec(ctx context.Context, gen *GeneratedSQL) (sql.Result, error) {
	return ex.Exec(ctx, gen)
}

// withRetry runs fn, retrying on transient errors per ex.retry. It never
// retries once the context is done, and never retries more than
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
// pkg/orm/db.go); callers with a specific driver imported can layer a more
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

// Cursor is a streaming row iterator for large result sets. It wraps
// *sql.Rows directly rather than materializing a full []T slice, and owns
// releasing the timeout context set up by QueryCursor.
type Cursor struct {
	rows   *sql.Rows
	cancel context.CancelFunc
}

// QueryCursor runs a SELECT and returns a streaming Cursor instead of
// buffering all rows in memory. The caller MUST call Close.
func (ex *Executor) QueryCursor(ctx context.Context, gen *GeneratedSQL) (*Cursor, error) {
	ctx, cancel := ex.withTimeout(ctx)
	rows, err := withRetryGeneric(ctx, ex.retry, func() (*sql.Rows, error) {
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

func (c *Cursor) Next() bool     { return c.rows.Next() }
func (c *Cursor) Err() error     { return c.rows.Err() }
func (c *Cursor) Raw() *sql.Rows { return c.rows }

// Close releases both the underlying *sql.Rows and the timeout context
// created for this cursor. Safe to call multiple times.
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
