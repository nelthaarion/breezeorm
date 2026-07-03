// Package execution turns a compiled, optimized plan into SQL text and then
// runs it against a real *sql.DB. Prepared-statement and plan caches ensure
// that compilation (and the DB-side parse/plan) happens once per distinct
// query shape.
//
// SECURITY: query.SelectExpr.Expr, query.RawExpr.SQL, and query.CTE column
// lists are trusted, developer-authored SQL fragments — the same trust level
// as a format string passed to fmt.Sprintf. They exist so application code
// can express things the typed builder API doesn't cover (aggregates, window
// functions, vendor-specific expressions). They MUST NEVER be built by
// concatenating end-user input.
//
// Every identifier the builder itself supplies (table names, column names,
// aliases) IS validated and safely quoted below — that boundary is what
// prevents SQL injection through the typed API surface (Where/OrderBy/
// GroupBy/Join/...).
package execution

import (
        "bytes"
        "fmt"
        "strings"

        "github.com/nelthaarion/breezeorm/pkg/dialect"
        "github.com/nelthaarion/breezeorm/pkg/planner"
        "github.com/nelthaarion/breezeorm/pkg/pool"
        "github.com/nelthaarion/breezeorm/pkg/query"
)

// GeneratedSQL is the rendered statement plus its positional bind arguments,
// in the order the dialect's placeholders expect.
type GeneratedSQL struct {
        SQL  string
        Args []any

        // CacheKey mirrors compiler.CompiledQuery.CacheKey (the same shape-hash
        // used by compiledCache/planTextCache/scanPlanCache). Carrying it here
        // lets Executor.prepare key its prepared-statement cache by this
        // existing uint64 instead of hashing gen.SQL's full text on every call.
        CacheKey uint64
}

// GenerateSQL renders a PhysicalPlan into SQL text for its target dialect.
// Every dynamic identifier (table/column/alias) is validated against the
// dialect's ValidateIdentifier before being quoted; validation failures
// short-circuit generation and are returned as an error rather than ever
// reaching the query string.
func GenerateSQL(pp *planner.PhysicalPlan) (*GeneratedSQL, error) {
        root := pp.Logical.Root
        if root == nil {
                return nil, fmt.Errorf("execution: empty plan")
        }

        buf := pool.Buffers.Get()
        defer pool.Buffers.Put(buf)

        g := &sqlGen{d: pp.Dialect, b: buf}

        switch root.Kind {
        case planner.NodeInsert:
                g.genInsert(root)
        case planner.NodeUpdate:
                g.genUpdate(root)
        case planner.NodeDelete:
                g.genDelete(root)
        case planner.NodeUpsert:
                g.genUpsert(root)
        default:
                g.genSelect(root, pp)
        }

        if g.err != nil {
                return nil, g.err
        }
        // b.String() copies out of the pooled buffer, so returning it to the
        // pool afterward (via defer above) is safe: the caller's string is
        // independent of the buffer's backing array.
        return &GeneratedSQL{SQL: g.b.String(), Args: g.args}, nil
}

// ExtractArgsFromBuilder derives the bind-argument list directly from the
// CURRENT call's query.Builder[T] — never from a cached compiler.CompiledQuery
// / planner.PhysicalPlan. This matters for correctness, not just performance:
// db.compiledCache is keyed by structural shape only (see compiler.PreHash /
// structuralHash, which deliberately excludes literal Values so that
// Where(Eq("id", 1)) and Where(Eq("id", 42)) share one cached plan+prepared
// statement). planner.Build bakes each call's concrete Predicate/Assignment
// values directly into the LogicalNode tree it returns — so a cached
// CompiledQuery's cq.Physical is frozen with whichever call's values first
// populated that cache slot. The old ExtractArgs(pp *planner.PhysicalPlan)
// read values out of that shared, frozen tree, meaning every cache hit for
// an already-seen shape silently rebound the FIRST call's values instead of
// the current call's — e.g. FindByID(1) then FindByID(42) would both
// execute "WHERE id = 1" once the shape was cached. This function reads
// straight from b (this call's own accessors — WhereExpr/HavingExpr/Joins/
// Assignments), which is never shared or cached across calls, so it always
// reflects the values actually passed this call.
//
// As a side benefit this also skips the flatten() tree-walk ExtractArgs used
// to locate where/having/joins inside the LogicalNode chain: Builder already
// stores those as direct fields, so locating them is O(1) field access
// instead of a recursive tree search — the traversal that's actually
// per-call work (walking into where/having to collect literal values) is
// unavoidable either way, but finding *where to start* is now free.
//
// Binding order MUST exactly match GenerateSQL's genInsert/genUpdate/
// genSelect (joins, then where, then having for SELECT; assignments, then
// where for UPDATE) — see the argCollector doc comment below.
func ExtractArgsFromBuilder[T any](b query.Builder[T]) ([]any, error) {
        c := &argCollector{}
        switch b.StmtKind() {
        case query.KindInsert, query.KindUpsert:
                as := b.Assignments()
                c.args = make([]any, 0, len(as))
                c.assignments(as)
        case query.KindUpdate:
                as := b.Assignments()
                c.args = make([]any, 0, len(as))
                c.assignments(as)
                c.expr(b.WhereExpr())
        case query.KindDelete:
                c.expr(b.WhereExpr())
        default: // SELECT
                joins := b.Joins()
                where := b.WhereExpr()
                having := b.HavingExpr()
                n := exprArgCount(where) + exprArgCount(having)
                for _, j := range joins {
                        n += exprArgCount(j.Condition)
                }
                c.args = make([]any, 0, n)
                for _, j := range joins {
                        c.expr(j.Condition)
                }
                c.expr(where)
                c.expr(having)
        }
        return c.args, c.err
}

