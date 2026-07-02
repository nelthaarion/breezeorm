package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/nelthaarion/breezeorm/pkg/query"
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
// CAVEAT: a cache keyed by PreHash must not be used when a plugin chain's
// BeforePlan rewrite can vary by request (e.g. a multi-tenancy plugin
// injecting a per-request tenant predicate) — reusing a cached CompiledQuery
// would skip re-running BeforePlan for that request. Safe today because the
// example BeforePlan-using plugins in pkg/plugins (SoftDelete,
// MultiTenancy) are documented TODO stubs that don't yet mutate the plan;
// this MUST be revisited (e.g. by making such caches request-scoped, or by
// excluding builders from the cache when the plugin chain is non-empty and
// context-dependent) before those plugins are made functional.
func PreHash[T any](b query.Builder[T], dialectName string) string {
	h := sha256.New()
	h.Write([]byte(dialectName))
	fmt.Fprintf(h, "table=%s;kind=%d;distinct=%v;", b.Table(), b.StmtKind(), b.Distincted())

	for _, s := range b.Selects() {
		fmt.Fprintf(h, "sel=%s,%s;", s.Expr, s.Alias)
	}
	for _, j := range b.Joins() {
		fmt.Fprintf(h, "join=%s,%s,%s;", j.Kind, j.Table, j.Alias)
		writeBuilderExpr(h, j.Condition)
	}
	writeBuilderExpr(h, b.WhereExpr())
	for _, g := range b.GroupByCols() {
		fmt.Fprintf(h, "group=%s;", g)
	}
	writeBuilderExpr(h, b.HavingExpr())
	for _, o := range b.OrderByTerms() {
		fmt.Fprintf(h, "order=%s,%v;", o.Column, o.Desc)
	}
	if b.LimitVal() != nil {
		h.Write([]byte("limit=1;"))
	}
	if b.OffsetVal() != nil {
		h.Write([]byte("offset=1;"))
	}
	for _, a := range b.Assignments() {
		fmt.Fprintf(h, "assign=%s;", a.Column) // column names only — never values
	}
	fmt.Fprintf(h, "lock=%d;", b.LockMode())

	return hex.EncodeToString(h.Sum(nil))
}

func writeBuilderExpr(h interface{ Write([]byte) (int, error) }, e query.Expr) {
	switch v := e.(type) {
	case nil:
		h.Write([]byte("e:nil;"))
	case query.Predicate:
		fmt.Fprintf(h, "e:pred:%s:%s;", v.Column, v.Op)
	case query.LogicalExpr:
		fmt.Fprintf(h, "e:logic:%s(", v.Op)
		for _, c := range v.Children {
			writeBuilderExpr(h, c)
		}
		h.Write([]byte(")"))
	case query.RawExpr:
		fmt.Fprintf(h, "e:raw:%s;", v.SQL)
	default:
		h.Write([]byte("e:other;"))
	}
}
