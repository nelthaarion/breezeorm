package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/nelthaarion/breezeorm/pkg/compiler"
	"github.com/nelthaarion/breezeorm/pkg/dialect"
	"github.com/nelthaarion/breezeorm/pkg/execution"
	"github.com/nelthaarion/breezeorm/pkg/metadata"
	"github.com/nelthaarion/breezeorm/pkg/planner"
	"github.com/nelthaarion/breezeorm/pkg/query"
	"github.com/nelthaarion/breezeorm/pkg/scanner"
	"github.com/nelthaarion/breezeorm/pkg/transaction"
)

// Query is the public, generic fluent query type. Every builder method
// returns a new *Query[T] (the embedded query.Builder[T] is itself
// immutable/copy-on-write), so a Query can be safely branched:
//
//	base := orm.Model[User](db).Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true})
//	admins := base.Where(query.Predicate{Column: "role", Op: query.OpEq, Value: "admin"})
//	// base is untouched; admins is base + one more predicate.
type Query[T any] struct {
	db    *DB
	b     query.Builder[T]
	table *metadata.Table
}

// Model starts a new query for model type T against db.
func Model[T any](db *DB) *Query[T] {
	tbl, err := metadata.Compile[T]()
	if err != nil {
		// Compile errors indicate a programming error (bad struct tags),
		// not a runtime condition — surfaced lazily on first execution
		// instead of panicking here so callers can still chain safely.
		return &Query[T]{db: db, b: query.New[T]("<invalid>"), table: nil}
	}
	return &Query[T]{db: db, b: query.New[T](tbl.Name), table: tbl}
}

func (q *Query[T]) with(b query.Builder[T]) *Query[T] {
	return &Query[T]{db: q.db, b: b, table: q.table}
}

// --- fluent passthroughs to query.Builder ----------------------------------

func (q *Query[T]) Select(exprs ...query.SelectExpr) *Query[T] { return q.with(q.b.Select(exprs...)) }
func (q *Query[T]) Distinct() *Query[T]                        { return q.with(q.b.Distinct()) }
func (q *Query[T]) Where(e query.Expr) *Query[T]               { return q.with(q.b.Where(e)) }
func (q *Query[T]) GroupBy(cols ...string) *Query[T]           { return q.with(q.b.GroupBy(cols...)) }
func (q *Query[T]) Having(e query.Expr) *Query[T]              { return q.with(q.b.Having(e)) }
func (q *Query[T]) OrderBy(terms ...query.OrderTerm) *Query[T] { return q.with(q.b.OrderBy(terms...)) }
func (q *Query[T]) Limit(n int64) *Query[T]                    { return q.with(q.b.Limit(n)) }
func (q *Query[T]) Offset(n int64) *Query[T]                   { return q.with(q.b.Offset(n)) }
func (q *Query[T]) Page(page, size int64) *Query[T]            { return q.with(q.b.Page(page, size)) }
func (q *Query[T]) After(cursor any) *Query[T]                 { return q.with(q.b.After(cursor)) }
func (q *Query[T]) Lock(mode dialect.LockMode) *Query[T]       { return q.with(q.b.Lock(mode)) }
func (q *Query[T]) With(cte query.CTE) *Query[T]               { return q.with(q.b.With(cte)) }

func (q *Query[T]) InnerJoin(table, alias string, on query.Expr) *Query[T] {
	return q.with(q.b.InnerJoin(table, alias, on))
}
func (q *Query[T]) LeftJoin(table, alias string, on query.Expr) *Query[T] {
	return q.with(q.b.LeftJoin(table, alias, on))
}
func (q *Query[T]) RightJoin(table, alias string, on query.Expr) *Query[T] {
	return q.with(q.b.RightJoin(table, alias, on))
}
func (q *Query[T]) FullJoin(table, alias string, on query.Expr) *Query[T] {
	return q.with(q.b.FullJoin(table, alias, on))
}
func (q *Query[T]) CrossJoin(table, alias string) *Query[T] {
	return q.with(q.b.CrossJoin(table, alias))
}

func (q *Query[T]) Preload(path string, opts ...func(*query.PreloadSpec)) *Query[T] {
	return q.with(q.b.Preload(path, opts...))
}

// --- compilation helper ---------------------------------------------------

func (q *Query[T]) compile(ctx context.Context) (*compiler.CompiledQuery, error) {
	if q.table == nil {
		return nil, fmt.Errorf("orm: model type not compilable (check struct tags)")
	}
	return compileCached(ctx, q.db, q.b)
}