// ExtractArgs re-derives just the bind-argument list for a PhysicalPlan,
// without rebuilding any SQL text.
//
// DEPRECATED for the read/write paths in pkg/orm: reads values out of
// pp (== a cached compiler.CompiledQuery.Physical), which is shared across
// every call for a given query shape — see ExtractArgsFromBuilder's doc
// comment for why that's a correctness bug on cache hits, not just a
// performance concern. Kept only because sqlgen_test.go's
// TestGenerateSQL_ArgsMatchExtractArgs asserts against it for a
// single-call, never-cached PhysicalPlan (where pp's values are guaranteed
// to be this call's own, so the bug doesn't manifest); do not call this
// from any path that reuses a cached CompiledQuery across multiple calls.
func ExtractArgs(pp *planner.PhysicalPlan) ([]any, error) {
        root := pp.Logical.Root
        if root == nil {
                return nil, fmt.Errorf("execution: empty plan")
        }
        c := &argCollector{}
        switch root.Kind {
        case planner.NodeInsert, planner.NodeUpsert:
                // Exact-size preallocation: an INSERT/UPSERT's bind-arg count is
                // precisely len(Assignments) — one value per column, no predicate
                // or IN()-expansion involved. Without this, ExtractArgs (which runs
                // on *every* Resolve call once the SQL text is cached — see
                // Executor.Resolve) built this slice via bare append from a nil
                // slice, costing 3 grow-and-copy reallocations (cap 1→2→4) for a
                // typical 4-column INSERT instead of the 1 allocation this does.
                c.args = make([]any, 0, len(root.Assignments))
                c.assignments(root.Assignments)
        case planner.NodeUpdate:
                // Lower-bound preallocation: at least len(Assignments) values are
                // always bound (the SET list); the WHERE predicate adds more, but
                // starting from this floor instead of nil still avoids the first
                // couple of grow-and-copy steps for the common case.
                c.args = make([]any, 0, len(root.Assignments))
                c.assignments(root.Assignments)
                c.expr(root.Predicate)
        case planner.NodeDelete:
                c.expr(root.Predicate)
        default:
                var parts selectParts
                flatten(root, &parts)
                c.args = make([]any, 0, exprArgCount(parts.where)+exprArgCount(parts.having)+joinArgCount(parts.joins))
                for _, j := range parts.joins {
                        c.expr(j.JoinOn)
                }
                c.expr(parts.where)
                c.expr(parts.having)
        }
        return c.args, c.err
}

func joinArgCount(joins []*planner.LogicalNode) int {
        n := 0
        for _, j := range joins {
                n += exprArgCount(j.JoinOn)
        }
        return n
}

func exprArgCount(e query.Expr) int {
        switch v := e.(type) {
        case nil:
                return 0
        case query.Predicate:
                switch v.Op {
                case query.OpIsNull, query.OpIsNotNull:
                        return 0
                case query.OpIn, query.OpNotIn:
                        vals, _ := v.Value.([]any)
                        return len(vals)
                case query.OpBetween:
                        return 2
                default:
                        return 1
                }
        case query.LogicalExpr:
                n := 0
                for _, ch := range v.Children {
                        n += exprArgCount(ch)
                }
                return n
        case query.RawExpr:
                return len(v.Args)
        default:
                return 0
        }
}

