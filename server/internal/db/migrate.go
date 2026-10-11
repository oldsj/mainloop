package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationStatus is one migration file and whether it has been applied.
type MigrationStatus struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// Migrator applies the embedded migrations to one database. Every operation runs under goose's
// session advisory lock, so concurrent migrate commands (two replicas starting at once) take
// turns.
type Migrator struct {
	sqlDB    *sql.DB
	provider *goose.Provider
}

// NewMigrator opens a Migrator for a connection string. Close it when done.
func NewMigrator(ctx context.Context, url string) (*Migrator, error) {
	cc, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse connection settings: %w", err)
	}
	cc.RuntimeParams["search_path"] = Schema
	sqlDB := stdlib.OpenDB(*cc)
	if _, err := sqlDB.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+Schema); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: create schema: %w", err)
	}
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: embedded migrations: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, files,
		goose.WithSessionLocker(locker))
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: migrations: %w", err)
	}
	return &Migrator{sqlDB: sqlDB, provider: provider}, nil
}

// Close releases the connections.
func (m *Migrator) Close() error { return m.sqlDB.Close() }

// Up applies every pending migration and returns how many ran.
func (m *Migrator) Up(ctx context.Context) (int, error) {
	results, err := m.provider.Up(ctx)
	if err != nil {
		return len(results), fmt.Errorf("db: migrate up: %w", err)
	}
	return len(results), nil
}

// Status lists every migration file in version order.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	rows, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: migration status: %w", err)
	}
	out := make([]MigrationStatus, 0, len(rows))
	for _, r := range rows {
		out = append(out, MigrationStatus{
			Version:   r.Source.Version,
			Name:      r.Source.Path,
			Applied:   r.State == goose.StateApplied,
			AppliedAt: r.AppliedAt,
		})
	}
	return out, nil
}

// Reset reverts every migration, then applies them again. It destroys all data. The caller
// decides whether that is allowed (see config.Migrate.AllowReset).
func (m *Migrator) Reset(ctx context.Context) error {
	if _, err := m.provider.DownTo(ctx, 0); err != nil && !errors.Is(err, goose.ErrNoNextVersion) {
		return fmt.Errorf("db: migrate reset: %w", err)
	}
	if _, err := m.Up(ctx); err != nil {
		return err
	}
	return nil
}
