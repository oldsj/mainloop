package workspaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/gitref"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Workspace preparation. kagent admits no message to a Session that has a workspace and a
// development environment until the Session's workspace is prepared and the receipt is confirmed.
// Mainloop owns that step: it is a durable intent per workspace (workspace_preparations), driven
// until kagent confirms it or definitely refuses it. The row records the action id and the setup
// profile before kagent is asked, so a replay after a lost reply sends the same request, and it is
// never asked again once the receipt is confirmed (a repeat call opens a new challenge and closes
// admission).

// Preparation states, as stored.
const (
	prepPlanned   = dbstate.PreparationPlanned
	prepRequested = dbstate.PreparationRequested
	prepConfirmed = dbstate.PreparationConfirmed
	prepFailed    = dbstate.PreparationFailed
)

// Setup profiles and the digests of the setup each one runs, as pinned by the kagent release the
// harnesses run. A profile is chosen from the role of the task attempt the workspace serves.
const (
	profileSupervisor = "supervisor"
	profileChild      = "child"
	profileAgent      = "agent"
)

// setupDigestsSource is the oldsj/kagent commit the digests below were read at
// (go/harness/runtime/workspace/preparation_test.go). A digest hashes kagent's standing-context
// text, and a mismatch makes the controller refuse every preparation. TestSetupDigestsMatchTheKagentPin
// fails when third_party/kagentapi/SOURCE moves off this commit: re-read the digests at the new
// commit, update both, and keep the harness image pins in step.
const setupDigestsSource = "ce402bf76151bf9492e5dda53f310547794c3190"

var setupDigests = map[string]string{
	profileSupervisor: "c9b2c5bf815f6b0dd45ee11f02e343ab6a25d4ce39c93663e6feb25281cb87f6",
	profileChild:      "7e8a64abb17aaa575ab317f07ad673defc495d97adb641283e5d1cc636b30258",
	profileAgent:      "a5fb1bb1e406ff7937b9d9e2e862dff43925d4df54d4bdd9fb010d7e2825ccb3",
}

const (
	// preparationWindow bounds how long a workspace may stay unprepared before it is failed.
	preparationWindow = 30 * time.Minute
	// preparationRecheck is how soon a waiting preparation looks at the Session again.
	preparationRecheck = 2 * time.Second
)

// ErrPreparationFailed means the workspace's preparation was refused or can no longer happen; the
// session must be replaced.
var ErrPreparationFailed = errors.New("workspace preparation failed")

func preparationActionID(requestID string) string { return "prep:" + requestID }

// PreparationTable is the intent table of workspace preparations.
func PreparationTable() reconcile.Table {
	return reconcile.NewTable("workspace_preparations", prepPlanned, prepRequested)
}

func (s *Service) preparationLoop() reconcile.Loop {
	return reconcile.Loop{
		Name: "workspace-preparation", Table: PreparationTable(), Handle: s.prepare,
		Interval: time.Second, Backoff: reconcile.Backoff{Base: time.Second, Max: 10 * time.Second},
	}
}

func (s *Service) failPreparation(ctx context.Context, id, detail string) (reconcile.Outcome, error) {
	s.Log.WarnContext(ctx, "workspace preparation failed", "workspace", id, "detail", detail)
	_, err := dbq.New(s.Pool).SettleWorkspacePreparation(ctx, dbq.SettleWorkspacePreparationParams{ID: id, State: prepFailed, Detail: detail})
	return reconcile.Recorded(), err
}

func (s *Service) confirmPreparation(ctx context.Context, id string) (reconcile.Outcome, error) {
	_, err := dbq.New(s.Pool).SettleWorkspacePreparation(ctx, dbq.SettleWorkspacePreparationParams{ID: id, State: prepConfirmed})
	return reconcile.Recorded(), err
}

// receiptVerdict reads kagent's receipt for our action. It returns the settled state, or "" when
// kagent is still working or cannot yet tell.
func receiptVerdict(r *ka.WorkspacePreparationReceipt, action string) (state, detail string) {
	if r == nil {
		return "", ""
	}
	if r.GetOriginal().GetActionId() != action {
		return prepFailed, "kagent holds a preparation for a different action"
	}
	// kagent keeps one preparation per Session, so a historical receipt of ours never becomes
	// current again, whatever its classification says.
	if r.GetHistorical() {
		return prepFailed, "kagent reports the preparation as historical, not current"
	}
	switch r.GetClassification() {
	case "confirmed":
		return prepConfirmed, ""
	case "definite-failure":
		return prepFailed, "kagent reports the preparation failed"
	}
	return "", ""
}

