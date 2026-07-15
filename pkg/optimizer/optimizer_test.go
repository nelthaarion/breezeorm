package optimizer

import (
	"testing"

	"github.com/nelthaarion/breezeorm/pkg/planner"
	"github.com/nelthaarion/breezeorm/pkg/query"
)

// --- Task 1.2 regression tests: exprKey unsoundness -----------------------
//
// Before the fix, exprKey returned `v.Column + string(v.Op)` for Predicate,
// which collided Where(Eq("a",1)) with Where(Eq("a",2)) (same key "a="),
// causing dedupeAnd to silently drop the second predicate. It also returned
// "" for LogicalExpr, so all LogicalExpr children collided and only the
// first survived.

func TestExprKey_DifferentLiteralsAreEqual(t *testing.T) {
	// Eq("a",1) and Eq("a",2) — different literals, but same shape.
	// canonical-ordering wants them to hash equal so that
	// Where(Eq("a",1), Eq("a",2)) and Where(Eq("a",2), Eq("a",1)) canonicalize
	// to the same plan.
	a := query.Predicate{Column: "a", Op: query.OpEq, Value: 1}
	b := query.Predicate{Column: "a", Op: query.OpEq, Value: 2}
	if exprKey(a) != exprKey(b) {
		t.Errorf("Eq(a,1) and Eq(a,2) should have equal exprKey (same shape), got %q vs %q", exprKey(a), exprKey(b))
	}
}

func TestExprKey_DifferentColumnsAreDifferent(t *testing.T) {
	a := query.Predicate{Column: "a", Op: query.OpEq, Value: 1}
	b := query.Predicate{Column: "b", Op: query.OpEq, Value: 1}
	if exprKey(a) == exprKey(b) {
		t.Errorf("Eq(a,1) and Eq(b,1) should have different exprKey")
	}
}

func TestExprKey_DifferentOpsAreDifferent(t *testing.T) {
	a := query.Predicate{Column: "a", Op: query.OpEq, Value: 1}
	b := query.Predicate{Column: "a", Op: query.OpLt, Value: 1}
	if exprKey(a) == exprKey(b) {
		t.Errorf("Eq(a,1) and Lt(a,1) should have different exprKey")
	}
}

func TestExprKey_INListsOfDifferentLengthsAreDifferent(t *testing.T) {
	a := query.Predicate{Column: "id", Op: query.OpIn, Value: []any{1, 2, 3}}
	b := query.Predicate{Column: "id", Op: query.OpIn, Value: []any{1, 2, 3, 4, 5}}
	if exprKey(a) == exprKey(b) {
		t.Errorf("In([1,2,3]) and In([1,2,3,4,5]) should have different exprKey (different arg counts)")
	}
}

func TestExprKey_BetweenVsScalarAreDifferent(t *testing.T) {
	a := query.Predicate{Column: "n", Op: query.OpEq, Value: 1}
	b := query.Predicate{Column: "n", Op: query.OpBetween, Value: [2]any{1, 10}}
	if exprKey(a) == exprKey(b) {
		t.Errorf("Eq(n,1) and Between(n,1,10) should have different exprKey")
	}
}

func TestExprKey_IsNullVsScalarAreDifferent(t *testing.T) {
	a := query.Predicate{Column: "x", Op: query.OpEq, Value: 1}
	b := query.Predicate{Column: "x", Op: query.OpIsNull} // nil value
	if exprKey(a) == exprKey(b) {
		t.Errorf("Eq(x,1) and IsNull(x) should have different exprKey")
	}
}

func TestExprKey_LogicalExprChildrenAreDifferent(t *testing.T) {
	// Two different LogicalExpr children must NOT collide on the empty key
	// (the old behavior returned "" for all LogicalExpr, causing dedupeAnd
	// to drop every LogicalExpr child after the first).
	a := query.LogicalExpr{Op: query.OpOr, Children: []query.Expr{
		query.Predicate{Column: "a", Op: query.OpEq, Value: 1},
	}}
	b := query.LogicalExpr{Op: query.OpOr, Children: []query.Expr{
		query.Predicate{Column: "b", Op: query.OpEq, Value: 1},
	}}
	if exprKey(a) == exprKey(b) {
		t.Errorf("two different LogicalExpr children should have different exprKey")
	}
}

// --- dedupeAnd regression tests (the consumer that mattered) --------------

