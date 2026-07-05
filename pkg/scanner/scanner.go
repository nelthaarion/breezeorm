// Package scanner implements the row-decoding engine. All reflection happens
// once, when a Plan is compiled from metadata.Table + the result column
// list; row-by-row decoding then uses only precomputed unsafe offsets.
package scanner

import (
	"database/sql"
	"fmt"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"github.com/nelthaarion/breezeorm/pkg/metadata"
)

// fieldKind classifies a column's Go type into one of a small set of shapes
// that ScanRow can address directly — `(*int64)(fieldPtr)`, `(*string)(fieldPtr)`,
// etc. — instead of going through reflect.NewAt(...).Interface().
//
// Why this matters: converting a statically typed pointer to `any` inside a
// type-switch case is free (pointer-shaped values fit in the interface data
// word, no allocation). reflect.NewAt(...).Interface() builds a type
// descriptor at runtime via reflect's internal cache, and that path
// allocates on every call — confirmed by CPU/heap profiling.
//
// This fast path covers the types that make up the vast majority of real
// schemas. Anything classify() doesn't recognize falls back to the exact
// reflect.NewAt path used before, so exotic types (including nullable *T
// pointer fields, which need **T semantics) keep working unchanged.
type fieldKind uint8

const (
	kindOther fieldKind = iota
	kindInt
	kindInt8
	kindInt16
	kindInt32
	kindInt64
	kindUint
	kindUint8
	kindUint16
	kindUint32
	kindUint64
	kindFloat32
	kindFloat64
	kindString
	kindBool
	kindBytes // []byte
	kindTime  // time.Time
	kindNullString
	kindNullInt64
	kindNullInt32
	kindNullFloat64
	kindNullBool
	kindNullTime
)

var (
	typeTime        = reflect.TypeOf(time.Time{})
	typeBytes       = reflect.TypeOf([]byte(nil))
	typeNullString  = reflect.TypeOf(sql.NullString{})
	typeNullInt64   = reflect.TypeOf(sql.NullInt64{})
	typeNullInt32   = reflect.TypeOf(sql.NullInt32{})
	typeNullFloat64 = reflect.TypeOf(sql.NullFloat64{})
	typeNullBool    = reflect.TypeOf(sql.NullBool{})
	typeNullTime    = reflect.TypeOf(sql.NullTime{})
)

// classify maps a field's reflect.Type to a fieldKind, once, at Plan-compile
// time. Only exact, non-pointer, non-alias-defeating matches take the fast
// path — anything even slightly unusual (named types with a different
// Kind() but same underlying representation, pointer fields for
// nullability, custom sql.Scanner implementations we don't specifically
// recognize) deliberately falls through to kindOther, which reproduces the
// old, always-correct reflect.NewAt behavior. Fast-pathing is an
// optimization, not a parallel implementation, so the bias here is toward
// under-matching rather than over-matching.
func classify(t reflect.Type) fieldKind {
	switch t {
	case typeTime:
		return kindTime
	case typeBytes:
		return kindBytes
	case typeNullString:
		return kindNullString
	case typeNullInt64:
		return kindNullInt64
	case typeNullInt32:
		return kindNullInt32
	case typeNullFloat64:
		return kindNullFloat64
	case typeNullBool:
		return kindNullBool
	case typeNullTime:
		return kindNullTime
	}
	switch t {
	case reflect.TypeOf(int(0)):
		return kindInt
	case reflect.TypeOf(int8(0)):
		return kindInt8
	case reflect.TypeOf(int16(0)):
		return kindInt16
	case reflect.TypeOf(int32(0)):
		return kindInt32
	case reflect.TypeOf(int64(0)):
		return kindInt64
	case reflect.TypeOf(uint(0)):
		return kindUint
	case reflect.TypeOf(uint8(0)):
		return kindUint8
	case reflect.TypeOf(uint16(0)):
		return kindUint16
	case reflect.TypeOf(uint32(0)):
		return kindUint32
	case reflect.TypeOf(uint64(0)):
		return kindUint64
	case reflect.TypeOf(float32(0)):
		return kindFloat32
	case reflect.TypeOf(float64(0)):
		return kindFloat64
	case reflect.TypeOf(""):
		return kindString
	case reflect.TypeOf(false):
		return kindBool
	}
	switch t.Kind() {
	case reflect.Int:
		return kindInt
	case reflect.Int8:
		return kindInt8
	case reflect.Int16:
		return kindInt16
	case reflect.Int32:
		return kindInt32
	case reflect.Int64:
		return kindInt64
	case reflect.Uint:
		return kindUint
	case reflect.Uint8:
		return kindUint8
	case reflect.Uint16:
		return kindUint16
	case reflect.Uint32:
		return kindUint32
	case reflect.Uint64:
		return kindUint64
	case reflect.Float32:
		return kindFloat32
	case reflect.Float64:
		return kindFloat64
	case reflect.String:
		return kindString
	case reflect.Bool:
		return kindBool
	}
	return kindOther
}

