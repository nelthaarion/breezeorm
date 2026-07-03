package planner

import "github.com/nelthaarion/breezeorm/pkg/dialect"

// PhysicalPlan is the dialect-bound, execution-ready plan produced by the
// physical planner from an (optimized) LogicalPlan. Unlike LogicalPlan, it
// may carry dialect-specific decisions (e.g. whether a LIMIT/OFFSET needs a
// synthetic ORDER BY injected for SQL Server, or whether RETURNING is usable).
type PhysicalPlan struct {
	Logical               *LogicalPlan
	Dialect               dialect.Dialect
	UseReturning          bool
	NeedsSyntheticOrderBy bool
}

// PlanPhysical lowers a LogicalPlan into a PhysicalPlan for the given dialect.
// This step is intentionally thin in the scaffold: it records dialect
// capability decisions that the SQL generator (pkg/execution) needs, without
// yet doing physical operator selection (join algorithm choice, etc.) — that
// belongs here once a real cost model is implemented.
func PlanPhysical(lp *LogicalPlan, d dialect.Dialect) *PhysicalPlan {
	pp := &PhysicalPlan{Logical: lp, Dialect: d}

	if lp.Root != nil {
		hasLimit := containsKind(lp.Root, NodeLimit)
		hasOrder := containsKind(lp.Root, NodeSort)
		if d.Name() == "sqlserver" && hasLimit && !hasOrder {
			pp.NeedsSyntheticOrderBy = true
		}
	}

	switch {
	case lp.Root != nil && (lp.Root.Kind == NodeInsert || lp.Root.Kind == NodeUpdate || lp.Root.Kind == NodeUpsert):
		pp.UseReturning = d.SupportsReturning()
	}

	return pp
}

func containsKind(n *LogicalNode, k NodeKind) bool {
	if n == nil {
		return false
	}
	if n.Kind == k {
		return true
	}
	if containsKind(n.Input, k) {
		return true
	}
	for _, in := range n.Inputs {
		if containsKind(in, k) {
			return true
		}
	}
	return false
}
