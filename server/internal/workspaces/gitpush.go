package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// GitPush supplies metadata and enrollment after a creation-time reference is reserved. The
// reference carries no bearer; a standalone workspace cannot become a writer by being refreshed.
type GitPush struct {
	Upstream pushgate.Upstream
	Origin   string
}

func (s *Service) gitPushForCreate(ctx context.Context, w wsRow) (*ka.SessionCredential, error) {
	var raw json.RawMessage
	err := pgxTx(ctx, s, func(q *dbq.Queries) error {
		// Serialize the decision, including "none", with concurrent creates and deletion.
		if _, err := q.LockWorkspace(ctx, w.ID); err != nil {
			return err
		}
		cur, err := q.GetWorkspaceForOp(ctx, w.ID)
		if err != nil {
			return err
		}
		if cur.GitPushDecided {
			raw = cur.GitPushCredential
			return nil
		}
		var attempt string
		if cur.KagentSessionID == nil && s.GitPush != nil && s.GitPush.Upstream != nil && s.Credentials != nil &&
			s.GitRead.applies(wsRow(cur)) && len(cur.GitReadCredential) > 0 {
			_, off := s.Credentials.Secrets.(agentauth.Unconfigured)
			if !off {
				a, err := q.GetWorkspacePushAttempt(ctx, w.ID)
				if err == nil {
					// A live task attempt with no known default branch cannot be judged yet: freezing
					// "no push" now would leave the workspace read-only for good. Fail the create
					// retryably; the project-metadata sweep fills the default, and the create's retry
					// window bounds the wait and records this reason if GitHub never answers.
					if a.DefaultBranch == "" {
						return &unconfirmed{"waiting for the project's default branch from GitHub"}
					}
					allowed, policyErr := pushAllowed(ctx, q, cur.ProjectID, cur.Branch)
					if policyErr != nil {
						return policyErr
					}
					if allowed && cur.Branch != a.DefaultBranch {
						attempt = a.AttemptID
						raw, err = protojson.Marshal(pushgate.Reference(s.Credentials, s.GitPush.Origin, w.ID))
					}
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
		}
		if _, err := q.FreezeWorkspaceGitPush(ctx, dbq.FreezeWorkspaceGitPushParams{ID: w.ID, GitPushCredential: raw}); err != nil {
			return err
		}
		if attempt != "" {
			return q.InsertWorkspacePushReservation(ctx, dbq.InsertWorkspacePushReservationParams{
				ID: w.RequestID, IdempotencyKey: "workspace-push:" + attempt,
				WorkspaceID: w.ID, AttemptID: attempt, Reference: raw,
				ExpiresAt: s.now().Add(s.Config.Workspaces.CreateRetryWindow.Duration()),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return frozenReference(raw)
}

func pushAllowed(ctx context.Context, q *dbq.Queries, project, branch string) (bool, error) {
	p, err := q.GetPushPolicy(ctx, project)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} // the caller checked the project default
	if err != nil {
		return false, err
	}
	var policy pushgate.Policy
	return json.Unmarshal(p.Policy, &policy) == nil && policy.Allows(branch), nil
}

// PushReservationTable shares the grant's planned -> published/revoked state machine. A released
// reservation is terminal; its reference remains available solely for an identical native replay.
func PushReservationTable() reconcile.Table {
	return reconcile.NewTable("workspace_push_reservations", string(dbstate.GitGrantPlanned))
}

func (s *Service) pushEnrollmentLoop() reconcile.Loop {
	return reconcile.Loop{
		Name: "workspace-push-enrollment", Table: PushReservationTable(), Handle: s.enrollPush,
		Interval: time.Second, Backoff: reconcile.Backoff{Base: time.Second, Max: 10 * time.Second},
	}
}

func revokePush(ctx context.Context, q *dbq.Queries, id string) error {
	if err := q.RevokeWorkspacePushReservation(ctx, id); err != nil {
		return err
	}
	if err := q.RevokeWorkspacePushGrant(ctx, id); err != nil {
		return err
	}
	return q.RevokeWorkspacePushCredential(ctx, id)
}

func (s *Service) releasePush(ctx context.Context, id string) error {
	return pgxTx(ctx, s, func(q *dbq.Queries) error { return revokePush(ctx, q, id) })
}

func (s *Service) enrollPush(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	// The claim ID is the frozen native request ID, not a newly minted credential identity.
	w, err := q.GetWorkspaceByRequestID(ctx, dbq.GetWorkspaceByRequestIDParams{UserID: s.OwnerID, RequestID: c.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Done(string(dbstate.GitGrantRevoked)), nil
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	live, err := q.WorkspacePushReservationLive(ctx, w.ID)
	if err != nil {
		return reconcile.Retry(), err
	}
	if !live {
		return reconcile.Recorded(), s.releasePush(ctx, w.ID)
	}
	if s.GitPush == nil || s.Credentials == nil || w.KagentSessionID == nil {
		return reconcile.Retry(), nil
	}
	// GetSession is fresh on every retry. The existing Session must already carry the reserved
	// reference; neither refresh nor enrollment is allowed to retrofit it.
	n, err := s.Kagent.GetSession(ctx, *w.KagentSessionID)
	if err != nil {
		if notFoundErr(err) {
			return reconcile.Recorded(), s.releasePush(ctx, w.ID)
		}
		return reconcile.Retry(), err
	}
	if n.GetState() == ka.RuntimeState_RUNTIME_STATE_FAILED || n.GetState() == ka.RuntimeState_RUNTIME_STATE_DELETED || n.GetState() == ka.RuntimeState_RUNTIME_STATE_DELETING {
		return reconcile.Recorded(), s.releasePush(ctx, w.ID)
	}
	if n.GetState() != ka.RuntimeState_RUNTIME_STATE_READY || n.GetOperation() != ka.RuntimeOperation_RUNTIME_OPERATION_NONE {
		return reconcile.Retry(), nil
	}
	ref, err := frozenReference(w.GitPushCredential)
	if err != nil {
		return reconcile.Retry(), err
	}
	attached := 0
	for _, credential := range n.GetCredentials() {
		if ref != nil && proto.Equal(ref, credential) {
			attached++
		}
	}
	if attached != 1 {
		return reconcile.Recorded(), s.releasePush(ctx, w.ID)
	}
	gate := &pushgate.Service{Pool: s.Pool, Native: s.Kagent, Upstream: s.GitPush.Upstream, Credentials: s.Credentials}
	if _, err = gate.Enroll(ctx, w.ID); err != nil {
		return reconcile.Retry(), err
	}
	// A task may have ended while native/metadata/Secret I/O was in flight. Fence the outcome and
	// revoke only push authority if it did, leaving read and MCP cleanup to their own owners.
	err = pgxTx(ctx, s, func(q *dbq.Queries) error {
		live, err := q.WorkspacePushReservationLive(ctx, w.ID)
		if err != nil {
			return err
		}
		if !live {
			return revokePush(ctx, q, w.ID)
		}
		n, err := q.PublishWorkspacePushReservation(ctx, dbq.PublishWorkspacePushReservationParams{
			ID: c.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a bounded claim counter
		})
		if err == nil && n != 1 {
			return reconcile.ErrLeaseLost
		}
		return err
	})
	return reconcile.Recorded(), err
}

func (s *Service) releaseEndedPush(ctx context.Context) error {
	ids, err := dbq.New(s.Pool).ListUnsettledWorkspacePushReservations(ctx, 1000)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := pgxTx(ctx, s, func(q *dbq.Queries) error {
			live, err := q.WorkspacePushReservationLive(ctx, id)
			if err != nil || live {
				return err
			}
			return revokePush(ctx, q, id)
		}); err != nil {
			return err
		}
	}
	return nil
}
