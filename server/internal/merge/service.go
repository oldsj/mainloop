package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Channel wakes the merge loop when a candidate starts evaluating.
const Channel = "mainloop_merge_candidates"

// Errors a caller shows to the agent.
var (
	// ErrNotFound means the project or proposal is not the owner's.
	ErrNotFound = errors.New("merge: not found")
	// ErrConflict means the request id was used with other arguments.
	ErrConflict = errors.New("merge: request id reused with different arguments")
)

// Refusal is a request Mainloop refused. Code is stable.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

func refuse(code, msg string) error { return &Refusal{code, msg} }

// Service prepares and carries out merges.
type Service struct {
	Pool   *pgxpool.Pool
	GitHub GitHub
	Log    *slog.Logger
	// Configs is the operator's allowlist of protected merge tool mappings; consent counts only
	// for a tool named by one of them.
	Configs []config.MergeConfiguration

	// Settle is how long a tool call waits for the loop to settle a merge before it returns the
	// state it has (default 20s). A zero Settle returns at once.
	Settle time.Duration
	// Deadline is how long an evaluation waits for CI to turn green (default 30m).
	Deadline time.Duration
	// Poll is how often a waiting evaluation reads GitHub again (default 15s).
	Poll time.Duration
	// Window is how long a merge whose PUT may have run keeps looking before it concludes the
	// merge did not happen (default 10m). Recheck and Ceiling pace those reads (2s and 30s).
	Window, Recheck, Ceiling time.Duration
	Lease, SweepEvery        time.Duration
	Now                      func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func repoOf(fullName string) (github.Repo, bool) {
	owner, name, ok := strings.Cut(fullName, "/")
	return github.Repo{Owner: owner, Name: name}, ok && owner != "" && name != ""
}

// View is a proposal and where its merge stands, as a tool call reports it.
type View struct {
	ProposalID string
	Route      dbstate.MergeRoute
	State      dbstate.MergeState
	// Status is the word the agent acts on: approval_required for a prepared proposal that waits
	// for the owner, otherwise the candidate's state.
	Status     string
	Detail     string
	Repository string
	PRNumber   int64
	HeadSHA    string
	MergeSHA   string
	Deadline   *time.Time
	Summary    api.MergeSummary
	Digest     string
}

func viewOf(r dbq.GetMergeViewRow) (View, error) {
	v := View{
		ProposalID: r.ProposalID, Route: r.Route, State: r.State, Status: string(r.State),
		Repository: r.Repository, PRNumber: r.PrNumber, HeadSHA: r.HeadSha, Deadline: r.Deadline,
		Digest: r.SummaryDigest,
	}
	if r.State == dbstate.MergePrepared && r.Route == dbstate.RouteApproval {
		v.Status = "approval_required"
	}
	if r.Detail != nil {
		v.Detail = *r.Detail
	}
	if r.MergeSha != nil {
		v.MergeSHA = *r.MergeSha
	}
	if err := json.Unmarshal(r.Presentation, &v.Summary); err != nil {
		return View{}, fmt.Errorf("stored summary: %w", err)
	}
	return v, nil
}

func (s *Service) view(ctx context.Context, q *dbq.Queries, ownerID, proposalID string) (View, dbq.GetMergeViewRow, error) {
	row, err := q.GetMergeView(ctx, dbq.GetMergeViewParams{ID: proposalID, OwnerID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, row, ErrNotFound
	}
	if err != nil {
		return View{}, row, err
	}
	v, err := viewOf(row)
	return v, row, err
}

// PrepareInput is a prepare_pull_request_merge or merge_pull_request call.
type PrepareInput struct {
	OwnerID, ProjectID, SessionID string
	PRNumber                      int
	ExpectedSHA, RequestID        string
}

func (in PrepareInput) hash() string {
	return digest(map[string]any{"project_id": in.ProjectID, "pr_number": in.PRNumber, "expected_sha": in.ExpectedSHA})
}

// Prepare freezes the facts of a pull request into a proposal: it reads GitHub, decides the route
// (the project's policy and the protected paths), supersedes older proposals for the same pull
// request, and writes the proposal with no approval card. A replay of the request id returns the
// same proposal without reading GitHub again.
func (s *Service) Prepare(ctx context.Context, in PrepareInput) (View, error) {
	return s.prepare(ctx, dbstate.ToolPrepare, in)
}

func (s *Service) prepare(ctx context.Context, tool dbstate.MergeTool, in PrepareInput) (View, error) {
	q := dbq.New(s.Pool)
	hash := in.hash()
	replay := func() (View, bool, error) {
		k, err := q.GetMergeRequestKey(ctx, dbq.GetMergeRequestKeyParams{OwnerID: in.OwnerID, Tool: tool, RequestID: in.RequestID})
		if errors.Is(err, pgx.ErrNoRows) {
			return View{}, false, nil
		}
		if err != nil {
			return View{}, false, err
		}
		if k.PayloadHash != hash {
			return View{}, false, ErrConflict
		}
		v, _, err := s.view(ctx, q, in.OwnerID, k.ProposalID)
		return v, true, err
	}
	if v, ok, err := replay(); ok || err != nil {
		return v, err
	}

	project, err := q.GetProjectAuthority(ctx, dbq.GetProjectAuthorityParams{ID: in.ProjectID, UserID: in.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	repo, ok := repoOf(project.FullName)
	if !ok {
		return View{}, refuse("repository_mismatch", "the project has no usable repository name")
	}
	ev, err := gather(ctx, s.GitHub, repo, in.PRNumber, s.now())
	switch st, hasStatus := github.StatusOf(err); {
	case err == nil:
	case hasStatus && st == 404:
		return View{}, refuse("pull_request_not_found", "the pull request does not exist")
	case errors.Is(err, errStale):
		return View{}, refuse("evidence_changed", "the pull request, repository or default branch changed while it was read; prepare again")
	default:
		return View{}, refuse("github_unavailable", "GitHub could not be read: "+errStatus(err))
	}
	f := ev.Facts
	switch {
	case ev.PR.Mergeable != nil && (!*ev.PR.Mergeable || ev.PR.MergeableState == "dirty"):
		return View{}, refuse("merge_conflict", "the pull request conflicts with "+f.Base+"; merge or rebase it into "+f.Head+", push, then prepare again")
	case ev.PR.Mergeable == nil && len(f.Unavailable) == 0:
		return View{}, refuse("mergeability_pending", "GitHub has not worked out whether the pull request conflicts yet; try again shortly")
	}
	if !strings.EqualFold(f.Repository, project.FullName) {
		return View{}, refuse("repository_mismatch", "the repository is not the project's")
	}
	if f.HeadSHA != in.ExpectedSHA {
		return View{}, refuse("head_moved", "the pull request head is not expected_sha")
	}
	f.Policy, f.PolicyVer = string(project.MergePolicy), int(project.MergePolicyVersion)
	route := dbstate.RouteAuto
	if project.MergePolicy == dbstate.MergeApproval || len(f.Protected) > 0 || len(f.Unavailable) > 0 {
		route = dbstate.RouteApproval
	}
	f.Route = string(route)

	proposalID := uuid.NewString()
	presentation := summaryOf(proposalID, f)
	factsJSON, err := json.Marshal(f)
	if err != nil {
		return View{}, err
	}
	presJSON, err := json.Marshal(presentation)
	if err != nil {
		return View{}, err
	}
	candidateID := uuid.NewString()
	scope := fmt.Sprintf("merge:%s:%d:%d", in.OwnerID, f.RepositoryID, in.PRNumber)

	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		if err := tq.LockMergeScope(ctx, scope); err != nil {
			return err
		}
		// A racing call with the same request id may have won while GitHub was being read.
		if _, ok, err := replayIn(ctx, tq, in.OwnerID, tool, in.RequestID, hash); err != nil || ok {
			if err == nil {
				err = errReplayed
			}
			return err
		}
		held, err := tq.GetMergeInFlight(ctx, dbq.GetMergeInFlightParams{OwnerID: in.OwnerID, RepositoryID: f.RepositoryID, PrNumber: int64(in.PRNumber)})
		switch {
		case err == nil && held.State == dbstate.MergeMerged:
			return refuse("already_merged", "the pull request is already merged")
		case err == nil:
			return refuse("merge_in_progress", "a merge of this pull request is already under way")
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		_, err = tq.SupersedeOlderMergeCandidates(ctx, dbq.SupersedeOlderMergeCandidatesParams{
			OwnerID: in.OwnerID, RepositoryID: f.RepositoryID, PrNumber: int64(in.PRNumber), ID: candidateID,
		})
		if err != nil {
			return err
		}
		if err := tq.InsertMergeCandidate(ctx, dbq.InsertMergeCandidateParams{
			ID: candidateID, IdempotencyKey: fmt.Sprintf("merge:%s:%s:%s", in.OwnerID, tool, in.RequestID),
			OwnerID: in.OwnerID, ProjectID: in.ProjectID, RepositoryID: f.RepositoryID, Repository: f.Repository,
			PrNumber: int64(in.PRNumber), HeadSha: f.HeadSHA, Route: route,
		}); err != nil {
			return err
		}
		if err := tq.InsertMergeProposal(ctx, dbq.InsertMergeProposalParams{
			ID: proposalID, CandidateID: candidateID, OwnerID: in.OwnerID, SessionID: in.SessionID,
			Facts: factsJSON, Presentation: presJSON, SummaryDigest: digest(presentation),
		}); err != nil {
			return err
		}
		return tq.InsertMergeRequestKey(ctx, dbq.InsertMergeRequestKeyParams{
			OwnerID: in.OwnerID, Tool: tool, RequestID: in.RequestID, PayloadHash: hash, ProposalID: proposalID,
		})
	})
	if errors.Is(err, errReplayed) {
		v, _, rerr := replayView(ctx, s, q, in.OwnerID, tool, in.RequestID)
		return v, rerr
	}
	if err != nil {
		return View{}, err
	}
	v, _, err := s.view(ctx, q, in.OwnerID, proposalID)
	return v, err
}

var errReplayed = errors.New("replayed")

// replayIn reports whether the request id already names a proposal, inside a transaction.
func replayIn(ctx context.Context, q *dbq.Queries, ownerID string, tool dbstate.MergeTool, requestID, hash string) (string, bool, error) {
	k, err := q.GetMergeRequestKey(ctx, dbq.GetMergeRequestKeyParams{OwnerID: ownerID, Tool: tool, RequestID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if k.PayloadHash != hash {
		return "", false, ErrConflict
	}
	return k.ProposalID, true, nil
}

func replayView(ctx context.Context, s *Service, q *dbq.Queries, ownerID string, tool dbstate.MergeTool, requestID string) (View, bool, error) {
	k, err := q.GetMergeRequestKey(ctx, dbq.GetMergeRequestKeyParams{OwnerID: ownerID, Tool: tool, RequestID: requestID})
	if err != nil {
		return View{}, false, err
	}
	v, _, err := s.view(ctx, q, ownerID, k.ProposalID)
	return v, true, err
}

func errStatus(err error) string {
	if st, ok := github.StatusOf(err); ok {
		return fmt.Sprintf("status %d", st)
	}
	return "unreachable"
}

// Merge is merge_pull_request: it prepares the pull request and, when the route is automatic,
// starts the evaluation and waits briefly for the loop to settle it. An approval route returns the
// proposal and waits for the owner's consent to the approved-merge call.
func (s *Service) Merge(ctx context.Context, in PrepareInput) (View, error) {
	v, err := s.prepare(ctx, dbstate.ToolMerge, in)
	if err != nil {
		return View{}, err
	}
	if v.Route == dbstate.RouteApproval {
		return v, nil
	}
	if v.State == dbstate.MergePrepared {
		if len(v.Summary.UnavailableReasons) > 0 {
			return v, refuse("evidence_unavailable", strings.Join(v.Summary.UnavailableReasons, ","))
		}
		if err := s.begin(ctx, v.ProposalID, in.OwnerID, nil, nil); err != nil {
			return View{}, err
		}
	}
	return s.settle(ctx, in.OwnerID, v.ProposalID)
}

// begin moves a prepared candidate to evaluating (once) and wakes the loop.
func (s *Service) begin(ctx context.Context, proposalID, ownerID string, receipt, invocation *string) error {
	q := dbq.New(s.Pool)
	row, err := q.GetMergeView(ctx, dbq.GetMergeViewParams{ID: proposalID, OwnerID: ownerID})
	if err != nil {
		return err
	}
	deadline := s.now().Add(orDefault(s.Deadline, 30*time.Minute))
	_, err = q.BeginMergeEvaluation(ctx, dbq.BeginMergeEvaluationParams{
		ID: row.CandidateID, Deadline: &deadline, ReceiptActionID: receipt, InvocationRequestID: invocation,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if werr := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: Channel, Payload: row.CandidateID}); werr != nil {
		s.log().WarnContext(ctx, "could not wake the merge loop", "error", werr)
	}
	return nil
}

// ApprovedInput is a merge_pull_request_with_approval call.
type ApprovedInput struct {
	OwnerID, SessionID, ProposalID, RequestID string
}

// MergeApproved is merge_pull_request_with_approval: it evaluates the exact merge the owner
// approved, once. It needs the owner's recorded approval of this proposal and request id, given to
// the session that prepared it; without that, it refuses.
func (s *Service) MergeApproved(ctx context.Context, in ApprovedInput) (View, error) {
	q := dbq.New(s.Pool)
	v, row, err := s.view(ctx, q, in.OwnerID, in.ProposalID)
	if err != nil {
		return View{}, err
	}
	if row.SessionID != in.SessionID {
		return View{}, refuse("wrong_session", "the proposal was prepared by another session")
	}
	if v.Route != dbstate.RouteApproval {
		return View{}, refuse("approval_not_required", "this proposal merges without approval; use merge_pull_request")
	}
	hash := digest(map[string]any{"proposal_id": in.ProposalID})
	if _, _, err := replayIn(ctx, q, in.OwnerID, dbstate.ToolApprovedMerge, in.RequestID, hash); err != nil {
		return View{}, err
	}
	consent, ok, err := s.consentFor(ctx, in.OwnerID, row.SessionID, in.ProposalID, in.RequestID)
	if err != nil {
		return View{}, err
	}
	if !ok {
		return View{}, refuse("approval_required", "the owner has not approved this exact merge call")
	}
	if v.State == dbstate.MergePrepared && len(v.Summary.UnavailableReasons) > 0 {
		return View{}, refuse("evidence_unavailable", strings.Join(v.Summary.UnavailableReasons, ","))
	}
	// The key is the record that this call reached the owner's consent; it is written once.
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		if err := tq.LockMergeScope(ctx, "approved:"+in.OwnerID+":"+in.RequestID); err != nil {
			return err
		}
		if _, ok, err := replayIn(ctx, tq, in.OwnerID, dbstate.ToolApprovedMerge, in.RequestID, hash); err != nil || ok {
			return err
		}
		return tq.InsertMergeRequestKey(ctx, dbq.InsertMergeRequestKeyParams{
			OwnerID: in.OwnerID, Tool: dbstate.ToolApprovedMerge, RequestID: in.RequestID, PayloadHash: hash, ProposalID: in.ProposalID,
		})
	})
	if err != nil {
		return View{}, err
	}
	if v.State == dbstate.MergePrepared {
		if err := s.begin(ctx, in.ProposalID, in.OwnerID, &consent.ResponseID, &consent.InvocationRequestID); err != nil {
			return View{}, err
		}
	}
	return s.settle(ctx, in.OwnerID, in.ProposalID)
}

// Status is get_pull_request_merge_status: it reads the stored state of a proposal that this
// request id belongs to, and never approves or executes anything.
func (s *Service) Status(ctx context.Context, ownerID, proposalID, requestID string) (View, error) {
	q := dbq.New(s.Pool)
	for _, tool := range []dbstate.MergeTool{dbstate.ToolPrepare, dbstate.ToolMerge, dbstate.ToolApprovedMerge} {
		k, err := q.GetMergeRequestKey(ctx, dbq.GetMergeRequestKeyParams{OwnerID: ownerID, Tool: tool, RequestID: requestID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return View{}, err
		}
		if k.ProposalID != proposalID {
			continue
		}
		v, _, err := s.view(ctx, q, ownerID, proposalID)
		return v, err
	}
	return View{}, ErrNotFound
}

// settle waits for a candidate that is being worked on to reach a state that waits for nobody, up
// to Settle, and returns the view it has.
func (s *Service) settle(ctx context.Context, ownerID, proposalID string) (View, error) {
	q := dbq.New(s.Pool)
	deadline := s.now().Add(s.Settle)
	for {
		v, _, err := s.view(ctx, q, ownerID, proposalID)
		if err != nil {
			return View{}, err
		}
		working := v.State == dbstate.MergeEvaluating || v.State == dbstate.MergeMerging || v.State == dbstate.MergeUncertain
		if !working || !s.now().Before(deadline) {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Table is the reconcile table of merge candidates.
func Table() reconcile.Table {
	return reconcile.NewTable("merge_candidates",
		string(dbstate.MergeEvaluating), string(dbstate.MergeMerging), string(dbstate.MergeUncertain))
}
