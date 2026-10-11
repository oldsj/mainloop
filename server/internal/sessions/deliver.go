package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

const (
	userRole      = api.MessageRoleUser
	assistantRole = api.MessageRoleAssistant

	// createAttempts is how many times a delivery tries to create the kagent session before it is
	// failed as not sent. Creating is idempotent by request id, so each try is safe.
	createAttempts = 5
	createBackoff  = 2 * time.Second
	createBackoffM = time.Minute
)

// assistantNS derives the id of a mirrored reply, so mirroring one twice is a no-op.
var assistantNS = uuid.MustParse("6a2f6f0e-3a54-4c3c-9f0a-8b0d1b8f4e51")

func assistantID(sessionID string, taskID a2a.TaskID) string {
	return uuid.NewSHA1(assistantNS, []byte("assistant:"+sessionID+":"+string(taskID))).String()
}

// Table is the delivery ledger as an intent table.
func Table() reconcile.Table {
	return reconcile.NewTable("deliveries",
		string(dbstate.DeliveryRecorded), string(dbstate.DeliverySending),
		string(dbstate.DeliveryDelivered), string(dbstate.DeliveryUncertain))
}

// DeliveryLoop sends recorded messages, follows their tasks to the end and settles uncertain
// sends by lookup. A handler follows a whole turn, so the loop runs many at once.
func (s *Service) DeliveryLoop() reconcile.Loop {
	o := s.opts()
	return reconcile.Loop{
		Name: "deliveries", Table: Table(), Handle: s.handle,
		Concurrency: o.Concurrency, Lease: o.Lease, Wake: []string{DeliveryChannel},
	}
}

// EnsureSweep makes sure the main session exists, so the first GET /main-thread after a fresh
// install already shows it.
func (s *Service) EnsureSweep() reconcile.Sweep {
	return reconcile.Sweep{
		Name: "ensure-main-session", Every: s.opts().EnsureEvery,
		Run: func(ctx context.Context) error {
			_, err := s.EnsureMain(ctx)
			return err
		},
	}
}

// turn is one claimed delivery.
type turn struct {
	s   *Service
	c   reconcile.Claim
	d   dbq.GetDeliveryContextRow
	cur dbstate.DeliveryState
	// ksid is the kagent session, once known.
	ksid string
	// taskID is the task of this message, once known.
	taskID string
	// uncertainSince is when the delivery became uncertain.
	uncertainSince time.Time
}

func (s *Service) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	d, err := dbq.New(s.Pool).GetDeliveryContext(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil // the session was deleted; nothing is owed
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load delivery: %w", err)
	}
	t := &turn{s: s, c: c, d: d, cur: d.State, uncertainSince: d.UpdatedAt}
	if d.TaskID != nil {
		t.taskID = *d.TaskID
	}
	if d.KagentSessionID != nil {
		t.ksid = *d.KagentSessionID
	}
	var out reconcile.Outcome
	if c.State == string(dbstate.DeliveryRecorded) {
		out, err = t.send(ctx)
	} else {
		out, err = t.observe(ctx)
	}
	if errors.Is(err, reconcile.ErrLeaseLost) {
		s.log().WarnContext(ctx, "delivery lease lost", "message_id", c.ID, "error", err)
		return reconcile.Recorded(), nil
	}
	return out, err
}

func (t *turn) fence() (id string, owner *string, attempts int32) {
	return t.c.ID, &t.c.Owner, int32(t.c.Attempts) //nolint:gosec // attempts is a small counter
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

// ensureKagentSession returns the kagent Session of the binding, creating it on first use. The
// create is idempotent by request id, so repeating it after a crash is safe.
func (t *turn) ensureKagentSession(ctx context.Context) (string, error) {
	if t.ksid != "" {
		return t.ksid, nil
	}
	reqID := "mainloop-session-" + t.d.SessionID
	if t.d.KagentRequestID != nil && *t.d.KagentRequestID != "" {
		reqID = *t.d.KagentRequestID
	}
	sess, err := t.s.Kagent.CreateSession(ctx, &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: t.s.Agent.Namespace, Name: t.s.Agent.Name},
		RequestId: reqID,
		Name:      "Mainloop main thread",
	})
	if err != nil {
		return "", err
	}
	q := dbq.New(t.s.Pool)
	if _, err := q.SetBindingKagentSessionIfUnset(ctx, dbq.SetBindingKagentSessionIfUnsetParams{
		SessionID: t.d.SessionID, KagentSessionID: &sess.Id,
	}); err != nil {
		return "", fmt.Errorf("store kagent session: %w", err)
	}
	t.ksid = sess.Id
	return sess.Id, nil
}

