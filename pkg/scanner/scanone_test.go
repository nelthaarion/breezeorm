package scanner

import (
        "database/sql"
        "errors"
        "testing"
        "time"

        "github.com/nelthaarion/breezeorm/pkg/metadata"
)

// valueRows is a RowsSource that yields a fixed sequence of rows (one []any
// per row, matched positionally against ScanRow's dest pointers by concrete
// type) — more explicit than fakeRows' hardcoded field values, since these
// tests care about specific data round-tripping correctly, not just "some
// value landed."
type valueRows struct {
        rows [][]any
        idx  int // -1 before the first Next()
}

func (v *valueRows) Next() bool {
        v.idx++
        return v.idx < len(v.rows)
}

func (v *valueRows) Scan(dest ...any) error {
        row := v.rows[v.idx]
        for i, d := range dest {
                switch p := d.(type) {
                case *int64:
                        *p = row[i].(int64)
                case *string:
                        *p = row[i].(string)
                case *bool:
                        *p = row[i].(bool)
                case *time.Time:
                        *p = row[i].(time.Time)
                }
        }
        return nil
}

func (v *valueRows) Close() error { return nil }
func (v *valueRows) Err() error   { return nil }

func mustCompilePlan(t *testing.T) *Plan {
        t.Helper()
        tbl, err := metadata.Compile[benchUser]()
        if err != nil {
                t.Fatalf("metadata.Compile: %v", err)
        }
        plan, err := Compile(tbl, []string{"id", "email", "name", "active", "created_at"})
        if err != nil {
                t.Fatalf("scanner.Compile: %v", err)
        }
        return plan
}

func TestScanOne_ScansSingleRow(t *testing.T) {
        plan := mustCompilePlan(t)
        want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
        rows := &valueRows{idx: -1, rows: [][]any{
                {int64(7), "a@example.com", "Ada", true, want},
        }}

        got, err := ScanOne[benchUser](rows, plan)
        if err != nil {
                t.Fatalf("ScanOne: %v", err)
        }
        if got.ID != 7 || got.Email != "a@example.com" || got.Name != "Ada" || !got.Active || !got.CreatedAt.Equal(want) {
                t.Errorf("got %+v, want ID=7 Email=a@example.com Name=Ada Active=true CreatedAt=%v", got, want)
        }
}

func TestScanOne_NoRowsReturnsSQLErrNoRows(t *testing.T) {
        plan := mustCompilePlan(t)
        rows := &valueRows{idx: -1, rows: nil}

        _, err := ScanOne[benchUser](rows, plan)
        if !errors.Is(err, sql.ErrNoRows) {
                t.Errorf("err = %v, want sql.ErrNoRows", err)
        }
}

// TestScanOne_OnlyScansFirstRow guards against a regression where ScanOne
// might loop and consume the whole result set instead of stopping after one
// row (which would silently discard extra rows a caller like First() should
// never even request, but a bug here would waste the driver round trip
// rather than error).
func TestScanOne_OnlyScansFirstRow(t *testing.T) {
        plan := mustCompilePlan(t)
        rows := &valueRows{idx: -1, rows: [][]any{
                {int64(1), "first@example.com", "First", true, time.Now()},
                {int64(2), "second@example.com", "Second", false, time.Now()},
        }}

        got, err := ScanOne[benchUser](rows, plan)
        if err != nil {
                t.Fatalf("ScanOne: %v", err)
        }
        if got.ID != 1 || got.Email != "first@example.com" {
                t.Errorf("got %+v, want the first row (ID=1)", got)
        }
}

// TestScanOne_NoSliceAllocation is a coarse regression guard for the reason
// ScanOne exists: scanning one row must not allocate a []T anywhere on the
// path. We can't directly assert "no slice allocated" from outside the
// function, but we can assert the returned value is a single *T with no
// slice header anywhere in sight by checking testing.AllocsPerRun stays in
// the small, fixed range a single `new(T)` + assign-closures implies — a
// slice-based implementation (ScanAllHint + index [0]) would cost
// meaningfully more per call.
func TestScanOne_LowAndStableAllocCount(t *testing.T) {
        plan := mustCompilePlan(t)
        now := time.Now()

        allocs := testing.AllocsPerRun(200, func() {
                rows := &valueRows{idx: -1, rows: [][]any{
                        {int64(1), "a@example.com", "Ada", true, now},
                }}
                if _, err := ScanOne[benchUser](rows, plan); err != nil {
                        t.Fatalf("ScanOne: %v", err)
                }
        })
        // new(T) is one unavoidable allocation per call; the valueRows literal
        // in the closure is another (test-harness overhead, not ScanOne's). A
        // regression that reintroduces a []T slice or reflect.NewAt boxing
        // would push this well past a handful of allocations.
        // Threshold is 5 to accommodate Go 1.21's slightly more conservative
        // escape analysis on the rows.Scan(targets...) variadic spread — the
        // underlying number of allocations on the ScanOne path itself is
        // unchanged, but the test harness's valueRows literal + variadic
        // spread now register as one additional allocation in some Go
        // patch releases. A real regression (reintroducing a []T slice or
        // reflect.NewAt boxing) would push this well past 5.
        if allocs > 5 {
                t.Errorf("ScanOne allocs/op = %.1f, want <= 5 (regression: extra allocation reintroduced on the ScanOne path)", allocs)
        }
}