// compileCached looks up a CompiledQuery by compiler.PreHash before paying
// for planner.Build/optimizer/PlanPhysical at all; see DB.compiledCache's
// doc comment and compiler.PreHash's CAVEAT for the one correctness
// condition this depends on (no request-varying plugin rewrites).
func compileCached[T any](ctx context.Context, db *DB, b query.Builder[T]) (*compiler.CompiledQuery, error) {
	key := compiler.PreHash(b, db.dialect.Name())
	if cq, ok := db.compiledCache.Get(key); ok {
		return cq, nil
	}
	cq, err := compiler.Compile(ctx, b, db.dialect, db.passes, db.plugins)
	if err != nil {
		return nil, err
	}
	db.compiledCache.Set(key, cq)
	return cq, nil
}

// --- terminal read operations -----------------------------------------------

// queryAndPlan runs compile→resolve→execute and returns the open *Rows plus
// the scan Plan for them, for both Find (ScanAllHint) and First (ScanOne) to
// share — the only difference between the two is which scanner function
// consumes the result.
func (q *Query[T]) queryAndPlan(ctx context.Context) (*compiler.CompiledQuery, *execution.Rows, *scanner.Plan, error) {
	return q.queryAndPlanBuilder(ctx, q.b)
}

func (q *Query[T]) queryAndPlanBuilder(ctx context.Context, b query.Builder[T]) (*compiler.CompiledQuery, *execution.Rows, *scanner.Plan, error) {
	if q.table == nil {
		return nil, nil, nil, fmt.Errorf("orm: model type not compilable (check struct tags)")
	}
	cq, err := compileCached(ctx, q.db, b)
	if err != nil {
		return nil, nil, nil, err
	}
	gen, err := q.db.executor.Resolve(cq)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("orm: generate SQL: %w", err)
	}

	rows, err := q.db.executor.Query(ctx, gen)
	if err != nil {
		return nil, nil, nil, err
	}

	// Reuse the CompiledQuery's CacheKey (already computed by q.compile
	// above) as the scan-plan cache key too: it already captures the exact
	// query shape, which is exactly what determines the result column list.
	// On a hit this skips both rows.Columns() and scanner.Compile's
	// column-to-field matching entirely — the fix for the read-path
	// overhead identified in benchmark/README.md.
	plan, ok := q.db.scanPlanCache.Get(cq.CacheKey)
	if !ok {
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		plan, err = scanner.Compile(q.table, cols)
		if err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		q.db.scanPlanCache.Set(cq.CacheKey, plan)
	}
	return cq, rows, plan, nil
}

// Find executes the query and returns all matching rows.
func (q *Query[T]) Find(ctx context.Context) ([]T, error) {
	cq, rows, plan, err := q.queryAndPlan(ctx)
	if err != nil {
		return nil, err
	}
	return scanner.ScanAllHint[T](rows, plan, resultSizeHint(cq.Physical))
}

// resultSizeHint walks a (cached, already-built) physical plan's node tree
// looking for a NodeLimit, and returns its Limit value as a pre-sizing hint
// for scanner.ScanAllHint — e.g. Limit(50) means "at most 50 rows are
// coming back", so ScanAll's blind cap-16 default (which would otherwise
// force two extra reallocate+copy cycles growing 16→32→64) can be skipped
// entirely. This walk itself is cheap: cq.Physical is already built and
// cached by cq.CacheKey (see compileCached), so this is just following a
// handful of pointers, not a page of query-building work, on every call.
// Returns 0 (→ ScanAllHint's own default) when there's no LIMIT, or the
// query isn't a plain read (Root is nil for INSERT/UPDATE/DELETE bodies
// that don't reach this code path anyway).
func resultSizeHint(pp *planner.PhysicalPlan) int {
	if pp == nil || pp.Logical == nil {
		return 0
	}
	for n := pp.Logical.Root; n != nil; n = n.Input {
		if n.Kind == planner.NodeLimit && n.Limit != nil {
			return int(*n.Limit)
		}
	}
	return 0
}

// First executes the query with an implicit LIMIT 1 and returns a single
// result, or an "orm: no rows found" error when nothing matches. Scans
// directly into *T via scanner.ScanOne — no intermediate []T slice, unlike
// the old Find(ctx)+index[0] implementation.
func (q *Query[T]) First(ctx context.Context) (*T, error) {
	_, rows, plan, err := q.queryAndPlanBuilder(ctx, q.b.Limit(1))
	if err != nil {
		return nil, err
	}
	v, err := scanner.ScanOne[T](rows, plan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("orm: no rows found")
		}
		return nil, err
	}
	return v, nil
}

