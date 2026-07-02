package compiler

import (
	"testing"

	"github.com/nelthaarion/breezorm/pkg/query"
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

func TestPreHash_DeterministicAcrossCalls(t *testing.T) {
	// This is the correctness-critical property of the maphash-based
	// rewrite: hash/maphash's zero value picks a NEW random seed on first
	// write if SetSeed isn't called explicitly, which would make PreHash
	// return a different value every single call for identical input —
	// silently turning every cache keyed by it into a permanent miss. This
	// test would catch that regression immediately (it doesn't currently
	// fail, confirming the package-level hashSeed + explicit SetSeed is
	// wired correctly).
	b := query.New[prehashWidget]("widgets").
		Where(query.Predicate{Column: "id", Op: query.OpEq, Value: 1}).
		OrderBy(query.OrderTerm{Column: "id"})

	first := PreHash(b, "postgres")
	for i := 0; i < 1000; i++ {
		if got := PreHash(b, "postgres"); got != first {
			t.Fatalf("PreHash not deterministic: call %d = %d, want %d", i, got, first)
		}
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