func TestDedupeAnd_DoesNotDropDifferentLiterals(t *testing.T) {
	// Where(Eq("a",1), Eq("a",2)) — different literals, must NOT be deduped.
	// (They have the same exprKey, so the old "drop on key match" logic
	// dropped one — but they're not true duplicates, just same shape.
	// The fix: dedupeAnd is shape-canonicalization, not value-dedup, so
	// same-shape different-value predicates MUST be preserved.)
	e := query.LogicalExpr{
		Op: query.OpAnd,
		Children: []query.Expr{
			query.Predicate{Column: "a", Op: query.OpEq, Value: 1},
			query.Predicate{Column: "a", Op: query.OpEq, Value: 2},
		},
	}
	out := dedupeAnd(e)
	le := out.(query.LogicalExpr)
	if len(le.Children) != 2 {
		t.Fatalf("dedupeAnd dropped a same-shape-different-value predicate: got %d children, want 2", len(le.Children))
	}
}

func TestDedupeAnd_DropsTrueDuplicates(t *testing.T) {
	// Where(Eq("a",1), Eq("a",1)) — true duplicate, MUST be deduped to 1.
	e := query.LogicalExpr{
		Op: query.OpAnd,
		Children: []query.Expr{
			query.Predicate{Column: "a", Op: query.OpEq, Value: 1},
			query.Predicate{Column: "a", Op: query.OpEq, Value: 1},
		},
	}
	out := dedupeAnd(e)
	le := out.(query.LogicalExpr)
	if len(le.Children) != 1 {
		t.Fatalf("dedupeAnd kept a true duplicate: got %d children, want 1", len(le.Children))
	}
}

func TestDedupeAnd_DoesNotCollideINLists(t *testing.T) {
	// In([1,2,3]) and In([1,2,3,4,5]) — different list lengths, must NOT collide.
	e := query.LogicalExpr{
		Op: query.OpAnd,
		Children: []query.Expr{
			query.Predicate{Column: "id", Op: query.OpIn, Value: []any{1, 2, 3}},
			query.Predicate{Column: "id", Op: query.OpIn, Value: []any{1, 2, 3, 4, 5}},
		},
	}
	out := dedupeAnd(e)
	le := out.(query.LogicalExpr)
	if len(le.Children) != 2 {
		t.Fatalf("dedupeAnd collided two different-length IN lists: got %d, want 2", len(le.Children))
	}
}

func TestDedupeAnd_DoesNotDropLogicalExprChildren(t *testing.T) {
	// Two different LogicalExpr children must not collide.
	e := query.LogicalExpr{
		Op: query.OpAnd,
		Children: []query.Expr{
			query.LogicalExpr{Op: query.OpOr, Children: []query.Expr{
				query.Predicate{Column: "a", Op: query.OpEq, Value: 1},
			}},
			query.LogicalExpr{Op: query.OpOr, Children: []query.Expr{
				query.Predicate{Column: "b", Op: query.OpEq, Value: 1},
			}},
		},
	}
	out := dedupeAnd(e)
	le := out.(query.LogicalExpr)
	if len(le.Children) != 2 {
		t.Fatalf("dedupeAnd dropped a LogicalExpr child via empty-key collision: got %d, want 2", len(le.Children))
	}
}

// --- canonicalOrdering smoke test -----------------------------------------

func TestCanonicalOrdering_IsStable(t *testing.T) {
	// Same predicates in different order should produce the same canonical
	// output (this is what enables cache hits across structurally-identical
	// queries built with predicates in different source order).
	mk := func(first, second query.Predicate) *planner.LogicalPlan {
		return &planner.LogicalPlan{Root: &planner.LogicalNode{
			Kind:      planner.NodeFilter,
			Predicate: query.LogicalExpr{Op: query.OpAnd, Children: []query.Expr{first, second}},
		}}
	}
	p1 := query.Predicate{Column: "a", Op: query.OpEq, Value: 1}
	p2 := query.Predicate{Column: "b", Op: query.OpEq, Value: 1}

	plan1 := mk(p1, p2)
	plan2 := mk(p2, p1)

	canonicalOrdering{}.Apply(plan1)
	canonicalOrdering{}.Apply(plan2)

	k1 := exprKey(plan1.Root.Predicate)
	k2 := exprKey(plan2.Root.Predicate)
	if k1 != k2 {
		t.Errorf("canonical ordering should produce identical keys: got %q vs %q", k1, k2)
	}
}