func (s *Service) prepare(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	prep, err := q.GetWorkspacePreparation(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	w, err := q.GetWorkspaceForOp(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && w.KagentDeletedAt != nil) {
		return s.failPreparation(ctx, c.ID, "the workspace was deleted")
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	// The window is only checked where this pass would wait: a receipt kagent confirmed late still
	// counts, so it is read first.
	late := func() bool { return s.now().Sub(prep.CreatedAt) > preparationWindow }
	timedOut := func() (reconcile.Outcome, error) {
		return s.failPreparation(ctx, c.ID, "kagent did not confirm the preparation within "+preparationWindow.String())
	}
	wait := func() (reconcile.Outcome, error) {
		if late() {
			return timedOut()
		}
		return reconcile.RetryAfter(preparationRecheck), nil
	}
	if w.KagentSessionID == nil {
		return wait()
	}
	n, err := s.Kagent.GetSession(ctx, *w.KagentSessionID)
	if notFoundErr(err) {
		return s.failPreparation(ctx, c.ID, "the kagent session no longer exists")
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	switch n.GetState() {
	case ka.RuntimeState_RUNTIME_STATE_FAILED, ka.RuntimeState_RUNTIME_STATE_DELETING, ka.RuntimeState_RUNTIME_STATE_DELETED:
		return s.failPreparation(ctx, c.ID, "the kagent session ended before it was prepared")
	}
	if n.GetState() != ka.RuntimeState_RUNTIME_STATE_READY || n.GetOperation() != ka.RuntimeOperation_RUNTIME_OPERATION_NONE {
		return wait()
	}
	if r := n.GetWorkspacePreparation(); r != nil {
		if st, detail := receiptVerdict(r, prep.ActionID); st == prepConfirmed {
			return s.confirmPreparation(ctx, c.ID)
		} else if st == prepFailed {
			return s.failPreparation(ctx, c.ID, detail)
		}
		// Pending or uncertain: kagent has the request; look again.
		if prep.State == prepRequested && r.GetOriginal().GetActionId() == prep.ActionID {
			return wait()
		}
	}
	// Nothing to wake the Actor for: kagent's preparation needs the exact commit.
	if !gitref.IsSHA(w.Ref) {
		return s.failPreparation(ctx, c.ID, "the checkout ref is not a commit SHA")
	}
	// kagent prepares an Actor that is running. An idle Session has none, so wake it: resume joins
	// an operation already under way and is safe to repeat.
	ra := n.GetRuntimeAssociation()
	if ra.GetGenerationId() == "" || ra.GetActorUid() == "" || !ra.GetCurrentActive() {
		if _, err := s.Kagent.ResumeSession(ctx, *w.KagentSessionID); err != nil && !notFoundErr(err) {
			if late() {
				return timedOut()
			}
			// The loop's backoff spaces the retries and logs the error.
			return reconcile.Retry(), fmt.Errorf("wake the session to prepare its workspace: %w", err)
		}
		return wait()
	}

	profile, digest := prep.SetupProfile, prep.SetupDigest
	if prep.State == prepPlanned {
		profile = profileAgent
		role, err := q.GetWorkspaceAttemptRole(ctx, &w.RequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return reconcile.Retry(), err
		}
		if err == nil {
			profile = profileChild
			if role == dbstate.AttemptSupervisor {
				profile = profileSupervisor
			}
		}
		digest = setupDigests[profile]
		// Frozen before kagent is asked, so a replay sends this same profile.
		if err := q.RequestWorkspacePreparation(ctx, dbq.RequestWorkspacePreparationParams{ID: c.ID, SetupProfile: profile, SetupDigest: digest}); err != nil {
			return reconcile.Retry(), err
		}
	}
	req := &ka.PrepareSessionWorkspaceRequest{
		SessionId: n.GetId(), ActionId: prep.ActionID, CreateRequestId: w.RequestID,
		GenerationId: ra.GetGenerationId(), ActorUid: ra.GetActorUid(),
		PreparedRevision:       n.GetPreparedRevision(),
		Workspace:              proto.Clone(n.GetWorkspace()).(*ka.Workspace),
		DevelopmentEnvironment: proto.Clone(n.GetDevelopmentEnvironment()).(*ka.DevelopmentEnvironment),
		RuntimeComposition:     proto.Clone(n.GetRuntimeComposition()).(*ka.RuntimeComposition),
		SetupProfile:           profile, SetupDigest: digest,
	}
	receipt, err := s.Kagent.PrepareSessionWorkspace(ctx, req)
	if err != nil {
		var se *kagent.SessionError
		if errors.As(err, &se) && se.Wire && (se.Code == connect.CodeInvalidArgument || se.Code == connect.CodePermissionDenied) {
			return s.failPreparation(ctx, c.ID, fmt.Sprintf("kagent refused the preparation: %s", se.Detail))
		}
		// kagent keeps one preparation per Session: other bytes for it are a conflict no retry fixes.
		if errors.As(err, &se) && se.Wire && se.Code == connect.CodeAlreadyExists {
			return s.failPreparation(ctx, c.ID, "kagent holds a different preparation for this session")
		}
		// The outcome is unknown or transient: the next pass reads the Session for a receipt before
		// anything is sent again, and the loop's backoff spaces the retries.
		return reconcile.Retry(), err
	}
	switch st, detail := receiptVerdict(receipt, prep.ActionID); st {
	case prepConfirmed:
		return s.confirmPreparation(ctx, c.ID)
	case prepFailed:
		return s.failPreparation(ctx, c.ID, detail)
	}
	return wait()
}
