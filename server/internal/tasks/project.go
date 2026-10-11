package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Pull request and CI states stored on a task.
const (
	prOpen    = string(api.TaskProjectionPrStateOpen)
	prClosed  = string(api.TaskProjectionPrStateClosed)
	prMerged  = string(api.TaskProjectionPrStateMerged)
	stUnknown = string(api.TaskProjectionPrStateUnknown)

	ciPending = string(api.TaskProjectionCiStatePending)
	ciSuccess = string(api.TaskProjectionCiStateSuccess)
	ciFailure = string(api.TaskProjectionCiStateFailure)
)

// Observation is what GitHub shows for a task's branch: its pull request, and the CI result with
// the commit the result belongs to.
type Observation struct {
	// PRNumber is zero when the branch has no pull request.
	PRNumber int
	PRURL    string
	// PRHeadRef and PRRepo are what the pull request points at; the projection ignores a pull
	// request that is not for the task's own branch in the task's own repository.
	PRHeadRef string
	PRRepo    string
	PRHeadSHA string
	// PRBody is nil for unavailable text; a pointer to an empty string is an observed empty body.
	PRBody *string
	// PRState is open, closed or merged.
	PRState string
	// CIState is pending, success or failure for CIHeadSHA; unknown when GitHub gave no complete
	// answer.
	CIState   string
	CIHeadSHA string
	// ObservedAt is the collection-finish time, preserved through persistence. A zero value
	// means no observation time is known and cannot establish fresh CI.
	ObservedAt time.Time
}

// Observer reads the pull request and CI of a branch from GitHub. It makes no changes.
type Observer interface {
	Observe(ctx context.Context, repo, branch string) (Observation, error)
}

// projectionBatch is how many tasks one sweep looks at.
const projectionBatch = 50

// normalize turns an observation into the values stored on the task. CI counts only when the result
// is for the exact commit the pull request is at, in any state; a pull request for another
// branch or repository is not the task's. An observation that cannot be trusted is unknown.
func normalize(o Observation, repo, branch string) (pr *int32, url, head *string, prState, ciState string, ciHead *string) {
	prState, ciState = stUnknown, stUnknown
	if o.PRNumber <= 0 || !strings.EqualFold(o.PRRepo, repo) || o.PRHeadRef != branch {
		return nil, nil, nil, prState, ciState, nil
	}
	n := int32(o.PRNumber) //nolint:gosec // a pull request number is small
	pr = &n
	url, head = nonEmpty(o.PRURL), nonEmpty(o.PRHeadSHA)
	switch o.PRState {
	case prOpen, prClosed, prMerged:
		prState = o.PRState
	}
	// A result for any other commit than the pull request's head says nothing about it, whatever
	// its state: a failure of an older head must not show against a new one.
	if o.CIHeadSHA != "" && o.CIHeadSHA == o.PRHeadSHA {
		switch o.CIState {
		case ciPending, ciSuccess, ciFailure:
			ciState, ciHead = o.CIState, nonEmpty(o.CIHeadSHA)
		}
	}
	return pr, url, head, prState, ciState, ciHead
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ProjectionSweep refreshes the stored GitHub observation of code tasks that are still open. It
// changes only the projection: it never moves a task's status, and a task with no change gets a
// new observation time and no new version or event. A failed observation clears cached facts.
// It needs an Observer.
func (s *Service) ProjectionSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-projection", Every: s.Limits.ProjectionEvery, Run: s.runProjection}
}

func (s *Service) runProjection(ctx context.Context) error {
	if s.Observer == nil {
		return nil
	}
	rows, err := dbq.New(s.Pool).ListProjectionCandidates(ctx, dbq.ListProjectionCandidatesParams{
		Limit: projectionBatch, StaleSeconds: s.Limits.ProjectionEvery.Seconds(),
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		if r.CheckoutBranch == nil {
			continue
		}
		if err := s.project(ctx, r.ID, *r.CheckoutBranch, r.ProjectFullName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// project observes one task and stores the result.
func (s *Service) project(ctx context.Context, id, branch, repo string) error {
	before, err := dbq.New(s.Pool).GetTaskByID(ctx, id)
	if err != nil {
		return err
	}
	obs, err := s.Observer.Observe(ctx, repo, branch)
	if err != nil {
		// Degrade to unknown rather than keep showing a state nobody can vouch for.
		s.log().WarnContext(ctx, "task projection", "task", id, "error", err)
		// Capture the failed read before DB work too; unknown facts still need a sweep time.
		obs = Observation{ObservedAt: time.Now()}
	}
	// As on main, unsupported body text makes the observation unavailable. NUL beyond
	// the excerpt is allowed: it is hashed but never stored in JSONB.
	if obs.PRBody != nil {
		body := *obs.PRBody
		if !utf8.ValidString(body) || strings.ContainsRune(body[:min(len(body), descriptionLimit)], 0) {
			obs = Observation{ObservedAt: obs.ObservedAt}
		}
	}
	pr, url, head, prState, ciState, ciHead := normalize(obs, repo, branch)
	var observedAt *time.Time
	if !obs.ObservedAt.IsZero() {
		observedAt = &obs.ObservedAt
	}
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		cur, err := q.GetTaskByIDForUpdate(ctx, id)
		if err != nil {
			return nil, err
		}
		if TaskFinal(cur.Status) || !eqPtr(before.CurrentAttemptID, cur.CurrentAttemptID) ||
			cur.CheckoutBranch == nil || *cur.CheckoutBranch != branch {
			return nil, nil
		}
		var repository *string
		if pr != nil {
			repository = nonEmpty(strings.ToLower(repo))
		}
		description := describe(obs, cur.CurrentAttemptID, repo, branch)
		raw, err := json.Marshal(description)
		if err != nil {
			return nil, err
		}
		if description == nil {
			raw = nil
		}
		if eqPtr(cur.PrNumber, pr) && eqPtr(cur.PrUrl, url) && eqPtr(cur.PrHeadSha, head) &&
			cur.PrState == prState && cur.CiState == ciState && eqPtr(cur.CiHeadSha, ciHead) &&
			eqPtr(cur.PrRepository, repository) && sameDescription(cur.PrDescription, description) {
			return nil, q.TouchTaskProjection(ctx, dbq.TouchTaskProjectionParams{
				ID: cur.ID, ObservedAt: observedAt, PrDescription: raw,
			})
		}
		after, err := q.SetTaskProjection(ctx, dbq.SetTaskProjectionParams{
			ID: cur.ID, PrNumber: pr, PrUrl: url, PrHeadSha: head, PrState: prState, CiState: ciState, CiHeadSha: ciHead,
			ObservedAt: observedAt, PrRepository: repository, PrDescription: raw,
		})
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, nil
	})
}
