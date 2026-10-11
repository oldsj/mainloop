package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func newMigrator(t *testing.T, url string) *db.Migrator {
	t.Helper()
	m, err := db.NewMigrator(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func countTables(t *testing.T, d *testdb.DB) int {
	t.Helper()
	var n int
	err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'mainloop' AND table_name <> 'goose_db_version'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateUpFromEmpty(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewEmpty(t)
	m := newMigrator(t, d.URL)

	before, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || before[0].Applied || before[0].Version != 10 {
		t.Fatalf("status before up = %+v", before)
	}

	n, err := m.Up(ctx)
	if err != nil || n != len(before) {
		t.Fatalf("Up = %d, %v; want %d", n, err, len(before))
	}
	if got := countTables(t, d); got != 6 {
		t.Errorf("tables after up = %d, want 6", got)
	}
	after, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range after {
		if !s.Applied || s.AppliedAt.IsZero() {
			t.Errorf("migration %s not applied: %+v", s.Name, s)
		}
	}
	// Idempotent.
	if n, err := m.Up(ctx); err != nil || n != 0 {
		t.Errorf("second Up = %d, %v; want 0", n, err)
	}
	// Nothing leaked into public.
	var leaked int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("tables in public = %d, err = %v", leaked, err)
	}
}

func TestMigrateResetRebuildsSchema(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	m := newMigrator(t, d.URL)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topics (id, user_id, name) VALUES ('t', 'u', 'n')`); err != nil {
		t.Fatal(err)
	}
	if err := m.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM topics`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("topics after reset = %d, err = %v", n, err)
	}
	if got := countTables(t, d); got != 6 {
		t.Errorf("tables after reset = %d, want 6", got)
	}
}

func TestMigrateConcurrentUp(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewEmpty(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			m, err := db.NewMigrator(ctx, d.URL)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = m.Close() }()
			_, err = m.Up(ctx)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := countTables(t, d); got != 6 {
		t.Errorf("tables = %d, want 6", got)
	}
}
