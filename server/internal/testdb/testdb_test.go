package testdb_test

import (
	"context"
	"os"
	"testing"

	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func TestDatabaseIsMigrated(t *testing.T) {
	d := testdb.New(t)
	var n int
	err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'mainloop' AND table_name IN ('projects', 'sessions', 'credentials')`).Scan(&n)
	if err != nil || n != 3 {
		t.Fatalf("tables = %d, err = %v", n, err)
	}
}

func TestEmptyDatabaseHasNoSchema(t *testing.T) {
	d := testdb.NewEmpty(t)
	var n int
	err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_namespace WHERE nspname = 'mainloop'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("schemas = %d, err = %v", n, err)
	}
}

// Parallel tests each write the same primary key; neither may see the other's row.
func TestParallelTestsAreIsolated(t *testing.T) {
	for _, title := range []string{"first", "second"} {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := testdb.New(t)
			if _, err := d.Pool.Exec(ctx,
				`INSERT INTO topics (id, user_id, name) VALUES ('t1', 'u', $1)`, title); err != nil {
				t.Fatal(err)
			}
			var n int
			var name string
			if err := d.Pool.QueryRow(ctx, `SELECT count(*), min(name) FROM topics`).Scan(&n, &name); err != nil {
				t.Fatal(err)
			}
			if n != 1 || name != title {
				t.Fatalf("saw %d rows, name %q; want only %q", n, name, title)
			}
		})
	}
}
