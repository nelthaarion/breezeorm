package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/nelthaarion/breezorm/pkg/compiler"
	"github.com/nelthaarion/breezorm/pkg/dialect"
	"github.com/nelthaarion/breezorm/pkg/optimizer"
	"github.com/nelthaarion/breezorm/pkg/query"
)

type widget struct {
	ID   int64
	Name string
}

func TestGenerateSQL_SimpleSelect(t *testing.T) {
	b := query.New[widget]("widgets").
		Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true}).
		OrderBy(query.OrderTerm{Column: "id"}).
		Limit(10)

	cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	gen, err := GenerateSQL(cq.Physical)
	if err != nil {
		t.Fatalf("GenerateSQL: %v", err)
	}

	wantSQL := `SELECT * FROM "widgets" WHERE "active" = $1 ORDER BY "id" LIMIT 10`
	if gen.SQL != wantSQL {
		t.Errorf("SQL =\n  %s\nwant:\n  %s", gen.SQL, wantSQL)
	}
	if len(gen.Args) != 1 || gen.Args[0] != true {
		t.Errorf("Args = %v, want [true]", gen.Args)
	}
}

func TestGenerateSQL_InsertReturning(t *testing.T) {
	b := query.New[widget]("widgets").Insert(
		query.Assignment{Column: "name", Value: "gadget"},
	)
	cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	gen, err := GenerateSQL(cq.Physical)
	if err != nil {
		t.Fatalf("GenerateSQL: %v", err)
	}
	if !strings.Contains(gen.SQL, "INSERT INTO \"widgets\"") || !strings.Contains(gen.SQL, "RETURNING") {
		t.Errorf("unexpected INSERT SQL: %s", gen.SQL)
	}
}

func TestGenerateSQL_CacheKeyStable(t *testing.T) {
	mk := func(active bool) *compiler.CompiledQuery {
		b := query.New[widget]("widgets").
			Where(query.Predicate{Column: "active", Op: query.OpEq, Value: active})
		cq, _ := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
		return cq
	}
	a := mk(true)
	c := mk(false)
	if a.CacheKey != c.CacheKey {
		t.Error("expected identical cache key for structurally identical queries with different literals")
	}
}

func TestGenerateSQL_RejectsInvalidIdentifier(t *testing.T) {
	// A malicious/buggy caller passing an unsanitized "column name" through
	// the typed API (e.g. from an unvalidated sort-field query param) must
	// be rejected before it ever reaches the SQL string, not merely quoted.
	b := query.New[widget]("widgets").
		OrderBy(query.OrderTerm{Column: `id"; DROP TABLE widgets; --`})

	cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = GenerateSQL(cq.Physical)
	if err == nil {
		t.Fatal("expected GenerateSQL to reject an invalid identifier, got nil error")
	}
	if !strings.Contains(err.Error(), "invalid identifier") {
		t.Errorf("error = %v, want it to mention invalid identifier", err)
	}
}

func TestGenerateSQL_RejectsInvalidTableName(t *testing.T) {
	b := query.New[widget](`widgets; DROP TABLE users`)
	cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := GenerateSQL(cq.Physical); err == nil {
		t.Fatal("expected GenerateSQL to reject an invalid table name")
	}
}

func TestGenerateSQL_RejectsStackedQueryInRawFragment(t *testing.T) {
	b := query.New[widget]("widgets").
		Where(query.Raw("id = 1; DROP TABLE widgets"))
	cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := GenerateSQL(cq.Physical); err == nil {
		t.Fatal("expected GenerateSQL to reject a raw fragment containing a statement separator")
	}
}

func TestGenerateSQL_AcceptsValidIdentifiers(t *testing.T) {
	// Sanity check the validator isn't so strict it rejects normal usage.
	b := query.New[widget]("widgets").
		Where(query.Predicate{Column: "created_at", Op: query.OpGte, Value: 1}).
		OrderBy(query.OrderTerm{Column: "id", Desc: true}).
		GroupBy("category_id")
	cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := GenerateSQL(cq.Physical); err != nil {
		t.Fatalf("unexpected rejection of valid identifiers: %v", err)
	}
}

