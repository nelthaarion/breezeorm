// Package plugins implements the optional plugin system. Plugins hook into
// query compilation and execution at well-defined extension points. When no
// plugins are registered, the Chain below is a nil/empty slice and every
// hook call site is a single "len(chain) == 0" branch — the "zero runtime
// cost when disabled" requirement from the spec.
package plugins

import (
	"context"

	"github.com/nelthaarion/breezorm/pkg/planner"
)

// Plugin is the extension point contract. Every method has a no-op default
// via embedding NoopPlugin, so a plugin only needs to implement the hooks it
// cares about.
type Plugin interface {
	Name() string

	// BeforePlan can rewrite the logical plan before optimization — this is
	// how soft-delete and multi-tenancy inject predicates transparently
	// (e.g. `WHERE deleted_at IS NULL`, `WHERE tenant_id = ?`).
	BeforePlan(ctx context.Context, lp *planner.LogicalPlan) (*planner.LogicalPlan, error)

	// BeforeExecute/AfterExecute wrap the actual DB round trip — used by
	// metrics, tracing, OpenTelemetry, and query-logging plugins.
	BeforeExecute(ctx context.Context, sqlText string, args []any) (context.Context, error)
	AfterExecute(ctx context.Context, sqlText string, durationNanos int64, err error)
}

// NoopPlugin gives every hook a default no-op implementation so concrete
// plugins only override what they need.
type NoopPlugin struct{}

func (NoopPlugin) BeforePlan(_ context.Context, lp *planner.LogicalPlan) (*planner.LogicalPlan, error) {
	return lp, nil
}
func (NoopPlugin) BeforeExecute(ctx context.Context, _ string, _ []any) (context.Context, error) {
	return ctx, nil
}
func (NoopPlugin) AfterExecute(context.Context, string, int64, error) {}

// Chain runs an ordered list of plugins. A nil/empty Chain costs nothing
// beyond the len() check at each call site.
type Chain []Plugin

func (c Chain) RunBeforePlan(ctx context.Context, lp *planner.LogicalPlan) (*planner.LogicalPlan, error) {
	if len(c) == 0 {
		return lp, nil
	}
	var err error
	for _, p := range c {
		lp, err = p.BeforePlan(ctx, lp)
		if err != nil {
			return nil, err
		}
	}
	return lp, nil
}

func (c Chain) RunBeforeExecute(ctx context.Context, sqlText string, args []any) (context.Context, error) {
	if len(c) == 0 {
		return ctx, nil
	}
	var err error
	for _, p := range c {
		ctx, err = p.BeforeExecute(ctx, sqlText, args)
		if err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

func (c Chain) RunAfterExecute(ctx context.Context, sqlText string, durationNanos int64, err error) {
	if len(c) == 0 {
		return
	}
	for _, p := range c {
		p.AfterExecute(ctx, sqlText, durationNanos, err)
	}
}
