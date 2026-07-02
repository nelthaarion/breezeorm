// Package scanner implements the row-decoding engine. All reflection happens
// once, when a Plan is compiled from metadata.Table + the result column
// list; row-by-row decoding then uses only precomputed unsafe offsets.
package scanner

import (
	"fmt"
	"reflect"
	"unsafe"

	"github.com/nelthaarion/breezeorm/pkg/metadata"
)

// FieldAssignment binds a result-set column index to a destination field.
type FieldAssignment struct {
	ColumnIndex int
	Column      *metadata.Column
}

// Plan is the precompiled scan plan for one (Table, result-column-list) pair.
// Building a Plan reflects on the struct type; using a Plan to scan rows does not.
type Plan struct {
	Table       *metadata.Table
	Assignments []FieldAssignment
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
func (p *Plan) ScanRow(rows RowsSource, dest unsafe.Pointer) error {
	targets := make([]any, len(p.Assignments))
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
	var out []T
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
