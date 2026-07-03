package query

import "github.com/nelthaarion/breezeorm/pkg/dialect"

// PreloadSpec describes an eager-load / preload request, potentially nested
// (e.g. "Posts.Comments.Author") and conditional (extra Where applied to the
// related query).
type PreloadSpec struct {
        Path  string // dot-separated relation path, e.g. "Posts.Comments"
        Where Expr
        Limit *int64
        Batch bool // batch-load using WHERE IN(...) rather than N+1
}

// Kind identifies the statement type a Builder represents.
type Kind uint8

const (
        KindSelect Kind = iota
        KindInsert
        KindUpdate
        KindDelete
        KindUpsert
)

// Assignment is a single column=value pair for INSERT/UPDATE.
type Assignment struct {
        Column string
        Value  any
}

// Builder is the immutable, generic fluent query AST for a model type T.
// Every method returns a new Builder value (Go struct value semantics give
// us copy-on-write for free — no pointer aliasing, no shared mutable state).
// This is what makes instances safe to branch, cache, and reuse concurrently.
type Builder[T any] struct {
        kind Kind

        table    string
        distinct bool
        selects  []SelectExpr
        joins    []Join
        where    Expr
        groupBy  []string
        having   Expr
        orderBy  []OrderTerm

        // limit/offset are stored as value+flag rather than *int64 so that
        // Limit(n)/Offset(n) — called on essentially every read query — don't
        // heap-allocate a pointer on every call. LimitVal()/OffsetVal() still
        // hand back *int64 (unchanged external API, nil when unset) by
        // materializing a pointer lazily, but that only happens on the
        // planner.Build path, which runs once per query shape thanks to
        // DB.compiledCache — not once per call. PreHash (which runs on every
        // call, hit or miss) uses HasLimit()/HasOffset() for this reason.
        hasLimit  bool
        limitN    int64
        hasOffset bool
        offsetN   int64

        ctes     []CTE
        unions   []unionClause
        preloads []PreloadSpec
        lock     dialect.LockMode

        assignments      []Assignment // INSERT/UPDATE values
        upsertConflict   dialect.UpsertConflictTarget
        upsertUpdateCols []string

        namedParams map[string]any
        cursorAfter any // opaque cursor token for cursor pagination
}

type unionClause struct {
        all   bool
        other any // *Builder[T]
}

// New starts a new immutable query builder rooted at the given table.
func New[T any](table string) Builder[T] {
        return Builder[T]{kind: KindSelect, table: table}
}

// --- copy-on-write helper -------------------------------------------------

// clone returns a shallow copy of b. Slice fields still point at b's original
// backing arrays here — safety comes from an invariant every mutator below
// preserves: every slice field is always built at exact capacity (len == cap).
// Given that, a mutator doing append(b.field, x) is guaranteed to allocate a
// fresh backing array before writing (append can't satisfy the growth from
// existing capacity since there isn't any) — so it never touches b's array.
// This lets each mutator use one append() instead of two.
func (b Builder[T]) clone() Builder[T] {
        return b
}

// --- projection ------------------------------------------------------------

func (b Builder[T]) Select(exprs ...SelectExpr) Builder[T] {
        if len(exprs) == 0 {
                return b
        }
        n := b.clone()
        // One allocation, not two: b.selects is always exact-capacity (see
        // clone's doc), so this append always allocates fresh and never aliases
        // b's backing array.
        n.selects = append(b.selects, exprs...)
        return n
}

func (b Builder[T]) Distinct() Builder[T] {
        n := b.clone()
        n.distinct = true
        return n
}

// --- joins -------------------------------------------------------------

func (b Builder[T]) Join(j Join) Builder[T] {
        n := b.clone()
        n.joins = append(b.joins, j) // exact-capacity, so always allocates fresh
        return n
}

func (b Builder[T]) InnerJoin(table, alias string, on Expr) Builder[T] {
        return b.Join(Join{Kind: JoinInner, Table: table, Alias: alias, Condition: on})
}

func (b Builder[T]) LeftJoin(table, alias string, on Expr) Builder[T] {
        return b.Join(Join{Kind: JoinLeft, Table: table, Alias: alias, Condition: on})
}

func (b Builder[T]) RightJoin(table, alias string, on Expr) Builder[T] {
        return b.Join(Join{Kind: JoinRight, Table: table, Alias: alias, Condition: on})
}

func (b Builder[T]) FullJoin(table, alias string, on Expr) Builder[T] {
        return b.Join(Join{Kind: JoinFull, Table: table, Alias: alias, Condition: on})
}

func (b Builder[T]) CrossJoin(table, alias string) Builder[T] {
        return b.Join(Join{Kind: JoinCross, Table: table, Alias: alias})
}

// --- filtering -----------------------------------------------------------

func (b Builder[T]) Where(e Expr) Builder[T] {
        n := b.clone()
        if n.where == nil {
                n.where = e
        } else {
                n.where = And(n.where, e)
        }
        return n
}

func (b Builder[T]) GroupBy(cols ...string) Builder[T] {
        if len(cols) == 0 {
                return b
        }
        n := b.clone()
        n.groupBy = append(b.groupBy, cols...) // exact-capacity → fresh alloc
        return n
}

func (b Builder[T]) Having(e Expr) Builder[T] {
        n := b.clone()
        if n.having == nil {
                n.having = e
        } else {
                n.having = And(n.having, e)
        }
        return n
}

