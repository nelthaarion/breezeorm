// Package optimizer implements rule-based rewrite passes over a
// planner.LogicalPlan. Passes are pure functions (LogicalPlan -> LogicalPlan)
// run in a fixed, deterministic order so that plans are canonical and thus
// cacheable by structural hash (see pkg/cache).
package optimizer

import (
	"github.com/nelthaarion/breezeorm/pkg/planner"
	"github.com/nelthaarion/breezeorm/pkg/query"
)

// Pass is a single optimizer rewrite rule.
type Pass interface {
	Name() string
	Apply(*planner.LogicalPlan) *planner.LogicalPlan
}

// DefaultPipeline returns the standard set of passes, in the order the spec
// calls for: predicate simplification, constant folding, join optimization,
// projection pruning, ORDER BY optimization, LIMIT optimization, duplicate
// predicate removal, canonical query ordering.
func DefaultPipeline() []Pass {
	return []Pass{
		predicateSimplification{},
		constantFolding{},
		duplicatePredicateRemoval{},
		joinOptimization{},
		projectionPruning{},
		orderByOptimization{},
		limitOptimization{},
		canonicalOrdering{},
	}
}

// Optimize runs the given passes over lp in order and returns the rewritten plan.
func Optimize(lp *planner.LogicalPlan, passes []Pass) *planner.LogicalPlan {
	for _, p := range passes {
		lp = p.Apply(lp)
	}
	return lp
}

// --- predicate simplification ---------------------------------------------
//
// Collapses trivially-true/false predicates and flattens redundant
// single-child AND/OR nodes, e.g. AND(x) -> x.

type predicateSimplification struct{}

func (predicateSimplification) Name() string { return "predicate_simplification" }

func (p predicateSimplification) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	walk(lp.Root, func(n *planner.LogicalNode) {
		n.Predicate = simplifyExpr(n.Predicate)
		n.Having = simplifyExpr(n.Having)
	})
	return lp
}

func simplifyExpr(e query.Expr) query.Expr {
	le, ok := e.(query.LogicalExpr)
	if !ok {
		return e
	}
	simplified := make([]query.Expr, 0, len(le.Children))
	for _, c := range le.Children {
		simplified = append(simplified, simplifyExpr(c))
	}
	if len(simplified) == 1 && (le.Op == query.OpAnd || le.Op == query.OpOr) {
		return simplified[0]
	}
	le.Children = simplified
	return le
}

// --- constant folding -------------------------------------------------------
//
// Placeholder for folding constant sub-expressions in RawExpr/Predicate
// values known at plan-compile time (e.g. literal arithmetic). Full constant
// folding needs a typed expression evaluator; scaffolded as a no-op pass with
// the correct shape so it can be filled in incrementally.

type constantFolding struct{}

func (constantFolding) Name() string { return "constant_folding" }

func (constantFolding) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	// TODO: fold constant expressions once a typed literal evaluator exists.
	return lp
}

// --- duplicate predicate removal -------------------------------------------

type duplicatePredicateRemoval struct{}

func (duplicatePredicateRemoval) Name() string { return "duplicate_predicate_removal" }

func (duplicatePredicateRemoval) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	walk(lp.Root, func(n *planner.LogicalNode) {
		if le, ok := n.Predicate.(query.LogicalExpr); ok && le.Op == query.OpAnd {
			n.Predicate = dedupeAnd(le)
		}
	})
	return lp
}

func dedupeAnd(le query.LogicalExpr) query.Expr {
	seen := map[string]bool{}
	out := make([]query.Expr, 0, len(le.Children))
	for _, c := range le.Children {
		key := exprKey(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	le.Children = out
	return le
}

func exprKey(e query.Expr) string {
	switch v := e.(type) {
	case query.Predicate:
		return v.Column + string(v.Op)
	case query.RawExpr:
		return "raw:" + v.SQL
	default:
		return ""
	}
}

// --- join optimization -----------------------------------------------------
//
// Placeholder for join-reordering based on selectivity estimates, which in
// turn needs table/index statistics (out of scope for this scaffold — the
// metadata cache would be the natural place to keep such stats).

type joinOptimization struct{}

func (joinOptimization) Name() string { return "join_optimization" }

func (joinOptimization) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	// TODO: reorder joins by estimated selectivity once statistics exist.
	return lp
}

// --- projection pruning -----------------------------------------------------
//
// Placeholder: would drop unused columns from child Scan nodes once the
// scanner's requested-column-set can be threaded down from Project nodes.

type projectionPruning struct{}

func (projectionPruning) Name() string { return "projection_pruning" }

func (projectionPruning) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	// TODO: push required-column sets down into Scan nodes.
	return lp
}

// --- ORDER BY optimization ---------------------------------------------------
//
// Removes ORDER BY entirely when a LIMIT-less, offset-less aggregate query
// makes ordering observably irrelevant... left conservative (no-op) since
// removing ORDER BY changes result order, which many callers rely on even
// without LIMIT. Scaffolded for future safe cases (e.g. redundant duplicate
// order terms).

type orderByOptimization struct{}

func (orderByOptimization) Name() string { return "order_by_optimization" }

func (orderByOptimization) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	walk(lp.Root, func(n *planner.LogicalNode) {
		if len(n.OrderBy) < 2 {
			return
		}
		seen := map[string]bool{}
		out := make([]query.OrderTerm, 0, len(n.OrderBy))
		for _, t := range n.OrderBy {
			if seen[t.Column] {
				continue
			}
			seen[t.Column] = true
			out = append(out, t)
		}
		n.OrderBy = out
	})
	return lp
}

// --- LIMIT optimization -------------------------------------------------------
//
// Collapses nested LIMIT nodes to the tightest bound.

type limitOptimization struct{}

func (limitOptimization) Name() string { return "limit_optimization" }

func (limitOptimization) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	n := lp.Root
	for n != nil && n.Kind == planner.NodeLimit && n.Input != nil && n.Input.Kind == planner.NodeLimit {
		inner := n.Input
		if inner.Limit != nil && (n.Limit == nil || *inner.Limit < *n.Limit) {
			n.Limit = inner.Limit
		}
		n.Input = inner.Input
	}
	return lp
}

// --- canonical query ordering -------------------------------------------------
//
// Sorts AND-group children into a stable, deterministic order so that
// semantically identical queries built with predicates in different source
// order still produce the same plan hash for the plan cache.

type canonicalOrdering struct{}

func (canonicalOrdering) Name() string { return "canonical_ordering" }

func (canonicalOrdering) Apply(lp *planner.LogicalPlan) *planner.LogicalPlan {
	walk(lp.Root, func(n *planner.LogicalNode) {
		if le, ok := n.Predicate.(query.LogicalExpr); ok && le.Op == query.OpAnd {
			sortExprs(le.Children)
		}
	})
	return lp
}

func sortExprs(exprs []query.Expr) {
	for i := 1; i < len(exprs); i++ {
		for j := i; j > 0 && exprKey(exprs[j]) < exprKey(exprs[j-1]); j-- {
			exprs[j], exprs[j-1] = exprs[j-1], exprs[j]
		}
	}
}

// --- shared tree walk helper -------------------------------------------------

func walk(n *planner.LogicalNode, fn func(*planner.LogicalNode)) {
	if n == nil {
		return
	}
	fn(n)
	walk(n.Input, fn)
	for _, in := range n.Inputs {
		walk(in, fn)
	}
}
