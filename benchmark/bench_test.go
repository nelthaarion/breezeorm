// Package benchmark compares Breeze ORM against database/sql (raw, prepared —
// the fastest-possible baseline), GORM, Bun, and sqlx, on PostgreSQL via the
// shared jackc/pgx driver. Run with:
//
//	go test -bench . -benchmem ./...
//
// Requires a reachable Postgres instance — see pgDSNBase in setup.go
// (override with BENCH_POSTGRES_DSN).
//
// See README.md in this directory for methodology notes and results.
package benchmark

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nelthaarion/breezeorm/pkg/dialect"
	"github.com/nelthaarion/breezeorm/pkg/orm"
	"github.com/nelthaarion/breezeorm/pkg/query"

	"github.com/jmoiron/sqlx"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// --- per-library setup -----------------------------------------------------

func openBreezeORM(b *testing.B, schema string) (*orm.DB, func()) {
	sqlDB := rawOpen(b, schema)
	db := orm.Open(sqlDB, dialect.Postgres{})
	return db, func() { db.Close() }
}

func openGorm(b *testing.B, schema string) (*gorm.DB, func()) {
	raw := rawOpen(b, schema)
	raw.Close() // schema is created + applied; GORM opens its own connection below, scoped to it via pgDSN's search_path
	g, err := gorm.Open(postgres.Open(pgDSN(schema)), &gorm.Config{
		Logger:      logger.Default.LogMode(logger.Silent),
		PrepareStmt: true, // GORM's own statement cache — fair comparison against Breeze ORM's
	})
	if err != nil {
		b.Fatalf("gorm.Open: %v", err)
	}
	sqlDB, err := g.DB()
	if err != nil {
		b.Fatalf("gorm underlying DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return g, func() { sqlDB.Close() }
}

func openBun(b *testing.B, schema string) (*bun.DB, func()) {
	sqlDB := rawOpen(b, schema)
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	return bunDB, func() { bunDB.Close() }
}

func openSqlx(b *testing.B, schema string) (*sqlx.DB, func()) {
	sqlDB := rawOpen(b, schema)
	sdb := sqlx.NewDb(sqlDB, "pgx")
	return sdb, func() { sdb.Close() }
}

// --- Insert ------------------------------------------------------------

func BenchmarkInsert(b *testing.B) {
	b.Run("raw_sql_prepared", func(b *testing.B) {
		db := rawOpen(b, newBenchSchema(b, "raw_insert"))
		defer db.Close()
		stmt, err := db.Prepare(`INSERT INTO bench_users (email, name, active, created_at) VALUES ($1, $2, $3, $4)`)
		if err != nil {
			b.Fatal(err)
		}
		defer stmt.Close()
		now := time.Now().UTC()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := stmt.Exec(fmt.Sprintf("ins%d@example.com", i), "Insert User", true, now); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("breezeorm", func(b *testing.B) {
		db, cleanup := openBreezeORM(b, newBenchSchema(b, "breezeorm_insert"))
		defer cleanup()
		ctx := context.Background()
		now := time.Now().UTC()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			u := BenchUser{Email: fmt.Sprintf("ins%d@example.com", i), Name: "Insert User", Active: true, CreatedAt: now}
			if err := orm.Model[BenchUser](db).Create(ctx, &u); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("gorm", func(b *testing.B) {
		g, cleanup := openGorm(b, newBenchSchema(b, "gorm_insert"))
		defer cleanup()
		now := time.Now().UTC()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			u := BenchUserGorm{Email: fmt.Sprintf("ins%d@example.com", i), Name: "Insert User", Active: true, CreatedAt: now}
			if err := g.Create(&u).Error; err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("bun", func(b *testing.B) {
		bunDB, cleanup := openBun(b, newBenchSchema(b, "bun_insert"))
		defer cleanup()
		ctx := context.Background()
		now := time.Now().UTC()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			u := BenchUserBun{Email: fmt.Sprintf("ins%d@example.com", i), Name: "Insert User", Active: true, CreatedAt: now}
			if _, err := bunDB.NewInsert().Model(&u).Exec(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("sqlx", func(b *testing.B) {
		sdb, cleanup := openSqlx(b, newBenchSchema(b, "sqlx_insert"))
		defer cleanup()
		now := time.Now().UTC()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := sdb.Exec(`INSERT INTO bench_users (email, name, active, created_at) VALUES ($1, $2, $3, $4)`,
				fmt.Sprintf("ins%d@example.com", i), "Insert User", true, now)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// --- FindByID ------------------------------------------------------------

func BenchmarkFindByID(b *testing.B) {
	b.Run("raw_sql_prepared", func(b *testing.B) {
		db := rawOpen(b, newBenchSchema(b, "raw_find"))
		defer db.Close()
		seedRows(b, db, seedRowCount)
		stmt, err := db.Prepare(`SELECT id, email, name, active, created_at FROM bench_users WHERE id = $1`)
		if err != nil {
			b.Fatal(err)
		}
		defer stmt.Close()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			var u BenchUser
			row := stmt.QueryRow(id)
			if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Active, &u.CreatedAt); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("breezeorm", func(b *testing.B) {
		db, cleanup := openBreezeORM(b, newBenchSchema(b, "breezeorm_find"))
		defer cleanup()
		seedRows(b, db.SQLDB(), seedRowCount)
		ctx := context.Background()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			_, err := orm.Model[BenchUser](db).
				Where(query.Predicate{Column: "id", Op: query.OpEq, Value: id}).
				First(ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("gorm", func(b *testing.B) {
		g, cleanup := openGorm(b, newBenchSchema(b, "gorm_find"))
		defer cleanup()
		sqlDB, _ := g.DB()
		seedRows(b, sqlDB, seedRowCount)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			var u BenchUserGorm
			if err := g.Where("id = ?", id).First(&u).Error; err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("bun", func(b *testing.B) {
		bunDB, cleanup := openBun(b, newBenchSchema(b, "bun_find"))
		defer cleanup()
		seedRows(b, bunDB.DB, seedRowCount)
		ctx := context.Background()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			u := new(BenchUserBun)
			if err := bunDB.NewSelect().Model(u).Where("id = ?", id).Scan(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("sqlx", func(b *testing.B) {
		sdb, cleanup := openSqlx(b, newBenchSchema(b, "sqlx_find"))
		defer cleanup()
		seedRows(b, sdb.DB, seedRowCount)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			var u BenchUser
			if err := sdb.Get(&u, "SELECT id, email, name, active, created_at FROM bench_users WHERE id = $1", id); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// --- SelectWhereLimit (list query) ------------------------------------------

func BenchmarkSelectWhereLimit(b *testing.B) {
	b.Run("raw_sql_prepared", func(b *testing.B) {
		db := rawOpen(b, newBenchSchema(b, "raw_select"))
		defer db.Close()
		seedRows(b, db, seedRowCount)
		stmt, err := db.Prepare(`SELECT id, email, name, active, created_at FROM bench_users WHERE active = $1 ORDER BY id LIMIT 50`)
		if err != nil {
			b.Fatal(err)
		}
		defer stmt.Close()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rows, err := stmt.Query(true)
			if err != nil {
				b.Fatal(err)
			}
			var out []BenchUser
			for rows.Next() {
				var u BenchUser
				if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Active, &u.CreatedAt); err != nil {
					b.Fatal(err)
				}
				out = append(out, u)
			}
			rows.Close()
		}
	})

	b.Run("breezeorm", func(b *testing.B) {
		db, cleanup := openBreezeORM(b, newBenchSchema(b, "breezeorm_select"))
		defer cleanup()
		seedRows(b, db.SQLDB(), seedRowCount)
		ctx := context.Background()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := orm.Model[BenchUser](db).
				Where(query.Predicate{Column: "active", Op: query.OpEq, Value: true}).
				OrderBy(query.OrderTerm{Column: "id"}).
				Limit(50).
				Find(ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("gorm", func(b *testing.B) {
		g, cleanup := openGorm(b, newBenchSchema(b, "gorm_select"))
		defer cleanup()
		sqlDB, _ := g.DB()
		seedRows(b, sqlDB, seedRowCount)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var us []BenchUserGorm
			if err := g.Where("active = ?", true).Order("id").Limit(50).Find(&us).Error; err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("bun", func(b *testing.B) {
		bunDB, cleanup := openBun(b, newBenchSchema(b, "bun_select"))
		defer cleanup()
		seedRows(b, bunDB.DB, seedRowCount)
		ctx := context.Background()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var us []BenchUserBun
			err := bunDB.NewSelect().Model(&us).Where("active = ?", true).OrderExpr("id").Limit(50).Scan(ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("sqlx", func(b *testing.B) {
		sdb, cleanup := openSqlx(b, newBenchSchema(b, "sqlx_select"))
		defer cleanup()
		seedRows(b, sdb.DB, seedRowCount)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var us []BenchUser
			err := sdb.Select(&us, "SELECT id, email, name, active, created_at FROM bench_users WHERE active = $1 ORDER BY id LIMIT 50", true)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// --- Update ------------------------------------------------------------

func BenchmarkUpdate(b *testing.B) {
	b.Run("raw_sql_prepared", func(b *testing.B) {
		db := rawOpen(b, newBenchSchema(b, "raw_update"))
		defer db.Close()
		seedRows(b, db, seedRowCount)
		stmt, err := db.Prepare(`UPDATE bench_users SET name = $1 WHERE id = $2`)
		if err != nil {
			b.Fatal(err)
		}
		defer stmt.Close()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			if _, err := stmt.Exec("updated", id); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("breezeorm", func(b *testing.B) {
		db, cleanup := openBreezeORM(b, newBenchSchema(b, "breezeorm_update"))
		defer cleanup()
		seedRows(b, db.SQLDB(), seedRowCount)
		ctx := context.Background()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			_, err := orm.Model[BenchUser](db).
				Where(query.Predicate{Column: "id", Op: query.OpEq, Value: id}).
				UpdateAll(ctx, query.Assignment{Column: "name", Value: "updated"})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("gorm", func(b *testing.B) {
		g, cleanup := openGorm(b, newBenchSchema(b, "gorm_update"))
		defer cleanup()
		sqlDB, _ := g.DB()
		seedRows(b, sqlDB, seedRowCount)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			if err := g.Model(&BenchUserGorm{}).Where("id = ?", id).Update("name", "updated").Error; err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("bun", func(b *testing.B) {
		bunDB, cleanup := openBun(b, newBenchSchema(b, "bun_update"))
		defer cleanup()
		seedRows(b, bunDB.DB, seedRowCount)
		ctx := context.Background()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			_, err := bunDB.NewUpdate().Model(&BenchUserBun{Name: "updated"}).Column("name").Where("id = ?", id).Exec(ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("sqlx", func(b *testing.B) {
		sdb, cleanup := openSqlx(b, newBenchSchema(b, "sqlx_update"))
		defer cleanup()
		seedRows(b, sdb.DB, seedRowCount)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := int64(i%seedRowCount) + 1
			if _, err := sdb.Exec("UPDATE bench_users SET name = $1 WHERE id = $2", "updated", id); err != nil {
				b.Fatal(err)
			}
		}
	})
}
