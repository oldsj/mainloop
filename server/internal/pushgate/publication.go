package pushgate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

type intent struct {
	Update          Update          `json:"update"`
	CredentialID    string          `json:"credential_id"`
	Generation      int32           `json:"generation"`
	PolicyVersion   int64           `json:"policy_version"`
	NativeSessionID string          `json:"native_session_id"`
	Runtime         json.RawMessage `json:"runtime"`
	BodyDigest      string          `json:"body_digest"`
	BodyBytes       int64           `json:"body_bytes"`
}

func publicationID(c capability, requestID string) string {
	sum := sha256.Sum256([]byte(c.WorkspaceID + "\x00" + c.CredentialID + "\x00" + requestID))
	return hex.EncodeToString(sum[:])
}

func (s *Service) reserve(ctx context.Context, c capability, a authority, cmd commands, sp *spool, owner string) (dbq.GitPublication, error) {
	id := publicationID(c, owner)
	var row dbq.GitPublication
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.LockPushWriter(ctx, dbq.LockPushWriterParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch}); err != nil {
			return err
		}
		cur, err := load(ctx, q, c)
		if err != nil {
			return err
		}
		if !sameAuthority(a, cur) {
			return denial("authority_changed")
		}
		existing, err := q.GetPushPublication(ctx, id)
		if err == nil {
			row = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		confirmed, err := q.GetConfirmedPushBody(ctx, dbq.GetConfirmedPushBodyParams{WorkspaceID: c.WorkspaceID, CredentialID: c.CredentialID, BodyDigest: sp.Digest})
		if err == nil {
			row = confirmed
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		held, err := q.PushBranchHeld(ctx, dbq.PushBranchHeldParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch})
		if err != nil {
			return err
		}
		if held {
			return denial("publication_unresolved")
		}
		attempt, _ := json.Marshal(intent{Update: cmd.Update, CredentialID: c.CredentialID, Generation: a.W.Generation, PolicyVersion: a.PolicyVersion, NativeSessionID: a.W.NativeSessionID, Runtime: a.G.Runtime, BodyDigest: sp.Digest, BodyBytes: sp.Size})
		row, err = q.InsertPushPublication(ctx, dbq.InsertPushPublicationParams{ID: id, WorkspaceID: c.WorkspaceID, RequestID: owner, OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch, Attempt: attempt, TransportEvidence: json.RawMessage(`{"validated":false}`), LeaseOwner: &owner})
		return err
	})
	return row, err
}

func (s *Service) finish(ctx context.Context, id, owner string, state dbstate.GitPublicationState, kind string) error {
	evidence, _ := json.Marshal(map[string]string{"classification": kind})
	n, err := dbq.New(s.Pool).FinishPushPublication(ctx, dbq.FinishPushPublicationParams{ID: id, LeaseOwner: &owner, State: state, TransportReceipt: evidence})
	if err == nil && n != 1 {
		return reconcile.ErrLeaseLost
	}
	return err
}

