package compiler

import (
        "hash/maphash"

        "github.com/nelthaarion/breezeorm/pkg/dialect"
        "github.com/nelthaarion/breezeorm/pkg/query"
)

// PreHash computes a structural cache key straight from a query.Builder's
// recorded operations — before planner.Build, the optimizer, or PlanPhysical
// ever run. This lets callers (pkg/orm) cache the *entire* CompiledQuery
// (logical plan, optimized plan, physical plan) keyed on something cheap,
// rather than only being able to cache downstream SQL text keyed on
// structuralHash (which is itself only available *after* paying for the
// full compile pipeline).
//
// Like structuralHash, PreHash depends only on query *shape* (table, kind,
// which columns/operators appear, in what structure) — never on bound
// literal values. So `Where(id=1)` and `Where(id=2)` hit the same cache entry.
//
// PERFORMANCE: PreHash runs on *every* Find/Create/UpdateAll/Delete call —
// cache hit or miss — since it IS the cache lookup key. A CPU profile of a
// real benchmark run showed the original fmt.Fprintf + crypto/sha256 +
// hex.EncodeToString version costing ~11% of total per-query CPU time by
// itself, more expensive than the actual database round trip's Go-side
// overhead. See structuralHash's doc comment in compiler.go for the same
// rationale, applied here.
//
// CAVEAT: a cache keyed by PreHash must not be used when a plugin chain's
// BeforePlan rewrite can vary by request (e.g. a multi-tenancy plugin
// injecting a per-request tenant predicate) — reusing a cached CompiledQuery
// would skip re-running BeforePlan for that request. Safe today because the
// example BeforePlan-using plugins (SoftDelete, MultiTenancy) are documented
// TODO stubs that don't yet mutate the plan. This MUST be revisited before
// those plugins are made functional (the compileCached guard in pkg/orm
// already handles this via Chain.IsCacheSafe).
func PreHash[T any](b query.Builder[T], dialectName string) uint64 {
        var h maphash.Hash
        h.SetSeed(hashSeed)
        h.WriteString(dialectName)
        h.WriteByte(sepField)

        h.WriteString(b.Table())
        h.WriteByte(sepField)
        h.WriteByte(byte(b.StmtKind()))
        writeBool(&h, b.Distincted())

        for _, s := range b.Selects() {
                h.WriteString(s.Expr)
                h.WriteByte(sepField)
                h.WriteString(s.Alias)
                h.WriteByte(sepField)
        }
        for _, j := range b.Joins() {
                h.WriteString(string(j.Kind))
                h.WriteByte(sepField)
                h.WriteString(j.Table)
                h.WriteByte(sepField)
                h.WriteString(j.Alias)
                h.WriteByte(sepField)
                writeBuilderExpr(&h, j.Condition)
        }
        writeBuilderExpr(&h, b.WhereExpr())
        for _, g := range b.GroupByCols() {
                h.WriteString(g)
                h.WriteByte(sepField)
        }
        writeBuilderExpr(&h, b.HavingExpr())
        for _, o := range b.OrderByTerms() {
                h.WriteString(o.Column)
                writeBool(&h, o.Desc)
        }
        writeBool(&h, b.HasLimit())
        writeBool(&h, b.HasOffset())
        for _, a := range b.Assignments() {
                h.WriteString(a.Column) // column names only — never values
                h.WriteByte(sepField)
        }
        h.WriteByte(byte(b.LockMode()))

        // Upsert conflict target + update-column list (shape-affecting:
        // different conflict columns or different DO UPDATE SET columns =
        // different SQL shape, so they MUST participate in the cache key).
        // Without this, Upsert ON CONFLICT (email) and Upsert ON CONFLICT (id)
        // would collide and silently reuse each other's cached plan.
        if b.StmtKind() == query.KindUpsert {
                target := b.UpsertConflict()
                h.WriteByte(sepField)
                h.WriteString(target.Constraint)
                h.WriteByte(sepField)
                for _, c := range target.Columns {
                        h.WriteString(c)
                        h.WriteByte(sepField)
                }
                for _, c := range b.UpsertUpdateCols() {
                        h.WriteString(c)
                        h.WriteByte(sepField)
                }
        }

        // CTEs (shape-affecting: different WITH clauses = different SQL).
        for _, cte := range b.CTEs() {
                h.WriteByte(sepField)
                h.WriteString(cte.Name)
                h.WriteByte(sepField)
                writeBool(&h, cte.Recursive)
                for _, col := range cte.Columns {
                        h.WriteString(col)
                        h.WriteByte(sepField)
                }
        }

        // Preloads (shape-affecting: different preload paths trigger different
        // relation loaders queries downstream). The Where/Limit/Batch flags
        // are shape-only (presence/absence), never literal values.
        for _, p := range b.Preloads() {
                h.WriteByte(sepField)
                h.WriteString(p.Path)
                h.WriteByte(sepField)
                writeBool(&h, p.Where != nil)
                writeBool(&h, p.Limit != nil)
                writeBool(&h, p.Batch)
        }

        // Cursor pagination token presence (NOT the token itself — that's a
        // per-call literal and would break the shape-only invariant).
        writeBool(&h, b.CursorAfter() != nil)

        return h.Sum64()
}

// Compile-time assertion that dialect is referenced (suppresses "imported and
// not used" if a future edit removes the only other use; dialect is used by
// the upsert test in prehash_test.go which constructs UpsertConflictTarget
// via the dialect package).
var _ = dialect.UpsertConflictTarget{}

func writeBuilderExpr(h *maphash.Hash, e query.Expr) {
        switch v := e.(type) {
        case nil:
                h.WriteByte(0)
        case query.Predicate:
                h.WriteByte('p')
                h.WriteString(v.Column)
                h.WriteByte(sepField)
                h.WriteString(string(v.Op))
                h.WriteByte(sepField)
        case query.LogicalExpr:
                h.WriteByte('l')
                h.WriteString(string(v.Op))
                h.WriteByte(sepOpen)
                for _, c := range v.Children {
                        writeBuilderExpr(h, c)
                }
                h.WriteByte(sepClose)
        case query.RawExpr:
                h.WriteByte('r')
                h.WriteString(v.SQL)
                h.WriteByte(sepField)
        default:
                h.WriteByte('?')
        }
}
