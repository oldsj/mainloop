package agentauth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// freeName revokes a seeded credential and waits for its Secret to go, so the binding can be issued
// again.
func freeName(t *testing.T, env *authtest.Env, s authtest.Seeded) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	runCleanup(t, env, func() bool {
		got, _ := dbq.New(env.Pool).GetCredential(ctx, s.CredentialID)
		return got.State == dbstate.CredentialDeleted
	})
}

func TestIssueRefusesABindingWhoseSessionHasEnded(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	for name, a := range map[string]authtest.Agent{
		"archived":  {Archived: true},
		"completed": {Status: dbstate.SessionCompleted},
		"failed":    {Status: dbstate.SessionFailed},
		"cancelled": {Status: dbstate.SessionCancelled},
	} {
		a.Role, a.Grant = dbstate.RoleAgent, dbstate.GrantWorkspace
		s := env.Seed(t, a)
		freeName(t, env, s)
		if _, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP); !errors.Is(err, agentauth.ErrBindingEnded) {
			t.Errorf("%s: Issue err = %v, want ErrBindingEnded", name, err)
		}
		if env.Cluster.UID(authtest.Namespace, "mainloop-mcp-"+s.SessionID) != "" {
			t.Errorf("%s: a Secret was created for an ended session", name)
		}
		if live, _ := dbq.New(d.Pool).ListCredentialsByBinding(ctx, s.SessionID); len(live) != 1 {
			t.Errorf("%s: %d credential rows, want only the revoked one", name, len(live))
		}
	}
	// Superseded, idle and waiting sessions can still reach MCP, so they can still be issued one.
	for _, st := range []dbstate.SessionStatus{dbstate.SessionSuperseded, dbstate.SessionIdle, dbstate.SessionWaitingOnUser} {
		s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace, Status: st})
		freeName(t, env, s)
		if _, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP); err != nil {
			t.Errorf("%s: Issue = %v", st, err)
		}
	}
}

