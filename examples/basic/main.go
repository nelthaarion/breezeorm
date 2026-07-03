// Example: this shows the fluent API surface and SQL generation pipeline.
// It intentionally does not open a real database connection — this module
// has zero database driver dependencies by design (see pkg/orm/db.go), so
// wiring in `lib/pq`, `go-sql-driver/mysql`, etc. is left to the application.
// To run against a real Postgres instance, replace `demoDB()` with:
//
//	sqlDB, _ := sql.Open("postgres", "postgres://user:pass@localhost/db")
//	db := orm.Open(sqlDB, dialect.Postgres{})
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nelthaarion/breezeorm/pkg/compiler"
	"github.com/nelthaarion/breezeorm/pkg/dialect"
	"github.com/nelthaarion/breezeorm/pkg/execution"
	"github.com/nelthaarion/breezeorm/pkg/optimizer"
	"github.com/nelthaarion/breezeorm/pkg/query"
)

// User is a model. Struct tags drive the metadata compiler.
type User struct {
	ID        int64     `db:"id,pk,autoincrement"`
	Email     string    `db:"email,unique" validate:"required,email"`
	Name      string    `db:"name" validate:"required,min=2,max=100"`
	Active    bool      `db:"active,default=true"`
	CreatedAt time.Time `db:"created_at"`
}

func main() {
	d := dialect.Postgres{}

	b := query.New[User]("users").
		Select(
			query.SelectExpr{Expr: "id"},
			query.SelectExpr{Expr: "email"},
			query.SelectExpr{Expr: "name"},
		).
		Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true}).
		Where(query.Predicate{Column: "created_at", Op: query.OpGte, Value: time.Now().AddDate(0, -1, 0)}).
		OrderBy(query.OrderTerm{Column: "created_at", Desc: true}).
		Limit(20)

	cq, err := compiler.Compile(context.Background(), b, d, optimizer.DefaultPipeline(), nil)
	if err != nil {
		panic(err)
	}
	gen, err := execution.GenerateSQL(cq.Physical)
	if err != nil {
		panic(err)
	}

	fmt.Println("Generated SQL:")
	fmt.Println(" ", gen.SQL)
	fmt.Println("Args:", gen.Args)
	fmt.Println("Structural cache key:", cq.CacheKey)

	// Recompiling a structurally identical query (different literal values)
	// hits the same cache key — the whole point of the plan cache.
	b2 := query.New[User]("users").
		Select(query.SelectExpr{Expr: "id"}, query.SelectExpr{Expr: "email"}, query.SelectExpr{Expr: "name"}).
		Where(query.Predicate{Column: "active", Op: query.OpEq, Value: false}). // different literal
		Where(query.Predicate{Column: "created_at", Op: query.OpGte, Value: time.Now()}).
		OrderBy(query.OrderTerm{Column: "created_at", Desc: true}).
		Limit(20)
	cq2, _ := compiler.Compile(context.Background(), b2, d, optimizer.DefaultPipeline(), nil)
	fmt.Println("Same shape -> same cache key:", cq.CacheKey == cq2.CacheKey)
}