type sqlGen struct {
        d    dialect.Dialect
        b    *bytes.Buffer
        args []any
        err  error
}

func (g *sqlGen) bind(v any) string {
        g.args = append(g.args, v)
        return g.d.Placeholder(len(g.args))
}

// quoteIdent validates then quotes a single dynamic identifier (table,
// column, or alias). Any failure is sticky on g.err — once set, further
// writes are harmless no-ops since GenerateSQL discards the buffer on error.
func (g *sqlGen) quoteIdent(name string) string {
        if g.err != nil {
                return ""
        }
        if err := g.d.ValidateIdentifier(name); err != nil {
                g.err = fmt.Errorf("execution: %w", err)
                return ""
        }
        return g.d.QuoteIdentifier(name)
}

func (g *sqlGen) quoteIdents(names []string) []string {
        out := make([]string, len(names))
        for i, n := range names {
                out[i] = g.quoteIdent(n)
        }
        return out
}

// checkRawFragment is a defense-in-depth guard against stacked-query
// injection in the (trusted, developer-authored) raw-SQL escape hatches —
// it does not replace the requirement that these fragments never contain
// end-user input. A semicolon is never legitimate in a single-statement
// fragment bound via placeholders; literal string values belong in bind
// params, not inlined into the fragment.
func (g *sqlGen) checkRawFragment(s string) {
        if g.err != nil {
                return
        }
        if strings.ContainsRune(s, ';') {
                g.err = fmt.Errorf("execution: raw SQL fragment must not contain a statement separator (';')")
        }
}

// selectParts accumulates clauses discovered while flattening the
// operator-wrapped LogicalNode chain built by planner.Build.
type selectParts struct {
        ctes        []query.CTE
        distinct    bool
        projections []query.SelectExpr
        joins       []*planner.LogicalNode // in original join order
        where       query.Expr
        groupBy     []string
        having      query.Expr
        orderBy     []query.OrderTerm
        limit       *int64
        offset      *int64
        baseTable   string
        baseAlias   string
        lock        dialect.LockMode
}

func flatten(n *planner.LogicalNode, p *selectParts) {
        if n == nil {
                return
        }
        if n.Lock != dialect.LockNone {
                p.lock = n.Lock
        }
        switch n.Kind {
        case planner.NodeCTE:
                p.ctes = n.CTEs
                flatten(n.Input, p)
        case planner.NodeLimit:
                p.limit, p.offset = n.Limit, n.Offset
                flatten(n.Input, p)
        case planner.NodeSort:
                p.orderBy = n.OrderBy
                flatten(n.Input, p)
        case planner.NodeDistinct:
                p.distinct = true
                flatten(n.Input, p)
        case planner.NodeProject:
                p.projections = n.Projections
                flatten(n.Input, p)
        case planner.NodeAggregate:
                p.groupBy, p.having = n.GroupBy, n.Having
                flatten(n.Input, p)
        case planner.NodeFilter:
                p.where = n.Predicate
                flatten(n.Input, p)
        case planner.NodeJoin:
                p.joins = append([]*planner.LogicalNode{n}, p.joins...)
                if len(n.Inputs) > 0 {
                        flatten(n.Inputs[0], p)
                }
        case planner.NodeScan:
                p.baseTable, p.baseAlias = n.Table, n.Alias
        }
}