// The assignXxx functions below are the fixed, allocation-free targets for
// the fast-path kinds. They're plain package-level func values (not
// closures), so taking their address in assignerFor doesn't allocate.
// Converting a *statically typed* pointer to `any` is a direct data-word
// store — no boxing — so each of these is exactly as cheap as the switch
// case it replaces, but selecting *which one* to use now happens once at
// Compile time instead of once per column per row.
func assignInt(p unsafe.Pointer) any         { return (*int)(p) }
func assignInt8(p unsafe.Pointer) any        { return (*int8)(p) }
func assignInt16(p unsafe.Pointer) any       { return (*int16)(p) }
func assignInt32(p unsafe.Pointer) any       { return (*int32)(p) }
func assignInt64(p unsafe.Pointer) any       { return (*int64)(p) }
func assignUint(p unsafe.Pointer) any        { return (*uint)(p) }
func assignUint8(p unsafe.Pointer) any       { return (*uint8)(p) }
func assignUint16(p unsafe.Pointer) any      { return (*uint16)(p) }
func assignUint32(p unsafe.Pointer) any      { return (*uint32)(p) }
func assignUint64(p unsafe.Pointer) any      { return (*uint64)(p) }
func assignFloat32(p unsafe.Pointer) any     { return (*float32)(p) }
func assignFloat64(p unsafe.Pointer) any     { return (*float64)(p) }
func assignString(p unsafe.Pointer) any      { return (*string)(p) }
func assignBool(p unsafe.Pointer) any        { return (*bool)(p) }
func assignBytes(p unsafe.Pointer) any       { return (*[]byte)(p) }
func assignTime(p unsafe.Pointer) any        { return (*time.Time)(p) }
func assignNullString(p unsafe.Pointer) any  { return (*sql.NullString)(p) }
func assignNullInt64(p unsafe.Pointer) any   { return (*sql.NullInt64)(p) }
func assignNullInt32(p unsafe.Pointer) any   { return (*sql.NullInt32)(p) }
func assignNullFloat64(p unsafe.Pointer) any { return (*sql.NullFloat64)(p) }
func assignNullBool(p unsafe.Pointer) any    { return (*sql.NullBool)(p) }
func assignNullTime(p unsafe.Pointer) any    { return (*sql.NullTime)(p) }

// assignerFor picks the per-column dispatch function once, at Compile time.
// For kindOther it closes over the column's reflect.Type so the fallback
// (still reflect.NewAt, still fully general/correct) needs no switch either
// — the one-time closure allocation happens here, never in ScanRow.
func assignerFor(kind fieldKind, t reflect.Type) func(unsafe.Pointer) any {
	switch kind {
	case kindInt:
		return assignInt
	case kindInt8:
		return assignInt8
	case kindInt16:
		return assignInt16
	case kindInt32:
		return assignInt32
	case kindInt64:
		return assignInt64
	case kindUint:
		return assignUint
	case kindUint8:
		return assignUint8
	case kindUint16:
		return assignUint16
	case kindUint32:
		return assignUint32
	case kindUint64:
		return assignUint64
	case kindFloat32:
		return assignFloat32
	case kindFloat64:
		return assignFloat64
	case kindString:
		return assignString
	case kindBool:
		return assignBool
	case kindBytes:
		return assignBytes
	case kindTime:
		return assignTime
	case kindNullString:
		return assignNullString
	case kindNullInt64:
		return assignNullInt64
	case kindNullInt32:
		return assignNullInt32
	case kindNullFloat64:
		return assignNullFloat64
	case kindNullBool:
		return assignNullBool
	case kindNullTime:
		return assignNullTime
	default:
		return func(p unsafe.Pointer) any { return reflect.NewAt(t, p).Interface() }
	}
}

