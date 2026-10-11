package db_test

import (
	"context"
	"os"
	"strconv"
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

// wantTables is the table count of a freshly migrated database. It comes from the template the
// other tests clone, so adding a migration does not need a number bumped here.
func wantTables(t *testing.T) int {
	t.Helper()
	return countTables(t, testdb.New(t))
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
	if got := countTables(t, d); got != wantTables(t) {
		t.Errorf("tables after up = %d, want %d", got, wantTables(t))
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
	if got := countTables(t, d); got != wantTables(t) {
		t.Errorf("tables after reset = %d, want %d", got, wantTables(t))
	}
}

// A Migrator that already ran Up or Status holds a goose provider that remembers the version
// table; Reset drops that table, so it must build a fresh provider.
func TestMigrateResetAfterUp(t *testing.T) {
	ctx := context.Background()
	d := testdb.NewEmpty(t)
	m := newMigrator(t, d.URL)
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topics (id, user_id, name) VALUES ('t', 'u', 'n')`); err != nil {
		t.Fatal(err)
	}
	for round := range 2 {
		if err := m.Reset(ctx); err != nil {
			t.Fatalf("reset %d: %v", round, err)
		}
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM topics`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("round %d: topics = %d, err = %v", round, n, err)
		}
		if got := countTables(t, d); got != wantTables(t) {
			t.Errorf("round %d: tables = %d, want %d", round, got, wantTables(t))
		}
		rows, err := m.Status(ctx)
		if err != nil || len(rows) == 0 || !rows[len(rows)-1].Applied {
			t.Errorf("round %d: status = %+v, %v", round, rows, err)
		}
	}
}

// Migrators that start together on an empty database must all succeed: creating the schema is
// serialised. Several rounds of several migrators make a lucky pass very unlikely.
func TestMigrateConcurrentUp(t *testing.T) {
	const rounds, migrators = 4, 4
	ctx := context.Background()
	for round := range rounds {
		t.Run(strconv.Itoa(round), func(t *testing.T) {
			t.Parallel()
			d := testdb.NewEmpty(t)
			start := make(chan struct{})
			errs := make(chan error, migrators)
			for range migrators {
				go func() {
					<-start
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
			close(start)
			for range migrators {
				if err := <-errs; err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
			}
			if got := countTables(t, d); got != wantTables(t) {
				t.Fatalf("round %d: tables = %d, want %d", round, got, wantTables(t))
			}
		})
	}
}

// Reset must not depend on the Down sections or the embedded files matching what was applied.
func TestMigrateResetWithUnknownAppliedVersion(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999, true)`); err != nil {
		t.Fatal(err)
	}
	// A table no migration knows about, with a foreign key into one that does.
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE stray (id text PRIMARY KEY, session_id text REFERENCES sessions (id))`); err != nil {
		t.Fatal(err)
	}
	m := newMigrator(t, d.URL)
	if err := m.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countTables(t, d); got != wantTables(t) {
		t.Errorf("tables after reset = %d, want %d (stray table should be gone)", got, wantTables(t))
	}
	var stray int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id = 99999`).Scan(&stray); err != nil || stray != 0 {
		t.Errorf("unknown version survived reset: %d, %v", stray, err)
	}
	rows, err := m.Status(ctx)
	if err != nil || len(rows) == 0 || !rows[len(rows)-1].Applied {
		t.Errorf("status after reset = %+v, %v", rows, err)
	}
}
