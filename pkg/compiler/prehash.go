package compiler

import (
	"hash/maphash"

	"github.com/nelthaarion/breezorm/pkg/query"
)

// PreHash computes a structural cache key directly from a query.Builder's
// recorded operations — before planner.Build, the optimizer pipeline, or
// PlanPhysical ever run. This exists so callers (pkg/orm) can cache the
// *entire* CompiledQuery (logical plan, optimized plan, physical plan) keyed
// on something cheap to compute, rather than only being able to cache
// downstream SQL text keyed on structuralHash — which is itself only
// available *after* paying for Build+Optimize+PlanPhysical, defeating the
// point for that layer.
//
// Like structuralHash, PreHash depends only on query *shape* (table, kind,
// which columns/operators appear, in what structure) — never on bound
// literal values — so `Where(id=1)` and `Where(id=2)` hit the same cache
// entry.
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
// example BeforePlan-using plugins in pkg/plugins (SoftDelete,
// MultiTenancy) are documented TODO stubs that don't yet mutate the plan;
// this MUST be revisited (e.g. by making such caches request-scoped, or by
// excluding builders from the cache when the plugin chain is non-empty and
// context-dependent) before those plugins are made functional.
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
	writeBool(&h, b.LimitVal() != nil)
	writeBool(&h, b.OffsetVal() != nil)
	for _, a := range b.Assignments() {
		h.WriteString(a.Column) // column names only — never values
		h.WriteByte(sepField)
	}
	h.WriteByte(byte(b.LockMode()))

	return h.Sum64()
}

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
