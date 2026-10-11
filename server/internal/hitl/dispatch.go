package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Table is the reconcile table of recorded responses. A row is worked on while it is recorded
// (not yet sent), sending (a claimer died mid-send) or uncertain.
func Table() reconcile.Table {
	return reconcile.NewTable("hitl_responses",
		string(dbstate.HITLRecorded), string(dbstate.HITLSending), string(dbstate.HITLUncertain))
}

// ResponseLoop sends recorded responses and settles uncertain ones.
func (s *Service) ResponseLoop() reconcile.Loop {
	o := s.opts()
	return reconcile.Loop{
		Name: "hitl-responses", Table: Table(), Handle: s.handle,
		Concurrency: o.Concurrency, Lease: o.Lease, Wake: []string{ResponseChannel},
	}
}

// dispatch is one claimed response.
type dispatch struct {
	s     *Service
	c     reconcile.Claim
	w     dbq.GetHITLResponseWorkRow
	agent kagent.AgentRef
}

func (s *Service) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	w, err := dbq.New(s.Pool).GetHITLResponseWork(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil // the request was deleted with its session; nothing is owed
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load response: %w", err)
	}
	d := &dispatch{s: s, c: c, w: w, agent: s.agentOf(w.AgentNamespace, w.AgentName)}
	var out reconcile.Outcome
	if c.State == string(dbstate.HITLRecorded) {
		out, err = d.send(ctx)
	} else {
		out, err = d.settle(ctx)
	}
	if errors.Is(err, reconcile.ErrLeaseLost) {
		s.log().WarnContext(ctx, "hitl response lease lost", "action_id", c.ID, "error", err)
		return reconcile.Recorded(), nil
	}
	return out, err
}

func (d *dispatch) fence() (id string, owner *string, attempts int32) {
	return d.c.ID, &d.c.Owner, int32(d.c.Attempts) //nolint:gosec // attempts is a small counter
}

func lost(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return reconcile.ErrLeaseLost
	}
	return nil
}

// finish moves the response to a final state and, when it was accepted, the request to answered.
func (d *dispatch) finish(ctx context.Context, state dbstate.HITLResponseState, detail string) (reconcile.Outcome, error) {
	id, owner, attempts := d.fence()
	var why *string
	if detail != "" {
		why = &detail
	}
	err := lost(dbq.New(d.s.Pool).FinishHITLResponse(ctx, dbq.FinishHITLResponseParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, State: state, Detail: why,
	}))
	if err != nil {
		return reconcile.Retry(), err
	}
	if state == dbstate.HITLAccepted {
		req, rerr := dbq.New(d.s.Pool).GetHITLRequest(ctx, dbq.GetHITLRequestParams{ID: d.w.RequestID, OwnerID: d.w.OwnerID})
		if rerr == nil {
			if err := d.s.setAvailability(ctx, req, dbstate.HITLAnswered, "", dbstate.HITLPending, dbstate.HITLStale); err != nil {
				d.s.log().WarnContext(ctx, "mark hitl request answered", "request_id", d.w.RequestID, "error", err)
			}
		}
	}
	return reconcile.Recorded(), nil
}

// matches reports whether the task still shows the ask this response answers.
func (d *dispatch) stillAsks(t *a2a.Task) bool {
	if t.Status.State != a2a.TaskStateInputRequired {
		return false
	}
	obs, err := readRequest(t.Status.Message)
	return err == nil && obs.statusMessageID == d.w.StatusMessageID && obs.hash == d.w.RequestHash
}

// send handles a response nobody has sent. It reads the task first; the send happens only if the
// task still waits for exactly this ask. The row is marked sending and committed before the call,
// so a crash in the call leaves a row that is never sent again.
func (d *dispatch) send(ctx context.Context) (reconcile.Outcome, error) {
	o := d.s.opts()
	switch d.w.Availability {
	case dbstate.HITLUnavailable, dbstate.HITLAnswered:
		return d.finish(ctx, dbstate.HITLRejectedTransport, "the request changed before the answer was sent")
	case dbstate.HITLStale:
		return reconcile.RetryAfter(o.UnsupportedRetry), nil // wait for kagent to confirm the ask
	}
	rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
	task, err := d.s.Kagent.GetTask(rctx, d.agent, d.w.TaskID)
	cancel()
	var gone *kagent.TaskNotFound
	switch {
	case errors.As(err, &gone):
		return d.finish(ctx, dbstate.HITLRejectedTransport, "kagent no longer has the task")
	case err != nil:
		return reconcile.Retry(), fmt.Errorf("check task before send: %w", err) // nothing was sent
	case hasMessage(task, d.w.OutboundMessageID):
		return d.finish(ctx, dbstate.HITLAccepted, "")
	case !d.stillAsks(task):
		return d.finish(ctx, dbstate.HITLRejectedTransport, "the task no longer waits for this request")
	}
	id, owner, attempts := d.fence()
	if err := lost(dbq.New(d.s.Pool).MarkHITLResponseSending(ctx, dbq.MarkHITLResponseSendingParams{
		ID: id, LeaseOwner: owner, Attempts: attempts,
	})); err != nil {
		return reconcile.Retry(), err
	}
	var native map[string]any
	if err := json.Unmarshal(d.w.Decision, &native); err != nil {
		return d.finish(ctx, dbstate.HITLRejectedTransport, "the stored answer cannot be read")
	}
	sctx, cancelSend := context.WithTimeout(ctx, o.SendTimeout)
	err = d.s.Kagent.SendHITL(sctx, d.agent, Send{
		MessageID: d.w.OutboundMessageID, ContextID: d.w.ContextID, TaskID: d.w.TaskID, Native: native,
	})
	cancelSend()
	switch {
	case err == nil:
		return d.confirmSent(ctx)
	case errors.Is(err, ErrSendUnsupported):
		return d.notSent(ctx, err, o.UnsupportedRetry)
	case kagent.NotSent(err):
		return d.notSent(ctx, err, 0)
	}
	// The call may have reached kagent. Record that, and let settle look the message up.
	return d.markUncertain(ctx, errDetail(err))
}

