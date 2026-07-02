package compiler

import (
	"testing"

	"github.com/nelthaarion/breezeorm/pkg/query"
)

type prehashWidget struct {
	ID   int64
	Name string
}

func TestPreHash_StableAcrossLiteralValues(t *testing.T) {
	mk := func(active bool) query.Builder[prehashWidget] {
		return query.New[prehashWidget]("widgets").
			Where(query.Predicate{Column: "active", Op: query.OpEq, Value: active}).
			Limit(10)
	}
	a := PreHash(mk(true), "postgres")
	b := PreHash(mk(false), "postgres")
	if a != b {
		t.Error("expected identical PreHash for queries differing only in a literal value")
	}
}

func TestPreHash_DiffersAcrossShape(t *testing.T) {
	base := query.New[prehashWidget]("widgets").
		Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true})
	withOrder := base.OrderBy(query.OrderTerm{Column: "id"})

	a := PreHash(base, "postgres")
	b := PreHash(withOrder, "postgres")
	if a == b {
		t.Error("expected different PreHash for queries with different structure (ORDER BY added)")
	}
}

func TestPreHash_DiffersAcrossDialect(t *testing.T) {
	b := query.New[prehashWidget]("widgets")
	a := PreHash(b, "postgres")
	c := PreHash(b, "mysql")
	if a == c {
		t.Error("expected different PreHash for different dialects (SQL text differs by dialect)")
	}
}
