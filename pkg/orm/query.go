package orm

import (
        "context"
        "database/sql"
        "errors"
        "fmt"
        "reflect"
        "time"
        "unsafe"

        "github.com/nelthaarion/breezeorm/pkg/compiler"
        "github.com/nelthaarion/breezeorm/pkg/dialect"
        ormdriver "github.com/nelthaarion/breezeorm/pkg/driver"
        "github.com/nelthaarion/breezeorm/pkg/execution"
        "github.com/nelthaarion/breezeorm/pkg/hooks"
        "github.com/nelthaarion/breezeorm/pkg/metadata"
        "github.com/nelthaarion/breezeorm/pkg/planner"
        "github.com/nelthaarion/breezeorm/pkg/query"
        "github.com/nelthaarion/breezeorm/pkg/scanner"
        "github.com/nelthaarion/breezeorm/pkg/transaction"
)

// Query is the fluent query type you build with orm.Model[T](db). Every
// method (Where, OrderBy, Limit, ...) returns a brand-new *Query[T], so
// you can branch queries safely:
//
//      base := orm.Model[User](db).Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true})
//      admins := base.Where(query.Predicate{Column: "role", Op: query.OpEq, Value: "admin"})
//      // base is untouched — admins is base plus one more predicate.
//
// The embedded query.Builder[T] is immutable (copy-on-write), so this
// branching is cheap and race-free.
type Query[T any] struct {
        db    *DB
        b     query.Builder[T]
        table *metadata.Table
}

