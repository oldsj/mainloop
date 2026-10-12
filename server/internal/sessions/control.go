package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// UnconfirmedError is a stop that kagent did not confirm. Nothing was written, so it is safe to
// try again.
type UnconfirmedError struct{ Detail string }

func (e *UnconfirmedError) Error() string { return e.Detail }

// KagentError is a stop that failed in kagent. The delivery was left as it was.
type KagentError struct{ Err error }

func (e *KagentError) Error() string { return e.Err.Error() }
func (e *KagentError) Unwrap() error { return e.Err }

// StopOutcome is what a stop did.
type StopOutcome string

// Stop outcomes.
const (
	// Stopped means kagent confirmed the cancel and the delivery ended cancelled.
	Stopped StopOutcome = "stopped"
	// Finished means the turn ended on its own first and was recorded as it ended.
	Finished StopOutcome = "finished"
	// NoOpenTurn means there was nothing to stop.
	NoOpenTurn StopOutcome = "no_open_turn"
)

// Cancel outcomes for the agent.
const (
	AgentStopped    = "stopped"
	AgentNotRunning = "not_running"
	AgentUnknown    = "unknown"
)

// finishedStatus is a status after which nothing more will run, so the session can be cleared.
func finishedStatus(st dbstate.SessionStatus) bool {
	return st == dbstate.SessionCompleted || st == dbstate.SessionFailed || st == dbstate.SessionCancelled
}

// ownedSession is the owner's session with its binding, or ErrNotFound. A session without a
// native binding is a conflict.
func (s *Service) ownedBinding(ctx context.Context, sessionID string) (dbq.GetOwnedSessionBindingRow, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: sessionID, UserID: s.OwnerID}); errors.Is(err, pgx.ErrNoRows) {
		return dbq.GetOwnedSessionBindingRow{}, ErrNotFound
	} else if err != nil {
		return dbq.GetOwnedSessionBindingRow{}, fmt.Errorf("get session: %w", err)
	}
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return b, conflict("Session has no native agent binding")
	}
	if err != nil {
		return b, fmt.Errorf("get binding: %w", err)
	}
	return b, nil
}

// bindingAgentOf prefers the stored Agent, then the workspace, main configuration or provider.
func (s *Service) bindingAgentOf(b dbq.GetOwnedSessionBindingRow) (kagent.AgentRef, error) {
	return s.ResolveAgent(&b.ResolvedAgentNamespace, &b.ResolvedAgentName, &b.Kind, b.Role)
}

// kagentSessionOf is the kagent Session a delivery was sent to, else the binding's.
func kagentSessionOf(d dbq.Delivery, b dbq.GetOwnedSessionBindingRow) string {
	if d.KagentSessionID != nil && *d.KagentSessionID != "" {
		return *d.KagentSessionID
	}
	if b.KagentSessionID != nil {
		return *b.KagentSessionID
	}
	return ""
}

// ending is how a stop or a cancel ends a delivery from outside its claim.
type ending struct {
	final   dbstate.DeliveryState
	detail  string
	task    *a2a.Task
	partial string
	// nativeFailure marks a task that failed on its own.
	nativeFailure bool
	// note is whether the conversation gets what goes with the ending (the reply, the stop note and
	// the queue hold); a cancelled session needs none of it.
	note bool
}

// endDelivery ends the delivery in one transaction, from one of the given states. It returns the
// state the delivery was in, and false when it was no longer in one of them (the delivery loop got
// there first), in which case nothing is written.
func (s *Service) endDelivery(ctx context.Context, d dbq.Delivery, from []dbstate.DeliveryState, e ending) (dbstate.DeliveryState, bool, error) {
	det, ref, tid := finishFields(e.detail, e.task, e.nativeFailure)
	states := make([]string, 0, len(from))
	for _, f := range from {
		states = append(states, string(f))
	}
	var previous dbstate.DeliveryState
	moved := false
	var stored string
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		prev, err := q.EndDelivery(ctx, dbq.EndDeliveryParams{
			ID: d.ID, FromStates: states, State: e.final, Detail: det, TaskID: tid, EvidenceRef: ref,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("end delivery: %w", err)
		}
		previous, moved = dbstate.DeliveryState(prev), true
		if !e.note {
			return nil, nil
		}
		msg, err := writeEnded(ctx, q, ended{
			sessionID: d.SessionID, userID: s.OwnerID, messageID: d.ID, final: e.final,
			task: e.task, partial: e.partial,
		})
		if err != nil {
			return nil, err
		}
		stored = msg
		return []events.Draft{endedEvent(s.OwnerID, d.SessionID, msg)}, nil
	})
	if moved && err == nil {
		s.endProgress(s.OwnerID, d.SessionID, d.ID, d.CreatedAt, stored)
	}
	return previous, moved, err
}

