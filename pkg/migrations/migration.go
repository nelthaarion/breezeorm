// Package migrations implements the migration engine: a version table,
// ordered up/down migrations, seeding, rollback, and (scaffolded) automatic
// schema diffing for auto-migration.
package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Migration is a single versioned schema change.
type Migration struct {
	Version string // sortable, e.g. "20260701120000"
	Name    string
	Up      func(ctx context.Context, tx *sql.Tx) error
	Down    func(ctx context.Context, tx *sql.Tx) error
	Seed    func(ctx context.Context, tx *sql.Tx) error
}

// Migrator applies and rolls back Migrations against a *sql.DB, tracking
// applied versions in a version table.
type Migrator struct {
	db           *sql.DB
	versionTable string
	migrations   []Migration
}

// New creates a Migrator. versionTable defaults to "schema_migrations".
func New(db *sql.DB, versionTable string) *Migrator {
	if versionTable == "" {
		versionTable = "schema_migrations"
	}
	return &Migrator{db: db, versionTable: versionTable}
}

// Register adds migrations to the ordered set (sorted by Version on Up/Down).
func (m *Migrator) Register(migs ...Migration) {
	m.migrations = append(m.migrations, migs...)
}

// EnsureVersionTable creates the tracking table if it doesn't exist.
// The exact DDL is dialect-specific in production; this scaffold uses
// widely-portable syntax that works across all four target databases for
// this one bootstrap table.
func (m *Migrator) EnsureVersionTable(ctx context.Context) error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		version VARCHAR(255) PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		applied_at TIMESTAMP NOT NULL
	)`, m.versionTable)
	_, err := m.db.ExecContext(ctx, ddl)
	return err
}

// AppliedVersions returns the set of already-applied migration versions.
func (m *Migrator) AppliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := m.db.QueryContext(ctx, fmt.Sprintf("SELECT version FROM %s", m.versionTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// Up applies all pending migrations, in version order, each in its own
// transaction (so a failure mid-run leaves prior migrations committed).
func (m *Migrator) Up(ctx context.Context) error {
	if err := m.EnsureVersionTable(ctx); err != nil {
		return fmt.Errorf("migrations: ensure version table: %w", err)
	}
	applied, err := m.AppliedVersions(ctx)
	if err != nil {
		return err
	}

	sorted := append([]Migration{}, m.migrations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })

	for _, mig := range sorted {
		if applied[mig.Version] {
			continue
		}
		if err := m.runOne(ctx, mig, true); err != nil {
			return fmt.Errorf("migrations: up %s (%s): %w", mig.Version, mig.Name, err)
		}
	}
	return nil
}

// Down rolls back the most recently applied migration.
func (m *Migrator) Down(ctx context.Context) error {
	applied, err := m.AppliedVersions(ctx)
	if err != nil {
		return err
	}
	sorted := append([]Migration{}, m.migrations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version > sorted[j].Version })

	for _, mig := range sorted {
		if !applied[mig.Version] {
			continue
		}
		return m.runOne(ctx, mig, false)
	}
	return fmt.Errorf("migrations: nothing to roll back")
}

func (m *Migrator) runOne(ctx context.Context, mig Migration, up bool) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if up {
		if mig.Up != nil {
			if err := mig.Up(ctx, tx); err != nil {
				return err
			}
		}
		if mig.Seed != nil {
			if err := mig.Seed(ctx, tx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (version, name, applied_at) VALUES (?, ?, ?)", m.versionTable),
			mig.Version, mig.Name, time.Now().UTC(),
		); err != nil {
			return err
		}
	} else {
		if mig.Down != nil {
			if err := mig.Down(ctx, tx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE version = ?", m.versionTable), mig.Version,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
