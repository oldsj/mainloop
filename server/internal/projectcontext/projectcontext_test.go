package projectcontext_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

func newService(t *testing.T) *projectcontext.Service {
	t.Helper()
	db := testdb.New(t)
	for _, p := range []struct{ id, user string }{{"p1", owner}, {"p2", "someone-else"}} {
		if _, err := db.Pool.Exec(context.Background(),
			`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/' || $1, 'https://github.com/o/' || $1)`,
			p.id, p.user); err != nil {
			t.Fatal(err)
		}
	}
	return &projectcontext.Service{Pool: db.Pool, OwnerID: owner}
}

func TestInstructionsAreVersionedAndConflictChecked(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	in, err := svc.Instructions(ctx, "p1")
	if err != nil || in.Version != 0 || in.Text != "" || in.UpdatedAt != nil {
		t.Fatalf("unset = %+v, %v", in, err)
	}
	v1, err := svc.SetInstructions(ctx, "p1", "  Use fictional data.\n", 0)
	if err != nil || v1.Version != 1 || v1.Text != "Use fictional data." || v1.UpdatedAt == nil {
		t.Fatalf("v1 = %+v, %v", v1, err)
	}
	// A save from a stale read is refused and changes nothing.
	if _, err := svc.SetInstructions(ctx, "p1", "other", 0); !errors.Is(err, projectcontext.ErrVersionConflict) {
		t.Fatalf("stale save: %v", err)
	}
	// The same text again keeps the version.
	if same, err := svc.SetInstructions(ctx, "p1", "Use fictional data.", 1); err != nil || same.Version != 1 {
		t.Fatalf("same = %+v, %v", same, err)
	}
	if v2, err := svc.SetInstructions(ctx, "p1", "", 1); err != nil || v2.Version != 2 || v2.Text != "" {
		t.Fatalf("clear = %+v, %v", v2, err)
	}
	if projectcontext.Section(projectcontext.Instructions{ProjectID: "p1", Version: 2}) != "" {
		t.Fatal("cleared instructions render a section")
	}
	vs, err := svc.Versions(ctx, "p1")
	if err != nil || len(vs) != 2 || vs[0].Version != 2 || vs[1].Version != 1 || vs[1].Text != "Use fictional data." {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
}

func TestInstructionsAreBoundedAndOwnerScoped(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	if _, err := svc.SetInstructions(ctx, "p1", strings.Repeat("é", projectcontext.MaxInstructionsBytes/2+1), 0); !errors.Is(err, projectcontext.ErrTooLarge) {
		t.Fatalf("over the byte bound: %v", err)
	}
	if in, err := svc.SetInstructions(ctx, "p1", strings.Repeat("x", projectcontext.MaxInstructionsBytes), 0); err != nil || in.Version != 1 {
		t.Fatalf("at the bound: %+v, %v", in, err)
	}
	if _, err := svc.SetInstructions(ctx, "p1", "bad\x00", 1); !errors.Is(err, projectcontext.ErrInvalidText) {
		t.Fatalf("NUL: %v", err)
	}
	if _, err := svc.SetInstructions(ctx, "p1", "bad\xff", 1); !errors.Is(err, projectcontext.ErrInvalidText) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
	for _, id := range []string{"p2", "missing"} {
		if _, err := svc.Instructions(ctx, id); !errors.Is(err, projectcontext.ErrNotFound) {
			t.Errorf("%s read: %v", id, err)
		}
		if _, err := svc.SetInstructions(ctx, id, "x", 0); !errors.Is(err, projectcontext.ErrNotFound) {
			t.Errorf("%s write: %v", id, err)
		}
		if _, err := svc.Versions(ctx, id); !errors.Is(err, projectcontext.ErrNotFound) {
			t.Errorf("%s versions: %v", id, err)
		}
	}
}

// TestConcurrentSavesFromOneReadKeepOne: two saves made from the same read race; exactly one
// becomes the next version and the other is told the instructions changed.
func TestConcurrentSavesFromOneReadKeepOne(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = svc.SetInstructions(ctx, "p1", strings.Repeat("v", i+1), 0)
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, projectcontext.ErrVersionConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	vs, err := svc.Versions(ctx, "p1")
	if ok != 1 || err != nil || len(vs) != 1 {
		t.Fatalf("saved %d, versions %+v, %v", ok, vs, err)
	}
	if cur, err := projectcontext.Current(ctx, dbq.New(svc.Pool), "p1"); err != nil || cur.Version != 1 {
		t.Fatalf("current = %+v, %v", cur, err)
	}
}