// openStates are the states a delivery can be stopped from.
var openStates = []dbstate.DeliveryState{
	dbstate.DeliveryRecorded, dbstate.DeliverySending, dbstate.DeliveryDelivered, dbstate.DeliveryUncertain,
}

// Stop cancels the session's open turn with CancelTask, keeping the session and its kagent Session.
// The delivery ends cancelled, in one transaction with the conversation's stop note and the queue
// hold, only after kagent returned the task cancelled; nothing is written before that, so a kagent
// error (*KagentError) leaves the delivery as it was. A message kagent never saw is cancelled
// without a call. The outcome is stopped, finished (the turn ended on its own first, and is
// recorded as it ended) or no_open_turn. *UnconfirmedError is a send in flight that kagent shows no
// task for yet, or a task still running after the cancel.
func (s *Service) Stop(ctx context.Context, sessionID string) (StopOutcome, error) {
	b, err := s.ownedBinding(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if s.Kagent == nil {
		return "", &KagentError{Err: errors.New("kagent is not configured")}
	}
	open, err := dbq.New(s.Pool).ListOpenDeliveries(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("list open turns: %w", err)
	}
	out := NoOpenTurn
	hold := s.opts().UncertainHold
	for _, d := range open {
		if d.State == dbstate.DeliveryUncertain && time.Since(d.UpdatedAt) > hold {
			// Past its hold the row no longer blocks sends, so a newer turn may be running behind it.
			// The delivery loop fails it on its next recheck; stopping it here would only mask the
			// turn that is running.
			continue
		}
		res, err := s.stopDelivery(ctx, b, d, false)
		if err != nil {
			return "", err
		}
		if out != Stopped {
			out = res
		}
	}
	return out, nil
}

func (s *Service) stopDelivery(ctx context.Context, b dbq.GetOwnedSessionBindingRow, d dbq.Delivery, retried bool) (StopOutcome, error) {
	q := dbq.New(s.Pool)
	if d.State == dbstate.DeliveryRecorded {
		// kagent never saw it: there is no task to cancel.
		_, moved, err := s.endDelivery(ctx, d, []dbstate.DeliveryState{dbstate.DeliveryRecorded}, ending{
			final: dbstate.DeliveryCancelled, detail: "stopped by user before it was sent", note: true,
		})
		if err != nil {
			return "", err
		}
		if moved {
			return Stopped, nil
		}
		cur, err := q.GetDelivery(ctx, d.ID)
		if err != nil {
			return "", fmt.Errorf("get delivery: %w", err)
		}
		if !retried && cur.State != dbstate.DeliveryRecorded && openState(cur.State) {
			// The delivery loop claimed it after this stop read it, so kagent can hold a task for it
			// now. Stop it as the in-flight turn it has become, once.
			return s.stopDelivery(ctx, b, cur, true)
		}
		return stopResult(cur.State), nil
	}
	agent, err := s.bindingAgentOf(b)
	if err != nil {
		return "", &KagentError{Err: err}
	}
	ksid := kagentSessionOf(d, b)
	if ksid == "" {
		return "", &UnconfirmedError{Detail: "The agent session is not ready, so the turn cannot be stopped."}
	}
	taskID := ""
	if d.TaskID != nil {
		taskID = *d.TaskID
	} else {
		task, found, err := s.Kagent.FindTaskForMessage(ctx, agent, ksid, d.ID)
		if err != nil && !found {
			return "", &KagentError{Err: err}
		}
		if !found || task == nil {
			return "", &UnconfirmedError{Detail: "The message was just sent and kagent shows no task for it yet; try again."}
		}
		taskID = string(task.ID)
	}
	task, err := s.Kagent.CancelTask(ctx, agent, taskID)
	if err != nil {
		return "", &KagentError{Err: err}
	}
	switch st := normState(task.Status.State); {
	case st == "canceled":
		partial := artifactText(task)
		if partial == "" {
			// A cancel response may leave the artifacts out; read the task again. A failed read keeps
			// nothing.
			if again, err := s.Kagent.GetTask(ctx, agent, taskID); err == nil {
				partial = artifactText(again)
			}
		}
		if _, _, err := s.endDelivery(ctx, d, openStates, ending{
			final: dbstate.DeliveryCancelled, detail: "stopped by user", task: task, partial: partial, note: true,
		}); err != nil {
			return "", err
		}
		// Stopped by this call or by the delivery loop seeing the same cancellation.
		cur, err := q.GetDelivery(ctx, d.ID)
		if err != nil {
			return "", fmt.Errorf("get delivery: %w", err)
		}
		return stopResult(cur.State), nil
	case isTerminal(task.Status.State):
		// The turn ended first (kagent returns a finished task unchanged): record how it ended.
		e := ending{final: dbstate.DeliveryCompleted, task: task, note: true}
		if st != "completed" {
			e.final, e.detail, e.nativeFailure = dbstate.DeliveryFailed, "task "+st+": "+statusText(task), st == "failed"
		}
		if _, _, err := s.endDelivery(ctx, d, openStates, e); err != nil {
			return "", err
		}
		return Finished, nil
	default:
		return "", &UnconfirmedError{Detail: fmt.Sprintf("kagent still shows the task as %s after the cancel.", st)}
	}
}

func openState(st dbstate.DeliveryState) bool {
	for _, o := range openStates {
		if o == st {
			return true
		}
	}
	return false
}

// stopResult is stopped when the delivery is cancelled (by this call, or by the delivery loop
// seeing the same cancellation), otherwise finished.
func stopResult(st dbstate.DeliveryState) StopOutcome {
	if st == dbstate.DeliveryCancelled {
		return Stopped
	}
	return Finished
}

// errFinishedMeanwhile aborts a cancel's transaction for a session that finished while it ran.
var errFinishedMeanwhile = errors.New("sessions: finished meanwhile")

// CancelResult is what cancelling a session did.
type CancelResult struct {
	// Status is the session's status after the call.
	Status dbstate.SessionStatus
	// Agent is stopped, not_running (nothing was running) or unknown (a cancel could not be
	// confirmed; the agent may still be running, and it is not retried).
	Agent string
}

// Cancel ends a session: it sets the final cancelled status and cancels non-final native tasks,
// including those started outside the delivery ledger. Task-owned sending and uncertain deliveries
// stay open for reconciliation; other open deliveries are closed. The status and an unknown agent
// outcome are written first and are sticky, so no later delivery
// brings the session back and no message is taken. A cancel kagent does not confirm is reported as
// unknown. The scheduled-cancel sweep revisits it without replaying the scheduled send. A session
// that had already finished is left as it is.
func (s *Service) Cancel(ctx context.Context, sessionID string) (CancelResult, error) {
	q := dbq.New(s.Pool)
	row, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelResult{}, ErrNotFound
	}
	if err != nil {
		return CancelResult{}, fmt.Errorf("get session: %w", err)
	}
	if finishedStatus(row.Status) {
		return cancelledResult(row), nil
	}
	b, err := s.ownedBinding(ctx, sessionID)
	if err != nil {
		return CancelResult{}, err
	}
	if b.Role == dbstate.RoleMain {
		return CancelResult{}, conflict("The main thread cannot be cancelled.")
	}

	var closedNow []cancelledDelivery
	var finished dbq.GetSessionRowRow
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		closedNow = nil
		// Sends take this lock before they look at the status, so none is recorded after this commits.
		if err := db.XactLock(ctx, tx, "mainloop:send:"+sessionID); err != nil {
			return nil, err
		}
		q := dbq.New(tx)
		// Task-owned sends that won admission must remain visible until the delivery loop resolves
		// them. Lock the attempt before deliveries, in the same order as the sending decision.
		_, aerr := q.LockSessionTaskAttempt(ctx, &sessionID)
		if aerr != nil && !errors.Is(aerr, pgx.ErrNoRows) {
			return nil, aerr
		}
		taskOwned := aerr == nil
		// Deliveries first, then the session: a settle locks them in that order too (FinishDelivery,
		// then RefreshSessionStatus), so the two cannot deadlock.
		open, err := q.ListOpenDeliveries(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("list open turns: %w", err)
		}
		states := make([]string, 0, len(openStates))
		for _, st := range openStates {
			if taskOwned && (st == dbstate.DeliverySending || st == dbstate.DeliveryUncertain) {
				continue
			}
			states = append(states, string(st))
		}
		var drafts []events.Draft
		for _, d := range open {
			det := "cancelled by user"
			prev, err := q.EndDelivery(ctx, dbq.EndDeliveryParams{
				ID: d.ID, FromStates: states, State: dbstate.DeliveryCancelled, Detail: &det,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				if taskOwned && (d.State == dbstate.DeliverySending || d.State == dbstate.DeliveryUncertain) {
					// Native cancellation still observes these sends; only their ledger close is deferred.
					closedNow = append(closedNow, cancelledDelivery{d: d, prev: d.State})
				}
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("close delivery: %w", err)
			}
			closedNow = append(closedNow, cancelledDelivery{d: d, prev: dbstate.DeliveryState(prev)})
			// Every ending publishes one event, so open clients see the turn end.
			drafts = append(drafts, endedEvent(s.OwnerID, sessionID, d.ID))
		}
		if _, err := q.SetSessionCancelled(ctx, sessionID); errors.Is(err, pgx.ErrNoRows) {
			cur, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: sessionID, UserID: s.OwnerID})
			if err != nil {
				return nil, err
			}
			finished = cur
			return nil, errFinishedMeanwhile // rolls back: a finished session keeps its deliveries as they are
		} else if err != nil {
			return nil, fmt.Errorf("cancel session: %w", err)
		}
		// The agent can no longer reach MCP; the Secret is deleted after this commits.
		if _, err := agentauth.RevokeInTx(ctx, q, sessionID); err != nil {
			return nil, err
		}
		return drafts, nil
	})
	if err != nil && !errors.Is(err, errFinishedMeanwhile) {
		return CancelResult{}, err
	}
	if errors.Is(err, errFinishedMeanwhile) {
		return cancelledResult(finished), nil
	}
	for _, c := range closedNow {
		s.endProgress(s.OwnerID, sessionID, c.d.ID, c.d.CreatedAt, "")
	}
	// Reload after the intent commits: an in-flight create may have bound its Session meanwhile.
	b, err = s.ownedBinding(ctx, sessionID)
	if err != nil {
		return CancelResult{}, err // the durable agent outcome stays unknown
	}
	agentOutcome := s.cancelNativeTasks(ctx, sessionID, b, closedNow)
	if err := q.RecordSessionCancelOutcome(ctx, dbq.RecordSessionCancelOutcomeParams{
		ID: sessionID, AgentCancelOutcome: &agentOutcome,
	}); err != nil {
		return CancelResult{}, fmt.Errorf("record agent cancel outcome: %w", err)
	}
	return CancelResult{Status: dbstate.SessionCancelled, Agent: agentOutcome}, nil
}

