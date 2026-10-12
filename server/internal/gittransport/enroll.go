// Package gittransport is the git smart-HTTP listener agents fetch through. A workspace is
// enrolled for one repository and given a read capability: a bearer the agent's git sends, kept in
// an immutable Secret in the agents' namespace. The listener proxies upload-pack (clone and fetch)
// for exactly that repository, using the GitHub App's installation token, which never reaches the
// agent. Pushes are not served here; the push gate owns them.
package gittransport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Errors an enrollment can end with. The first three mean "try again later" or "it is over"; none
// carries remote text.
var (
	// ErrNoWorkspace means no workspace has the id.
	ErrNoWorkspace = errors.New("gittransport: no such workspace")
	// ErrWorkspaceEnded means the workspace's session has ended, so it gets no capability.
	ErrWorkspaceEnded = errors.New("gittransport: the workspace has ended")
	// ErrNotGitHub means the workspace's repository is not a github.com repository the transport
	// can serve.
	ErrNotGitHub = errors.New("gittransport: the workspace's repository is not a GitHub repository")
	// ErrEnrollmentRevoked means the enrollment was revoked while it was being published.
	ErrEnrollmentRevoked = errors.New("gittransport: the enrollment was revoked while being published")
)

// strandedAfter is how long an enrollment may sit without a live credential before the sweep marks
// it revoked. It matches how long a credential may wait for its publish.
const strandedAfter = 10 * time.Minute

// Enrollment is a workspace's published read capability.
type Enrollment struct {
	WorkspaceID  string
	Repository   string // "owner/name", lower-case
	CredentialID string
	SecretName   string
}

// Enroller enrolls workspaces for the git read transport. The capability is revoked with the
// workspace: the credential service revokes it when the session ends, and the sweep below keeps
// the enrollment row in step.
type Enroller struct {
	Pool  *pgxpool.Pool
	Creds *agentauth.Credentials
	Log   *slog.Logger
}

// Enroll binds the workspace to its repository and publishes its read capability. It is
// idempotent. The intent row is written first; the Secret is created with no transaction open; the
// row is marked published last. A replay after any interruption resumes. ErrCleanupPending and
// ErrRevoked, from the credential service, mean "try again later".
func (e *Enroller) Enroll(ctx context.Context, workspaceID string) (Enrollment, error) {
	q := dbq.New(e.Pool)
	w, err := q.GetGitWorkspace(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, ErrNoWorkspace
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("gittransport: read workspace: %w", err)
	}
	if w.SessionArchivedAt != nil || ended(w.SessionStatus) {
		return Enrollment{}, ErrWorkspaceEnded
	}
	repo, err := github.ParseRepo(w.RepoURL)
	if err != nil {
		return Enrollment{}, ErrNotGitHub
	}
	if _, err := q.PlanGitEnrollment(ctx, dbq.PlanGitEnrollmentParams{
		WorkspaceID: workspaceID, Repository: strings.ToLower(repo.FullName()),
	}); err != nil {
		return Enrollment{}, fmt.Errorf("gittransport: plan enrollment: %w", err)
	}
	issued, err := e.Creds.Issue(ctx, workspaceID, dbstate.CredentialGitRead)
	if err != nil {
		if errors.Is(err, agentauth.ErrBindingEnded) {
			return Enrollment{}, ErrWorkspaceEnded
		}
		return Enrollment{}, err
	}
	row, err := q.PublishGitEnrollment(ctx, dbq.PublishGitEnrollmentParams{WorkspaceID: workspaceID, CredentialID: &issued.CredentialID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, ErrEnrollmentRevoked
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("gittransport: record enrollment: %w", err)
	}
	return Enrollment{
		WorkspaceID: workspaceID, Repository: row.Repository,
		CredentialID: issued.CredentialID, SecretName: issued.SecretName,
	}, nil
}

// Reference returns the kagent credential reference for a workspace's read capability: the agent's
// egress sends the capability from the Secret to origin, the git read Service. The bearer travels
// in the Authorization header, spelled lowercase because kagent's preparation check compares it
// exactly. The Secret's name is derived from the workspace id alone, so the
// reference can be built, with nothing issued, wherever a session's credentials are listed: on
// create and on a replayed create alike.
func Reference(creds *agentauth.Credentials, origin, workspaceID string) *kagentapi.SessionCredential {
	return &kagentapi.SessionCredential{
		Origin: origin, Header: "authorization",
		SecretRef: &kagentapi.SecretKeyReference{
			Name: creds.SecretName(dbstate.CredentialGitRead, workspaceID), Key: k8ssecrets.DataKey,
		},
	}
}

// RevokeStranded marks revoked the enrollments whose credential is gone: the credential service
// revokes a credential when its session ends, and this keeps the enrollment table in step.
// Authentication does not depend on it; it already requires a live credential and session.
func (e *Enroller) RevokeStranded(ctx context.Context) error {
	rows, err := dbq.New(e.Pool).RevokeStrandedGitEnrollments(ctx, strandedAfter.Seconds())
	if err != nil {
		return fmt.Errorf("gittransport: revoke stranded enrollments: %w", err)
	}
	if len(rows) > 0 && e.Log != nil {
		e.Log.InfoContext(ctx, "revoked stranded git enrollments", "count", len(rows))
	}
	return nil
}

// Register adds the stranded-enrollment sweep to the runner.
func (e *Enroller) Register(r *reconcile.Runner) {
	r.AddSweep(reconcile.Sweep{Name: "git-enrollment-strays", Every: time.Minute, Run: e.RevokeStranded})
}

func ended(s dbstate.SessionStatus) bool {
	switch s {
	case dbstate.SessionCompleted, dbstate.SessionFailed, dbstate.SessionCancelled:
		return true
	}
	return false
}
