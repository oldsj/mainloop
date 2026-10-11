// Package testdb gives each test its own migrated PostgreSQL database.
//
// The first test in a binary creates a template database and migrates it once. Every test then
// clones the template (CREATE DATABASE ... TEMPLATE, a file copy) under a unique name and drops
// the clone on cleanup. Tests in a binary, and test binaries, can run in parallel.
//
// The server comes from MAINLOOP_TEST_DATABASE_URL. When it is unset the test is skipped, except
// under CI (CI=true), where it fails: a silent skip would hide missing database coverage.
//
// A package that uses testdb should end its test run with [Main] so the template is dropped:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }
//
// Databases left behind by a killed run are dropped by the next run once they are an hour old.
// The same sweep would drop the template of a test binary that has itself run for over an hour,
// so keep test runs shorter than that (go test's default timeout is ten minutes).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
)

const (
	templatePrefix = "mainloop_tpl_"
	clonePrefix    = "mainloop_t_"
	staleAfter     = time.Hour
	opTimeout      = 2 * time.Minute
)

// DB is one test's database.
type DB struct {
	// Pool connects with search_path=mainloop. Closed on test cleanup.
	Pool *pgxpool.Pool
	// URL connects to this database. It contains the password.
	URL string
	// Name is the database name.
	Name string
}

var (
	tplOnce sync.Once
	tplName string
	tplErr  error
	cloneN  atomic.Int64
)

// New creates a migrated database for t from the template and drops it when t finishes.
func New(t testing.TB) *DB {
	t.Helper()
	return create(t, true)
}

// NewEmpty creates an empty database (no schema, no migrations) for tests of the migrations
// themselves.
func NewEmpty(t testing.TB) *DB {
	t.Helper()
	return create(t, false)
}

func create(t testing.TB, migrated bool) *DB {
	t.Helper()
	adminURL := config.TestDatabaseURL()
	if adminURL == "" {
		if config.RequireTestDatabase() {
			t.Fatal("MAINLOOP_TEST_DATABASE_URL is not set, but CI requires database tests")
		}
		t.Skip("MAINLOOP_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	createSQL := "CREATE DATABASE %s"
	var args []any
	name := fmt.Sprintf("%s%d_%s_%d", clonePrefix, time.Now().Unix(), tplSuffix(), cloneN.Add(1))
	args = append(args, ident(name))
	if migrated {
		tplOnce.Do(func() { tplName, tplErr = createTemplate(ctx, adminURL) })
		if tplErr != nil {
			t.Fatalf("testdb: template: %v", tplErr)
		}
		createSQL += " TEMPLATE %s"
		args = append(args, ident(tplName))
	}

	if err := adminExec(ctx, adminURL, createSQL, args...); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
		defer dcancel()
		if err := adminExec(dctx, adminURL, "DROP DATABASE IF EXISTS %s WITH (FORCE)", ident(name)); err != nil {
			t.Errorf("testdb: drop database: %v", err)
		}
	})

	u, err := withDatabase(adminURL, name)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	pool, err := db.NewPoolFromURL(ctx, u)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	// Registered after the drop, so it runs first: connections must close before the drop.
	t.Cleanup(pool.Close)
	return &DB{Pool: pool, URL: u, Name: name}
}

// Main runs the package's tests and then drops the template database. Use it from TestMain.
func Main(m *testing.M) int {
	code := m.Run()
	// A template whose migration failed is dropped too: it would otherwise linger until the sweep.
	if tplName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = adminExec(ctx, config.TestDatabaseURL(), "DROP DATABASE IF EXISTS %s WITH (FORCE)", ident(tplName))
	}
	return code
}

// createTemplate drops stale leftovers, then creates and migrates the template database.
func createTemplate(ctx context.Context, adminURL string) (string, error) {
	dropStale(ctx, adminURL)
	name := fmt.Sprintf("%s%d_%s", templatePrefix, time.Now().Unix(), tplSuffix())
	if err := adminExec(ctx, adminURL, "CREATE DATABASE %s", ident(name)); err != nil {
		return "", fmt.Errorf("create template database: %w", err)
	}
	u, err := withDatabase(adminURL, name)
	if err != nil {
		return name, err
	}
	m, err := db.NewMigrator(ctx, u)
	if err != nil {
		return name, err
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Up(ctx); err != nil {
		return name, err
	}
	return name, nil
}

// tplSuffix is a random token that keeps names unique across test binaries.
func tplSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

// dropStale removes template and clone databases that an earlier killed run left behind. The
// creation time is in the name.
func dropStale(ctx context.Context, adminURL string) {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE 'mainloop\_t\_%' OR datname LIKE 'mainloop\_tpl\_%'`)
	if err != nil {
		return
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleAfter).Unix()
	for _, n := range names {
		rest := strings.TrimPrefix(strings.TrimPrefix(n, templatePrefix), clonePrefix)
		ts, _, _ := strings.Cut(rest, "_")
		unix, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || unix > cutoff {
			continue
		}
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(n)+" WITH (FORCE)")
	}
}

func adminExec(ctx context.Context, adminURL, format string, args ...any) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	_, err = conn.Exec(ctx, fmt.Sprintf(format, args...))
	return err
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func withDatabase(connURL, name string) (string, error) {
	u, err := url.Parse(connURL)
	if err != nil {
		return "", fmt.Errorf("MAINLOOP_TEST_DATABASE_URL is not a URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}
