// Package compiler wires together the planner and optimizer into the
// query-compilation pipeline: query.Builder -> LogicalPlan -> optimized
// LogicalPlan -> PhysicalPlan. The result is cacheable by CacheKey so that
// structurally identical queries (same shape, different literal values)
// reuse the same plan and, downstream, the same prepared statement.
package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/nelthaarion/breezeorm/pkg/dialect"
	"github.com/nelthaarion/breezeorm/pkg/optimizer"
	"github.com/nelthaarion/breezeorm/pkg/planner"
	"github.com/nelthaarion/breezeorm/pkg/plugins"
	"github.com/nelthaarion/breezeorm/pkg/query"
)

// CompiledQuery is the immutable output of the compilation pipeline, ready
// for SQL generation and execution.
type CompiledQuery struct {
	Physical *planner.PhysicalPlan
	CacheKey string
}

// Compile runs the full pipeline for a builder against a target dialect:
// logical plan construction, plugin BeforePlan rewrites (soft delete,
// multi-tenancy, etc.), optimizer passes, and physical planning.
func Compile[T any](ctx context.Context, b query.Builder[T], d dialect.Dialect, passes []optimizer.Pass, chain plugins.Chain) (*CompiledQuery, error) {
	lp := planner.Build(b)

	lp, err := chain.RunBeforePlan(ctx, lp)
	if err != nil {
		return nil, fmt.Errorf("compiler: plugin BeforePlan: %w", err)
	}

	lp = optimizer.Optimize(lp, passes)
	pp := planner.PlanPhysical(lp, d)

	return &CompiledQuery{
		Physical: pp,
		CacheKey: structuralHash(pp.Logical.Root, d.Name()),
	}, nil
}

// structuralHash produces a cache key that depends only on query *shape*
// (table names, operator tree structure, column names, operators) — never on
// bound literal values — so that e.g. `Where(Eq("id", 1))` and
// `Where(Eq("id", 2))` hit the same cached plan and prepared statement.
func structuralHash(n *planner.LogicalNode, dialectName string) string {
	h := sha256.New()
	h.Write([]byte(dialectName))
	writeNode(h, n)
	return hex.EncodeToString(h.Sum(nil))
}

func writeNode(h interface{ Write([]byte) (int, error) }, n *planner.LogicalNode) {
	if n == nil {
		h.Write([]byte("nil;"))
		return
	}
	fmt.Fprintf(h, "k=%d;t=%s;a=%s;", n.Kind, n.Table, n.Alias)
	writeExpr(h, n.Predicate)
	writeExpr(h, n.Having)
	for _, g := range n.GroupBy {
		fmt.Fprintf(h, "g=%s;", g)
	}
	for _, o := range n.OrderBy {
		fmt.Fprintf(h, "o=%s,%v;", o.Column, o.Desc)
	}
	if n.Limit != nil {
		fmt.Fprintf(h, "lim=1;") // presence only — value is a bind param, not part of shape
	}
	if n.Offset != nil {
		fmt.Fprintf(h, "off=1;")
	}
	for _, p := range n.Projections {
		fmt.Fprintf(h, "p=%s,%s;", p.Expr, p.Alias)
	}
	writeNode(h, n.Input)
	for _, in := range n.Inputs {
		writeNode(h, in)
	}
}

func writeExpr(h interface{ Write([]byte) (int, error) }, e query.Expr) {
	switch v := e.(type) {
	case nil:
		h.Write([]byte("e:nil;"))
	case query.Predicate:
		fmt.Fprintf(h, "e:pred:%s:%s;", v.Column, v.Op)
	case query.LogicalExpr:
		fmt.Fprintf(h, "e:logic:%s(", v.Op)
		for _, c := range v.Children {
			writeExpr(h, c)
		}
		h.Write([]byte(")"))
	case query.RawExpr:
		fmt.Fprintf(h, "e:raw:%s;", v.SQL)
	default:
		h.Write([]byte("e:other;"))
	}
}