// notSent returns a response that provably did not leave to recorded, to be tried again.
func (d *dispatch) notSent(ctx context.Context, cause error, after time.Duration) (reconcile.Outcome, error) {
	id, owner, attempts := d.fence()
	detail := errDetail(cause)
	if err := lost(dbq.New(d.s.Pool).MarkHITLResponseRecorded(ctx, dbq.MarkHITLResponseRecordedParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &detail,
	})); err != nil {
		return reconcile.Retry(), err
	}
	if after > 0 {
		return reconcile.RetryAfter(after), nil
	}
	return reconcile.Retry(), nil
}

// confirmSent follows a send that returned cleanly: the message must show up in the task before
// the response counts as accepted.
func (d *dispatch) confirmSent(ctx context.Context) (reconcile.Outcome, error) {
	o := d.s.opts()
	rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
	defer cancel()
	task, found, err := d.s.Kagent.FindTaskForMessage(rctx, d.agent, d.w.ContextID, d.w.OutboundMessageID)
	if found && (err != nil || hasMessage(task, d.w.OutboundMessageID)) {
		return d.finish(ctx, dbstate.HITLAccepted, "")
	}
	if err != nil {
		return d.markUncertain(ctx, errDetail(err))
	}
	return d.markUncertain(ctx, "kagent does not show the answer yet")
}

func (d *dispatch) markUncertain(ctx context.Context, detail string) (reconcile.Outcome, error) {
	id, owner, attempts := d.fence()
	if err := lost(dbq.New(d.s.Pool).MarkHITLResponseUncertain(ctx, dbq.MarkHITLResponseUncertainParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &detail,
	})); err != nil {
		return reconcile.Retry(), err
	}
	return reconcile.RetryAfter(d.s.opts().UncertainRecheck), nil
}

// settle handles a response whose send may have happened. It never sends: it looks the message
// id up in the task. A response that no task shows after UncertainHold is failed, but only while
// the task still waits for the ask, which proves kagent did not act on the answer.
func (d *dispatch) settle(ctx context.Context) (reconcile.Outcome, error) {
	o := d.s.opts()
	rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
	task, found, err := d.s.Kagent.FindTaskForMessage(rctx, d.agent, d.w.ContextID, d.w.OutboundMessageID)
	cancel()
	if found && (err != nil || hasMessage(task, d.w.OutboundMessageID)) {
		return d.finish(ctx, dbstate.HITLAccepted, "")
	}
	if err != nil {
		return d.markUncertain(ctx, errDetail(err)) // keeps the original uncertain-since time
	}
	since := d.uncertainSince(ctx)
	if time.Since(since) < o.UncertainHold {
		return d.markUncertain(ctx, "kagent does not show the answer yet")
	}
	gctx, cancelGet := context.WithTimeout(ctx, o.ReadTimeout)
	t, gerr := d.s.Kagent.GetTask(gctx, d.agent, d.w.TaskID)
	cancelGet()
	var gone *kagent.TaskNotFound
	switch {
	case errors.As(gerr, &gone):
		return d.finish(ctx, dbstate.HITLRejectedTransport, "kagent no longer has the task")
	case gerr != nil:
		return d.markUncertain(ctx, errDetail(gerr))
	case hasMessage(t, d.w.OutboundMessageID):
		return d.finish(ctx, dbstate.HITLAccepted, "")
	case d.stillAsks(t):
		return d.finish(ctx, dbstate.HITLRejectedTransport, "kagent never showed the answer; the request can be answered again")
	}
	return d.markUncertain(ctx, "kagent does not show the answer, and the task has moved on")
}

func (d *dispatch) uncertainSince(ctx context.Context) time.Time {
	r, err := dbq.New(d.s.Pool).GetHITLResponse(ctx, dbq.GetHITLResponseParams{ID: d.c.ID, OwnerID: d.w.OwnerID})
	if err != nil {
		return time.Now()
	}
	return r.UpdatedAt
}