type cancelledDelivery struct {
	d    dbq.Delivery
	prev dbstate.DeliveryState
}

// cancelledResult never treats an earlier uncertain cancel as evidence the agent stopped.
func cancelledResult(row dbq.GetSessionRowRow) CancelResult {
	agent := AgentNotRunning
	if row.Status == dbstate.SessionCancelled {
		agent = AgentUnknown
		if row.AgentCancelOutcome != nil {
			agent = *row.AgentCancelOutcome
		}
	}
	return CancelResult{Status: row.Status, Agent: agent}
}

// cancelNativeTasks observes every bound or previously used context before cancelling. Network
// calls happen after the cancelled intent commits; none run under a database lock or transaction.
func (s *Service) cancelNativeTasks(ctx context.Context, sessionID string, b dbq.GetOwnedSessionBindingRow, closedNow []cancelledDelivery) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	outcome := AgentNotRunning
	q := dbq.New(s.Pool)
	contexts, err := q.ListSessionKagentContexts(ctx, sessionID)
	if err != nil {
		return AgentUnknown
	}
	link, err := q.GetSessionScheduledExecution(ctx, &sessionID)
	scheduled := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentUnknown
	}
	if scheduled && link.KagentSessionID != nil && *link.KagentSessionID != "" {
		// The original execution's context still matters if the binding was replaced later.
		found := false
		for _, id := range contexts {
			found = found || id == *link.KagentSessionID
		}
		if !found {
			contexts = append(contexts, *link.KagentSessionID)
		}
	}
	mayHaveSent := false
	for _, c := range closedNow {
		mayHaveSent = mayHaveSent || c.prev != dbstate.DeliveryRecorded
	}
	if len(contexts) == 0 && !mayHaveSent && !scheduled {
		return outcome
	}
	if s.Kagent == nil {
		return AgentUnknown
	}
	agent, err := s.bindingAgentOf(b)
	if err != nil {
		return AgentUnknown
	}
	var execution *kagentapi.ScheduledRunExecution
	if scheduled {
		// A reserved Session or RUNNING execution can precede its A2A send. Only a fresh
		// execution read, never an empty task list or the adoption snapshot, can close that gap.
		reader, ok := s.Kagent.(scheduledExecutionReader)
		if ok {
			execution, err = reader.GetScheduledRunExecution(ctx, link.ExecutionID)
		}
		if !ok || err != nil || execution.GetId() != link.ExecutionID || execution.GetScheduledRunId() != link.ScheduleID ||
			link.KagentSessionID == nil || execution.GetSessionId() != *link.KagentSessionID {
			execution = nil
			outcome = AgentUnknown
		}
	}
	tasks := make(map[string]*a2a.Task)
	for _, ksid := range contexts {
		listed, err := s.Kagent.ListTasks(ctx, agent, ksid)
		if err != nil {
			s.log().WarnContext(ctx, "cancel: task listing unconfirmed", "session_id", sessionID, "error", err)
			outcome = AgentUnknown
			continue
		}
		for _, task := range listed {
			if task == nil || task.ID == "" || task.ContextID != ksid {
				outcome = AgentUnknown
				continue
			}
			tasks[string(task.ID)] = task
		}
	}
	for _, c := range closedNow {
		if c.prev == dbstate.DeliveryRecorded {
			continue // never sent
		}
		ksid := kagentSessionOf(c.d, b)
		taskID := ""
		var task *a2a.Task
		if c.d.TaskID != nil {
			taskID = *c.d.TaskID
			task = tasks[taskID]
			if task == nil && taskID != "" {
				task, err = s.Kagent.GetTask(ctx, agent, taskID)
				if err != nil {
					outcome = AgentUnknown
					continue
				}
			}
		} else if ksid != "" {
			var found bool
			task, found, err = s.Kagent.FindTaskForMessage(ctx, agent, ksid, c.d.ID)
			if err != nil || !found {
				outcome = AgentUnknown
				continue
			}
			if task != nil {
				taskID = string(task.ID)
			}
		}
		if task == nil || taskID == "" || string(task.ID) != taskID || task.ContextID != ksid {
			// An unresolved send may still land and start a task this cancel cannot see.
			outcome = AgentUnknown
			continue
		}
		tasks[taskID] = task
	}
	if taskID := execution.GetTaskId(); taskID != "" && tasks[taskID] == nil {
		task, err := s.Kagent.GetTask(ctx, agent, taskID)
		if err != nil || task == nil || string(task.ID) != taskID || task.ContextID != execution.GetSessionId() {
			outcome = AgentUnknown
		} else {
			tasks[taskID] = task
		}
	}
	outcome = s.cancelTaskSet(ctx, sessionID, agent, tasks, outcome)
	if scheduled && !scheduledExecutionFinal(execution) {
		task := tasks[execution.GetTaskId()]
		if execution == nil || task == nil || task.ContextID != execution.GetSessionId() || !task.Status.State.Terminal() {
			return AgentUnknown
		}
		// The execution's task is now positively final. Re-list after that evidence so a task
		// that appeared during the first listing cannot be missed when we settle the outcome.
		remaining := make(map[string]*a2a.Task)
		for _, ksid := range contexts {
			listed, err := s.Kagent.ListTasks(ctx, agent, ksid)
			if err != nil {
				outcome = AgentUnknown
				continue
			}
			for _, task := range listed {
				if task == nil || task.ID == "" || task.ContextID != ksid {
					outcome = AgentUnknown
					continue
				}
				remaining[string(task.ID)] = task
			}
		}
		outcome = s.cancelTaskSet(ctx, sessionID, agent, remaining, outcome)
	}
	return outcome
}

