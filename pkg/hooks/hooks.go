// Package hooks defines the lifecycle hook interfaces a model may implement.
// pkg/orm checks for these interfaces (via type assertion) at the relevant
// point in each operation's execution — no reflection needed since Go
// interface satisfaction is checked at the call site.
package hooks

import "context"

type BeforeCreate interface {
	BeforeCreate(ctx context.Context) error
}
type AfterCreate interface {
	AfterCreate(ctx context.Context) error
}

type BeforeUpdate interface {
	BeforeUpdate(ctx context.Context) error
}
type AfterUpdate interface {
	AfterUpdate(ctx context.Context) error
}

type BeforeDelete interface {
	BeforeDelete(ctx context.Context) error
}
type AfterDelete interface {
	AfterDelete(ctx context.Context) error
}

type BeforeSave interface {
	BeforeSave(ctx context.Context) error
}
type AfterSave interface {
	AfterSave(ctx context.Context) error
}

// BeforeQuery/AfterQuery operate on the query builder state rather than a
// model instance, since no row exists yet when a query begins.
type BeforeQuery interface {
	BeforeQuery(ctx context.Context) error
}
type AfterQuery interface {
	AfterQuery(ctx context.Context, rowCount int) error
}

// Chain runs a set of hook functions in order, stopping at the first error.
func Chain(fns ...func() error) error {
	for _, fn := range fns {
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

// RunBeforeCreate invokes BeforeCreate on model if it implements the interface.
func RunBeforeCreate(ctx context.Context, model any) error {
	if h, ok := model.(BeforeCreate); ok {
		return h.BeforeCreate(ctx)
	}
	return nil
}

func RunAfterCreate(ctx context.Context, model any) error {
	if h, ok := model.(AfterCreate); ok {
		return h.AfterCreate(ctx)
	}
	return nil
}

func RunBeforeUpdate(ctx context.Context, model any) error {
	if h, ok := model.(BeforeUpdate); ok {
		return h.BeforeUpdate(ctx)
	}
	return nil
}

func RunAfterUpdate(ctx context.Context, model any) error {
	if h, ok := model.(AfterUpdate); ok {
		return h.AfterUpdate(ctx)
	}
	return nil
}

func RunBeforeDelete(ctx context.Context, model any) error {
	if h, ok := model.(BeforeDelete); ok {
		return h.BeforeDelete(ctx)
	}
	return nil
}

func RunAfterDelete(ctx context.Context, model any) error {
	if h, ok := model.(AfterDelete); ok {
		return h.AfterDelete(ctx)
	}
	return nil
}

func RunBeforeSave(ctx context.Context, model any) error {
	if h, ok := model.(BeforeSave); ok {
		return h.BeforeSave(ctx)
	}
	return nil
}

func RunAfterSave(ctx context.Context, model any) error {
	if h, ok := model.(AfterSave); ok {
		return h.AfterSave(ctx)
	}
	return nil
}