// A late Issue meets an archive that has not committed yet: the archive holds the session row, the
// Issue waits for it and then sees the end instead of leaving a live credential behind.
func TestIssueWaitsForAnArchiveInFlightAndRefuses(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	freeName(t, env, s)

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := dbq.New(tx)
	if _, err := q.ArchiveSession(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := agentauth.RevokeInTx(ctx, q, s.SessionID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("Issue returned %v before the archive committed", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, agentauth.ErrBindingEnded) {
		t.Fatalf("Issue err = %v, want ErrBindingEnded", err)
	}
}

// An Issue that planned first is seen by the archive that follows: the archive's revoke takes the
// planned row, and the Issue's publish finds it revoked and removes its own Secret.
func TestArchiveAfterPlanRevokesThePlannedCredential(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	freeName(t, env, s)

	gate := &gatedStore{SecretStore: env.Creds.Secrets, created: make(chan struct{}), release: make(chan struct{})}
	gated := *env.Creds
	gated.Secrets = gate
	done := make(chan error, 1)
	go func() { _, err := gated.Issue(ctx, s.SessionID, dbstate.CredentialMCP); done <- err }()
	<-gate.created // planned, Secret created, publish not recorded

	if err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		if _, err := tq.ArchiveSession(ctx, s.SessionID); err != nil {
			return err
		}
		n, err := agentauth.RevokeInTx(ctx, tq, s.SessionID)
		if n != 1 {
			t.Errorf("RevokeInTx revoked %d rows, want the planned one", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Cleanup may even have marked it deleted before the publish lands.
	rows, _ := q.ListCredentialsByBinding(ctx, s.SessionID)
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state='deleted' WHERE id=$1`, rows[len(rows)-1].ID); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; !errors.Is(err, agentauth.ErrRevoked) {
		t.Fatalf("Issue err = %v, want ErrRevoked", err)
	}
	if env.Cluster.UID(authtest.Namespace, "mainloop-mcp-"+s.SessionID) != "" {
		t.Error("the Secret of the revoked credential leaked")
	}
}

func TestRevokeStraysTakesTheCredentialsOfEndedSessions(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	agent := func(a authtest.Agent) authtest.Seeded {
		a.Role, a.Grant = dbstate.RoleAgent, dbstate.GrantWorkspace
		return env.Seed(t, a)
	}
	ended := map[string]authtest.Seeded{
		"archived":  agent(authtest.Agent{Archived: true}),
		"completed": agent(authtest.Agent{Status: dbstate.SessionCompleted}),
		"failed":    agent(authtest.Agent{Status: dbstate.SessionFailed}),
		"cancelled": agent(authtest.Agent{Status: dbstate.SessionCancelled}),
	}
	live := map[string]authtest.Seeded{
		"active":     agent(authtest.Agent{}),
		"superseded": agent(authtest.Agent{Status: dbstate.SessionSuperseded}),
		"idle":       agent(authtest.Agent{Status: dbstate.SessionIdle}),
		"waiting":    agent(authtest.Agent{Status: dbstate.SessionWaitingOnUser}),
	}
	if err := env.Creds.RevokeStrays(ctx); err != nil { // no age needed: the end is certain
		t.Fatal(err)
	}
	for name, s := range ended {
		if got, _ := q.GetCredential(ctx, s.CredentialID); got.State != dbstate.CredentialRevoked {
			t.Errorf("%s: credential is %s, want revoked", name, got.State)
		}
	}
	for name, s := range live {
		if got, _ := q.GetCredential(ctx, s.CredentialID); got.State != dbstate.CredentialPublished {
			t.Errorf("%s: credential is %s, want published", name, got.State)
		}
	}
}

// A revoked credential with no recorded UID and no Secret may belong to an Issue whose create is
// still in flight, so cleanup keeps looking until the create's deadline has passed.
func TestCleanupKeepsLookingForASecretWithinTheCreateDeadline(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	name := "mainloop-mcp-" + s.SessionID
	// The row is old but its create has only just started: the grace counts from the start.
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state='planned', secret_uid=NULL, created_at = now() - interval '1 hour', create_started_at = now() WHERE id=$1`, s.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err := env.Cluster.CoreV1().Secrets(authtest.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}

	rctx, cancel := context.WithCancel(ctx)
	r := reconcile.NewRunner(d.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	env.Creds.Register(r)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()
	defer func() { cancel(); <-done }()

	time.Sleep(700 * time.Millisecond)
	if got, _ := q.GetCredential(ctx, s.CredentialID); got.State != dbstate.CredentialRevoked {
		t.Fatalf("state = %s: cleanup gave up on a Secret that may still be created", got.State)
	}
	// Past the create deadline nothing can land any more.
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET create_started_at = now() - interval '1 hour', next_attempt_at = now() WHERE id=$1`, s.CredentialID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if got, _ := q.GetCredential(ctx, s.CredentialID); got.State == dbstate.CredentialDeleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not finish after the grace")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A revoked credential whose Secret create never started has nothing to wait for: cleanup frees the
// name at once instead of waiting out a deadline.
func TestCleanupDoesNotWaitForACreateThatNeverStarted(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state='planned', secret_uid=NULL, create_started_at=NULL WHERE id=$1`, s.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err := env.Cluster.CoreV1().Secrets(authtest.Namespace).Delete(ctx, "mainloop-mcp-"+s.SessionID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	r := reconcile.NewRunner(d.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	env.Creds.Register(r)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, _ := q.GetCredential(ctx, s.CredentialID); got.State == dbstate.CredentialDeleted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup waited for a create that never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Issue records that the create is starting before it calls the store.
func TestIssueRecordsTheCreateStart(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	freeName(t, env, s)
	if _, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ListCredentialsByBinding(ctx, s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started bool
	for _, r := range rows {
		if r.State == dbstate.CredentialPublished && r.CreateStartedAt != nil {
			started = true
		}
	}
	if !started {
		t.Fatalf("no published credential records its create start: %+v", rows)
	}
}

// A publish that misses while the row is neither revoked nor deleted must not delete the Secret:
// something else owns it.
func TestPublishMissOnALiveRowLeavesTheSecret(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	freeName(t, env, s)

	gate := &gatedStore{SecretStore: env.Creds.Secrets, created: make(chan struct{}), release: make(chan struct{})}
	gated := *env.Creds
	gated.Secrets = gate
	done := make(chan error, 1)
	go func() { _, err := gated.Issue(ctx, s.SessionID, dbstate.CredentialMCP); done <- err }()
	<-gate.created
	// Another publisher recorded a different Secret UID for the row.
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state='published', secret_uid='someone-else' WHERE binding_id=$1 AND state='planned'`, s.SessionID); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	err := <-done
	if err == nil || errors.Is(err, agentauth.ErrRevoked) {
		t.Fatalf("Issue err = %v, want a plain error", err)
	}
	if env.Cluster.UID(authtest.Namespace, "mainloop-mcp-"+s.SessionID) == "" {
		t.Error("the Secret was deleted although the row is live")
	}
}

// deadlineStore records the context deadline of the Secret create.
type deadlineStore struct {
	agentauth.SecretStore
	deadline time.Time
	has      bool
}

func (s *deadlineStore) Create(ctx context.Context, name, purpose, bearer string) (string, error) {
	s.deadline, s.has = ctx.Deadline()
	return s.SecretStore.Create(ctx, name, purpose, bearer)
}

func TestIssueBoundsTheSecretCreate(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	freeName(t, env, s)
	rec := &deadlineStore{SecretStore: env.Creds.Secrets}
	c := *env.Creds
	c.Secrets = rec
	if _, err := c.Issue(ctx, s.SessionID, dbstate.CredentialMCP); err != nil {
		t.Fatal(err)
	}
	if !rec.has || time.Until(rec.deadline) > time.Minute {
		t.Fatalf("create deadline = %v (set %v), want one under a minute", rec.deadline, rec.has)
	}
}
