package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
		return reconcile.RetryAfter(o.StaleRetry), nil // wait for kagent to confirm the ask
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
	}
	if present, matches := messageIn(task, d.w.OutboundMessageID, d.w.Decision); present {
		if matches {
			return d.finish(ctx, dbstate.HITLAccepted, "")
		}
		return d.finish(ctx, dbstate.HITLRejectedTransport, detailOtherAnswer)
	}
	if !d.stillAsks(task) {
		return d.finish(ctx, dbstate.HITLRejectedTransport, "the task no longer waits for this request")
	}
	// kagent refuses a send to a Session that is suspended or changing, so make it ready first. This
	// is not a send: while the workspace or Session is still waking the response stays recorded and
	// is tried again after StaleRetry.
	if err := d.ready(ctx); err != nil {
		if ctx.Err() != nil {
			return reconcile.Retry(), ctx.Err()
		}
		d.s.log().WarnContext(ctx, "hitl answer is waiting for its session to be ready", "action_id", d.c.ID, "error", err)
		// The card says why the answer is waiting. A failed write only loses that text.
		id, owner, attempts := d.fence()
		why := "waiting for the session to be ready: " + readyDetail(err)
		if _, werr := dbq.New(d.s.Pool).SetHITLResponseWaiting(ctx, dbq.SetHITLResponseWaitingParams{
			ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &why,
		}); werr != nil {
			d.s.log().WarnContext(ctx, "hitl waiting reason not recorded", "action_id", d.c.ID, "error", werr)
		}
		return reconcile.RetryAfter(o.StaleRetry), nil
	}
	// Nothing has left yet, so a stop here leaves the response recorded.
	if err := ctx.Err(); err != nil {
		return reconcile.Retry(), err
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
	case kagent.NotSent(err):
		return d.notSent(ctx, err, 0)
	}
	// The call may have reached kagent. Record that, and let settle look the message up.
	return d.markUncertain(ctx, errDetail(err))
}

// revertTimeout bounds the write that returns an unsent response to recorded.
const revertTimeout = 5 * time.Second

// ready makes the response's kagent Session ready for the send, by the rule deliveries use.
func (d *dispatch) ready(ctx context.Context) error {
	if d.s.Ready == nil {
		return nil
	}
	return d.s.Ready.ReadySession(ctx, d.w.SessionID, d.w.WorkspaceID, d.w.KagentSessionID)
}

// readyDetail words a readiness failure for the card. The sessions package's own errors name what
// is still missing (the workspace is waking, the session is being deleted); kagent's are described,
// never echoed, since their text is untrusted.
func readyDetail(err error) string {
	if d := errDetail(err); d != detailUnclassified {
		return d
	}
	var kagentErr *kagent.SessionError
	if errors.As(err, &kagentErr) {
		return detailUnclassified // kagent's code is not one we describe; its text stays out of the card
	}
	return clip(err.Error(), 200)
}

// notSent returns a response that provably did not leave to recorded, to be tried again.
func (d *dispatch) notSent(ctx context.Context, cause error, after time.Duration) (reconcile.Outcome, error) {
	id, owner, attempts := d.fence()
	detail := errDetail(cause)
	// The revert must land even when ctx has ended: the row is still sending, and a row that stays so
	// is settled and held as if the answer may have left.
	wctx, stop := context.WithTimeout(context.WithoutCancel(ctx), revertTimeout)
	defer stop()
	if err := lost(dbq.New(d.s.Pool).MarkHITLResponseRecorded(wctx, dbq.MarkHITLResponseRecordedParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &detail,
	})); err != nil {
		return reconcile.Retry(), err
	}
	if after > 0 {
		return reconcile.RetryAfter(after), nil
	}
	return reconcile.Retry(), nil
}

// Details of a response that was not taken.
const (
	detailNewTurn     = "kagent took the answer as a new turn, not as the answer to this request"
	detailOtherAnswer = "kagent holds a different answer under this message id"
	detailNeverShown  = "kagent never showed the answer; the request can be answered again"
	detailMovedOn     = "the task moved on without the answer"
)

// confirmSent follows a send that returned cleanly: the task must show the message before the
// response counts as accepted.
func (d *dispatch) confirmSent(ctx context.Context) (reconcile.Outcome, error) {
	return d.resolve(ctx, false)
}

// settle handles a response whose send may have happened. It never sends: it looks the message id
// up. After UncertainHold without the message it fails the response, which is safe in both cases
// it can end in: the task still waits for the ask (kagent did not act on the answer) or has moved
// on without the message (the answer is not what moved it).
func (d *dispatch) settle(ctx context.Context) (reconcile.Outcome, error) {
	return d.resolve(ctx, time.Since(d.uncertainSince(ctx)) >= d.s.opts().UncertainHold)
}