// send delivers a recorded message for the first time.
func (t *turn) send(ctx context.Context) (reconcile.Outcome, error) {
	if t.d.Content == "" {
		return t.settle(ctx, dbstate.DeliveryFailed, "the message text is missing", nil, false)
	}
	ksid, err := t.ensureKagentSession(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return reconcile.Retry(), ctx.Err()
		}
		var cfg *kagent.ConfigError
		if errors.As(err, &cfg) || t.c.Attempts >= createAttempts {
			return t.settle(ctx, dbstate.DeliveryFailed,
				"not sent: could not create the kagent session: "+err.Error(), nil, false)
		}
		t.s.log().WarnContext(ctx, "create kagent session failed", "message_id", t.c.ID, "error", err)
		return reconcile.RetryAfter(min(createBackoff<<(t.c.Attempts-1), createBackoffM)), nil
	}

	// Commit "sending" before the call: if this process dies mid-call, the next claimer sees
	// sending and looks the message up instead of sending it again.
	id, owner, attempts := t.fence()
	if err := lost(dbq.New(t.s.Pool).MarkDeliverySending(ctx,
		dbq.MarkDeliverySendingParams{ID: id, LeaseOwner: owner, Attempts: attempts})); err != nil {
		return reconcile.Retry(), err
	}
	t.cur = dbstate.DeliverySending

	var sendErr error
	for ev, err := range t.s.Kagent.SendMessage(ctx, t.s.Agent, kagent.SendParams{
		Text: t.d.Content, MessageID: t.c.ID, ContextID: ksid,
	}) {
		if err != nil {
			sendErr = err
			break
		}
		if tid := string(ev.TaskInfo().TaskID); tid != "" && t.taskID == "" {
			if err := t.markDelivered(ctx, tid); err != nil {
				return reconcile.Retry(), err
			}
		}
	}

	switch {
	case ctx.Err() != nil:
		// The lease was lost or the process is stopping. The row stays sending (or delivered), and
		// whoever claims it next looks the message up.
		return reconcile.Retry(), ctx.Err()
	case t.taskID != "":
		return t.refresh(ctx)
	case sendErr == nil:
		return t.undecided(ctx, "the stream ended without a task; the message is looked up, not sent again")
	}
	var cfg *kagent.ConfigError
	var rejected *kagent.A2AError
	switch {
	case kagent.NotSent(sendErr):
		return t.settle(ctx, dbstate.DeliveryFailed, "not sent: "+sendErr.Error(), nil, false)
	case errors.As(sendErr, &cfg):
		return t.settle(ctx, dbstate.DeliveryFailed, "not sent: "+sendErr.Error(), nil, false)
	case errors.As(sendErr, &rejected):
		return t.settle(ctx, dbstate.DeliveryFailed, "send rejected: "+sendErr.Error(), nil, false)
	}
	return t.undecided(ctx, "the send may have reached kagent: "+sendErr.Error())
}

// errOpenTurn means another delivery of the session is open, so this one cannot become delivered.
var errOpenTurn = errors.New("sessions: another turn is open")

func (t *turn) markDelivered(ctx context.Context, taskID string) error {
	id, owner, attempts := t.fence()
	ref := "a2a:task/" + taskID
	n, err := dbq.New(t.s.Pool).MarkDeliveryDelivered(ctx, dbq.MarkDeliveryDeliveredParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, TaskID: &taskID, EvidenceRef: &ref,
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return errOpenTurn
	}
	if err := lost(n, err); err != nil {
		return err
	}
	t.cur, t.taskID = dbstate.DeliveryDelivered, taskID
	return nil
}

func (t *turn) markUncertain(ctx context.Context, detail string) error {
	id, owner, attempts := t.fence()
	d := safeDetail(detail)
	if err := lost(dbq.New(t.s.Pool).MarkDeliveryUncertain(ctx, dbq.MarkDeliveryUncertainParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &d,
	})); err != nil {
		return err
	}
	t.cur, t.uncertainSince = dbstate.DeliveryUncertain, time.Now()
	return nil
}