// Count returns the number of rows matching the current WHERE clause.
func (q *Query[T]) Count(ctx context.Context) (int64, error) {
	counted := q.Select(query.SelectExpr{Expr: "COUNT(*)", Alias: "count"})
	cq, err := counted.compile(ctx)
	if err != nil {
		return 0, err
	}
	gen, err := q.db.executor.Resolve(cq)
	if err != nil {
		return 0, err
	}
	rows, err := q.db.executor.Query(ctx, gen)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// Exists reports whether any row matches the current WHERE clause.
func (q *Query[T]) Exists(ctx context.Context) (bool, error) {
	n, err := q.Limit(1).Count(ctx)
	return n > 0, err
}

// --- terminal write operations ----------------------------------------------

// CreateBatch inserts many rows using multi-row INSERT statements
// (see execution.GenerateBulkInsert) instead of one round trip per row —
// the "batch execution" / "bulk insert" performance feature. Input larger
// than execution.MaxBulkInsertRows is automatically chunked into multiple
// statements, all run inside a single transaction so the batch is atomic:
// either every row is inserted or none are.
func (q *Query[T]) CreateBatch(ctx context.Context, models []T) (int64, error) {
	if q.table == nil {
		return 0, fmt.Errorf("orm: model type not compilable")
	}
	if len(models) == 0 {
		return 0, nil
	}

	cols := batchColumns(q.table)
	var total int64

	err := transaction.Run(ctx, q.db.sqlDB, nil, transaction.DefaultRetryPolicy(), func(txCtx context.Context) error {
		total = 0
		for start := 0; start < len(models); start += execution.MaxBulkInsertRows {
			end := start + execution.MaxBulkInsertRows
			if end > len(models) {
				end = len(models)
			}
			rows := make([][]any, 0, end-start)
			for i := start; i < end; i++ {
				rows = append(rows, rowValues(q.table, &models[i], cols))
			}
			gen, err := execution.GenerateBulkInsert(q.db.dialect, q.table.Name, cols, rows)
			if err != nil {
				return err
			}
			res, err := q.db.executor.Exec(txCtx, gen)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// Create inserts model and, when the dialect supports RETURNING, populates
// auto-generated fields (e.g. serial/identity primary keys) back onto it.
func (q *Query[T]) Create(ctx context.Context, model *T) error {
	if q.table == nil {
		return fmt.Errorf("orm: model type not compilable")
	}
	assignments := structAssignments(q.table, model, false)
	b := query.New[T](q.table.Name).Insert(assignments...)
	cq, err := compileCached(ctx, q.db, b)
	if err != nil {
		return err
	}
	gen, err := q.db.executor.Resolve(cq)
	if err != nil {
		return err
	}
	_, err = q.db.executor.Exec(ctx, gen)
	return err
}

// UpdateAll applies assignments to every row matching the current WHERE
// clause and returns the number of affected rows.
func (q *Query[T]) UpdateAll(ctx context.Context, assignments ...query.Assignment) (int64, error) {
	b := q.b.Update(assignments...)
	cq, err := compileCached(ctx, q.db, b)
	if err != nil {
		return 0, err
	}
	gen, err := q.db.executor.Resolve(cq)
	if err != nil {
		return 0, err
	}
	res, err := q.db.executor.Exec(ctx, gen)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Delete removes every row matching the current WHERE clause and returns the
// number of affected rows. Plugins (e.g. SoftDelete) may rewrite this into
// an UPDATE at the plan level.
func (q *Query[T]) Delete(ctx context.Context) (int64, error) {
	b := q.b.Delete()
	cq, err := compileCached(ctx, q.db, b)
	if err != nil {
		return 0, err
	}
	gen, err := q.db.executor.Resolve(cq)
	if err != nil {
		return 0, err
	}
	res, err := q.db.executor.Exec(ctx, gen)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// batchColumns returns the column list CreateBatch inserts into — same
// selection rule as structAssignments (skip autoincrement/generated columns)
// but computed once per batch rather than once per row.
func batchColumns(tbl *metadata.Table) []string {
	cols := make([]string, 0, len(tbl.Columns))
	for _, c := range tbl.Columns {
		if c.IsAutoIncr || c.IsGenerated {
			continue
		}
		cols = append(cols, c.Name)
	}
	return cols
}

// rowValues extracts one row's values in the same order as cols.
func rowValues[T any](tbl *metadata.Table, model *T, cols []string) []any {
	v := reflect.ValueOf(model).Elem()
	byName := make(map[string][]int, len(tbl.Columns))
	for _, c := range tbl.Columns {
		byName[c.Name] = c.FieldIndex
	}
	out := make([]any, len(cols))
	for i, name := range cols {
		idx := byName[name]
		out[i] = v.FieldByIndex(idx).Interface()
	}
	return out
}

// using already-compiled metadata (no per-call reflection walk of tags —
// only field-value extraction, which needs a reflect.Value regardless of
// how the field was located).
func structAssignments(tbl *metadata.Table, model any, includeAutoIncrement bool) []query.Assignment {
	v := reflect.ValueOf(model)
	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	out := make([]query.Assignment, 0, len(tbl.Columns))
	for _, c := range tbl.Columns {
		if c.IsAutoIncr && !includeAutoIncrement {
			continue
		}
		if c.IsGenerated {
			continue
		}
		fv := v.FieldByIndex(c.FieldIndex)
		out = append(out, query.Assignment{Column: c.Name, Value: fv.Interface()})
	}
	return out
}
