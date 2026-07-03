// Package sqladapter adapts a standard-library *sql.DB to pkg/driver.DB.
// This is the only driver.DB implementation in this module today — it's
// what every existing orm.Open(sqlDB, ...) call uses under the hood, kept
// so that call sites and behavior are unchanged from before the driver
// abstraction was introduced. A future native-driver adapter (e.g. pgx)
// would live in a sibling package implementing the same pkg/driver
// interfaces, not by modifying this one.
package sqladapter

import (
	"context"
	"database/sql"

	"github.com/nelthaarion/breezeorm/pkg/driver"
)

// Wrap adapts db to driver.DB.
func Wrap(db *sql.DB) driver.DB {
	return &dbAdapter{db: db}
}

type dbAdapter struct{ db *sql.DB }

func (a *dbAdapter) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := a.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &stmtAdapter{stmt: stmt}, nil
}

func (a *dbAdapter) PingContext(ctx context.Context) error { return a.db.PingContext(ctx) }
func (a *dbAdapter) Close() error                          { return a.db.Close() }

type stmtAdapter struct{ stmt *sql.Stmt }

// QueryContext/ExecContext return *sql.Rows / sql.Result directly as the
// driver.Rows / driver.Result interfaces — both already satisfy those
// interfaces structurally, so no further wrapping is needed here.
func (s *stmtAdapter) QueryContext(ctx context.Context, args ...any) (driver.Rows, error) {
	return s.stmt.QueryContext(ctx, args...)
}

func (s *stmtAdapter) ExecContext(ctx context.Context, args ...any) (driver.Result, error) {
	return s.stmt.ExecContext(ctx, args...)
}

func (s *stmtAdapter) Close() error { return s.stmt.Close() }