// undecided is a delivery whose fate kagent has not shown. It becomes uncertain and is looked up
// now and again, never sent again. Once the hold is over and no task shows the message, it fails,
// and the owner may send the message again.
func (t *turn) undecided(ctx context.Context, reason string) (reconcile.Outcome, error) {
	o := t.s.opts()
	if t.cur != dbstate.DeliveryUncertain {
		if err := t.markUncertain(ctx, reason); err != nil {
			return reconcile.Retry(), err
		}
		return t.observe(ctx)
	}
	if time.Since(t.uncertainSince) > o.UncertainHold {
		return t.settle(ctx, dbstate.DeliveryFailed,
			"kagent shows no task for this message after "+o.UncertainHold.String()+"; it was not sent again", nil, false)
	}
	return reconcile.RetryAfter(o.UncertainRecheck), nil
}

// observe settles a delivery that was sent, or may have been, by looking at kagent.
func (t *turn) observe(ctx context.Context) (reconcile.Outcome, error) {
	if t.cur == dbstate.DeliverySending {
		// The process that was sending stopped before it saw the outcome.
		if err := t.markUncertain(ctx, "the process that was sending stopped; the message is looked up, not sent again"); err != nil {
			return reconcile.Retry(), err
		}
	}
	if t.taskID != "" {
		return t.refresh(ctx)
	}
	ksid := t.ksid
	if ksid == "" {
		// Nothing can have been sent to a session that was never created.
		return t.settle(ctx, dbstate.DeliveryFailed, "not sent: the kagent session was never created", nil, false)
	}
	task, found, err := t.s.Kagent.FindTaskForMessage(ctx, t.s.Agent, ksid, t.c.ID)
	switch {
	case ctx.Err() != nil:
		return reconcile.Retry(), ctx.Err()
	case err != nil && !found:
		t.s.log().WarnContext(ctx, "look up message failed", "message_id", t.c.ID, "error", err)
		return reconcile.RetryAfter(t.s.opts().UncertainRecheck), nil
	case found && task == nil:
		// The message is in a task, but reading the task failed. It was sent: do not decide yet.
		t.s.log().WarnContext(ctx, "read task failed", "message_id", t.c.ID, "error", err)
		return reconcile.RetryAfter(t.s.opts().UncertainRecheck), nil
	case !found:
		return t.undecided(ctx, "no task shows this message; it is looked up, not sent again")
	}
	return t.adopt(ctx, task)
}

// refresh reads the delivery's task and carries on from there.
func (t *turn) refresh(ctx context.Context) (reconcile.Outcome, error) {
	task, err := t.s.Kagent.GetTask(ctx, t.s.Agent, t.taskID)
	var gone *kagent.TaskNotFound
	switch {
	case ctx.Err() != nil:
		return reconcile.Retry(), ctx.Err()
	case errors.As(err, &gone):
		if t.cur != dbstate.DeliveryUncertain {
			if err := t.markUncertain(ctx, "the task no longer exists in kagent; the message is not sent again"); err != nil {
				return reconcile.Retry(), err
			}
		}
		return t.undecided(ctx, "")
	case err != nil:
		t.s.log().WarnContext(ctx, "read task failed", "message_id", t.c.ID, "task_id", t.taskID, "error", err)
		return reconcile.RetryAfter(t.pollDelay()), nil
	}
	return t.adopt(ctx, task)
}

func (t *turn) pollDelay() time.Duration {
	if t.cur == dbstate.DeliveryUncertain {
		return t.s.opts().UncertainRecheck
	}
	return t.s.opts().FollowPoll
}

// adopt records that the message has a task and moves the turn along: finalize a finished task,
// wait on a parked one, follow a running one.
func (t *turn) adopt(ctx context.Context, task *a2a.Task) (reconcile.Outcome, error) {
	if t.cur != dbstate.DeliveryDelivered || t.taskID != string(task.ID) {
		if err := t.markDelivered(ctx, string(task.ID)); errors.Is(err, errOpenTurn) {
			t.s.log().WarnContext(ctx, "message has a task but another turn is open", "message_id", t.c.ID)
			return reconcile.RetryAfter(t.s.opts().UncertainRecheck), nil
		} else if err != nil {
			return reconcile.Retry(), err
		}
	}
	return t.advance(ctx, task, true)
}

