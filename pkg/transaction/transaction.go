// Package transaction implements context-aware transactions with nested
// transaction support via savepoints, and automatic retry for transient
// failures such as deadlocks and serialization failures.
package transaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

type ctxKey struct{}

// Tx wraps *sql.Tx with a savepoint depth counter so nested calls to
// transaction.Run against a context that already holds a Tx transparently
// become savepoints instead of attempting a second BEGIN.
type Tx struct {
	sqlTx *sql.Tx
	mu    sync.Mutex
	depth int
}

func (tx *Tx) nextSavepoint() (name string, depth int) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.depth++
	return fmt.Sprintf("sp_%d", tx.depth), tx.depth
}

func (tx *Tx) releaseSavepoint() {
	tx.mu.Lock()
	tx.depth--
	tx.mu.Unlock()
}

// FromContext returns the active *Tx, if any.
func FromContext(ctx context.Context) (*Tx, bool) {
	tx, ok := ctx.Value(ctxKey{}).(*Tx)
	return tx, ok
}

// RetryPolicy controls automatic retry behavior for transient errors.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// IsRetryable classifies an error as transient (deadlock, serialization
	// failure, connection reset, etc.). Defaults to a Postgres/MySQL-aware
	// heuristic based on error text if nil — see defaultIsRetryable.
	IsRetryable func(error) bool
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    500 * time.Millisecond,
		IsRetryable: defaultIsRetryable,
	}
}

func defaultIsRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Heuristic string matching across dialects; production code should
	// prefer typed driver errors (e.g. *pgconn.PgError.Code == "40P01") once
	// a specific driver is wired in — kept dialect-agnostic here since this
	// package has no dialect dependency.
	//
	// Aligned with pkg/execution/executor.go's isRetryableError so both
	// statement-level and transaction-level retry classify the same errors
	// consistently. "database is locked" covers SQLite's busy/locked state,
	// which the executor already retried but the transaction layer didn't —
	// an inconsistency that caused a transient SQLite lock to surface as a
	// hard transaction failure instead of being retried.
	for _, needle := range []string{"deadlock", "serialization failure", "lock wait timeout", "could not serialize", "database is locked"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// Run executes fn within a transaction. If ctx already carries a Tx, fn runs
// inside a SAVEPOINT (nested transaction) instead of starting a new one —
// committing/rolling back only that savepoint. If ctx has no Tx, Run starts
// one at the outermost level and applies the retry policy around the whole
// attempt (retrying a nested savepoint alone would leave the outer
// transaction in an inconsistent state, so only outermost Run retries).
func Run(ctx context.Context, db *sql.DB, opts *sql.TxOptions, policy RetryPolicy, fn func(ctx context.Context) error) error {
	if existing, ok := FromContext(ctx); ok {
		return runNested(ctx, existing, fn)
	}
	return runWithRetry(ctx, db, opts, policy, fn)
}

func runWithRetry(ctx context.Context, db *sql.DB, opts *sql.TxOptions, policy RetryPolicy, fn func(ctx context.Context) error) error {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.IsRetryable == nil {
		policy.IsRetryable = defaultIsRetryable
	}

	var lastErr error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := backoff(policy, attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := runOnce(ctx, db, opts, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if !policy.IsRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("transaction: exhausted %d attempts: %w", policy.MaxAttempts, lastErr)
}

func runOnce(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(ctx context.Context) error) (err error) {
	sqlTx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("transaction: begin: %w", err)
	}
	tx := &Tx{sqlTx: sqlTx}
	txCtx := context.WithValue(ctx, ctxKey{}, tx)

	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()

	if err = fn(txCtx); err != nil {
		if rbErr := sqlTx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("transaction: rollback after error %v: %w", err, rbErr)
		}
		return err
	}
	if err = sqlTx.Commit(); err != nil {
		return fmt.Errorf("transaction: commit: %w", err)
	}
	return nil
}

func runNested(ctx context.Context, parent *Tx, fn func(ctx context.Context) error) error {
	spName, depth := parent.nextSavepoint()

	if _, err := parent.sqlTx.ExecContext(ctx, "SAVEPOINT "+spName); err != nil {
		parent.releaseSavepoint()
		return fmt.Errorf("transaction: savepoint: %w", err)
	}

	nested := &Tx{sqlTx: parent.sqlTx, depth: depth}
	nestedCtx := context.WithValue(ctx, ctxKey{}, nested)

	if err := fn(nestedCtx); err != nil {
		if _, rbErr := parent.sqlTx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+spName); rbErr != nil {
			return fmt.Errorf("transaction: rollback to savepoint after error %v: %w", err, rbErr)
		}
		return err
	}
	_, err := parent.sqlTx.ExecContext(ctx, "RELEASE SAVEPOINT "+spName)
	return err
}

func backoff(policy RetryPolicy, attempt int) time.Duration {
	d := policy.BaseDelay * time.Duration(1<<uint(attempt-1))
	if d > policy.MaxDelay {
		d = policy.MaxDelay
	}
	// Full jitter to avoid thundering-herd retries against the same lock.
	return time.Duration(rand.Int63n(int64(d) + 1))
}