func (b Builder[T]) OrderBy(terms ...OrderTerm) Builder[T] {
        if len(terms) == 0 {
                return b
        }
        n := b.clone()
        n.orderBy = append(b.orderBy, terms...) // exact-capacity → fresh alloc
        return n
}

func (b Builder[T]) Limit(n int64) Builder[T] {
        c := b.clone()
        c.hasLimit = true
        c.limitN = n
        return c
}

func (b Builder[T]) Offset(n int64) Builder[T] {
        c := b.clone()
        c.hasOffset = true
        c.offsetN = n
        return c
}

// Page applies LIMIT/OFFSET for classic page-number pagination.
func (b Builder[T]) Page(pageNum, pageSize int64) Builder[T] {
        if pageNum < 1 {
                pageNum = 1
        }
        return b.Limit(pageSize).Offset((pageNum - 1) * pageSize)
}

// After sets a cursor-pagination token; the executor translates this into a
// keyset WHERE predicate based on the current OrderBy terms.
func (b Builder[T]) After(cursor any) Builder[T] {
        n := b.clone()
        n.cursorAfter = cursor
        return n
}

func (b Builder[T]) Lock(mode dialect.LockMode) Builder[T] {
        n := b.clone()
        n.lock = mode
        return n
}

// --- CTE / set ops ---------------------------------------------------------

func (b Builder[T]) With(cte CTE) Builder[T] {
        n := b.clone()
        n.ctes = append(b.ctes, cte) // exact-capacity → fresh alloc
        return n
}

func (b Builder[T]) Union(other Builder[T]) Builder[T] {
        n := b.clone()
        n.unions = append(b.unions, unionClause{all: false, other: other})
        return n
}

func (b Builder[T]) UnionAll(other Builder[T]) Builder[T] {
        n := b.clone()
        n.unions = append(b.unions, unionClause{all: true, other: other})
        return n
}

// --- relations ---------------------------------------------------------

func (b Builder[T]) Preload(path string, opts ...func(*PreloadSpec)) Builder[T] {
        spec := PreloadSpec{Path: path}
        for _, o := range opts {
                o(&spec)
        }
        n := b.clone()
        n.preloads = append(b.preloads, spec)
        return n
}

func WithPreloadWhere(e Expr) func(*PreloadSpec) {
        return func(s *PreloadSpec) { s.Where = e }
}

func WithPreloadLimit(n int64) func(*PreloadSpec) {
        return func(s *PreloadSpec) { s.Limit = &n }
}

func WithBatchPreload() func(*PreloadSpec) {
        return func(s *PreloadSpec) { s.Batch = true }
}

// --- mutation builders ---------------------------------------------------

func (b Builder[T]) Insert(assignments ...Assignment) Builder[T] {
        n := b.clone()
        n.kind = KindInsert
        n.assignments = assignments
        return n
}

func (b Builder[T]) Update(assignments ...Assignment) Builder[T] {
        n := b.clone()
        n.kind = KindUpdate
        n.assignments = assignments
        return n
}

func (b Builder[T]) Delete() Builder[T] {
        n := b.clone()
        n.kind = KindDelete
        return n
}

func (b Builder[T]) Upsert(assignments []Assignment, conflict dialect.UpsertConflictTarget, updateCols []string) Builder[T] {
        n := b.clone()
        n.kind = KindUpsert
        n.assignments = assignments
        n.upsertConflict = conflict
        n.upsertUpdateCols = updateCols
        return n
}

// --- introspection (used by pkg/compiler) ---------------------------------

func (b Builder[T]) Table() string             { return b.table }
func (b Builder[T]) StmtKind() Kind            { return b.kind }
func (b Builder[T]) Distincted() bool          { return b.distinct }
func (b Builder[T]) Selects() []SelectExpr     { return b.selects }
func (b Builder[T]) Joins() []Join             { return b.joins }
func (b Builder[T]) WhereExpr() Expr           { return b.where }
func (b Builder[T]) GroupByCols() []string     { return b.groupBy }
func (b Builder[T]) HavingExpr() Expr          { return b.having }
func (b Builder[T]) OrderByTerms() []OrderTerm { return b.orderBy }
func (b Builder[T]) LimitVal() *int64 {
        if !b.hasLimit {
                return nil
        }
        n := b.limitN
        return &n
}
func (b Builder[T]) OffsetVal() *int64 {
        if !b.hasOffset {
                return nil
        }
        n := b.offsetN
        return &n
}
func (b Builder[T]) CTEs() []CTE                { return b.ctes }
func (b Builder[T]) Preloads() []PreloadSpec    { return b.preloads }
func (b Builder[T]) LockMode() dialect.LockMode { return b.lock }
func (b Builder[T]) Assignments() []Assignment  { return b.assignments }
func (b Builder[T]) UpsertConflict() dialect.UpsertConflictTarget {
        return b.upsertConflict
}
func (b Builder[T]) UpsertUpdateCols() []string { return b.upsertUpdateCols }
func (b Builder[T]) CursorAfter() any           { return b.cursorAfter }

// HasLimit / HasOffset report whether Limit/Offset were set, without paying
// for LimitVal()/OffsetVal()'s pointer allocation. compiler.PreHash — which
// runs on every call, hit or miss — only needs the yes/no, so it uses these.
func (b Builder[T]) HasLimit() bool  { return b.hasLimit }
func (b Builder[T]) HasOffset() bool { return b.hasOffset }