func (s *Service) cancelTaskSet(ctx context.Context, sessionID string, agent kagent.AgentRef, tasks map[string]*a2a.Task, outcome string) string {
	for taskID, task := range tasks {
		if task.Status.State.Terminal() {
			if task.Status.State == a2a.TaskStateCanceled && outcome != AgentUnknown {
				outcome = AgentStopped
			}
			continue
		}
		cancelled, err := s.Kagent.CancelTask(ctx, agent, taskID)
		if err != nil {
			s.log().WarnContext(ctx, "cancel: task cancel unconfirmed", "session_id", sessionID, "error", err)
			outcome = AgentUnknown
			continue
		}
		if cancelled == nil || string(cancelled.ID) != taskID || cancelled.ContextID != task.ContextID || !cancelled.Status.State.Terminal() {
			outcome = AgentUnknown
			continue
		}
		tasks[taskID] = cancelled
		if cancelled.Status.State == a2a.TaskStateCanceled && outcome != AgentUnknown {
			outcome = AgentStopped
		}
	}
	return outcome
}

type scheduledExecutionReader interface {
	GetScheduledRunExecution(context.Context, string) (*kagentapi.ScheduledRunExecution, error)
}

func scheduledExecutionFinal(e *kagentapi.ScheduledRunExecution) bool {
	switch e.GetState() {
	case kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_SUCCEEDED,
		kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_FAILED,
		kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_TIMED_OUT:
		return true
	default:
		return false
	}
}

