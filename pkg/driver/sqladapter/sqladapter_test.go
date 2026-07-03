package sqladapter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	ormdriver "github.com/nelthaarion/breezeorm/pkg/driver"
)

// Minimal in-memory database/sql/driver fake, mirroring the one in
// pkg/execution's test suite, kept local and small since this package only
// needs to prove the adapter itself works — not exercise caching/retry
// again (that's pkg/execution's job).
type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) { return &fakeStmt{}, nil }
func (c *fakeConn) Close() error                              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)                 { return nil, errors.New("not supported") }

type fakeStmt struct{}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) { return &fakeRows{}, nil }

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"id"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(42)
	return nil
}

func TestWrap_SatisfiesDriverDB(t *testing.T) {
	sql.Register("sqladapter_fake_test_driver", fakeDriver{})
	db, err := sql.Open("sqladapter_fake_test_driver", "test")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var wrapped ormdriver.DB = Wrap(db) // compile-time interface satisfaction check
	ctx := context.Background()

	if err := wrapped.PingContext(ctx); err != nil {
		t.Fatalf("PingContext: %v", err)
	}

	stmt, err := wrapped.PrepareContext(ctx, "SELECT id FROM t WHERE id = ?")
	if err != nil {
		t.Fatalf("PrepareContext: %v", err)
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx, 1)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected one row")
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}

	res, err := stmt.ExecContext(ctx, 1)
	if err != nil {
		t.Fatalf("ExecContext: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		t.Errorf("RowsAffected = %d, %v, want 1, nil", n, err)
	}
}