func (t *turn) advance(ctx context.Context, task *a2a.Task, follow bool) (reconcile.Outcome, error) {
	switch st := normState(task.Status.State); {
	case st == "completed":
		return t.settle(ctx, dbstate.DeliveryCompleted, "", task, false)
	case st == "canceled":
		return t.settle(ctx, dbstate.DeliveryCancelled, "the turn was stopped", task, false)
	case st == "failed" || st == "rejected":
		return t.settle(ctx, dbstate.DeliveryFailed, "task "+st+": "+statusText(task), task, st == "failed")
	case st == "input_required" || st == "auth_required" || !follow:
		// Parked for someone else, or still running after we followed it: look again later. A
		// parked task keeps the session's turn open until it is answered.
		return reconcile.RetryAfter(t.s.opts().FollowPoll), nil
	}
	for ev, err := range t.s.Kagent.SubscribeToTask(ctx, t.s.Agent, string(task.ID)) {
		if err != nil || endsTask(ev) {
			break
		}
	}
	if ctx.Err() != nil {
		return reconcile.Retry(), ctx.Err()
	}
	fresh, err := t.s.Kagent.GetTask(ctx, t.s.Agent, string(task.ID))
	if err != nil {
		t.s.log().WarnContext(ctx, "read task failed", "message_id", t.c.ID, "task_id", t.taskID, "error", err)
		return reconcile.RetryAfter(t.pollDelay()), nil
	}
	return t.advance(ctx, fresh, false)
}

// settle ends the delivery in a final state. For a completed task it also mirrors the reply,
// counts the turn and publishes session:message, all in one transaction with the state change.
func (t *turn) settle(
	ctx context.Context, final dbstate.DeliveryState, detail string, task *a2a.Task, nativeFailure bool,
) (reconcile.Outcome, error) {
	var det, ref, tid *string
	if d := safeDetail(detail); d != "" {
		det = &d
	}
	if task != nil {
		s := string(task.ID)
		tid = &s
		r := "a2a:task/" + s
		if nativeFailure {
			r += "#failed"
		}
		ref = &r
	}
	err := events.InTxPublish(ctx, t.s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		id, owner, attempts := t.fence()
		if err := lost(q.FinishDelivery(ctx, dbq.FinishDeliveryParams{
			ID: id, LeaseOwner: owner, Attempts: attempts, State: final, Detail: det, TaskID: tid, EvidenceRef: ref,
		})); err != nil {
			return nil, err
		}
		var drafts []events.Draft
		if final == dbstate.DeliveryCompleted && task != nil {
			if reply := replyText(task); reply != "" {
				mid := assistantID(t.d.SessionID, task.ID)
				n, err := q.InsertSessionMessage(ctx, dbq.InsertSessionMessageParams{
					ID: mid, SessionID: t.d.SessionID, Role: string(assistantRole), Content: reply,
				})
				if err != nil {
					return nil, fmt.Errorf("mirror reply: %w", err)
				}
				if n > 0 {
					drafts = append(drafts, events.Draft{
						UserID: t.d.UserID, Type: events.TypeSessionMessage,
						Payload: api.EventSessionMessage{
							SessionId: t.d.SessionID, MessageId: mid, Role: api.EventSessionMessageRoleAssistant,
						},
					})
				}
			}
			if err := q.BumpBindingTurns(ctx, t.d.SessionID); err != nil {
				return nil, fmt.Errorf("count turn: %w", err)
			}
		}
		if err := q.RefreshSessionStatus(ctx, t.d.SessionID); err != nil {
			return nil, fmt.Errorf("refresh session status: %w", err)
		}
		return drafts, nil
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	t.cur = final
	return reconcile.Recorded(), nil
}

// normState turns TASK_STATE_INPUT_REQUIRED into input_required.
func normState(s a2a.TaskState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_")), "-", "_")
}

func isTerminal(s a2a.TaskState) bool {
	switch normState(s) {
	case "completed", "canceled", "failed", "rejected":
		return true
	}
	return false
}

// endsTask reports whether a stream event says the task is over.
func endsTask(ev a2a.Event) bool {
	switch e := ev.(type) {
	case *a2a.Task:
		return isTerminal(e.Status.State)
	case *a2a.TaskStatusUpdateEvent:
		return isTerminal(e.Status.State)
	}
	return false
}

func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p != nil {
			b.WriteString(p.Text())
		}
	}
	return strings.TrimSpace(b.String())
}

func statusText(task *a2a.Task) string { return messageText(task.Status.Message) }

// replyText is the agent's reply: the text of the artifacts in order, else the last agent message
// of the task's history.
func replyText(task *a2a.Task) string {
	var parts []string
	for _, a := range task.Artifacts {
		if a == nil {
			continue
		}
		var b strings.Builder
		for _, p := range a.Parts {
			if p != nil {
				b.WriteString(p.Text())
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	for i := len(task.History) - 1; i >= 0; i-- {
		if m := task.History[i]; m != nil && m.Role == a2a.MessageRoleAgent {
			if s := messageText(m); s != "" {
				return s
			}
		}
	}
	return statusText(task)
}