// CancelSweep revisits unknown outcomes on adopted scheduled sessions. There is no cancel-
// execution RPC: it observes before repeating idempotent task cancellation, never the send.
func (s *Service) CancelSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "cancel-scheduled-sessions", Every: 30 * time.Second, Run: s.ReconcileScheduledCancels}
}

// ReconcileScheduledCancels runs a bounded batch with durable exponential backoff. A timeout or
// exhausted pass leaves unknown intact; later passes continue until there is positive evidence.
func (s *Service) ReconcileScheduledCancels(ctx context.Context) error {
	if s.Kagent == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	q := dbq.New(s.Pool)
	rows, err := q.ListScheduledSessionCancelsDue(ctx, dbq.ListScheduledSessionCancelsDueParams{UserID: s.OwnerID, Limit: 50})
	if err != nil {
		return err
	}
	var errs []error
	for _, sessionID := range rows {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if err := s.reconcileScheduledCancel(ctx, q, sessionID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileScheduledCancel(ctx context.Context, q *dbq.Queries, sessionID string) error {
	if n, err := q.BeginScheduledSessionCancelReconcile(ctx, sessionID); err != nil || n == 0 {
		return err
	}
	b, err := s.ownedBinding(ctx, sessionID)
	if err != nil {
		return err
	}
	rows, err := q.ListCancelledSessionSends(ctx, sessionID)
	if err != nil {
		return err
	}
	closed := make([]cancelledDelivery, 0, len(rows))
	for _, d := range rows {
		closed = append(closed, cancelledDelivery{d: d, prev: dbstate.DeliveryUncertain})
	}
	outcome := s.cancelNativeTasks(ctx, sessionID, b, closed)
	if outcome == AgentUnknown {
		return nil
	}
	return q.RecordSessionCancelOutcome(ctx, dbq.RecordSessionCancelOutcomeParams{ID: sessionID, AgentCancelOutcome: &outcome})
}

// Archive clears one finished session from the list. A live one must be cancelled first, and the
// main thread is never archived.
func (s *Service) Archive(ctx context.Context, sessionID string) error {
	q := dbq.New(s.Pool)
	row, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if b, err := q.GetAgentBinding(ctx, sessionID); err == nil && b.Role == dbstate.RoleMain {
		return conflict("The main thread cannot be archived.")
	}
	if !finishedStatus(row.Status) {
		return conflict("This session is still running or waiting; cancel it first.")
	}
	var n int64
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		var err error
		if n, err = tq.ArchiveFinishedSession(ctx, sessionID); err != nil || n == 0 {
			return err
		}
		_, err = agentauth.RevokeInTx(ctx, tq, sessionID)
		return err
	})
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	if n == 0 {
		return conflict("This session is still running or waiting; cancel it first.")
	}
	return nil
}

// sessionColors are assigned to new sessions in turn, as CSS variables so themes can recolor them.
var sessionColors = []string{
	"var(--term-cyan)", "var(--term-green)", "var(--term-orange)",
	"var(--term-purple)", "var(--term-magenta)", "var(--term-yellow)",
}

// NewSession is a request to start a native session.
type NewSession struct {
	Title, Description, Prompt string
	RepoURL                    *string
	// AgentKind is a provider profile id or alias; empty means Claude Code.
	AgentKind string
}

// Create starts a native session: the session and its ungranted agent binding, and the prompt as
// its first message, in one transaction. The delivery loop sends the prompt and creates the kagent
// Session. A provider that is unknown or disabled is ErrInvalid.
func (s *Service) Create(ctx context.Context, n NewSession) (string, error) {
	prompt, err := checkText(uuid.NewString(), n.Prompt)
	if err != nil {
		return "", err
	}
	kind := n.AgentKind
	if kind == "" {
		kind = "claude"
	}
	if s.Providers == nil {
		return "", fmt.Errorf("%w: no provider is configured", ErrInvalid)
	}
	profile, err := s.Providers.Resolve(kind, string(dbstate.RoleAgent), true)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	id, messageID := uuid.NewString(), uuid.NewString()
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		count, err := q.CountSessions(ctx, s.OwnerID)
		if err != nil {
			return fmt.Errorf("count sessions: %w", err)
		}
		color := sessionColors[int(count)%len(sessionColors)]
		if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
			ID: id, UserID: s.OwnerID, Title: n.Title, Description: n.Description, Prompt: n.Prompt,
			RepoURL: n.RepoURL, BaseBranch: "", Color: &color,
		}); err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
			SessionID: id, Kind: profile.ID, Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantNone,
		}); err != nil {
			return fmt.Errorf("create binding: %w", err)
		}
		if _, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: id, Status: dbstate.SessionActive}); err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		return s.record(ctx, tx, SourceUser, id, messageID, prompt)
	})
	if err != nil {
		return "", busyOrErr(err)
	}
	return id, nil
}

// HasOpen reports whether the session has a turn that idle suspend must not interrupt: a delivery
// that is recorded, sending or delivered, or an uncertain one still inside its hold (a task may be
// running). It implements workspaces.Deliveries.
func (s *Service) HasOpen(ctx context.Context, sessionID string) (bool, error) {
	return (&Ledger{Pool: s.Pool, Opts: s.Opts}).HasOpen(ctx, sessionID)
}

// LastActivity is the newest update of any delivery of the session, or the zero time when it has
// none. It implements workspaces.Deliveries.
func (s *Service) LastActivity(ctx context.Context, sessionID string) (time.Time, error) {
	return (&Ledger{Pool: s.Pool, Opts: s.Opts}).LastActivity(ctx, sessionID)
}
