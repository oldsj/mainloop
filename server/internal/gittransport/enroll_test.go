package gittransport_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/gittransport"
)

func TestEnrollReplaysIdempotently(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace("OldSJ/Alpha.git")
	ctx := context.Background()
	first, err := e.enroller.Enroll(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if first.Repository != "oldsj/alpha" {
		t.Fatalf("repository = %q", first.Repository)
	}
	uid := e.auth.Cluster.UID(authtest.Namespace, first.SecretName)
	if uid == "" {
		t.Fatal("no Secret")
	}
	second, err := e.enroller.Enroll(ctx, ws)
	if err != nil || second != first {
		t.Fatalf("replay = %+v, %v; want %+v", second, err, first)
	}
	if e.auth.Cluster.UID(authtest.Namespace, first.SecretName) != uid {
		t.Fatal("a replay replaced the Secret")
	}

	// Concurrent replays still end with one enrollment, one credential and one Secret.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := e.enroller.Enroll(ctx, ws); err != nil || got.CredentialID != first.CredentialID {
				t.Errorf("concurrent enroll = %+v, %v", got, err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := e.auth.Pool.QueryRow(ctx, `SELECT count(*) FROM credentials WHERE binding_id = $1 AND kind = 'git_read'`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("credentials = %d, %v", n, err)
	}
}

func TestEnrollResumesAnInterruptedPublish(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	ctx := context.Background()
	q := dbq.New(e.auth.Pool)
	// The process died after the Secret and credential existed, before the enrollment was marked.
	if _, err := q.PlanGitEnrollment(ctx, dbq.PlanGitEnrollmentParams{WorkspaceID: ws, Repository: repoA}); err != nil {
		t.Fatal(err)
	}
	issued, err := e.auth.Creds.Issue(ctx, ws, dbstate.CredentialGitRead)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := q.GetGitEnrollment(ctx, ws)
	if row.State != dbstate.GitEnrollmentPlanned {
		t.Fatalf("state = %s", row.State)
	}
	// Authentication refuses it meanwhile: an unpublished enrollment is not a capability.
	auth := &gittransport.Authenticator{Q: q}
	if _, err := auth.Authenticate(ctx, "Bearer "+agentauth.Token(authtest.Key, issued.CredentialID)); !errors.Is(err, gittransport.ErrUnauthorized) {
		t.Fatalf("planned enrollment authenticated: %v", err)
	}
	en, err := e.enroller.Enroll(ctx, ws)
	if err != nil || en.CredentialID != issued.CredentialID {
		t.Fatalf("resume = %+v, %v", en, err)
	}
	got, err := auth.Authenticate(ctx, "Bearer "+agentauth.Token(authtest.Key, issued.CredentialID))
	if err != nil || got.WorkspaceID != ws || got.Repository != repoA {
		t.Fatalf("capability = %+v, %v", got, err)
	}
}

func TestEnrollAgainAfterRevocation(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	ctx := context.Background()
	first, err := e.enroller.Enroll(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	e.revoke(ws)
	// The old Secret still holds the name, so the new capability waits for the cleanup.
	if _, err := e.enroller.Enroll(ctx, ws); !errors.Is(err, agentauth.ErrCleanupPending) {
		t.Fatalf("enroll before cleanup = %v", err)
	}
	runCleanup(t, e, func() bool { return e.auth.Cluster.UID(authtest.Namespace, first.SecretName) == "" })
	second, err := e.enroller.Enroll(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if second.CredentialID == first.CredentialID {
		t.Fatal("a revoked credential came back")
	}
	row, _ := dbq.New(e.auth.Pool).GetGitEnrollment(ctx, ws)
	if row.State != dbstate.GitEnrollmentPublished || row.RevokedAt != nil || row.CredentialID == nil || *row.CredentialID != second.CredentialID {
		t.Fatalf("enrollment = %+v", row)
	}
}

func TestEnrollRefusals(t *testing.T) {
	e := newGitEnv(t)
	ctx := context.Background()
	if _, err := e.enroller.Enroll(ctx, "nope"); !errors.Is(err, gittransport.ErrNoWorkspace) {
		t.Fatalf("unknown workspace: %v", err)
	}
	other := e.workspace(repoA)
	if _, err := e.auth.Pool.Exec(ctx, `UPDATE workspaces SET repo_url = 'https://gitlab.com/o/r' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := e.enroller.Enroll(ctx, other); !errors.Is(err, gittransport.ErrNotGitHub) {
		t.Fatalf("non-GitHub repository: %v", err)
	}
	var n int
	if err := e.auth.Pool.QueryRow(ctx, `SELECT count(*) FROM git_enrollments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("enrollments = %d, %v", n, err)
	}
}

func TestEnrollOutsideAClusterWritesNoCredential(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	off := &gittransport.Enroller{Pool: e.auth.Pool, Creds: agentauth.NewCredentials(e.auth.Pool, agentauth.Unconfigured{}, authtest.Key, "mainloop", nil)}
	if _, err := off.Enroll(context.Background(), ws); !errors.Is(err, agentauth.ErrNotConfigured) {
		t.Fatalf("enroll = %v", err)
	}
	var n int
	if err := e.auth.Pool.QueryRow(context.Background(), `SELECT count(*) FROM credentials WHERE kind = 'git_read'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("credentials = %d, %v", n, err)
	}
}

func TestStrandedEnrollmentsAreRevoked(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	ctx := context.Background()
	if _, err := e.enroller.Enroll(ctx, ws); err != nil {
		t.Fatal(err)
	}
	// The credential service revokes the credential (the session ended); the enrollment row follows
	// once it has been quiet for a while.
	if _, err := e.auth.Creds.Revoke(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := e.enroller.RevokeStranded(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ := dbq.New(e.auth.Pool).GetGitEnrollment(ctx, ws); row.State != dbstate.GitEnrollmentPublished {
		t.Fatalf("a fresh enrollment was swept: %s", row.State)
	}
	if _, err := e.auth.Pool.Exec(ctx, `UPDATE git_enrollments SET updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := e.enroller.RevokeStranded(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ := dbq.New(e.auth.Pool).GetGitEnrollment(ctx, ws); row.State != dbstate.GitEnrollmentRevoked {
		t.Fatalf("state = %s", row.State)
	}
}

func TestReferenceNamesTheCapabilitySecret(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	// Built before anything is issued, as a replayed CreateSession does, and equal to the Secret
	// that Enroll then publishes.
	ref := gittransport.Reference(e.auth.Creds, "http://mainloop-git-read.mainloop.svc.cluster.local", ws)
	en, err := e.enroller.Enroll(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Header != "Authorization" || ref.Origin == "" || ref.SecretRef == nil || ref.SecretRef.Name != en.SecretName || ref.SecretRef.Key != "authorization" {
		t.Fatalf("reference = %+v, secret %s", ref, en.SecretName)
	}
}

// An enrollment whose publish never got a credential must still be revocable, or one such row
// fails the whole stranded sweep on every pass.
func TestStrandedPlannedEnrollmentWithoutCredentialIsRevoked(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	ctx := context.Background()
	if _, err := e.auth.Pool.Exec(ctx,
		`INSERT INTO git_enrollments (workspace_id, repository, state, updated_at) VALUES ($1, 'oldsj/mainloop', 'planned', now() - interval '1 hour')`, ws); err != nil {
		t.Fatal(err)
	}
	if err := e.enroller.RevokeStranded(ctx); err != nil {
		t.Fatalf("a credential-less planned enrollment failed the sweep: %v", err)
	}
	if row, err := dbq.New(e.auth.Pool).GetGitEnrollment(ctx, ws); err != nil || row.State != dbstate.GitEnrollmentRevoked {
		t.Fatalf("state = %s, %v", row.State, err)
	}
}
