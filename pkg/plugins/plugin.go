// Package plugins implements the optional plugin system. Plugins hook into
// query compilation and execution at well-defined extension points. When no
// plugins are registered, the Chain below is a nil/empty slice and every
// hook call site is a single "len(chain) == 0" branch — the "zero runtime
// cost when disabled" requirement from the spec.
package plugins

import (
        "context"

        "github.com/nelthaarion/breezeorm/pkg/planner"
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

// RequestScopedPlugin is implemented by plugins whose BeforePlan output
// varies by request context (e.g. MultiTenancy, which injects a per-request
// tenant predicate). When such a plugin is present in a Chain, compileCached
// (pkg/orm/query.go) MUST bypass the compiled-plan cache — a cached plan
// baked with one request's context must never be served to a different
// request (cross-tenant data leak, wrong soft-delete scope, etc.).
//
// Plugins whose BeforePlan output is request-invariant (SoftDelete with a
// static column, Metrics, Tracing, Auditing — all of which either don't
// implement BeforePlan or produce the same plan for every request) do NOT
// implement this interface and are safe for plan caching.
//
// This is an optional interface checked via type assertion in IsCacheSafe,
// so existing plugins that don't implement it are unaffected — they're
// treated as cache-safe by default, which is correct for everything except
// MultiTenancy (which explicitly implements IsRequestScoped).
type RequestScopedPlugin interface {
        IsRequestScoped() bool
}

// IsCacheSafe reports whether every plugin in the chain declares itself as
// NOT request-scoped. Returns true for an empty chain (the common case —
// zero-cost check, just len()). Returns false if any plugin implements
// RequestScopedPlugin with IsRequestScoped() == true, which causes
// compileCached to bypass the plan cache for correctness.
//
// Performance: one type assertion per plugin per compileCached call. With
// an empty chain (the default benchmark configuration), this is a single
// len()==0 check — no type assertions, no loop, no overhead.
func (c Chain) IsCacheSafe() bool {
        for _, p := range c {
                if rs, ok := p.(RequestScopedPlugin); ok && rs.IsRequestScoped() {
                        return false
                }
        }
        return true
}

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
