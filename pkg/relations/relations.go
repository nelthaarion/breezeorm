// Package relations implements relationship loading strategies (lazy, eager,
// preload, nested preload, conditional preload, batch preload) on top of
// compiled metadata.Relationship edges. The actual query construction for a
// given relation kind lives here; pkg/orm wires it into Builder.Preload(...).
package relations

import (
	"context"
	"fmt"

	"github.com/nelthaarion/breezeorm/pkg/metadata"
)

// LoadStrategy identifies how a relation's data should be fetched.
type LoadStrategy uint8

const (
	StrategyLazy LoadStrategy = iota
	StrategyEager
	StrategyBatch // single WHERE IN(...) query for the whole parent result set
)

// Loader loads related rows for a batch of parent primary keys. Each
// relation kind (HasOne, HasMany, BelongsTo, ManyToMany) gets its own Loader
// implementation; all share this interface so pkg/orm's preload dispatcher
// stays generic.
type Loader interface {
	// Load fetches related rows keyed by the parent PK values that produced
	// them, so the caller can assign results back onto the right parent
	// struct without a second round trip per parent (the N+1 query the
	// "batch preload" feature exists to avoid).
	Load(ctx context.Context, rel *metadata.Relationship, parentKeys []any) (map[any][]any, error)
}

// HasManyLoader loads the "many" side of a HasMany relation with a single
// batched query: WHERE fk_column IN (parentKeys...).
type HasManyLoader struct {
	// Query is injected by pkg/orm to avoid an import cycle (relations
	// doesn't know about query.Builder or the executor); it receives the
	// relation and parent key batch and returns rows already grouped by FK
	// value.
	Query func(ctx context.Context, rel *metadata.Relationship, parentKeys []any) (map[any][]any, error)
}

func (l *HasManyLoader) Load(ctx context.Context, rel *metadata.Relationship, parentKeys []any) (map[any][]any, error) {
	if l.Query == nil {
		return nil, fmt.Errorf("relations: HasManyLoader.Query not configured")
	}
	return l.Query(ctx, rel, parentKeys)
}

// BelongsToLoader loads the "one" side referenced by a foreign key on the
// parent — a single WHERE pk IN (fkValues...) batched query.
type BelongsToLoader struct {
	Query func(ctx context.Context, rel *metadata.Relationship, fkValues []any) (map[any][]any, error)
}

func (l *BelongsToLoader) Load(ctx context.Context, rel *metadata.Relationship, fkValues []any) (map[any][]any, error) {
	if l.Query == nil {
		return nil, fmt.Errorf("relations: BelongsToLoader.Query not configured")
	}
	return l.Query(ctx, rel, fkValues)
}

// ManyToManyLoader loads related rows through a join table.
type ManyToManyLoader struct {
	Query func(ctx context.Context, rel *metadata.Relationship, parentKeys []any) (map[any][]any, error)
}

func (l *ManyToManyLoader) Load(ctx context.Context, rel *metadata.Relationship, parentKeys []any) (map[any][]any, error) {
	if l.Query == nil {
		return nil, fmt.Errorf("relations: ManyToManyLoader.Query not configured")
	}
	return l.Query(ctx, rel, parentKeys)
}

// Registry resolves the correct Loader for a relation kind.
type Registry struct {
	hasOne     Loader
	hasMany    Loader
	belongsTo  Loader
	manyToMany Loader
}

func NewRegistry(hasOne, hasMany, belongsTo, manyToMany Loader) *Registry {
	return &Registry{hasOne: hasOne, hasMany: hasMany, belongsTo: belongsTo, manyToMany: manyToMany}
}

func (r *Registry) For(kind metadata.RelationKind) (Loader, error) {
	switch kind {
	case metadata.RelationHasOne:
		return r.hasOne, nil
	case metadata.RelationHasMany:
		return r.hasMany, nil
	case metadata.RelationBelongsTo:
		return r.belongsTo, nil
	case metadata.RelationManyToMany:
		return r.manyToMany, nil
	default:
		return nil, fmt.Errorf("relations: unknown relation kind %d", kind)
	}
}