type verdict int

const (
	// The original task shows the message, with the stored answer.
	shown verdict = iota
	// The original task shows the message id with a different answer.
	otherAnswer
	// The original task still waits for the ask, without the message.
	waiting
	// The message is in another task: kagent took it as a new turn.
	newTurn
	// The original task moved on (or is gone) and no task shows the message.
	movedOn
)

// resolve looks the message up and settles the response by what it finds. Only the original task
// can accept a response: it must show the outbound message id with the HITL metadata that was
// sent. A message that only another task shows was not the answer to this request.
func (d *dispatch) resolve(ctx context.Context, pastHold bool) (reconcile.Outcome, error) {
	v, err := d.judge(ctx)
	if err != nil {
		return d.markUncertain(ctx, errDetail(err)) // keeps the original uncertain-since time
	}
	switch v {
	case shown:
		return d.finish(ctx, dbstate.HITLAccepted, "")
	case otherAnswer:
		return d.finish(ctx, dbstate.HITLRejectedTransport, detailOtherAnswer)
	case newTurn:
		return d.finish(ctx, dbstate.HITLRejectedTransport, detailNewTurn)
	case waiting:
		if pastHold {
			return d.finish(ctx, dbstate.HITLRejectedTransport, detailNeverShown)
		}
		return d.markUncertain(ctx, "kagent does not show the answer yet")
	default:
		if pastHold {
			return d.finish(ctx, dbstate.HITLRejectedTransport, detailMovedOn)
		}
		return d.markUncertain(ctx, "kagent does not show the answer, and the task has moved on")
	}
}

// judge reads the original task, and the rest of the conversation only when that task has moved on
// without showing the message.
func (d *dispatch) judge(ctx context.Context) (verdict, error) {
	rctx, cancel := context.WithTimeout(ctx, d.s.opts().ReadTimeout)
	defer cancel()
	t, err := d.s.Kagent.GetTask(rctx, d.agent, d.w.TaskID)
	var gone *kagent.TaskNotFound
	switch {
	case err == nil:
		if present, matches := messageIn(t, d.w.OutboundMessageID, d.w.Decision); present {
			if matches {
				return shown, nil
			}
			return otherAnswer, nil
		}
		if d.stillAsks(t) {
			return waiting, nil
		}
	case !errors.As(err, &gone):
		return 0, err
	}
	_, found, err := d.s.Kagent.FindTaskForMessage(rctx, d.agent, d.w.ContextID, d.w.OutboundMessageID)
	switch {
	case err != nil:
		return 0, err
	case found:
		return newTurn, nil
	}
	return movedOn, nil
}

// markUncertain keeps the response uncertain and asks again after a delay that doubles with each
// look, up to UncertainCeiling.
func (d *dispatch) markUncertain(ctx context.Context, detail string) (reconcile.Outcome, error) {
	id, owner, attempts := d.fence()
	marked, err := dbq.New(d.s.Pool).MarkHITLResponseUncertain(ctx, dbq.MarkHITLResponseUncertainParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &detail,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Retry(), reconcile.ErrLeaseLost
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	o := d.s.opts()
	delay := backoff(o.UncertainRecheck, o.UncertainCeiling, int(marked.Rechecks))
	// Look again when the hold ends, not at the first doubled delay after it: that is what lets the
	// answer be tried again on time.
	if left := o.UncertainHold - time.Since(marked.UpdatedAt); left > 0 {
		delay = min(delay, left)
	}
	return reconcile.RetryAfter(delay), nil
}

func (d *dispatch) uncertainSince(ctx context.Context) time.Time {
	r, err := dbq.New(d.s.Pool).GetHITLResponse(ctx, dbq.GetHITLResponseParams{ID: d.c.ID, OwnerID: d.w.OwnerID})
	if err != nil {
		return time.Now()
	}
	return r.UpdatedAt
}

// messageIn reports whether the task's history shows a message with this id, and whether that
// message carries the HITL metadata that was sent.
func messageIn(t *a2a.Task, messageID string, sent []byte) (present, matches bool) {
	if t == nil {
		return false, false
	}
	i := slices.IndexFunc(t.History, func(m *a2a.Message) bool { return m != nil && m.ID == messageID })
	if i < 0 {
		return false, false
	}
	return true, sameJSON(t.History[i].Metadata[kagent.HITLExtension], sent)
}

// sameJSON reports whether got, a value kagent returned, is the JSON document sent.
func sameJSON(got any, sent []byte) bool {
	var want any
	if json.Unmarshal(sent, &want) != nil {
		return false
	}
	raw, err := json.Marshal(got)
	if err != nil {
		return false
	}
	var back any
	return json.Unmarshal(raw, &back) == nil && reflect.DeepEqual(back, want)
}
