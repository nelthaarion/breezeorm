package execution

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breezeorm/pkg/dialect"
)

func TestExecutor_PrepareIsCachedAcrossCalls(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{})
	defer ex.Close()

	gen := &GeneratedSQL{SQL: "SELECT id FROM widgets WHERE id = ?", Args: []any{1}}

	for i := 0; i < 10; i++ {
		rows, err := ex.Query(context.Background(), gen)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		rows.Close()
	}

	if got := fakePrepareCalls.Load(); got != 1 {
		t.Errorf("Prepare called %d times for 10 identical-shape queries, want 1", got)
	}
}

func TestExecutor_PrepareCoalescesConcurrentMisses(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{})
	defer ex.Close()

	gen := &GeneratedSQL{SQL: "SELECT id FROM widgets WHERE id = ?", Args: []any{1}}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := ex.Query(context.Background(), gen)
			if err != nil {
				t.Errorf("Query: %v", err)
				return
			}
			rows.Close()
		}()
	}
	wg.Wait()

	if got := fakePrepareCalls.Load(); got != 1 {
		t.Errorf("Prepare called %d times for 50 concurrent identical-shape queries, want 1 (coalesced)", got)
	}
}

func TestExecutor_StmtCacheEvictionClosesStatement(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{}, WithStmtCacheSize(2))
	defer ex.Close()

	sqlTexts := []string{
		"SELECT 1 FROM widgets WHERE id = ?",
		"SELECT 2 FROM widgets WHERE id = ?",
		"SELECT 3 FROM widgets WHERE id = ?", // pushes the cache over capacity 2
	}
	for _, s := range sqlTexts {
		rows, err := ex.Query(context.Background(), &GeneratedSQL{SQL: s, Args: []any{1}})
		if err != nil {
			t.Fatalf("Query(%s): %v", s, err)
		}
		rows.Close()
	}

	if got := fakeCloseCalls.Load(); got < 1 {
		t.Errorf("expected at least 1 Stmt.Close() from eviction over a size-2 cache with 3 distinct statements, got %d", got)
	}
}

func TestExecutor_CloseClosesAllCachedStatements(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{})

	for _, s := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		rows, err := ex.Query(context.Background(), &GeneratedSQL{SQL: s})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		rows.Close()
	}

	ex.Close()

	if got := fakeCloseCalls.Load(); got != 3 {
		t.Errorf("Executor.Close() closed %d statements, want 3 (every cached statement)", got)
	}
}

func TestExecutor_AppliesDefaultTimeout(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{}, WithDefaultTimeout(5*time.Second))
	defer ex.Close()

	ctx, cancel := ex.withTimeout(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected withTimeout to attach a deadline to a bare context")
	}
	if time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
		t.Errorf("deadline = %v from now, want ~5s", time.Until(deadline))
	}
}

func TestExecutor_DoesNotOverrideExistingDeadline(t *testing.T) {
	db := openFakeDB()
	defer db.Close()
	ex := New(db, dialect.SQLite{}, WithDefaultTimeout(30*time.Second))
	defer ex.Close()

	callerCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ctx, cancel2 := ex.withTimeout(callerCtx)
	defer cancel2()

	deadline, _ := ctx.Deadline()
	if time.Until(deadline) > time.Second {
		t.Error("expected the caller's tighter deadline to be preserved, not overridden by the executor default")
	}
}

func TestExecutor_RetriesTransientErrors(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{}, WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}))
	defer ex.Close()

	sqlText := "SELECT 1 FROM widgets"
	fakeMu.Lock()
	fakeFailNTimes[sqlText] = 2 // fail twice, succeed on the 3rd attempt
	fakeMu.Unlock()

	rows, err := ex.Query(context.Background(), &GeneratedSQL{SQL: sqlText})
	if err != nil {
		t.Fatalf("expected retry to eventually succeed, got: %v", err)
	}
	rows.Close()
}

func TestExecutor_DoesNotRetryNonTransientErrors(t *testing.T) {
	if isRetryableError(errors.New("syntax error near SELECT")) {
		t.Error("a plain syntax error must not be classified as retryable")
	}
	if !isRetryableError(errors.New("ERROR: deadlock detected")) {
		t.Error("a deadlock error must be classified as retryable")
	}
	if isRetryableError(context.DeadlineExceeded) {
		t.Error("a context deadline must never be retried")
	}
}

func TestExecutor_RowsCloseCancelsTimeoutContext(t *testing.T) {
	resetFakeDriverState()
	db := openFakeDB()
	defer db.Close()

	ex := New(db, dialect.SQLite{}, WithDefaultTimeout(time.Minute))
	defer ex.Close()

	rows, err := ex.Query(context.Background(), &GeneratedSQL{SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Closing rows must not hang or panic, and must be safe to call twice
	// (idempotent), proving the wrapped cancel is wired correctly.
	if err := rows.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("second Close (idempotency check): %v", err)
	}
}