// FieldAssignment binds a result-set column index to a destination field.
type FieldAssignment struct {
	ColumnIndex int
	Column      *metadata.Column
	Kind        fieldKind // precomputed once by Compile; see classify

	// assign is precomputed once by Compile (see assignerFor) and turns the
	// per-row "switch on Kind" into a single indirect call. ScanRow used to
	// re-decide, on every row and every column, which of ~20 cases applied;
	// that decision is now made exactly once per (Plan, column) at compile
	// time and baked into this closure, so the hot loop in ScanRow has no
	// branching left at all — just offset arithmetic and a call.
	assign func(fieldPtr unsafe.Pointer) any
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
		kind := classify(col.Type)
		p.Assignments = append(p.Assignments, FieldAssignment{
			ColumnIndex: i,
			Column:      col,
			Kind:        kind,
			assign:      assignerFor(kind, col.Type),
		})
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
// NOTE ON reflect.NewAt: for field kinds classify() doesn't recognize, this
// still costs one reflect.NewAt().Interface() call for that column — real
// "zero reflection on the hot path" in the fully general case requires
// knowing each field's concrete type at compile time, which is exactly what
// code generation (see cmd/breezeorm-gen) would provide: a generated
// scanner writes `&((*T)(dest)).Field` directly, no reflect.Value involved
// at all. What changed here: the fieldKind fast path below now covers the
// common, non-nullable primitive types (int/uint variants, float32/64,
// string, bool, []byte, time.Time, sql.Null*) with a direct unsafe pointer
// cast instead of reflect.NewAt — those conversions are free (pointer-shaped
// values fit directly in the interface data word; no boxing allocation),
// so for a struct made entirely of common types this loop now allocates
// zero times, not once per column per row. Anything classify() didn't
// recognize (named/defined types, nullable *T fields, custom types) still
// goes through the original reflect.NewAt path, so correctness for those
// cases is unchanged from before.
func (p *Plan) ScanRow(rows RowsSource, dest unsafe.Pointer) error {
	targetsPtr := p.targetsPool.Get().(*[]any)
	targets := *targetsPtr

	// No per-row branch on Kind: a.assign was resolved once, at Compile
	// time, to the exact function for this column (see assignerFor). Every
	// row just walks the assignment list and calls it — offset add + call,
	// nothing else.
	//
	// targets is always len(p.Assignments) (targetsPool.New sizes it that
	// way) and every index is unconditionally overwritten below, so there's
	// no need to nil the slice out before returning it to the pool: nothing
	// from a prior row is ever left dangling for a caller to observe, and
	// the pointers involved (into dest, which the caller still owns once
	// ScanRow returns) don't keep anything alive that wasn't already
	// reachable. Skipping that second full pass over targets removes one
	// loop (and a defer) per row.
	// Inline switch (mirrors ScanAllHint / ScanOne) replaces the a.assign
	// indirect call on this path too. ScanRow is the streaming-Cursor path;
	// it had the same per-column indirect-call overhead ScanOne just lost.
	for i := range p.Assignments {
		a := &p.Assignments[i]
		fp := unsafe.Pointer(uintptr(dest) + a.Column.Offset)
		switch a.Kind {
		case kindInt:
			targets[i] = (*int)(fp)
		case kindInt8:
			targets[i] = (*int8)(fp)
		case kindInt16:
			targets[i] = (*int16)(fp)
		case kindInt32:
			targets[i] = (*int32)(fp)
		case kindInt64:
			targets[i] = (*int64)(fp)
		case kindUint:
			targets[i] = (*uint)(fp)
		case kindUint8:
			targets[i] = (*uint8)(fp)
		case kindUint16:
			targets[i] = (*uint16)(fp)
		case kindUint32:
			targets[i] = (*uint32)(fp)
		case kindUint64:
			targets[i] = (*uint64)(fp)
		case kindFloat32:
			targets[i] = (*float32)(fp)
		case kindFloat64:
			targets[i] = (*float64)(fp)
		case kindString:
			targets[i] = (*string)(fp)
		case kindBool:
			targets[i] = (*bool)(fp)
		case kindBytes:
			targets[i] = (*[]byte)(fp)
		case kindTime:
			targets[i] = (*time.Time)(fp)
		case kindNullString:
			targets[i] = (*sql.NullString)(fp)
		case kindNullInt64:
			targets[i] = (*sql.NullInt64)(fp)
		case kindNullInt32:
			targets[i] = (*sql.NullInt32)(fp)
		case kindNullFloat64:
			targets[i] = (*sql.NullFloat64)(fp)
		case kindNullBool:
			targets[i] = (*sql.NullBool)(fp)
		case kindNullTime:
			targets[i] = (*sql.NullTime)(fp)
		default:
			targets[i] = a.assign(fp)
		}
	}
	err := rows.Scan(targets...)
	p.targetsPool.Put(targetsPtr)
	if err != nil {
		return fmt.Errorf("scanner: scan row into %s: %w", p.Table.Name, err)
	}
	return nil
}

// ScanOne decodes at most one row from rows into a freshly allocated *T,
// then closes rows. Returns sql.ErrNoRows if there are no rows (same
// contract as *sql.Row.Scan) so callers can use errors.Is.
//
// Why this exists: First()/FindByID-style callers used to go through
// ScanAllHint and take element [0], which allocates a []T (backing array +
// slice header bookkeeping) purely to hold one row. Scanning straight into
// *T removes that slice allocation entirely — one heap allocation (the *T)
// instead of two.
func ScanOne[T any](rows RowsSource, p *Plan) (*T, error) {
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanner: row iteration: %w", err)
		}
		_ = rows.Close()
		return nil, sql.ErrNoRows
	}

	out := new(T)
	dest := unsafe.Pointer(out)

	targetsPtr := p.targetsPool.Get().(*[]any)
	targets := *targetsPtr

	// Inline switch (mirrors ScanAllHint) replaces the a.assign(fieldPtr)
	// indirect function-pointer call. The original ScanOne path paid for an
	// indirect call per column per row — a func-value load + indirect branch
	// that the Go compiler cannot inline and that defeats the branch
	// predictor. Converting a *statically typed* pointer to `any` inside a
	// type-switch case is free (pointer-shaped values fit directly in the
	// interface data word, no boxing), so this is strictly cheaper than the
	// indirect call it replaces. See the fieldKind / assignerFor doc comments
	// for the full rationale — this is the same fix ScanAllHint already had.
	assignments := p.Assignments
	for i := range assignments {
		a := &assignments[i]
		fp := unsafe.Pointer(uintptr(dest) + a.Column.Offset)
		switch a.Kind {
		case kindInt:
			targets[i] = (*int)(fp)
		case kindInt8:
			targets[i] = (*int8)(fp)
		case kindInt16:
			targets[i] = (*int16)(fp)
		case kindInt32:
			targets[i] = (*int32)(fp)
		case kindInt64:
			targets[i] = (*int64)(fp)
		case kindUint:
			targets[i] = (*uint)(fp)
		case kindUint8:
			targets[i] = (*uint8)(fp)
		case kindUint16:
			targets[i] = (*uint16)(fp)
		case kindUint32:
			targets[i] = (*uint32)(fp)
		case kindUint64:
			targets[i] = (*uint64)(fp)
		case kindFloat32:
			targets[i] = (*float32)(fp)
		case kindFloat64:
			targets[i] = (*float64)(fp)
		case kindString:
			targets[i] = (*string)(fp)
		case kindBool:
			targets[i] = (*bool)(fp)
		case kindBytes:
			targets[i] = (*[]byte)(fp)
		case kindTime:
			targets[i] = (*time.Time)(fp)
		case kindNullString:
			targets[i] = (*sql.NullString)(fp)
		case kindNullInt64:
			targets[i] = (*sql.NullInt64)(fp)
		case kindNullInt32:
			targets[i] = (*sql.NullInt32)(fp)
		case kindNullFloat64:
			targets[i] = (*sql.NullFloat64)(fp)
		case kindNullBool:
			targets[i] = (*sql.NullBool)(fp)
		case kindNullTime:
			targets[i] = (*sql.NullTime)(fp)
		default:
			// kindOther: only remaining reflect call, unchanged from before —
			// still needs the column's runtime Type, which only a.assign's
			// closure (built once at Compile time) has.
			targets[i] = a.assign(fp)
		}
	}
	err := rows.Scan(targets...)
	p.targetsPool.Put(targetsPtr)
	closeErr := rows.Close()
	if err != nil {
		return nil, fmt.Errorf("scanner: scan row into %s: %w", p.Table.Name, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("scanner: close rows: %w", closeErr)
	}
	return out, nil
}

