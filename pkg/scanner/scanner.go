// Package scanner implements the row-decoding engine. All reflection happens
// once, when a Plan is compiled from metadata.Table + the result column
// list; row-by-row decoding then uses only precomputed unsafe offsets.
package scanner

import (
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/nelthaarion/breezorm/pkg/metadata"
)

// FieldAssignment binds a result-set column index to a destination field.
type FieldAssignment struct {
	ColumnIndex int
	Column      *metadata.Column
}

// Plan is the precompiled scan plan for one (Table, result-column-list) pair.
// Building a Plan reflects on the struct type; using a Plan to scan rows does
// not. Plan is intentionally NOT safe to mutate after Compile returns it —
// callers should treat it as immutable and cache it (see pkg/orm's
// scanPlanCache), the same "compile once" contract as every other plan type
// in this codebase.
type Plan struct {
	Table       *metadata.Table
	Assignments []FieldAssignment

	// targetsPool recycles the []any destination slice ScanRow needs to
	// pass to rows.Scan. Before this, every single row scanned allocated a
	// fresh []any (plus N reflect.NewAt().Interface() boxes into it) —
	// for a 50-row result set that's 50 slice allocations that this pool
	// turns into (after warmup) zero.
	targetsPool sync.Pool
}

// Compile builds a Plan by matching SQL result columns against the table's
// compiled metadata. This is the one place reflection-adjacent work happens
// (matching names); it should be called once per distinct result shape and
// cached (see pkg/cache) — never once per row.
func Compile(tbl *metadata.Table, resultColumns []string) (*Plan, error) {
	p := &Plan{Table: tbl, Assignments: make([]FieldAssignment, 0, len(resultColumns))}
	for i, name := range resultColumns {
		col, ok := tbl.ColumnByName[name]
		if !ok {
			continue // extra/computed column with no destination field: skipped, not an error
		}
		p.Assignments = append(p.Assignments, FieldAssignment{ColumnIndex: i, Column: col})
	}
	n := len(p.Assignments)
	p.targetsPool.New = func() any {
		s := make([]any, n)
		return &s
	}
	return p, nil
}

// RowsSource is the subset of *sql.Rows that the scanner needs. Accepting
// this interface instead of a concrete *sql.Rows lets callers wrap rows
// (pkg/execution.Rows does this to tie context-cancellation to Close)
// without the scanner needing to know about the wrapper.
type RowsSource interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// ScanRow decodes a single database/sql row into a *T using the plan's
// precomputed field offsets, without reflecting on T's type again.
//
// dest must point to a value of the same Go type the Plan's Table was
// compiled from (enforced by callers via generics in pkg/execution).
//
// NOTE ON reflect.NewAt: this still costs one reflect.NewAt().Interface()
// call per column per row — real "zero reflection on the hot path" for the
// scan step specifically requires knowing each field's concrete type at
// compile time, which is exactly what code generation (see
// cmd/breezorm-gen) provides: a generated scanner writes
// `&((*T)(dest)).Field` directly, no reflect.Value involved at all. This
// path is the honest current state for the reflection-based (non-generated)
// route; the pooling below removes the *allocation* overhead, not the
// reflect.NewAt call itself.
func (p *Plan) ScanRow(rows RowsSource, dest unsafe.Pointer) error {
	targetsPtr := p.targetsPool.Get().(*[]any)
	targets := *targetsPtr
	defer func() {
		for i := range targets {
			targets[i] = nil // drop references before returning to pool
		}
		p.targetsPool.Put(targetsPtr)
	}()

	for i, a := range p.Assignments {
		fieldPtr := unsafe.Pointer(uintptr(dest) + a.Column.Offset)
		targets[i] = reflect.NewAt(a.Column.Type, fieldPtr).Interface()
	}
	if err := rows.Scan(targets...); err != nil {
		return fmt.Errorf("scanner: scan row into %s: %w", p.Table.Name, err)
	}
	return nil
}

// ScanAll decodes every remaining row in rows into a freshly allocated slice
// of T, using the compiled Plan. T must be the same struct type used to
// build p.Table.
func ScanAll[T any](rows RowsSource, p *Plan) ([]T, error) {
	defer rows.Close()
	// Pre-sizing at a small default avoids the first few append-triggered
	// reallocations/copies that grow-from-nil would otherwise cost on every
	// call; callers with a known LIMIT could size this exactly in a future
	// revision by threading the limit through from pkg/orm.
	out := make([]T, 0, 16)
	for rows.Next() {
		var v T
		ptr := unsafe.Pointer(&v)
		if err := p.ScanRow(rows, ptr); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanner: row iteration: %w", err)
	}
	return out, nil
}
