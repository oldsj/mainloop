package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
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
	switch {
	case d.BindingSessionID == nil:
		// The binding is gone, so nothing can be sent or followed. Fail the row; leaving it would
		// have it claimed again every lease.
		out, err = t.settle(ctx, dbstate.DeliveryFailed, "the session has no agent binding", nil, false)
	case c.State == string(dbstate.DeliveryRecorded):
		out, err = t.send(ctx)
	default:
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

// ensureKagentSession returns the binding's kagent Session, ready for a turn. It is created on
// first use and resumed if suspended, and a Session that is still starting is waited for (bounded).
// A Session kagent no longer has (its idle TTL, or deleted out of band) is replaced once, under a
// new create request id. Every step is idempotent, so repeating it after a crash is safe, and none
// holds a database lock across a kagent call.
func (t *turn) ensureKagentSession(ctx context.Context) (string, error) {
	for replaced := false; ; replaced = true {
		var sess *kagentapi.Session
		var err error
		if t.ksid == "" {
			sess, err = t.createKagentSession(ctx)
		} else {
			sess, err = t.s.Kagent.GetSession(ctx, t.ksid)
		}
		var se *kagent.SessionError
		gone := errors.As(err, &se) && ((t.ksid != "" && se.Code == connect.CodeNotFound) ||
			(t.ksid == "" && createHitDeleted(se)))
		switch {
		case err != nil && !gone:
			return "", err
		case err == nil && sessionEnded(sess):
			gone = true
		}
		if !gone {
			return t.readyKagentSession(ctx, sess)
		}
		if replaced {
			return "", errors.New("the replacement kagent session is already gone")
		}
		if err := t.replaceKagentSession(ctx); err != nil {
			return "", err
		}
	}
}

// createHitDeleted reports a create that kagent refused because its request id belongs to a
// Session that has since been deleted.
func createHitDeleted(e *kagent.SessionError) bool {
	return e.Code == connect.CodeAlreadyExists && strings.Contains(strings.ToLower(e.Detail), "deleted")
}

func sessionEnded(s *kagentapi.Session) bool {
	st := s.GetState()
	return st == kagentapi.RuntimeState_RUNTIME_STATE_DELETING || st == kagentapi.RuntimeState_RUNTIME_STATE_DELETED
}

// createKagentSession creates the Session under the binding's request id and stores it. The create
// is idempotent by request id.
func (t *turn) createKagentSession(ctx context.Context) (*kagentapi.Session, error) {
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
		return nil, err
	}
	id, owner, attempts := t.fence()
	if err := lost(dbq.New(t.s.Pool).SetDeliveryKagentSession(ctx, dbq.SetDeliveryKagentSessionParams{
		ID: id, LeaseOwner: owner, Attempts: attempts,
		KagentSessionID: &sess.Id, RequestID: t.d.KagentRequestID,
	})); err != nil {
		return nil, fmt.Errorf("store kagent session: %w", err)
	}
	t.ksid = sess.Id
	return sess, nil
}

// replaceKagentSession forgets the Session kagent no longer has and picks a new create request id,
// in one write fenced on the delivery's claim and on the values it read.
func (t *turn) replaceKagentSession(ctx context.Context) error {
	id, owner, attempts := t.fence()
	var old *string
	if t.ksid != "" {
		old = &t.ksid
	}
	newReq := uuid.NewString()
	if err := lost(dbq.New(t.s.Pool).ReplaceDeliveryKagentSession(ctx, dbq.ReplaceDeliveryKagentSessionParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, NewRequestID: &newReq,
		OldSessionID: old, OldRequestID: t.d.KagentRequestID,
	})); err != nil {
		return fmt.Errorf("replace kagent session: %w", err)
	}
	t.s.log().WarnContext(ctx, "kagent session is gone; a new one will be created",
		"session_id", t.d.SessionID, "old_kagent_session", t.ksid)
	t.ksid, t.d.KagentRequestID = "", &newReq
	return nil
}

