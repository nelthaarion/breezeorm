package execution

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// fakeDriver is a minimal, entirely in-memory database/sql/driver
// implementation used only by this package's tests, so the Executor's
// caching/eviction/timeout/retry behavior can be verified against a real
// *sql.DB without adding an external driver dependency to the module.
type fakeDriver struct{}

var (
	fakePrepareCalls atomic.Int32
	fakeCloseCalls   atomic.Int32
	fakeMu           sync.Mutex
	fakePrepared     []string
	fakeFailNTimes   = map[string]int{} // sql text -> remaining failures before success
)

func resetFakeDriverState() {
	fakePrepareCalls.Store(0)
	fakeCloseCalls.Store(0)
	fakeMu.Lock()
	fakePrepared = nil
	fakeFailNTimes = map[string]int{}
	fakeMu.Unlock()
}

func (fakeDriver) Open(name string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	fakePrepareCalls.Add(1)
	fakeMu.Lock()
	fakePrepared = append(fakePrepared, query)
	fakeMu.Unlock()
	return &fakeStmt{query: query}, nil
}

func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeDriver: transactions not supported")
}

type fakeStmt struct{ query string }

func (s *fakeStmt) Close() error  { fakeCloseCalls.Add(1); return nil }
func (s *fakeStmt) NumInput() int { return -1 } // skip driver-side arg-count validation

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.maybeFail(); err != nil {
		return nil, err
	}
	return fakeResult{}, nil
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	if err := s.maybeFail(); err != nil {
		return nil, err
	}
	return &fakeRows{}, nil
}

// maybeFail lets a test pre-arm a SQL text to fail N times before
// succeeding, to exercise Executor's retry path deterministically.
func (s *fakeStmt) maybeFail() error {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	if n, ok := fakeFailNTimes[s.query]; ok && n > 0 {
		fakeFailNTimes[s.query] = n - 1
		return errors.New("database is locked") // matches isRetryableError's heuristic
	}
	return nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"id"} }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

var registerFakeDriverOnce sync.Once

func openFakeDB() *sql.DB {
	registerFakeDriverOnce.Do(func() {
		sql.Register("breezorm_fake_test_driver", fakeDriver{})
	})
	db, err := sql.Open("breezorm_fake_test_driver", "test")
	if err != nil {
		panic(err) // test-only helper; a failure here is a test bug, not a runtime condition
	}
	return db
}
