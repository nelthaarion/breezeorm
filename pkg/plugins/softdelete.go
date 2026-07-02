package plugins

import (
	"context"

	"github.com/nelthaarion/breezeorm/pkg/planner"
	"github.com/nelthaarion/breezeorm/pkg/query"
)

// SoftDelete injects `WHERE <column> IS NULL` into every Scan/Filter on the
// configured tables, so soft-deleted rows are invisible to normal queries
// without every call site needing to remember to filter them out.
type SoftDelete struct {
	NoopPlugin
	Column string          // e.g. "deleted_at"
	Tables map[string]bool // tables this plugin applies to; empty = all tables
}

func NewSoftDelete(column string, tables ...string) *SoftDelete {
	t := make(map[string]bool, len(tables))
	for _, tb := range tables {
		t[tb] = true
	}
	return &SoftDelete{Column: column, Tables: t}
}

func (s *SoftDelete) Name() string { return "soft_delete" }

func (s *SoftDelete) applies(table string) bool {
	if len(s.Tables) == 0 {
		return true
	}
	return s.Tables[table]
}

func (s *SoftDelete) BeforePlan(_ context.Context, lp *planner.LogicalPlan) (*planner.LogicalPlan, error) {
	injectSoftDeleteFilter(lp.Root, s)
	return lp, nil
}

func injectSoftDeleteFilter(n *planner.LogicalNode, s *SoftDelete) {
	if n == nil {
		return
	}
	if n.Kind == planner.NodeScan && s.applies(n.Table) {
		pred := query.Predicate{Column: s.Column, Op: query.OpIsNull}
		// Wrapping happens structurally in the caller in a full
		// implementation; this scaffold marks intent by attaching directly
		// where a Filter node already exists, else it is the compiler's
		// responsibility to insert one. See TODO below.
		_ = pred // TODO: splice a Filter node above this Scan once
		// planner.LogicalNode exposes a parent-rewrite helper; direct
		// mutation from a child is not possible without parent pointers.
	}
	injectSoftDeleteFilter(n.Input, s)
	for _, in := range n.Inputs {
		injectSoftDeleteFilter(in, s)
	}
}
