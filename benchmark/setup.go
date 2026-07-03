package benchmark

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgDSNBase is the Postgres connection string all benchmarks connect
// through, without any per-benchmark schema pinned. Override with
// BENCH_POSTGRES_DSN (e.g. in CI) to point at a different server; the
// default assumes a local instance reserved for benchmarking.
func pgDSNBase() string {
	if v := os.Getenv("BENCH_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/breezeorm_bench?sslmode=disable"
}

// pgDSN returns pgDSNBase with search_path pinned to schema, via libpq's
// standard `search_path` connection parameter (respected by pgx). Every
// library's connection — including GORM's, which opens its own connection
// separately from rawOpen — goes through this so unqualified table names
// (e.g. "bench_users") all resolve inside that one benchmark's schema.
func pgDSN(schema string) string {
	base := pgDSNBase()
	sep := "&"
	if !strings.Contains(base, "?") {
		sep = "?"
	}
	return base + sep + "search_path=" + schema
}

// newBenchSchema returns a per-library, per-benchmark Postgres schema name.
// Postgres has no equivalent of "a fresh SQLite file" — the isolation this
// benchmark relies on (each library gets its own tables, none of them
// contend with or see each other's data) is provided by giving each library
// its own schema instead, via pgDSN's search_path.
func newBenchSchema(b *testing.B, name string) string {
	b.Helper()
	return "bench_" + name
}

// rawOpen (re)creates schema on a throwaway admin connection, then opens
// and returns the *sql.DB every library in this benchmark actually uses —
// pinned to that schema via pgDSN, over the shared pgx driver (Bun and sqlx
// both accept a *sql.DB directly, and GORM's postgres driver wraps pgx
// internally too). The pool is pinned to a single connection so search_path
// and everything else about session state stays consistent across the
// whole benchmark, mirroring the single-connection setup the old SQLite
// version used (there for lock-avoidance; here, for consistency).
func rawOpen(b *testing.B, schema string) *sql.DB {
	b.Helper()

	admin, err := sql.Open("pgx", pgDSNBase())
	if err != nil {
		b.Fatalf("open postgres (admin): %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema)); err != nil {
		admin.Close()
		b.Fatalf("drop schema %s: %v", schema, err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`CREATE SCHEMA %s`, schema)); err != nil {
		admin.Close()
		b.Fatalf("create schema %s: %v", schema, err)
	}
	admin.Close()

	db, err := sql.Open("pgx", pgDSN(schema))
	if err != nil {
		b.Fatalf("open postgres: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		b.Fatalf("apply schema: %v", err)
	}
	return db
}

// seedRows inserts n rows directly via database/sql (bypassing every ORM
// under test) so seed cost is identical and never counted against any
// library's benchmark numbers.
func seedRows(b *testing.B, db *sql.DB, n int) {
	b.Helper()
	tx, err := db.Begin()
	if err != nil {
		b.Fatalf("begin seed tx: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO bench_users (email, name, active, created_at) VALUES ($1, $2, $3, $4)`)
	if err != nil {
		b.Fatalf("prepare seed stmt: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		active := i%2 == 0
		if _, err := stmt.Exec(fmt.Sprintf("user%d@example.com", i), fmt.Sprintf("User %d", i), active, now); err != nil {
			b.Fatalf("seed row %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit seed tx: %v", err)
	}
}

// seedRowCount is the fixed dataset size used for all read/update
// benchmarks, chosen to be large enough that an index scan and a full
// table scan behave differently, small enough to seed quickly per benchmark.
const seedRowCount = 10000
