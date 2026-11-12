package sessions

// Steers: a message the owner sends while a turn runs joins that turn at its next tool boundary,
// as a message typed while Claude Code works does, instead of running as a turn of its own after it.
//
// The message is recorded first, with a delivery_steers row that names the turn it joins (X), in the
// transaction that decides it. The steer loop then sends it to kagent as an A2A message on X's task
// (kagent.steer metadata, the message id as the A2A messageId) and settles it:
//
//   - kagent took it: delivered. It ends when X ends, in X's final state, and X's reply is its reply.
//     If X ends other than completed and kagent lists the steer as unconsumed, it converts instead.
//   - X ended before it was sent, or kagent refused it as a steer (the turn is not running, waits for
//     input, or runs on a driver without steering): converted, once. It becomes an ordinary queued
//     delivery under the same id, in the order it was recorded, so it runs as the next turn. A steer
//     into a turn that was stopped is cancelled with it instead.
//   - The outcome is unknown: uncertain, settled by reading X's task (the message in its history)
//     and never by sending again.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

const (
	// SteerChannel wakes the steer loop.
	SteerChannel = "mainloop_steers"

	// steerMetadataKey marks an A2A message as a steer into the running task it names.
	steerMetadataKey = "kagent.steer"
	// steerUnconsumedKey lists, in an ended task's status metadata, the steers kagent took but never
	// gave to the model.
	steerUnconsumedKey = "kagent.steer.unconsumed"
	steerTurnStopped   = "not sent: the turn it was sent into was stopped"
)

// SteerTable is the steer ledger as an intent table.
func SteerTable() reconcile.Table {
	return reconcile.NewTable("delivery_steers",
		string(dbstate.SteerRecorded), string(dbstate.SteerSending),
		string(dbstate.SteerUncertain), string(dbstate.SteerDelivered))
}

// SteerLoop sends steers into their turns and settles them with those turns.
func (s *Service) SteerLoop() reconcile.Loop {
	o := s.opts()
	return reconcile.Loop{
		Name: "steers", Table: SteerTable(), Handle: s.handleSteer,
		Concurrency: o.Concurrency, Lease: o.Lease, Wake: []string{SteerChannel},
	}
}

// slashCommand reports a message that is a command to the native agent (/compact, /clear, …). A
// command never joins a running turn: Claude Code runs it after the turn, and so does Mainloop.
func slashCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}

// recordSteer writes the message and its steer into the session's running turn, when there is one
// with a task to join. It reports false, with nothing written, when there is none; the caller then
// records an ordinary message. The caller holds mainloop:send:<session>.
func (s *Service) recordSteer(ctx context.Context, q *dbq.Queries, source Source, sessionID, messageID, text string) (bool, error) {
	x, err := q.GetSteerableDelivery(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find the running turn: %w", err)
	}
	if n, err := q.InsertSessionMessage(ctx, dbq.InsertSessionMessageParams{
		ID: messageID, SessionID: sessionID, Role: string(userRole), Content: text,
	}); err != nil {
		return false, fmt.Errorf("record message: %w", err)
	} else if n == 0 {
		return false, fmt.Errorf("%w: the message id is already used", ErrInvalid)
	}
	if _, err := q.InsertSteer(ctx, dbq.InsertSteerParams{
		ID: messageID, SessionID: sessionID, DeliveryID: x.ID, Source: string(source),
	}); err != nil {
		return false, fmt.Errorf("record steer: %w", err)
	}
	return true, q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: SteerChannel, Payload: messageID})
}