// Model starts a new query for type T against db. This is the entry point
// for every operation — Find, First, Create, UpdateAll, Delete all chain
// from here.
//
// Compile errors (bad struct tags, non-struct type) are surfaced lazily on
// first execution rather than panicking here, so callers can still chain
// safely. The returned *Query[T] has a nil table in that case; every
// terminal method checks for nil and returns the error.
func Model[T any](db *DB) *Query[T] {
        tbl, err := metadata.Compile[T]()
        if err != nil {
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

// compileCached is the heart of the "compile once" design. It looks up a
// CompiledQuery by PreHash *before* paying for the planner or optimizer at
// all. On a hit, the entire compile pipeline is skipped.
//
// It uses GetOrCompute (not check-then-Set) so concurrent first-time callers
// for a new query shape coalesce into a single Compile call. Without that,
// N goroutines hitting a cold cache for the same shape would each run the
// full planner+optimizer+PlanPhysical pipeline independently, and only the
// last writer's result would stick — the other N-1 compiles pure waste.
//
// SECURITY: if the plugin chain contains a request-scoped plugin (like
// MultiTenancy, which injects a per-request tenant predicate), the cache is
// bypassed entirely. A plan baked with one tenant's predicate must never be
// served to a different tenant — that's a cross-tenant data leak. For an
// empty chain (the common case) IsCacheSafe returns true after a single
// len()==0 check, so there's no overhead when no plugins are registered.
func compileCached[T any](ctx context.Context, db *DB, b query.Builder[T]) (*compiler.CompiledQuery, error) {
        key := compiler.PreHash(b, db.dialect.Name())
        if !db.plugins.IsCacheSafe() {
                // Request-scoped plugin present — every call gets a fresh compile.
                // This is correct (the plan may vary by request) and the only cost
                // is recompiling, which is what the pre-cache code did every call.
                return compiler.Compile(ctx, b, db.dialect, db.passes, db.plugins)
        }
        return db.compiledCache.GetOrCompute(key, func() (*compiler.CompiledQuery, error) {
                return compiler.Compile(ctx, b, db.dialect, db.passes, db.plugins)
        })
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
        args, err := execution.ExtractArgsFromBuilder(b)
        if err != nil {
                return nil, nil, nil, fmt.Errorf("orm: extract args: %w", err)
        }
        gen, err := q.db.executor.Resolve(cq, args)
        if err != nil {
                return nil, nil, nil, fmt.Errorf("orm: generate SQL: %w", err)
        }

        // Plugin hooks (BeforeExecute/AfterExecute). Guarded by len()==0 so
        // the zero-plugin case (the benchmark default) pays only a single
        // slice-length check — no time.Now(), no function call. When plugins
        // ARE registered, BeforeExecute fires before the DB round trip and
        // AfterExecute fires after it with the wall-clock duration + error,
        // enabling Auditing/Tracing/Metrics plugins to observe every query.
        var pluginStart time.Time
        if len(q.db.plugins) > 0 {
                ctx, err = q.db.plugins.RunBeforeExecute(ctx, gen.SQL, gen.Args)
                if err != nil {
                        return nil, nil, nil, fmt.Errorf("orm: plugin BeforeExecute: %w", err)
                }
                pluginStart = time.Now()
        }
        rows, err := q.db.executor.Query(ctx, gen)
        if len(q.db.plugins) > 0 {
                q.db.plugins.RunAfterExecute(ctx, gen.SQL, int64(time.Since(pluginStart)), err)
        }
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
//
// If a generated FastScanFunc[T] is registered for this query's CacheKey
// (see pkg/scanner/registry.go + pkg/scanner/gen), Find dispatches through
// scanner.ScanAllHintFast instead of ScanAllHint. The fast path skips the
// entire Assignments loop + per-column type switch + targetsPool — the
// generated function writes `&dest.Field` pointers straight into
// rows.Scan, matching hand-written sqlx. Falls back to the reflection-free
// ScanAllHint path (which already inlines its own type switch) when no
// generated scanner is registered, so un-generated models are unchanged.
func (q *Query[T]) Find(ctx context.Context) ([]T, error) {
        cq, rows, plan, err := q.queryAndPlan(ctx)
        if err != nil {
                return nil, err
        }
        if fn, ok := scanner.LookupFastScan[T](cq.CacheKey); ok {
                return scanner.ScanAllHintFast[T](rows, fn, resultSizeHint(cq.Physical))
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
//
// Like Find, First dispatches through scanner.ScanOneFast when a generated
// FastScanFunc is registered for the (Limit(1)-appended) query shape —
// skipping the Plan + Assignments loop + targetsPool entirely on the
// FindByID hot path.
func (q *Query[T]) First(ctx context.Context) (*T, error) {
        cq, rows, plan, err := q.queryAndPlanBuilder(ctx, q.b.Limit(1))
        if err != nil {
                return nil, err
        }
        var v *T
        if fn, ok := scanner.LookupFastScan[T](cq.CacheKey); ok {
                v, err = scanner.ScanOneFast[T](rows, fn)
        } else {
                v, err = scanner.ScanOne[T](rows, plan)
        }
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
        args, err := execution.ExtractArgsFromBuilder(counted.b)
        if err != nil {
                return 0, err
        }
        gen, err := q.db.executor.Resolve(cq, args)
        if err != nil {
                return 0, err
        }
        // Plugin hooks — same zero-cost-when-empty guard as queryAndPlanBuilder.
        var pluginStart time.Time
        if len(q.db.plugins) > 0 {
                ctx, err = q.db.plugins.RunBeforeExecute(ctx, gen.SQL, gen.Args)
                if err != nil {
                        return 0, fmt.Errorf("orm: plugin BeforeExecute: %w", err)
                }
                pluginStart = time.Now()
        }
        rows, err := q.db.executor.Query(ctx, gen)
        if len(q.db.plugins) > 0 {
                q.db.plugins.RunAfterExecute(ctx, gen.SQL, int64(time.Since(pluginStart)), err)
        }
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

        cols, colMeta := batchColumns(q.table)
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
                                // rowValues now takes the precomputed colMeta slice (built
                                // once per batch above) instead of rebuilding a per-row
                                // name→FieldIndex map. See rowValues's doc comment.
                                rows = append(rows, rowValues(&models[i], colMeta))
                        }
                        gen, err := execution.GenerateBulkInsert(q.db.dialect, q.table.Name, cols, rows)
                        if err != nil {
                                return err
                        }
                        res, err := q.db.execWithPlugins(txCtx, gen)
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

// Create inserts model. When the dialect supports RETURNING (Postgres does),
// auto-generated fields like serial primary keys get populated back onto the
// struct after insert.
//
// Lifecycle hooks (BeforeCreate / AfterCreate) fire if the model's type
// implements them. The check is a single type assertion that returns nil
// immediately when the interface isn't satisfied — so models without hooks
// (the common case) pay no real cost.
func (q *Query[T]) Create(ctx context.Context, model *T) error {
        if q.table == nil {
                return fmt.Errorf("orm: model type not compilable")
        }
        if err := hooks.RunBeforeCreate(ctx, model); err != nil {
                return fmt.Errorf("orm: BeforeCreate hook: %w", err)
        }
        assignments := structAssignments(q.table, model, false)
        b := query.New[T](q.table.Name).Insert(assignments...)
        cq, err := compileCached(ctx, q.db, b)
        if err != nil {
                return err
        }
        args, err := execution.ExtractArgsFromBuilder(b)
        if err != nil {
                return err
        }
        gen, err := q.db.executor.Resolve(cq, args)
        if err != nil {
                return err
        }
        _, err = q.db.execWithPlugins(ctx, gen)
        if err != nil {
                return err
        }
        if err := hooks.RunAfterCreate(ctx, model); err != nil {
                return fmt.Errorf("orm: AfterCreate hook: %w", err)
        }
        return nil
}

// UpdateAll applies assignments to every row matching the current WHERE
// clause and returns the number of affected rows.
func (q *Query[T]) UpdateAll(ctx context.Context, assignments ...query.Assignment) (int64, error) {
        b := q.b.Update(assignments...)
        cq, err := compileCached(ctx, q.db, b)
        if err != nil {
                return 0, err
        }
        args, err := execution.ExtractArgsFromBuilder(b)
        if err != nil {
                return 0, err
        }
        gen, err := q.db.executor.Resolve(cq, args)
        if err != nil {
                return 0, err
        }
        res, err := q.db.execWithPlugins(ctx, gen)
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
        args, err := execution.ExtractArgsFromBuilder(b)
        if err != nil {
                return 0, err
        }
        gen, err := q.db.executor.Resolve(cq, args)
        if err != nil {
                return 0, err
        }
        res, err := q.db.execWithPlugins(ctx, gen)
        if err != nil {
                return 0, err
        }
        return res.RowsAffected()
}

// execWithPlugins wraps executor.Exec with BeforeExecute/AfterExecute plugin
// hooks. Same zero-cost-when-empty guard as the read path: when no plugins
// are registered (the benchmark default), the only cost is a single
// len()==0 check — no time.Now(), no function call. When plugins ARE
// registered, Auditing/Tracing/Metrics get to observe every write.
func (db *DB) execWithPlugins(ctx context.Context, gen *execution.GeneratedSQL) (ormdriver.Result, error) {
        var pluginStart time.Time
        if len(db.plugins) > 0 {
                var err error
                ctx, err = db.plugins.RunBeforeExecute(ctx, gen.SQL, gen.Args)
                if err != nil {
                        return nil, fmt.Errorf("orm: plugin BeforeExecute: %w", err)
                }
                pluginStart = time.Now()
        }
        res, err := db.executor.Exec(ctx, gen)
        if len(db.plugins) > 0 {
                db.plugins.RunAfterExecute(ctx, gen.SQL, int64(time.Since(pluginStart)), err)
        }
        return res, err
}

// batchColumns returns the column list CreateBatch inserts into — same
// selection rule as structAssignments (skip autoincrement/generated columns)
// but computed once per batch rather than once per row. It ALSO returns a
// parallel slice of *metadata.Column so the per-row extractor can use the
// precomputed Offset (unsafe pointer arithmetic) instead of building a
// per-row name→FieldIndex map and walking it via reflect.FieldByIndex.
func batchColumns(tbl *metadata.Table) ([]string, []*metadata.Column) {
        cols := make([]string, 0, len(tbl.Columns))
        matched := make([]*metadata.Column, 0, len(tbl.Columns))
        for i := range tbl.Columns {
                c := &tbl.Columns[i]
                if c.IsAutoIncr || c.IsGenerated {
                        continue
                }
                cols = append(cols, c.Name)
                matched = append(matched, c)
        }
        return cols, matched
}

// rowValues extracts one row's values in column order, using the precomputed
// column metadata (from batchColumns) to address each field by its unsafe
// Offset — no per-row map, no reflect.FieldByIndex walk.
//
// This used to be a real allocation hotspot: the old version built a
// map[string][]int of every column's FieldIndex on EVERY row. For a 5000-row
// batch that's 5000 throwaway maps plus 5000 × (field count) FieldByIndex
// calls, all computing the same answer. Now it's a single pointer add per
// column, matching what the scanner already does on the read side.
func rowValues[T any](model *T, colMeta []*metadata.Column) []any {
        out := make([]any, len(colMeta))
        // Safe: model is a *T pointing to a live struct owned by the caller
        // for the duration of this call.
        dest := unsafe.Pointer(model)
        for i, c := range colMeta {
                fieldPtr := unsafe.Pointer(uintptr(dest) + c.Offset)
                // reflect.NewAt(t, ptr).Elem().Interface() is the standard way to
                // read a typed value out of an unsafe pointer using the column's
                // cached reflect.Type. It's cheaper than FieldByIndex (which
                // re-walks the embedding chain every call) because the offset is
                // already known. A future codegen pass would replace even this with
                // a direct `*(*T)(ptr)` cast per known column type.
                out[i] = reflect.NewAt(c.Type, fieldPtr).Elem().Interface()
        }
        return out
}

// structAssignments extracts (column, value) pairs for an INSERT or UPDATE,
// using the precomputed Offset on each metadata.Column instead of walking
// the struct via reflect.FieldByIndex. Same "compile once, offset forever"
// principle the scanner follows — the offset was already computed at
// metadata.Compile time but the old version didn't use it here.
func structAssignments(tbl *metadata.Table, model any, includeAutoIncrement bool) []query.Assignment {
        v := reflect.ValueOf(model)
        for v.Kind() == reflect.Ptr {
                v = v.Elem()
        }
        // v.UnsafeAddr() is valid because v is addressable (it came from
        // dereferencing a pointer above). This is the base pointer the Offset
        // is relative to.
        dest := unsafe.Pointer(v.UnsafeAddr())
        out := make([]query.Assignment, 0, len(tbl.Columns))
        for i := range tbl.Columns {
                c := &tbl.Columns[i]
                if c.IsAutoIncr && !includeAutoIncrement {
                        continue
                }
                if c.IsGenerated {
                        continue
                }
                fieldPtr := unsafe.Pointer(uintptr(dest) + c.Offset)
                val := reflect.NewAt(c.Type, fieldPtr).Elem().Interface()
                out = append(out, query.Assignment{Column: c.Name, Value: val})
        }
        return out
}