func (s *Service) receive(ctx context.Context, c capability, a authority, cmd commands, sp *spool, owner string, l Limits) ([]byte, error) {
	if err := a.P.update(a.W.Branch, cmd.Update); err != nil {
		return nil, err
	}
	row, err := s.reserve(ctx, c, a, cmd, sp, owner)
	if err != nil {
		return nil, err
	}
	if row.State == dbstate.PublicationConfirmed {
		return report(cmd, true), nil
	}
	if row.State == dbstate.PublicationRejected {
		return report(cmd, false), nil
	}
	if textPointer(row.LeaseOwner) != owner || row.State != dbstate.PublicationPending {
		return nil, denial("publication_unresolved")
	}
	// Until dispatch is durably committed, failures are known not to have sent publication bytes.
	dispatched := false
	finished := false
	defer func() {
		if finished {
			return
		}
		outcome, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		state, kind := dbstate.PublicationRejected, "not_dispatched"
		if dispatched {
			state, kind = dbstate.PublicationUnknown, "unknown"
		}
		_ = s.finish(outcome, row.ID, owner, state, kind) // an outage leaves the durable unresolved fence
	}()
	repo, _ := github.ParseRepo(a.W.Repository)
	guard := func(guardCtx context.Context) error {
		cur, err := s.check(guardCtx, c, row.ID)
		if err != nil {
			return err
		}
		if !sameAuthority(a, cur) {
			return denial("authority_changed")
		}
		return nil
	}
	vctx, cancel := context.WithTimeout(ctx, l.Timeout)
	facts, err := sp.validate(vctx, cmd, repo, a.P, s.Upstream, l, guard)
	cancel()
	if err != nil {
		return nil, err
	}
	evidence, _ := json.Marshal(struct {
		validation
		Validated bool `json:"validated"`
	}{facts, true})
	n, err := dbq.New(s.Pool).RecordPushValidation(ctx, dbq.RecordPushValidationParams{ID: row.ID, LeaseOwner: &owner, TransportEvidence: evidence})
	if err != nil || n != 1 {
		return nil, denial("publication_unknown")
	}
	// Re-check the actual default and target ref after quarantine. The probe never reaches here.
	def, err := s.Upstream.DefaultBranch(ctx, repo)
	if err != nil || def != a.P.DefaultBranch {
		return nil, denial("metadata_unavailable")
	}
	if err = guard(ctx); err != nil {
		return nil, err
	}
	refs, err := s.Upstream.Refs(ctx, repo)
	if err != nil {
		return nil, err
	}
	old := refs[cmd.Ref]
	if old == "" {
		old = zeroOID
	}
	if old != cmd.Old {
		return nil, denial("remote_ref_changed")
	}
	if err = guard(ctx); err != nil {
		return nil, err
	}
	if err = sp.verify(); err != nil {
		return nil, err
	}
	// Treat even an uncertain COMMIT reply as dispatched. The resolver observes; it never replays.
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.LockPushWriter(ctx, dbq.LockPushWriterParams{OwnerID: a.W.UserID, Repository: a.W.Repository, Branch: a.W.Branch}); err != nil {
			return err
		}
		cur, err := load(ctx, q, c)
		if err != nil {
			return err
		}
		if !sameAuthority(a, cur) {
			return denial("authority_changed")
		}
		n, err := q.DispatchPushPublication(ctx, dbq.DispatchPushPublicationParams{ID: row.ID, LeaseOwner: &owner})
		if err != nil {
			return err
		}
		if n != 1 {
			return reconcile.ErrLeaseLost
		}
		dispatched = true
		return nil
	})
	if err != nil {
		return nil, denial("publication_unknown")
	}
	// A disconnected client cannot cancel outcome persistence. The upstream call and recording
	// have independent finite deadlines, and this handler waits for both before returning.
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), l.Timeout)
	defer dcancel()
	data, err := s.Upstream.Push(dctx, repo, sp.body)
	if err != nil {
		return nil, denial("publication_unknown")
	}
	accepted, err := receipt(data, cmd)
	if err != nil {
		return nil, denial("publication_unknown")
	}
	state, kind := dbstate.PublicationRejected, "receipt_rejected"
	if accepted {
		state, kind = dbstate.PublicationConfirmed, "receipt_confirmed"
	}
	outcome, ocancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer ocancel()
	if err = s.finish(outcome, row.ID, owner, state, kind); err != nil {
		return nil, denial("publication_unknown")
	}
	finished = true
	return report(cmd, accepted), nil
}

// ResolverLoop works abandoned pending intents and uncertain dispatches. It only observes refs;
// it cannot replay a push because no body is kept in the durable ledger.
func (s *Service) ResolverLoop() reconcile.Loop {
	return reconcile.Loop{Name: "git-publications", Table: reconcile.NewTable("git_publications", "pending", "dispatching", "unknown"), Concurrency: 1, Lease: 30 * time.Second, Interval: 5 * time.Second, Handle: s.resolve}
}

// Register runs uncertainty resolution in serve-api, the sole owner of background loops.
func (s *Service) Register(r *reconcile.Runner) { r.Add(s.ResolverLoop()) }
func (s *Service) resolve(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	row, err := dbq.New(s.Pool).GetPushPublication(ctx, c.ID)
	if err != nil {
		return reconcile.Retry(), err
	}
	var in intent
	if row.OwnerID == "" || json.Unmarshal(row.Attempt, &in) != nil || !oidPattern.MatchString(in.Update.Old) || !oidPattern.MatchString(in.Update.New) || in.Update.New == zeroOID || in.Update.Ref != "refs/heads/"+row.Branch || in.Generation < 1 {
		return reconcile.RetryAfter(time.Minute), nil
	}
	repo, err := github.ParseRepo(row.Repository)
	if err != nil || strings.ToLower(repo.FullName()) != row.Repository {
		return reconcile.RetryAfter(time.Minute), nil
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	refs, err := s.Upstream.Refs(rctx, repo)
	if err != nil {
		return reconcile.Retry(), denial("upstream_unavailable")
	}
	remote := refs[in.Update.Ref]
	if remote == "" {
		remote = zeroOID
	}
	state, kind := dbstate.PublicationUnknown, "remote_diverged"
	switch {
	case row.State == dbstate.PublicationPending:
		state, kind = dbstate.PublicationRejected, "not_dispatched"
	case remote == in.Update.New:
		state, kind = dbstate.PublicationConfirmed, "remote_ref_confirmed"
	case remote == in.Update.Old:
		state, kind = dbstate.PublicationRejected, "remote_ref_unchanged"
	default:
		return reconcile.RetryAfter(time.Minute), nil
	}
	receipt, _ := json.Marshal(map[string]string{"classification": kind, "remote_oid": remote})
	n, err := dbq.New(s.Pool).ResolvePushPublication(ctx, dbq.ResolvePushPublicationParams{ID: c.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), State: state, TransportReceipt: receipt}) //nolint:gosec // attempts is a PostgreSQL int4
	if err != nil {
		return reconcile.Retry(), err
	}
	if n != 1 {
		return reconcile.Retry(), reconcile.ErrLeaseLost
	}
	return reconcile.Recorded(), nil
}