func TestExtractArgs_MatchesGenerateSQL(t *testing.T) {
	// The whole point of splitting text/args caching is that ExtractArgs
	// must reproduce exactly what GenerateSQL would have bound, in the same
	// order, for every statement kind. This is the regression test the
	// comment above argCollector points at.
	cases := []query.Builder[widget]{
		query.New[widget]("widgets").
			Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true}).
			Where(query.Predicate{Column: "id", Op: query.OpIn, Value: []any{1, 2, 3}}).
			Having(query.Predicate{Column: "name", Op: query.OpBetween, Value: [2]any{"a", "z"}}).
			GroupBy("name"),
		query.New[widget]("widgets").Insert(
			query.Assignment{Column: "name", Value: "gadget"},
			query.Assignment{Column: "id", Value: 42},
		),
		query.New[widget]("widgets").
			Where(query.Predicate{Column: "id", Op: query.OpEq, Value: 7}).
			Update(query.Assignment{Column: "name", Value: "updated"}),
		query.New[widget]("widgets").
			Where(query.Predicate{Column: "id", Op: query.OpEq, Value: 9}).
			Delete(),
	}

	for i, b := range cases {
		cq, err := compiler.Compile(context.Background(), b, dialect.Postgres{}, optimizer.DefaultPipeline(), nil)
		if err != nil {
			t.Fatalf("case %d: Compile: %v", i, err)
		}
		gen, err := GenerateSQL(cq.Physical)
		if err != nil {
			t.Fatalf("case %d: GenerateSQL: %v", i, err)
		}
		args, err := ExtractArgs(cq.Physical)
		if err != nil {
			t.Fatalf("case %d: ExtractArgs: %v", i, err)
		}
		if len(args) != len(gen.Args) {
			t.Fatalf("case %d: ExtractArgs returned %d args, GenerateSQL bound %d: %v vs %v", i, len(args), len(gen.Args), args, gen.Args)
		}
		for j := range args {
			if args[j] != gen.Args[j] {
				t.Errorf("case %d: arg[%d] = %v, want %v", i, j, args[j], gen.Args[j])
			}
		}
	}
}

func TestGenerateBulkInsert(t *testing.T) {
	gen, err := GenerateBulkInsert(dialect.Postgres{}, "widgets", []string{"name", "id"}, [][]any{
		{"a", 1}, {"b", 2}, {"c", 3},
	})
	if err != nil {
		t.Fatalf("GenerateBulkInsert: %v", err)
	}
	if !strings.Contains(gen.SQL, "VALUES ($1, $2), ($3, $4), ($5, $6)") {
		t.Errorf("unexpected bulk insert SQL: %s", gen.SQL)
	}
	if len(gen.Args) != 6 {
		t.Fatalf("Args len = %d, want 6", len(gen.Args))
	}
}

func TestGenerateBulkInsert_RejectsOverCap(t *testing.T) {
	rows := make([][]any, MaxBulkInsertRows+1)
	for i := range rows {
		rows[i] = []any{i}
	}
	if _, err := GenerateBulkInsert(dialect.Postgres{}, "widgets", []string{"id"}, rows); err == nil {
		t.Fatal("expected GenerateBulkInsert to reject a row count over MaxBulkInsertRows")
	}
}

func TestGenerateBulkInsert_RejectsMismatchedRowLength(t *testing.T) {
	_, err := GenerateBulkInsert(dialect.Postgres{}, "widgets", []string{"a", "b"}, [][]any{{1, 2}, {3}})
	if err == nil {
		t.Fatal("expected GenerateBulkInsert to reject a row with the wrong number of values")
	}
}
