// Package optimizer implements rule-based rewrite passes over a
// planner.LogicalPlan. Passes are pure functions (LogicalPlan -> LogicalPlan)
// run in a fixed, deterministic order so that plans are canonical and thus
// cacheable by structural hash (see pkg/cache).
package optimizer

import (
        "fmt"

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
        // dedupeAnd removes EXACT duplicates — same predicate structure AND
        // same literal value. It must NOT remove same-shape-different-value
        // predicates like Where(Eq("a",1), Eq("a",2)) — those are
        // semantically distinct (a=1 AND a=2 is a contradiction, not a
        // duplicate of a=1).
        //
        // So this uses exprIdentityKey (shape + value), NOT exprKey (shape
        // only). The shape-only exprKey is reserved for canonicalOrdering,
        // where we want Eq("a",1) and Eq("a",2) to hash equal so that
        // Where(Eq("a",1), Eq("b",2)) and Where(Eq("b",2), Eq("a",1))
        // canonicalize to the same plan.
        seen := map[string]bool{}
        out := make([]query.Expr, 0, len(le.Children))
        for _, c := range le.Children {
                key := exprIdentityKey(c)
                if seen[key] {
                        continue
                }
                seen[key] = true
                out = append(out, c)
        }
        le.Children = out
        return le
}

// exprIdentityKey is like exprKey but ALSO includes literal values, so that
// Eq("a",1) and Eq("a",2) produce different keys. Used by dedupeAnd, which
// must only drop *exact* duplicates (same shape + same value), not
// same-shape-different-value predicates.
//
// Implementation mirrors exprKey but appends fmt.Sprint(v) for scalar
// values, the full slice for []any, and the pair for [2]any.
func exprIdentityKey(e query.Expr) string {
        var buf []byte
        writeExprIdentityKey(&buf, e)
        return string(buf)
}

func writeExprIdentityKey(buf *[]byte, e query.Expr) {
        switch v := e.(type) {
        case nil:
                *buf = append(*buf, 'n')
        case query.Predicate:
                *buf = append(*buf, 'p')
                *buf = append(*buf, v.Column...)
                *buf = append(*buf, 0x1f)
                *buf = append(*buf, string(v.Op)...)
                *buf = append(*buf, 0x1f)
                switch val := v.Value.(type) {
                case nil:
                        *buf = append(*buf, 'N')
                case []any:
                        *buf = append(*buf, 'I')
                        for _, item := range val {
                                *buf = append(*buf, fmt.Sprint(item)...)
                                *buf = append(*buf, 0x1e)
                        }
                case [2]any:
                        *buf = append(*buf, 'B')
                        *buf = append(*buf, fmt.Sprint(val[0])...)
                        *buf = append(*buf, 0x1e)
                        *buf = append(*buf, fmt.Sprint(val[1])...)
                        *buf = append(*buf, 0x1e)
                default:
                        *buf = append(*buf, 'S')
                        *buf = append(*buf, fmt.Sprint(val)...)
                }
        case query.LogicalExpr:
                *buf = append(*buf, 'l')
                *buf = append(*buf, string(v.Op)...)
                *buf = append(*buf, 0x1c)
                for _, c := range v.Children {
                        writeExprIdentityKey(buf, c)
                }
                *buf = append(*buf, 0x1d)
        case query.RawExpr:
                *buf = append(*buf, 'r')
                *buf = append(*buf, v.SQL...)
                *buf = append(*buf, 0x1f)
                for _, a := range v.Args {
                        *buf = append(*buf, fmt.Sprint(a)...)
                        *buf = append(*buf, 0x1e)
                }
        default:
                *buf = append(*buf, 'u')
                *buf = append(*buf, fmt.Sprintf("%T", e)...)
                *buf = append(*buf, 0x1f)
        }
}

// exprKey returns a structural-shape key for e, used by canonicalOrdering
// to put AND-group children into a deterministic order so that
// structurally-identical queries built with predicates in different source
// order canonicalize to the same plan (and thus the same cache key).
//
// The key depends on:
//   - Expression kind (Predicate, LogicalExpr, RawExpr)
//   - For Predicate: column, op, AND the *kind* of the value (nil vs []any
//     vs [2]any vs scalar), AND for []any the *count* of values (so
//     In([1,2,3]) and In([1,2,3,4,5]) don't collide — different list
//     lengths produce different SQL shapes and different bind-arg counts).
//   - For LogicalExpr: op + recursive key of each child.
//   - For RawExpr: the SQL text (raw SQL is developer-authored, so identical
//     text means identical shape).
//
// The key MUST NOT depend on literal values, so that Where(Eq("a",1)) and
// Where(Eq("a",2)) — which are the same predicate *shape* — produce the
// same key. But In([1,2,3]) and In([1,2,3,4,5]) MUST produce different keys
// because the IN-list length affects the bind-arg count and the SQL shape.
//
// NOTE: dedupeAnd does NOT use exprKey — it uses exprIdentityKey (shape +
// value), because dedupeAnd must only drop EXACT duplicates, not
// same-shape-different-value predicates like Eq("a",1) vs Eq("a",2).
//
// Implementation: a []byte buffer with separator bytes, returned as a
// string. Cheaper than fmt.Sprintf and unambiguous (the leading kind byte
// prevents cross-kind collisions even with empty content).
func exprKey(e query.Expr) string {
        var buf []byte
        writeExprKey(&buf, e)
        return string(buf)
}

func writeExprKey(buf *[]byte, e query.Expr) {
        switch v := e.(type) {
        case nil:
                *buf = append(*buf, 'n')
        case query.Predicate:
                *buf = append(*buf, 'p')
                *buf = append(*buf, v.Column...)
                *buf = append(*buf, 0x1f)
                *buf = append(*buf, string(v.Op)...)
                *buf = append(*buf, 0x1f)
                // Value kind tag — NOT the value itself. This is what
                // distinguishes Eq("a",1) from In("a",[1,2,3]) from Between("a",1,2).
                switch v.Value.(type) {
                case nil:
                        *buf = append(*buf, 'N') // IS NULL / IS NOT NULL
                case []any:
                        // Include the count so different-length IN lists collide
                        // neither with each other nor with scalar predicates.
                        n := len(v.Value.([]any))
                        *buf = append(*buf, 'I')
                        *buf = append(*buf, byte(n>>8), byte(n))
                case [2]any:
                        *buf = append(*buf, 'B') // BETWEEN
                default:
                        *buf = append(*buf, 'S') // scalar (Eq, Neq, Lt, Gt, etc.)
                }
        case query.LogicalExpr:
                *buf = append(*buf, 'l')
                *buf = append(*buf, string(v.Op)...)
                *buf = append(*buf, 0x1c)
                for _, c := range v.Children {
                        writeExprKey(buf, c)
                }
                *buf = append(*buf, 0x1d)
        case query.RawExpr:
                *buf = append(*buf, 'r')
                *buf = append(*buf, v.SQL...)
                *buf = append(*buf, 0x1f)
        default:
                // Unknown expression type — give it a unique key prefix so
                // two distinct unknown types don't collide on the empty
                // string. Use the type's stringified representation, which
                // is stable for the lifetime of the program.
                *buf = append(*buf, 'u')
                *buf = append(*buf, fmt.Sprintf("%T", e)...)
                *buf = append(*buf, 0x1f)
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