func (g *sqlGen) genSelect(root *planner.LogicalNode, pp *planner.PhysicalPlan) {
        var parts selectParts
        flatten(root, &parts)

        if len(parts.ctes) > 0 {
                g.b.WriteString("WITH ")
                if parts.ctes[0].Recursive {
                        g.b.WriteString("RECURSIVE ")
                }
                for i, c := range parts.ctes {
                        if i > 0 {
                                g.b.WriteString(", ")
                        }
                        g.b.WriteString(g.quoteIdent(c.Name))
                        if len(c.Columns) > 0 {
                                g.b.WriteString(" (")
                                g.b.WriteString(strings.Join(g.quoteIdents(c.Columns), ", "))
                                g.b.WriteString(")")
                        }
                        g.b.WriteString(" AS (...)") // nested builder SQL generation wired in pkg/orm
                }
                g.b.WriteString(" ")
        }

        g.b.WriteString("SELECT ")
        if parts.distinct {
                g.b.WriteString("DISTINCT ")
        }
        if len(parts.projections) == 0 {
                g.b.WriteString("*")
        } else {
                for i, p := range parts.projections {
                        if i > 0 {
                                g.b.WriteString(", ")
                        }
                        g.checkRawFragment(p.Expr)
                        g.b.WriteString(p.Expr)
                        if p.Alias != "" {
                                g.b.WriteString(" AS ")
                                g.b.WriteString(g.quoteIdent(p.Alias))
                        }
                }
        }

        g.b.WriteString(" FROM ")
        g.b.WriteString(g.quoteIdent(parts.baseTable))

        for _, j := range parts.joins {
                g.b.WriteString(" ")
                g.b.WriteString(string(j.JoinKind))
                g.b.WriteString(" ")
                g.b.WriteString(g.quoteIdent(j.Table))
                if j.Alias != "" {
                        g.b.WriteString(" AS ")
                        g.b.WriteString(g.quoteIdent(j.Alias))
                }
                if j.JoinOn != nil {
                        g.b.WriteString(" ON ")
                        g.writeExpr(j.JoinOn)
                }
        }

        if parts.where != nil {
                g.b.WriteString(" WHERE ")
                g.writeExpr(parts.where)
        }

        if len(parts.groupBy) > 0 {
                g.b.WriteString(" GROUP BY ")
                g.b.WriteString(strings.Join(g.quoteIdents(parts.groupBy), ", "))
        }
        if parts.having != nil {
                g.b.WriteString(" HAVING ")
                g.writeExpr(parts.having)
        }

        if len(parts.orderBy) > 0 {
                g.b.WriteString(" ORDER BY ")
                for i, t := range parts.orderBy {
                        if i > 0 {
                                g.b.WriteString(", ")
                        }
                        g.b.WriteString(g.quoteIdent(t.Column))
                        if t.Desc {
                                g.b.WriteString(" DESC")
                        }
                        switch t.Nulls {
                        case query.NullsFirst:
                                g.b.WriteString(" NULLS FIRST")
                        case query.NullsLast:
                                g.b.WriteString(" NULLS LAST")
                        }
                }
        } else if pp.NeedsSyntheticOrderBy {
                // T-SQL requires ORDER BY before OFFSET/FETCH; fall back to a stable
                // ordering. Real implementation should source this from metadata;
                // scaffolded with a conservative default.
                g.b.WriteString(" ORDER BY (SELECT NULL)")
        }

        if lo := g.d.LimitOffset(parts.limit, parts.offset); lo != "" {
                g.b.WriteString(" ")
                g.b.WriteString(lo)
        }

        if lock := g.d.LockClause(parts.lock); lock != "" {
                g.b.WriteString(" ")
                g.b.WriteString(lock)
        }
}

func (g *sqlGen) genInsert(n *planner.LogicalNode) {
        g.b.WriteString("INSERT INTO ")
        g.b.WriteString(g.quoteIdent(n.Table))
        g.b.WriteString(" (")
        cols := make([]string, len(n.Assignments))
        for i, a := range n.Assignments {
                cols[i] = a.Column
        }
        g.b.WriteString(strings.Join(g.quoteIdents(cols), ", "))
        g.b.WriteString(") VALUES (")
        for i, a := range n.Assignments {
                if i > 0 {
                        g.b.WriteString(", ")
                }
                g.b.WriteString(g.bind(a.Value))
        }
        g.b.WriteString(")")
        if ret := g.d.ReturningClause([]string{"*"}); ret != "" {
                g.b.WriteString(" ")
                g.b.WriteString(ret)
        }
}

func (g *sqlGen) genUpdate(n *planner.LogicalNode) {
        g.b.WriteString("UPDATE ")
        g.b.WriteString(g.quoteIdent(n.Table))
        g.b.WriteString(" SET ")
        for i, a := range n.Assignments {
                if i > 0 {
                        g.b.WriteString(", ")
                }
                g.b.WriteString(g.quoteIdent(a.Column))
                g.b.WriteString(" = ")
                g.b.WriteString(g.bind(a.Value))
        }
        if n.Predicate != nil {
                g.b.WriteString(" WHERE ")
                g.writeExpr(n.Predicate)
        }
        if ret := g.d.ReturningClause([]string{"*"}); ret != "" {
                g.b.WriteString(" ")
                g.b.WriteString(ret)
        }
}

