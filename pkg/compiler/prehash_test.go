package compiler

import (
        "testing"

        "github.com/nelthaarion/breezeorm/pkg/dialect"
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

// --- Task 1.1 regression tests: missing PreHash fields -------------------
//
// These tests guard against the cache-key collisions fixed in Task 1.1.
// Before the fix, PreHash omitted UpsertConflict, UpsertUpdateCols, CTEs,
// Preloads, and CursorAfter — so semantically different queries hashed to
// the same key and silently reused each other's cached plan.

func TestPreHash_UpsertConflictTarget_DifferentKeys(t *testing.T) {
        cols := []query.Assignment{
                {Column: "email", Value: "x@y.z"},
                {Column: "name", Value: "X"},
        }
        b1 := query.New[prehashWidget]("widgets").Upsert(cols, dialect.UpsertConflictTarget{Columns: []string{"email"}}, nil)
        b2 := query.New[prehashWidget]("widgets").Upsert(cols, dialect.UpsertConflictTarget{Columns: []string{"id"}}, nil)
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("different upsert conflict target columns must produce different PreHash keys")
        }
}

func TestPreHash_UpsertConflictConstraint_DifferentKeys(t *testing.T) {
        cols := []query.Assignment{{Column: "email", Value: "x@y.z"}}
        b1 := query.New[prehashWidget]("widgets").Upsert(cols, dialect.UpsertConflictTarget{Constraint: "users_email_key"}, nil)
        b2 := query.New[prehashWidget]("widgets").Upsert(cols, dialect.UpsertConflictTarget{Constraint: "users_pkey"}, nil)
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("different upsert conflict constraints must produce different PreHash keys")
        }
}

func TestPreHash_UpsertUpdateCols_DifferentKeys(t *testing.T) {
        cols := []query.Assignment{{Column: "email", Value: "x@y.z"}}
        b1 := query.New[prehashWidget]("widgets").Upsert(cols, dialect.UpsertConflictTarget{Columns: []string{"email"}}, []string{"name"})
        b2 := query.New[prehashWidget]("widgets").Upsert(cols, dialect.UpsertConflictTarget{Columns: []string{"email"}}, []string{"name", "updated_at"})
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("different upsert DO UPDATE SET column lists must produce different PreHash keys")
        }
}

func TestPreHash_CTEs_DifferentKeys(t *testing.T) {
        b1 := query.New[prehashWidget]("widgets").With(query.CTE{Name: "recent"})
        b2 := query.New[prehashWidget]("widgets").With(query.CTE{Name: "old"})
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("different CTE names must produce different PreHash keys")
        }
}

func TestPreHash_CTERecursiveFlag_DifferentKeys(t *testing.T) {
        b1 := query.New[prehashWidget]("widgets").With(query.CTE{Name: "tree", Recursive: false})
        b2 := query.New[prehashWidget]("widgets").With(query.CTE{Name: "tree", Recursive: true})
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("RECURSIVE vs non-RECURSIVE CTE must produce different PreHash keys")
        }
}

func TestPreHash_PreloadPath_DifferentKeys(t *testing.T) {
        b1 := query.New[prehashWidget]("widgets").Preload("Author")
        b2 := query.New[prehashWidget]("widgets").Preload("Comments")
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("different preload paths must produce different PreHash keys")
        }
}

func TestPreHash_PreloadOptions_DifferentKeys(t *testing.T) {
        b1 := query.New[prehashWidget]("widgets").Preload("Author")
        b2 := query.New[prehashWidget]("widgets").Preload("Author", func(s *query.PreloadSpec) { s.Batch = true })
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("preload with Batch=true vs default must produce different PreHash keys")
        }
}

func TestPreHash_CursorAfterPresence_DifferentKeys(t *testing.T) {
        b1 := query.New[prehashWidget]("widgets").OrderBy(query.OrderTerm{Column: "id"})
        b2 := query.New[prehashWidget]("widgets").OrderBy(query.OrderTerm{Column: "id"}).After("cursor-token")
        if PreHash(b1, "postgres") == PreHash(b2, "postgres") {
                t.Fatal("presence vs absence of After() cursor must produce different PreHash keys")
        }
}

// Regression: literal values must NOT affect the hash (existing invariant,
// re-asserted since Task 1.1 added new code paths).
func TestPreHash_LiteralValues_DoNotAffectKey(t *testing.T) {
        b1 := query.New[prehashWidget]("widgets").Where(query.Predicate{Column: "id", Op: query.OpEq, Value: int64(1)})
        b2 := query.New[prehashWidget]("widgets").Where(query.Predicate{Column: "id", Op: query.OpEq, Value: int64(42)})
        if PreHash(b1, "postgres") != PreHash(b2, "postgres") {
                t.Fatal("Where(id=1) and Where(id=42) must share a PreHash key")
        }
}