// endUnsentSteers settles the steers a turn holds that were never sent, in the transaction that
// ends the turn and before its session's backlog is promoted: a stopped turn's are cancelled with
// it; any other turn's become ordinary queued deliveries, ahead of whatever was sent after them.
// The caller holds mainloop:send:<session>.
func endUnsentSteers(ctx context.Context, q *dbq.Queries, deliveryID string, final dbstate.DeliveryState) error {
	state, detail := dbstate.SteerConverted, "the turn ended before it was sent; it runs as the next turn"
	if final == dbstate.DeliveryCancelled {
		state, detail = dbstate.SteerCancelled, steerTurnStopped
	}
	rows, err := q.EndUnsentSteers(ctx, dbq.EndUnsentSteersParams{DeliveryID: deliveryID, State: state, Detail: &detail})
	if err != nil {
		return fmt.Errorf("end unsent steers: %w", err)
	}
	if state != dbstate.SteerConverted {
		return nil
	}
	for _, r := range rows {
		if err := convertSteer(ctx, q, r.ID, r.SessionID, r.Source, r.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

// convertSteer gives a steer that will not join its turn an ordinary delivery under its id: queued,
// at the time it was recorded. The backlog promotion admits it when nothing holds the turn.
func convertSteer(ctx context.Context, q *dbq.Queries, id, sessionID, source string, created time.Time) error {
	if _, err := q.InsertConvertedSteerDelivery(ctx, dbq.InsertConvertedSteerDeliveryParams{
		ID: id, SessionID: sessionID, Source: source, CreatedAt: created,
	}); err != nil {
		return fmt.Errorf("queue the converted steer: %w", err)
	}
	return nil
}

// steer is one claimed steer.
type steer struct {
	s  *Service
	c  reconcile.Claim
	st dbq.DeliverySteer
	// x is the turn it joins.
	x     dbq.GetDeliveryContextRow
	agent kagent.AgentRef
}

func (st *steer) fence() (id string, owner *string, attempts int32) {
	return st.c.ID, &st.c.Owner, int32(st.c.Attempts) //nolint:gosec // attempts is a small counter
}

func (s *Service) handleSteer(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	row, err := q.GetSteer(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load steer: %w", err)
	}
	x, err := q.GetDeliveryContext(ctx, row.DeliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil // the session is gone, and the steer with it
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load the steered turn: %w", err)
	}
	st := &steer{s: s, c: c, st: row, x: x}
	if x.BindingSessionID != nil {
		st.agent, err = s.agentFor(x)
	}
	if x.BindingSessionID == nil || err != nil {
		return st.finishOrConvert(ctx, dbstate.DeliveryFailed, nil)
	}
	var out reconcile.Outcome
	switch row.State {
	case dbstate.SteerRecorded:
		out, err = st.send(ctx)
	case dbstate.SteerDelivered:
		out, err = st.follow(ctx)
	default: // sending (its sender stopped) or uncertain
		out, err = st.observe(ctx)
	}
	if errors.Is(err, reconcile.ErrLeaseLost) {
		return reconcile.Recorded(), nil
	}
	return out, err
}

// turnEnded reports whether X is over, and how.
func (st *steer) turnEnded() (dbstate.DeliveryState, bool) {
	switch st.x.State {
	case dbstate.DeliveryCompleted, dbstate.DeliveryFailed, dbstate.DeliveryCancelled:
		return st.x.State, true
	}
	return "", false
}

// send sends a recorded steer into X, after deciding under the session's send lock that X is still
// running. A turn that ends takes the same lock, so either the steer is marked sending first and
// kagent decides, or the turn's ending converts it and this finds it no longer recorded.
func (st *steer) send(ctx context.Context) (reconcile.Outcome, error) {
	ksid := ""
	if st.x.SentKagentSessionID != nil {
		ksid = *st.x.SentKagentSessionID
	}
	if st.x.TaskID == nil || ksid == "" {
		if final, ended := st.turnEnded(); ended {
			return st.finishOrConvert(ctx, final, nil)
		}
		return reconcile.RetryAfter(st.s.opts().FollowPoll), nil
	}
	decided := false
	err := db.InTx(ctx, st.s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:send:"+st.x.SessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		x, err := q.GetDelivery(ctx, st.x.ID)
		if err != nil {
			return fmt.Errorf("read the steered turn: %w", err)
		}
		if x.State != dbstate.DeliveryDelivered && x.State != dbstate.DeliveryUncertain {
			return nil // ending, or ended: its ending converts the steer
		}
		id, owner, attempts := st.fence()
		n, err := q.MarkSteerSending(ctx, dbq.MarkSteerSendingParams{ID: id, LeaseOwner: owner, Attempts: attempts})
		decided = n == 1
		return err
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	if !decided {
		return reconcile.RetryAfter(st.s.opts().FollowPoll), nil
	}
	var sendErr error
	accepted := false
	for ev, err := range st.s.Kagent.SendMessage(ctx, st.agent, kagent.SendParams{
		Text: st.text(ctx), MessageID: st.c.ID, ContextID: ksid, TaskID: *st.x.TaskID,
		Metadata: map[string]any{steerMetadataKey: true},
	}) {
		if err != nil {
			sendErr = err
			break
		}
		if tid := string(ev.TaskInfo().TaskID); tid != "" {
			if tid != *st.x.TaskID {
				sendErr = fmt.Errorf("kagent answered the steer with task %s, not the running task %s", tid, *st.x.TaskID)
				break
			}
			accepted = true
			break // the turn's own delivery follows the task; the steer only needs the answer
		}
	}
	switch {
	case accepted:
		return st.setState(ctx, dbstate.SteerDelivered, "", st.s.opts().FollowPoll)
	case ctx.Err() != nil:
		return reconcile.Retry(), ctx.Err()
	case sendErr == nil:
		return st.setState(ctx, dbstate.SteerUncertain, "the stream ended without the task; it is looked up, not sent again", 0)
	case kagent.SteerRefusal(sendErr) != "":
		return st.convert(ctx, "kagent did not take it into the running turn ("+sendErr.Error()+"); it runs as the next turn")
	case kagent.NotSent(sendErr):
		id, owner, attempts := st.fence()
		d := safeDetail(sendErr.Error())
		if err := lost(dbq.New(st.s.Pool).MarkSteerUnsent(ctx, dbq.MarkSteerUnsentParams{
			ID: id, LeaseOwner: owner, Attempts: attempts, Detail: &d,
		})); err != nil {
			return reconcile.Retry(), err
		}
		return reconcile.RetryAfter(backoff(st.s.opts().FollowPoll, st.s.opts().UncertainRecheck, st.c.Attempts)), nil
	}
	var gone *kagent.TaskNotFound
	if errors.As(sendErr, &gone) {
		return st.convert(ctx, "the running turn's task is gone; it runs as the next turn")
	}
	return st.setState(ctx, dbstate.SteerUncertain, "the steer may have reached kagent: "+sendErr.Error(), 0)
}

// text is the message to send.
func (st *steer) text(ctx context.Context) string {
	rows, err := dbq.New(st.s.Pool).ListSessionMessagesByID(ctx, dbq.ListSessionMessagesByIDParams{
		SessionID: st.x.SessionID, Ids: []string{st.c.ID},
	})
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0].Content
}

// observe settles a steer whose send outcome is unknown by reading X's task, never by sending again.
func (st *steer) observe(ctx context.Context) (reconcile.Outcome, error) {
	if st.st.State == dbstate.SteerSending {
		if out, err := st.setState(ctx, dbstate.SteerUncertain,
			"the process that was sending stopped; it is looked up, not sent again", -1); err != nil {
			return out, err
		}
	}
	if st.x.TaskID == nil {
		if final, ended := st.turnEnded(); ended {
			return st.finishOrConvert(ctx, final, nil)
		}
		return reconcile.RetryAfter(st.s.opts().UncertainRecheck), nil
	}
	task, err := st.s.Kagent.GetTask(ctx, st.agent, *st.x.TaskID)
	var gone *kagent.TaskNotFound
	switch {
	case ctx.Err() != nil:
		return reconcile.Retry(), ctx.Err()
	case errors.As(err, &gone):
		// No task, so nothing can be running the steer.
		return st.convert(ctx, "the running turn's task is gone; it runs as the next turn")
	case err != nil:
		return reconcile.RetryAfter(st.s.opts().UncertainRecheck), nil
	}
	if inHistory(task, st.c.ID) {
		if isTerminal(task.Status.State) {
			return st.finishWithTask(ctx, task)
		}
		return st.setState(ctx, dbstate.SteerDelivered, "", st.s.opts().FollowPoll)
	}
	if isTerminal(task.Status.State) {
		return st.convert(ctx, "the turn ended without it; it runs as the next turn")
	}
	return reconcile.RetryAfter(st.s.opts().UncertainRecheck), nil
}

// follow waits for X to end, then ends the steer with it.
func (st *steer) follow(ctx context.Context) (reconcile.Outcome, error) {
	if _, ended := st.turnEnded(); !ended {
		return reconcile.RetryAfter(st.s.opts().UncertainRecheck), nil
	}
	if st.x.TaskID == nil {
		return st.finishOrConvert(ctx, st.x.State, nil)
	}
	task, err := st.s.Kagent.GetTask(ctx, st.agent, *st.x.TaskID)
	var gone *kagent.TaskNotFound
	switch {
	case ctx.Err() != nil:
		return reconcile.Retry(), ctx.Err()
	case errors.As(err, &gone):
		return st.finishOrConvert(ctx, st.x.State, nil)
	case err != nil:
		return reconcile.RetryAfter(st.s.opts().UncertainRecheck), nil
	}
	return st.finishWithTask(ctx, task)
}

// finishWithTask ends a steer kagent took, in X's final state, unless X's task lists it as never
// given to the model: then it converts.
func (st *steer) finishWithTask(ctx context.Context, task *a2a.Task) (reconcile.Outcome, error) {
	final := st.x.State
	if _, ended := st.turnEnded(); !ended {
		final = deliveryStateOf(task)
	}
	return st.finishOrConvert(ctx, final, task)
}

// deliveryStateOf is the delivery state a terminal task ends a turn in.
func deliveryStateOf(task *a2a.Task) dbstate.DeliveryState {
	switch normState(task.Status.State) {
	case "completed":
		return dbstate.DeliveryCompleted
	case "canceled":
		return dbstate.DeliveryCancelled
	}
	return dbstate.DeliveryFailed
}

// finishOrConvert ends the steer as X ended. A completed X consumed every steer it took. Otherwise a
// steer kagent lists as unconsumed, or one that never reached kagent, converts (or is cancelled with
// a stopped X); the rest end in X's state.
func (st *steer) finishOrConvert(ctx context.Context, final dbstate.DeliveryState, task *a2a.Task) (reconcile.Outcome, error) {
	reached := st.st.State == dbstate.SteerDelivered || (task != nil && inHistory(task, st.c.ID))
	if final != dbstate.DeliveryCompleted && (!reached || (task != nil && unconsumed(task, st.c.ID))) {
		if final == dbstate.DeliveryCancelled {
			return st.finish(ctx, dbstate.SteerCancelled, steerTurnStopped)
		}
		return st.convert(ctx, "the turn ended without it; it runs as the next turn")
	}
	switch final {
	case dbstate.DeliveryCompleted:
		return st.finish(ctx, dbstate.SteerCompleted, "")
	case dbstate.DeliveryCancelled:
		return st.finish(ctx, dbstate.SteerCancelled, "the turn was stopped")
	}
	return st.finish(ctx, dbstate.SteerFailed, "the turn it joined failed")
}

// convert makes the steer the session's next turn, once: the steer ends converted and an ordinary
// queued delivery takes its id, in one transaction under the session's send lock, which also admits
// it when nothing holds the turn.
func (st *steer) convert(ctx context.Context, detail string) (reconcile.Outcome, error) {
	err := db.InTx(ctx, st.s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:send:"+st.x.SessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		sess, err := q.GetSession(ctx, st.x.SessionID)
		if err != nil {
			return fmt.Errorf("read session: %w", err)
		}
		state, d := dbstate.SteerConverted, safeDetail(detail)
		if endedStatus(sess.Status) || sess.ArchivedAt != nil {
			state, d = dbstate.SteerCancelled, "not sent: the session has ended"
		}
		id, owner, attempts := st.fence()
		if err := lost(q.FinishSteer(ctx, dbq.FinishSteerParams{
			ID: id, LeaseOwner: owner, Attempts: attempts, State: state, Detail: &d,
		})); err != nil {
			return err
		}
		if state != dbstate.SteerConverted {
			return nil
		}
		if err := convertSteer(ctx, q, st.c.ID, st.x.SessionID, st.st.Source, st.st.CreatedAt); err != nil {
			return err
		}
		promoted, err := promoteNextQueued(ctx, q, st.x.SessionID, st.s.opts().UncertainHold.Seconds())
		if err != nil {
			return err
		}
		if err := q.RefreshSessionStatus(ctx, st.x.SessionID); err != nil {
			return fmt.Errorf("refresh session status: %w", err)
		}
		if promoted != "" {
			return q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: DeliveryChannel, Payload: promoted})
		}
		return nil
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	return reconcile.Recorded(), nil
}

func (st *steer) finish(ctx context.Context, state dbstate.SteerState, detail string) (reconcile.Outcome, error) {
	id, owner, attempts := st.fence()
	var d *string
	if detail != "" {
		s := safeDetail(detail)
		d = &s
	}
	if err := lost(dbq.New(st.s.Pool).FinishSteer(ctx, dbq.FinishSteerParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, State: state, Detail: d,
	})); err != nil {
		return reconcile.Retry(), err
	}
	return reconcile.Recorded(), nil
}

// setState moves the steer to an open state and looks again after delay; a negative delay only
// writes (the caller carries on).
func (st *steer) setState(ctx context.Context, state dbstate.SteerState, detail string, delay time.Duration) (reconcile.Outcome, error) {
	id, owner, attempts := st.fence()
	var d *string
	if detail != "" {
		s := safeDetail(detail)
		d = &s
	}
	if err := lost(dbq.New(st.s.Pool).SetSteerState(ctx, dbq.SetSteerStateParams{
		ID: id, LeaseOwner: owner, Attempts: attempts, State: state, Detail: d,
	})); err != nil {
		return reconcile.Retry(), err
	}
	st.st.State = state
	if delay == 0 {
		delay = st.s.opts().UncertainRecheck
	}
	return reconcile.RetryAfter(max(delay, 0)), nil
}

// inHistory reports whether the task's history holds the message.
func inHistory(task *a2a.Task, messageID string) bool {
	for _, m := range task.History {
		if m != nil && m.ID == messageID {
			return true
		}
	}
	return false
}

// unconsumed reports whether the ended task lists the message as a steer it took but never gave
// to the model. kagent lists them on the task; the final status message is read too.
func unconsumed(task *a2a.Task, messageID string) bool {
	listed := func(meta map[string]any) bool {
		list, _ := meta[steerUnconsumedKey].([]any)
		return slices.ContainsFunc(list, func(v any) bool { s, _ := v.(string); return s == messageID })
	}
	return listed(task.Metadata) || (task.Status.Message != nil && listed(task.Status.Message.Metadata))
}
