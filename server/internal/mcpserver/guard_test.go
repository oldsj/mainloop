package mcpserver_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/testdb"
	"github.com/oldsj/mainloop/server/internal/topics"
)

func guardEnv(t *testing.T) (*authtest.Env, agentauth.Actor, *topics.Service) {
	t.Helper()
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	seeded := env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	actor, err := (&agentauth.Authenticator{Q: dbq.New(d.Pool)}).Authenticate(context.Background(), seeded.Token)
	if err != nil {
		t.Fatal(err)
	}
	return env, actor, &topics.Service{Pool: d.Pool}
}

// A write a tool makes after its credential was revoked is refused and stores nothing, even though
// the caller authenticated before the revoke.
func TestAWriteAfterTheCredentialWasRevokedIsRefused(t *testing.T) {
	env, actor, svc := guardEnv(t)
	ctx := context.Background()
	if _, err := svc.Add(agentauth.Guarded(ctx, actor), actor.UserID, actor.BindingID, dbstate.RecordNote, "before", ""); err != nil {
		t.Fatalf("a write with a published credential: %v", err)
	}
	if _, err := agentauth.RevokeInTx(ctx, dbq.New(env.Pool), actor.BindingID); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Add(agentauth.Guarded(ctx, actor), actor.UserID, actor.BindingID, dbstate.RecordNote, "after", "")
	if !errors.Is(err, agentauth.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var n int
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_records WHERE text = 'after'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d records stored after the revoke (err %v)", n, err)
	}
}

// A revoke waits for a write transaction that already holds the credential, and the write commits.
func TestARevokeWaitsForAnInFlightToolWrite(t *testing.T) {
	env, actor, _ := guardEnv(t)
	ctx := context.Background()
	inWrite, release := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- db.InTx(agentauth.Guarded(ctx, actor), env.Pool, func(tx pgx.Tx) error {
			close(inWrite)
			<-release
			_, err := tx.Exec(ctx, `UPDATE sessions SET title = 'written under the lock' WHERE id = $1`, actor.BindingID)
			return err
		})
	}()
	<-inWrite
	revoked := make(chan error, 1)
	go func() {
		_, err := agentauth.RevokeInTx(ctx, dbq.New(env.Pool), actor.BindingID)
		revoked <- err
	}()
	select {
	case err := <-revoked:
		t.Fatalf("the revoke finished (%v) while a tool write held the credential", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatalf("the in-flight write: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("the revoke: %v", err)
	}
	var title string
	if err := env.Pool.QueryRow(ctx, `SELECT title FROM sessions WHERE id = $1`, actor.BindingID).Scan(&title); err != nil || title != "written under the lock" {
		t.Fatalf("title = %q, err %v", title, err)
	}
	if err := db.InTx(agentauth.Guarded(ctx, actor), env.Pool, func(pgx.Tx) error { return nil }); !errors.Is(err, agentauth.ErrUnauthorized) {
		t.Fatalf("a write after the revoke: %v, want ErrUnauthorized", err)
	}
}
