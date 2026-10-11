package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// goose polls for its lock; its default is every 5s for 5 minutes. Poll every second instead, so
// migrators that queue behind a running one start within a second of it finishing.
const (
	lockPollSeconds = 1
	lockWaitSeconds = 300
)

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
	sqlDB *sql.DB
	files fs.FS

	// provider caches what goose learned about the version table (created, or not), so Reset
	// replaces it after dropping the schema.
	mu       sync.Mutex
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
	if err := withSchemaLock(ctx, sqlDB, func(c *sql.Conn) error { return createSchema(ctx, c) }); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: embedded migrations: %w", err)
	}
	m := &Migrator{sqlDB: sqlDB, files: files}
	if _, err := m.newProvider(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return m, nil
}

// newProvider builds a goose provider with a fresh view of the database and makes it current.
func (m *Migrator) newProvider() (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(lockPollSeconds, lockWaitSeconds))
	if err != nil {
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, m.sqlDB, m.files,
		goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("db: migrations: %w", err)
	}
	m.mu.Lock()
	m.provider = provider
	m.mu.Unlock()
	return provider, nil
}

func (m *Migrator) current() *goose.Provider {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.provider
}

// Close releases the connections.
func (m *Migrator) Close() error { return m.sqlDB.Close() }

// Up applies every pending migration and returns how many ran.
func (m *Migrator) Up(ctx context.Context) (int, error) {
	results, err := m.current().Up(ctx)
	if err != nil {
		return len(results), fmt.Errorf("db: migrate up: %w", err)
	}
	return len(results), nil
}

// Status lists every migration file in version order.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	rows, err := m.current().Status(ctx)
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

// Reset drops the whole schema, recreates it and applies every migration. It destroys all data.
// It does not read any Down section, so it works however the migration files changed since the
// database was last migrated (an edited file, a deleted one). The caller decides whether it is
// allowed (see config.Migrate.AllowReset).
func (m *Migrator) Reset(ctx context.Context) error {
	err := withSchemaLock(ctx, m.sqlDB, func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+Schema+" CASCADE"); err != nil {
			return fmt.Errorf("db: reset: drop schema: %w", err)
		}
		return createSchema(ctx, c)
	})
	if err != nil {
		return err
	}
	// The old provider may remember a version table that is now gone.
	if _, err := m.newProvider(); err != nil {
		return err
	}
	_, err = m.Up(ctx)
	return err
}

// schemaLockName keys the advisory lock that serialises creating and dropping the schema. goose's
// own lock cannot cover this: its version table lives inside the schema.
const schemaLockName = "mainloop:migrate-schema"

func createSchema(ctx context.Context, c *sql.Conn) error {
	if _, err := c.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+Schema); err != nil {
		return fmt.Errorf("db: create schema: %w", err)
	}
	return nil
}

// withSchemaLock runs fn on a dedicated connection that holds the schema lock. CREATE SCHEMA IF
// NOT EXISTS is not safe to run concurrently (two sessions can both pass the existence check and
// one then fails on the catalog's unique index), so migrators take turns.
func withSchemaLock(ctx context.Context, sqlDB *sql.DB, fn func(c *sql.Conn) error) error {
	c, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db: schema lock: %w", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.ExecContext(ctx, "SELECT pg_advisory_lock("+lockKey+")", schemaLockName); err != nil {
		return fmt.Errorf("db: schema lock: %w", err)
	}
	// Closing the connection would release the lock too, but pooled connections are reused, so
	// release it explicitly and, if that fails, discard the connection. A deferred unlock also
	// runs when fn panics.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, err := c.ExecContext(unlockCtx, "SELECT pg_advisory_unlock("+lockKey+")", schemaLockName); err != nil {
			_ = c.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return fn(c)
}
