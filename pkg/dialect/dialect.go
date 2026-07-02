// Package dialect defines the SQL dialect abstraction. Every supported
// database (Postgres, MySQL, SQLite, SQL Server) implements this interface.
// The rest of the engine (compiler, planner, SQL generator) is dialect-agnostic
// and only ever talks to this interface.
package dialect

import "fmt"

// LockMode represents a row-locking clause (FOR UPDATE, etc.).
type LockMode uint8

const (
	LockNone LockMode = iota
	LockForUpdate
	LockForShare
	LockForNoKeyUpdate
	LockForUpdateSkipLocked
	LockForUpdateNoWait
)

// UpsertConflictTarget describes the columns/constraint used to detect
// conflicts for an upsert.
type UpsertConflictTarget struct {
	Columns    []string
	Constraint string // named constraint, if the dialect supports it (e.g. Postgres ON CONSTRAINT)
}

// Dialect is the contract every database backend must satisfy.
type Dialect interface {
	// Name returns a stable identifier, e.g. "postgres", "mysql".
	Name() string

	// Placeholder returns the parameter placeholder for the nth (1-indexed)
	// bound parameter in a statement, e.g. "$1" for Postgres, "?" for MySQL/SQLite.
	Placeholder(n int) string

	// QuoteIdentifier quotes a table/column identifier safely, e.g. `"users"`
	// for Postgres or "`users`" for MySQL.
	QuoteIdentifier(ident string) string

	// ReturningClause renders a RETURNING clause for the given columns, or ""
	// if the dialect doesn't support it (e.g. MySQL < 8, SQL Server uses OUTPUT).
	ReturningClause(columns []string) string

	// LimitOffset renders the LIMIT/OFFSET (or dialect-specific equivalent,
	// e.g. SQL Server's OFFSET...FETCH NEXT) suffix.
	LimitOffset(limit, offset *int64) string

	// LockClause renders a row-locking clause for the given mode.
	LockClause(mode LockMode) string

	// UpsertClause renders the dialect's upsert syntax fragment
	// (e.g. "ON CONFLICT (...) DO UPDATE SET ..." vs
	// "ON DUPLICATE KEY UPDATE ..." vs SQL Server MERGE).
	UpsertClause(target UpsertConflictTarget, updateColumns []string) string

	// BulkInsertSupported reports whether multi-row VALUES lists are supported
	// natively (all four target dialects: yes, but kept explicit for future
	// backends and for TVP-style bulk insert in SQL Server).
	BulkInsertSupported() bool

	// SupportsReturning reports whether RETURNING/OUTPUT is available.
	SupportsReturning() bool

	// ValidateIdentifier rejects identifiers that could enable SQL injection
	// via unescaped table/column names (dynamic query building safety net).
	ValidateIdentifier(ident string) error
}

// ErrInvalidIdentifier is returned by ValidateIdentifier implementations.
type ErrInvalidIdentifier struct{ Ident string }

func (e *ErrInvalidIdentifier) Error() string {
	return fmt.Sprintf("dialect: invalid identifier %q", e.Ident)
}