// readyKagentSession returns the Session's id once kagent reports it ready: it resumes a suspended
// Session once, and polls one that is starting, resuming or suspending until Opts.ReadyWait.
func (t *turn) readyKagentSession(ctx context.Context, sess *kagentapi.Session) (string, error) {
	wait := t.s.opts().ReadyWait
	poll := min(max(wait/20, 10*time.Millisecond), time.Second)
	deadline := time.Now().Add(wait)
	resumed := false
	for {
		switch st := sess.GetState(); {
		case st == kagentapi.RuntimeState_RUNTIME_STATE_READY && !changing(sess):
			return sess.GetId(), nil
		case st == kagentapi.RuntimeState_RUNTIME_STATE_FAILED:
			return "", errors.New("the kagent session has failed")
		case st == kagentapi.RuntimeState_RUNTIME_STATE_DELETING, st == kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
			return "", errors.New("the kagent session is being deleted")
		case st == kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED && !resumed:
			resumed = true
			var err error
			if sess, err = t.s.Kagent.ResumeSession(ctx, t.ksid); err != nil {
				return "", err
			}
			continue
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("the kagent session is not ready after %s (%s)", wait, sess.GetState())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
		var err error
		if sess, err = t.s.Kagent.GetSession(ctx, t.ksid); err != nil {
			return "", err
		}
	}
}

// changing reports a ready Session that is being suspended or deleted.
func changing(s *kagentapi.Session) bool {
	op := s.GetOperation()
	return op == kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND || op == kagentapi.RuntimeOperation_RUNTIME_OPERATION_DELETE
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
		if errors.Is(err, reconcile.ErrLeaseLost) {
			return reconcile.Retry(), err
		}
		var cfg *kagent.ConfigError
		if errors.As(err, &cfg) || t.c.Attempts >= createAttempts {
			return t.settle(ctx, dbstate.DeliveryFailed,
				"not sent: could not prepare the kagent session: "+err.Error(), nil, false)
		}
		t.s.log().WarnContext(ctx, "prepare kagent session failed", "message_id", t.c.ID, "error", err)
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
		return reconcile.RetryAfter(t.errorDelay()), nil
	}
	return t.adopt(ctx, task)
}

// backoff is base doubled per attempt after the first, up to ceil. Attempts count claims of the
// row, so a row that keeps coming back waits longer each time.
func backoff(base, ceil time.Duration, attempts int) time.Duration {
	d := base
	for i := 1; i < attempts && d < ceil; i++ {
		d *= 2
	}
	return min(d, max(ceil, base))
}

// errorDelay is the wait before reading a task again after a failed read: the uncertain recheck
// for an unresolved delivery, else a backoff from FollowPoll up to that same recheck delay.
func (t *turn) errorDelay() time.Duration {
	o := t.s.opts()
	if t.cur == dbstate.DeliveryUncertain {
		return o.UncertainRecheck
	}
	return backoff(o.FollowPoll, o.UncertainRecheck, t.c.Attempts)
}

// parkedDelay is the wait before looking again at a task parked for someone's answer. It can wait
// a long time, so it backs off from FollowPoll up to Opts.ParkedMax instead of reading every poll.
func (t *turn) parkedDelay() time.Duration {
	o := t.s.opts()
	return backoff(o.FollowPoll, o.ParkedMax, t.c.Attempts)
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
	case st == "input_required" || st == "auth_required":
		// Parked for someone else: it keeps the session's turn open until it is answered, which can
		// take long, so look again less and less often.
		return reconcile.RetryAfter(t.parkedDelay()), nil
	case !follow:
		// Still running after we followed it to the end of its stream: look again soon.
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
		return reconcile.RetryAfter(t.errorDelay()), nil
	}
	return t.advance(ctx, fresh, false)
}

// settle ends the delivery in a final state, in one transaction. A task's reply is mirrored: the
// whole reply for a completed task, and whatever the agent had produced for a failed or stopped one
// (the failure reason is the delivery's detail). Every settle publishes session:message last, so a
// client that is watching sees the turn end whichever way it ended; when no text was mirrored the
// event names the user message the turn answered.
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
		eventMessage := t.c.ID
		if task != nil {
			reply := artifactText(task)
			if final == dbstate.DeliveryCompleted {
				reply = replyText(task)
			}
			if reply != "" {
				mid := assistantID(t.d.SessionID, task.ID)
				if _, err := q.InsertSessionMessage(ctx, dbq.InsertSessionMessageParams{
					ID: mid, SessionID: t.d.SessionID, Role: string(assistantRole), Content: reply,
				}); err != nil {
					return nil, fmt.Errorf("mirror reply: %w", err)
				}
				eventMessage = mid
			}
		}
		if final == dbstate.DeliveryCompleted {
			if err := q.BumpBindingTurns(ctx, t.d.SessionID); err != nil {
				return nil, fmt.Errorf("count turn: %w", err)
			}
		}
		if err := q.RefreshSessionStatus(ctx, t.d.SessionID); err != nil {
			return nil, fmt.Errorf("refresh session status: %w", err)
		}
		return []events.Draft{{
			UserID: t.d.UserID, Type: events.TypeSessionMessage,
			Payload: api.EventSessionMessage{
				SessionId: t.d.SessionID, MessageId: eventMessage, Role: api.EventSessionMessageRoleAssistant,
			},
		}}, nil
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

// artifactText is the text the agent produced: the artifacts' text in order.
func artifactText(task *a2a.Task) string {
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
	return strings.Join(parts, "\n\n")
}

// replyText is a completed task's reply: its artifacts' text, else the last agent message of the
// task's history, else the status message.
func replyText(task *a2a.Task) string {
	if s := artifactText(task); s != "" {
		return s
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
