// Package driver defines the minimal interface pkg/execution depends on to
// talk to a database. This is the seam that keeps the query engine
// (metadata/query/planner/optimizer/compiler/execution) from being
// permanently welded to database/sql: today the only implementation is
// pkg/driver/sqladapter, which wraps a *sql.DB, but a future native-driver
// adapter (pgx, for example — which offers real advantages over
// database/sql for Postgres specifically: binary protocol, no
// interface{}-boxing at the driver.Value boundary, native COPY support)
// can implement this same interface without any redesign above this layer.
//
// This intentionally does NOT attempt to abstract transactions. database/sql
// and native drivers like pgx do not share a common transaction type, and
// forcing one through a lowest-common-denominator interface at this stage
// would either lose real capability (pgx's Tx has richer, faster batch
// support that a generic interface would flatten away) or add complexity
// this codebase doesn't need yet. pkg/transaction remains *sql.DB/*sql.Tx-
// based; abstracting it is a deliberately separate, larger effort — see
// the note in pkg/transaction/transaction.go.
package driver

import "context"

// DB is the connection-pool-level interface Executor needs: prepare a
// statement, ping, close. Anything satisfying this can back an Executor.
type DB interface {
	PrepareContext(ctx context.Context, query string) (Stmt, error)
	PingContext(ctx context.Context) error
	Close() error
}

// Stmt is a prepared statement: query for rows, execute for a result, close
// when evicted from the cache.
type Stmt interface {
	QueryContext(ctx context.Context, args ...any) (Rows, error)
	ExecContext(ctx context.Context, args ...any) (Result, error)
	Close() error
}

// Rows is a result-set cursor. This is intentionally exactly the subset
// *sql.Rows already implements structurally, so pkg/driver/sqladapter needs
// zero wrapping for it — only DB and Stmt need adapter types.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Close() error
	Err() error
}

// Result is an exec result: last insert ID and rows affected. Also exactly
// what *sql.Result already implements structurally.
type Result interface {
	LastInsertId() (int64, error)
	RowsAffected() (int64, error)
}