func (g *sqlGen) genDelete(n *planner.LogicalNode) {
        g.b.WriteString("DELETE FROM ")
        g.b.WriteString(g.quoteIdent(n.Table))
        if n.Predicate != nil {
                g.b.WriteString(" WHERE ")
                g.writeExpr(n.Predicate)
        }
}

func (g *sqlGen) genUpsert(n *planner.LogicalNode) {
        g.genInsert(n)
        // Upsert clause is appended by the caller (pkg/orm) once conflict target
        // and update-column info — carried on query.Builder, not LogicalNode in
        // this scaffold — is threaded through; kept minimal here.
}

func (g *sqlGen) writeExpr(e query.Expr) {
        if g.err != nil {
                return
        }
        switch v := e.(type) {
        case query.Predicate:
                g.writePredicate(v)
        case query.LogicalExpr:
                g.writeLogical(v)
        case query.RawExpr:
                g.checkRawFragment(v.SQL)
                g.b.WriteString(v.SQL)
                g.args = append(g.args, v.Args...)
        default:
                g.err = fmt.Errorf("execution: unsupported expression type %T", e)
        }
}

func (g *sqlGen) writePredicate(p query.Predicate) {
        col := g.quoteIdent(p.Column)
        switch p.Op {
        case query.OpIsNull, query.OpIsNotNull:
                g.b.WriteString(col)
                g.b.WriteString(" ")
                g.b.WriteString(string(p.Op))
        case query.OpIn, query.OpNotIn:
                vals, _ := p.Value.([]any)
                g.b.WriteString(col)
                g.b.WriteString(" ")
                g.b.WriteString(string(p.Op))
                g.b.WriteString(" (")
                for i, v := range vals {
                        if i > 0 {
                                g.b.WriteString(", ")
                        }
                        g.b.WriteString(g.bind(v))
                }
                g.b.WriteString(")")
        case query.OpBetween:
                pair, _ := p.Value.([2]any)
                g.b.WriteString(col)
                g.b.WriteString(" BETWEEN ")
                g.b.WriteString(g.bind(pair[0]))
                g.b.WriteString(" AND ")
                g.b.WriteString(g.bind(pair[1]))
        default:
                g.b.WriteString(col)
                g.b.WriteString(" ")
                g.b.WriteString(string(p.Op))
                g.b.WriteString(" ")
                g.b.WriteString(g.bind(p.Value))
        }
}

func (g *sqlGen) writeLogical(le query.LogicalExpr) {
        if le.Op == query.OpNot {
                g.b.WriteString("NOT (")
                g.writeExpr(le.Children[0])
                g.b.WriteString(")")
                return
        }
        g.b.WriteString("(")
        for i, c := range le.Children {
                if i > 0 {
                        g.b.WriteString(" ")
                        g.b.WriteString(string(le.Op))
                        g.b.WriteString(" ")
                }
                g.writeExpr(c)
        }
        g.b.WriteString(")")
}

// --- arg-only collector, mirroring the binding order above -----------------
//
// Kept as a small, explicit mirror of writeExpr/writePredicate/genInsert/
// genUpdate rather than a shared visitor, so the "what gets bound, in what
// order" logic is easy to eyeball against its sibling. If you change binding
// order in one, change it in the other — TestGenerateSQL_ArgsMatchExtractArgs
// in sqlgen_test.go will fail loudly if they drift apart.

type argCollector struct {
        args []any
        err  error
}

func (c *argCollector) bind(v any) { c.args = append(c.args, v) }

func (c *argCollector) assignments(as []query.Assignment) {
        for _, a := range as {
                c.bind(a.Value)
        }
}

func (c *argCollector) expr(e query.Expr) {
        if e == nil || c.err != nil {
                return
        }
        switch v := e.(type) {
        case query.Predicate:
                c.predicate(v)
        case query.LogicalExpr:
                for _, ch := range v.Children {
                        c.expr(ch)
                }
        case query.RawExpr:
                c.args = append(c.args, v.Args...)
        default:
                c.err = fmt.Errorf("execution: unsupported expression type %T", e)
        }
}

func (c *argCollector) predicate(p query.Predicate) {
        switch p.Op {
        case query.OpIsNull, query.OpIsNotNull:
                // no bound value
        case query.OpIn, query.OpNotIn:
                vals, _ := p.Value.([]any)
                for _, v := range vals {
                        c.bind(v)
                }
        case query.OpBetween:
                pair, _ := p.Value.([2]any)
                c.bind(pair[0])
                c.bind(pair[1])
        default:
                c.bind(p.Value)
        }
}
