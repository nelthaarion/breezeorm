package benchmark

import (
	"time"

	"github.com/uptrace/bun"
)

// BenchUser is the Breeze ORM model (also reused as the plain struct for
// database/sql and sqlx, which both use the `db` tag).
type BenchUser struct {
	ID        int64     `db:"id,pk,autoincrement"`
	Email     string    `db:"email"`
	Name      string    `db:"name"`
	Active    bool      `db:"active"`
	CreatedAt time.Time `db:"created_at"`
}

// BenchUserGorm is the GORM model.
type BenchUserGorm struct {
	ID        int64 `gorm:"column:id;primaryKey;autoIncrement"`
	Email     string
	Name      string
	Active    bool
	CreatedAt time.Time
}

func (BenchUserGorm) TableName() string { return "bench_users" }

// BenchUserBun is the Bun model.
type BenchUserBun struct {
	bun.BaseModel `bun:"table:bench_users"`

	ID        int64     `bun:"id,pk,autoincrement"`
	Email     string    `bun:"email"`
	Name      string    `bun:"name"`
	Active    bool      `bun:"active"`
	CreatedAt time.Time `bun:"created_at"`
}

// Task 2 — schema + model change
// Before: created_at DATETIME NOT NULL
// After:
const schemaSQL = `
CREATE TABLE bench_users (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL,
    name TEXT NOT NULL,
    active BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL

);`

// type User struct {
// 	ID        int64
// 	Email     string
// 	Name      string
// 	Active    bool
// 	CreatedAt xtypes.UnixTime `db:"created_at"`
// }