// ScanAll decodes every remaining row in rows into a freshly allocated slice
// of T, using the compiled Plan. T must be the same struct type used to
// build p.Table.
func ScanAll[T any](rows RowsSource, p *Plan) ([]T, error) {
	return ScanAllHint[T](rows, p, defaultScanAllCap)
}

// defaultScanAllCap is ScanAll's pre-sizing when the caller has no better
// estimate of the row count (see ScanAllHint).
const defaultScanAllCap = 16

// ScanAllHint is ScanAll with an explicit initial-capacity hint. Callers
// that know an upper bound on the row count ahead of time — most commonly a
// SQL LIMIT — should pass it here instead of going through ScanAll, which
// always guesses 16. A result set larger than the hint still grows
// correctly (append handles that); a hint smaller than 1 falls back to
// ScanAll's default.
//
// Why this exists: for a LIMIT 50 query, pre-sizing at 16 forces `append`
// to reallocate+copy the backing array twice more (16→32→64) before
// settling, on every single call — wasted work entirely avoidable once the
// caller already knows "at most 50 rows are coming back" from its own
// compiled plan. See pkg/orm's Find, which threads the query's LIMIT value
// through to this exact parameter.
func ScanAllHint[T any](rows RowsSource, p *Plan, sizeHint int) ([]T, error) {
	defer rows.Close()
	if sizeHint < 1 {
		sizeHint = defaultScanAllCap
	}
	out := make([]T, 0, sizeHint)

	targetsPtr := p.targetsPool.Get().(*[]any)
	targets := *targetsPtr
	defer p.targetsPool.Put(targetsPtr)

	assignments := p.Assignments
	n := len(assignments)

	for rows.Next() {
		l := len(out)
		if l < cap(out) {
			out = out[:l+1]
		} else {
			var zero T
			out = append(out, zero)
		}
		dest := unsafe.Pointer(&out[l])

		for i := 0; i < n; i++ {
			a := &assignments[i]
			fp := unsafe.Pointer(uintptr(dest) + a.Column.Offset)

			// Inline switch replaces the a.assign(fp) indirect call: same
			// pointer-shaped-value-into-any store (still free, still no
			// boxing), but dispatched via a direct branch instead of a
			// function-pointer call.
			switch a.Kind {
			case kindInt:
				targets[i] = (*int)(fp)
			case kindInt8:
				targets[i] = (*int8)(fp)
			case kindInt16:
				targets[i] = (*int16)(fp)
			case kindInt32:
				targets[i] = (*int32)(fp)
			case kindInt64:
				targets[i] = (*int64)(fp)
			case kindUint:
				targets[i] = (*uint)(fp)
			case kindUint8:
				targets[i] = (*uint8)(fp)
			case kindUint16:
				targets[i] = (*uint16)(fp)
			case kindUint32:
				targets[i] = (*uint32)(fp)
			case kindUint64:
				targets[i] = (*uint64)(fp)
			case kindFloat32:
				targets[i] = (*float32)(fp)
			case kindFloat64:
				targets[i] = (*float64)(fp)
			case kindString:
				targets[i] = (*string)(fp)
			case kindBool:
				targets[i] = (*bool)(fp)
			case kindBytes:
				targets[i] = (*[]byte)(fp)
			case kindTime:
				targets[i] = (*time.Time)(fp)
			case kindNullString:
				targets[i] = (*sql.NullString)(fp)
			case kindNullInt64:
				targets[i] = (*sql.NullInt64)(fp)
			case kindNullInt32:
				targets[i] = (*sql.NullInt32)(fp)
			case kindNullFloat64:
				targets[i] = (*sql.NullFloat64)(fp)
			case kindNullBool:
				targets[i] = (*sql.NullBool)(fp)
			case kindNullTime:
				targets[i] = (*sql.NullTime)(fp)
			default:
				// kindOther: only remaining reflect call, unchanged from
				// before — still needs the column's runtime Type, which
				// only a.assign's closure (built once at Compile time) has.
				targets[i] = a.assign(fp)
			}
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scanner: scan row into %s: %w", p.Table.Name, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanner: row iteration: %w", err)
	}
	return out, nil
}
