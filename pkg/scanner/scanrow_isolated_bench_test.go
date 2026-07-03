package scanner

// Isolated micro-benchmarks for Plan.Compile + Plan.ScanRow only.
//
// Why this file exists: BenchmarkFindByID / BenchmarkSelectWhereLimit in
// /benchmark measure the whole pipeline (SQL building, planner/optimizer,
// plan cache lookup, driver round-trip, *and* ScanRow). That's the right
// benchmark for the library as a whole, but it can't isolate whether a
// ScanRow-only change actually moved the needle — allocs from the cache,
// SQL builder, or database/sql's own row-scan machinery are all mixed in.
//
// These benchmarks call Compile once (outside the timed loop, exactly like
// real callers are supposed to — see the Compile doc comment) and then run
// ScanRow directly against a fake RowsSource that mimics *sql.Rows.Scan
// without a real driver/network/DB in the loop at all. That isolates
// exactly the two things this benchmark is allowed to change: the
// per-column dispatch in ScanRow and the compile-time setup in Compile.

import (
	"testing"
	"time"
	"unsafe"

	"github.com/nelthaarion/breezeorm/pkg/metadata"
)

// benchUser mirrors benchmark.BenchUser (the model FindByID/SelectWhereLimit
// scan into) so this benchmark exercises the same field mix: one PK int64,
// two strings, one bool, one time.Time. All five are fast-path kinds, which
// is the common case this change targets.
type benchUser struct {
	ID        int64     `db:"id,pk,autoincrement"`
	Email     string    `db:"email"`
	Name      string    `db:"name"`
	Active    bool      `db:"active"`
	CreatedAt time.Time `db:"created_at"`
}

// fakeRows is a minimal RowsSource that assigns fixed values into whatever
// pointers ScanRow hands it, the same way *sql.Rows.Scan would, but with no
// driver, no network, and no database/sql-internal allocation of its own —
// so every alloc reported by the benchmark is attributable to ScanRow, not
// to database/sql's row-scan glue.
type fakeRows struct {
	n   int // rows remaining
	now time.Time
}

func (f *fakeRows) Next() bool {
	if f.n <= 0 {
		return false
	}
	f.n--
	return true
}

func (f *fakeRows) Scan(dest ...any) error {
	for _, d := range dest {
		switch v := d.(type) {
		case *int64:
			*v = 42
		case *string:
			*v = "someone@example.com"
		case *bool:
			*v = true
		case *time.Time:
			*v = f.now
		}
	}
	return nil
}

func (f *fakeRows) Close() error { return nil }
func (f *fakeRows) Err() error   { return nil }

func mustCompileBenchUserPlan(b *testing.B) *Plan {
	b.Helper()
	tbl, err := metadata.Compile[benchUser]()
	if err != nil {
		b.Fatalf("metadata.Compile: %v", err)
	}
	plan, err := Compile(tbl, []string{"id", "email", "name", "active", "created_at"})
	if err != nil {
		b.Fatalf("scanner.Compile: %v", err)
	}
	return plan
}

// BenchmarkScanRowSingle isolates the FindByID shape: Compile once (as a
// real cached plan would be), then scan exactly one row per op.
func BenchmarkScanRowSingle(b *testing.B) {
	plan := mustCompileBenchUserPlan(b)
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var u benchUser
		rows := &fakeRows{n: 1, now: now}
		rows.Next()
		if err := plan.ScanRow(rows, unsafe.Pointer(&u)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanOneSingle isolates the First()/FindByID shape via ScanOne:
// one Compile, then one row scanned directly into *T per op — no []T slice
// allocation, unlike routing through ScanAll and taking element [0].
func BenchmarkScanOneSingle(b *testing.B) {
	plan := mustCompileBenchUserPlan(b)
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows := &fakeRows{n: 1, now: now}
		if _, err := ScanOne[benchUser](rows, plan); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanRowMany isolates the SelectWhereLimit shape: one Compile,
// then N rows scanned per op via ScanAll, matching how pkg/orm actually
// drives the scanner for multi-row results.
func BenchmarkScanRowMany(b *testing.B) {
	const rowsPerOp = 50
	plan := mustCompileBenchUserPlan(b)
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows := &fakeRows{n: rowsPerOp, now: now}
		if _, err := ScanAll[benchUser](rows, plan); err != nil {
			b.Fatal(err)
		}
	}
}
