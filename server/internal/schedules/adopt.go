package schedules

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Adoption turns kagent's executions into Mainloop state, once each:
//
//   - an execution that has reserved a kagent Session becomes a Mainloop session bound to it, so the
//     existing observers (HITL) and the session screens see it;
//   - an execution that failed or timed out opens one inbox item.
//
// It follows the reconciler rule. The sweep is the only writer and runs under the reconcile
// runner's sweep lease. scheduled_run_links.execution_id is the idempotency key: the link, the
// session, the binding, the inbox item and schedule:updated are written in one transaction, and the
// ids of the session and item are derived from the execution id. kagent is read before every
// write, never inside a transaction, so a crash or a second replica repeats the read and finds the
// link.

// pageLimit is how many executions the sweep reads per page.
const pageLimit = 100

// Sweep is the adoption sweep as a reconcile sweep.
func (s *Service) Sweep() reconcile.Sweep {
	every := s.SweepEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	return reconcile.Sweep{Name: "schedules", Every: every, Run: s.Adopt}
}

// Adopt runs one adoption pass: first it finishes the creates this instance sent and did not see
// answered, then it walks the schedules in the registry. kagent's list is never read here: the
// registry holds exactly the schedules this instance made, on Agents this deployment owns. A
// schedule that fails is logged and left for the next pass; the others still run.
func (s *Service) Adopt(ctx context.Context) error {
	if s.Kagent == nil || s.Pool == nil {
		return nil
	}
	q := dbq.New(s.Pool)
	var errs []error
	owned, err := s.ownAgents(ctx)
	if err != nil {
		return err
	}
	if err := s.recoverCreates(ctx, owned); err != nil {
		errs = append(errs, err)
	}
	rows, err := q.ListScheduledRunSchedulesToAdopt(ctx, s.OwnerID)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !owned[agentKey(row.AgentNamespace, row.AgentName)] {
			continue // the Agent left this deployment; its schedule is not ours to read any more
		}
		if err := s.adoptSchedule(ctx, row); err != nil {
			s.log().WarnContext(ctx, "schedule adoption failed", "schedule_id", *row.ScheduleID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverCreates replays the creates that were sent and never confirmed (the reply was lost, or the
// process ended between the intent and the answer). It sends a create only for an intent on an Agent
// this deployment owns now, as the routes do: a replay is a fresh create, so it must not outlive the
// ownership that allowed the first send. Other intents are left for the Agent to return. It replays
// only an intent with no unexpired attempt lease. kagent dedupes on the request id, so the replay is
// also the read: a schedule back confirms the intent; a definite refusal, with no other live lease,
// deletes it (nothing can still succeed); an uncertain answer leaves it for a later pass.
func (s *Service) recoverCreates(ctx context.Context, owned map[string]bool) error {
	q := dbq.New(s.Pool)
	rows, err := q.ListScheduledRunIntents(ctx, s.OwnerID)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !owned[agentKey(row.AgentNamespace, row.AgentName)] {
			continue
		}
		var cfg kagentapi.ScheduledRunConfig
		if err := protojson.Unmarshal(row.CreateConfig, &cfg); err != nil {
			errs = append(errs, err)
			continue
		}
		l, err := s.lease(ctx, row.RequestID, true)
		if errors.Is(err, errIntentGone) {
			continue // an attempt took a lease since the list, or the intent was settled
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		agent := &kagentapi.ResourceReference{Namespace: row.AgentNamespace, Name: row.AgentName}
		_, sendErr, regErr := s.attempt(ctx, l, row.RequestID, agent, &cfg, true)
		if regErr != nil || (sendErr != nil && !definite(sendErr)) {
			err := errors.Join(regErr, sendErr)
			s.log().WarnContext(ctx, "schedule create recovery failed", "request_id", row.RequestID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) adoptSchedule(ctx context.Context, row dbq.ScheduledRunSchedule) error {
	q := dbq.New(s.Pool)
	id := *row.ScheduleID
	deleted := row.DeletedAt != nil
	// The schedule is read every pass: it says whether it is deleted and what it is called. A
	// tombstone keeps its executions readable, so a deleted schedule is read one last time.
	sc, err := s.Kagent.GetScheduledRun(ctx, id)
	var se *kagent.SessionError
	switch {
	case errors.As(err, &se) && se.Code == connect.CodeNotFound:
		return s.settle(ctx, id, true)
	case err != nil:
		return err
	}
	name := sc.GetConfig().GetName()
	if sc.GetDeletedAt() != nil && !deleted {
		if err := q.MarkScheduledRunScheduleDeleted(ctx, row.ScheduleID); err != nil {
			return err
		}
		deleted = true
	}
	fresh, err := s.freshExecutions(ctx, row)
	if err != nil {
		return err
	}
	// Oldest first, so the cursor only moves over executions that are fully handled.
	slices.Reverse(fresh)
	ids := make([]string, len(fresh))
	for i, e := range fresh {
		ids[i] = e.GetId()
	}
	links, err := q.ListScheduledRunLinks(ctx, dbq.ListScheduledRunLinksParams{ScheduleID: id, ExecutionIds: ids})
	if err != nil {
		return err
	}
	byID := map[string]dbq.ScheduledRunLink{}
	for _, l := range links {
		byID[l.ExecutionID] = l
	}
	quietBefore := time.Now().Add(-s.Limits.cursorWindow())
	cursor, contiguous := "", true
	var errs []error
	for _, e := range fresh {
		done, err := s.adoptExecution(ctx, row, name, e, byID[e.GetId()])
		if err != nil {
			errs = append(errs, err)
			done = false
		}
		// An execution created within the safety window may still have a lower-id sibling that has
		// not committed, so the cursor stays behind it; it is read again next pass, which finds its
		// link and changes nothing.
		if e.GetCreatedAt().AsTime().After(quietBefore) {
			done = false
		}
		if contiguous && done {
			cursor = e.GetId()
		} else {
			contiguous = false
		}
	}
	if cursor != "" {
		if err := q.SetScheduledRunCursor(ctx, dbq.SetScheduledRunCursorParams{ScheduleID: row.ScheduleID, CursorExecutionID: cursor}); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 && deleted && contiguous {
		return s.settle(ctx, id, false)
	}
	return errors.Join(errs...)
}

// settle ends the sweep's interest in a deleted schedule whose executions are all handled.
// gone also marks it deleted, for a schedule kagent no longer knows.
func (s *Service) settle(ctx context.Context, id string, gone bool) error {
	q := dbq.New(s.Pool)
	if gone {
		if err := q.MarkScheduledRunScheduleDeleted(ctx, &id); err != nil {
			return err
		}
	}
	return q.SettleScheduledRunSchedule(ctx, &id)
}

// freshExecutions reads the executions newer than the schedule's cursor, newest first.
func (s *Service) freshExecutions(ctx context.Context, row dbq.ScheduledRunSchedule) ([]*kagentapi.ScheduledRunExecution, error) {
	var out []*kagentapi.ScheduledRunExecution
	token := ""
	seen := map[string]bool{}
	for {
		page, next, err := s.Kagent.ListScheduledRunExecutionsPage(ctx, *row.ScheduleID, token, pageLimit)
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			if e.GetId() <= row.CursorExecutionID {
				return out, nil
			}
			out = append(out, e)
		}
		if next == "" {
			return out, nil
		}
		// A kagent that repeats a token would hold the sweep, and its lock, for good.
		if seen[next] {
			return nil, fmt.Errorf("kagent repeated execution page token for schedule %s", *row.ScheduleID)
		}
		seen[next] = true
		token = next
	}
}

// adoptExecution brings one execution up to date. It reports whether the execution is finished
// with: ended, its session linked if it ever had one, and its inbox item opened if it failed.
func (s *Service) adoptExecution(
	ctx context.Context, row dbq.ScheduledRunSchedule, name string,
	e *kagentapi.ScheduledRunExecution, link dbq.ScheduledRunLink,
) (bool, error) {
	state := stateName(e.GetState())
	linked := link.ExecutionID != ""
	needSession := e.GetSessionId() != "" && (!linked || link.SessionID == nil)
	needAttention := failed(state) && (!linked || link.AttentionOpenedAt == nil)
	needState := !linked || link.State != state
	if !needSession && !needAttention && !needState {
		return terminal(state), nil
	}
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		// The link is the claim. Take it, then act on what the row says, so a replay that lost the
		// race finds the work done.
		claimed, err := q.InsertScheduledRunLink(ctx, dbq.InsertScheduledRunLinkParams{
			ExecutionID: e.GetId(), ScheduleID: *row.ScheduleID, State: state,
		})
		if err != nil {
			return nil, err
		}
		l, err := q.GetScheduledRunLink(ctx, e.GetId())
		if err != nil {
			return nil, err
		}
		var drafts []events.Draft
		changed := claimed > 0
		if ks := e.GetSessionId(); ks != "" && l.SessionID == nil {
			sid, err := s.createSession(ctx, q, row, name, e)
			if err != nil {
				return nil, err
			}
			if n, err := q.SetScheduledRunLinkSession(ctx, dbq.SetScheduledRunLinkSessionParams{
				ExecutionID: e.GetId(), KagentSessionID: &ks, SessionID: &sid,
			}); err != nil {
				return nil, err
			} else if n > 0 {
				l.SessionID, changed = &sid, true
			}
		}
		if l.State != state {
			if err := q.SetScheduledRunLinkState(ctx, dbq.SetScheduledRunLinkStateParams{ExecutionID: e.GetId(), State: state}); err != nil {
				return nil, err
			}
			changed = true
		}
		if l.SessionID != nil {
			if err := s.followSession(ctx, q, *l.SessionID, state, e); err != nil {
				return nil, err
			}
		}
		if failed(state) && l.AttentionOpenedAt == nil {
			ds, err := s.openAttention(ctx, tx, row, name, e, state, l.SessionID)
			if err != nil {
				return nil, err
			}
			if err := q.MarkScheduledRunLinkAttention(ctx, e.GetId()); err != nil {
				return nil, err
			}
			drafts, changed = append(drafts, ds...), true
		}
		if changed {
			drafts = append(drafts, updated(s.OwnerID, *row.ScheduleID))
		}
		return drafts, nil
	})
	if err != nil {
		return false, fmt.Errorf("adopt execution %s: %w", e.GetId(), err)
	}
	return terminal(state), nil
}

// sessionIDFor derives the session id from the execution, so a replay makes the same session.
func sessionIDFor(executionID string) string { return "sched_" + executionID }

func (s *Service) createSession(
	ctx context.Context, q *dbq.Queries, row dbq.ScheduledRunSchedule, name string, e *kagentapi.ScheduledRunExecution,
) (string, error) {
	id := sessionIDFor(e.GetId())
	if name == "" {
		name = "Scheduled run"
	}
	when := e.GetCreatedAt().AsTime().UTC().Format("2006-01-02 15:04 UTC")
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
		ID: id, UserID: s.OwnerID, Title: name + " " + when,
		Description: fmt.Sprintf("Scheduled run of %s/%s", row.AgentNamespace, row.AgentName),
		Prompt:      e.GetPrompt(), BaseBranch: "main",
	}); err != nil {
		return "", err
	}
	ks := e.GetSessionId()
	kind, role := s.bindingFor(row.AgentNamespace, row.AgentName)
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: id, Kind: kind, Role: role, MCPGrantKind: dbstate.GrantNone,
	}); err != nil {
		return "", err
	}
	if err := q.SetBindingAgentRef(ctx, dbq.SetBindingAgentRefParams{
		SessionID: id, KagentAgentNamespace: &row.AgentNamespace, KagentAgentName: &row.AgentName,
	}); err != nil {
		return "", err
	}
	if _, err := q.SetBindingKagentSession(ctx, dbq.SetBindingKagentSessionParams{
		SessionID: id, KagentSessionID: &ks,
	}); err != nil {
		return "", err
	}
	return id, nil
}

// bindingFor picks the provider kind and role whose Agent is the schedule's, because that pair is
// how the session is labelled. Routing uses the binding's stored Agent, including when no profile
// names that Agent.
func (s *Service) bindingFor(namespace, name string) (string, dbstate.BindingRole) {
	if s.Providers != nil {
		for _, role := range []dbstate.BindingRole{dbstate.RoleAgent, dbstate.RoleChild, dbstate.RoleSupervisor} {
			for _, p := range s.Providers.Profiles() {
				if ref, ok := p.Agents[string(role)]; ok && ref.Namespace == namespace && ref.Name == name {
					return p.ID, role
				}
			}
		}
	}
	return "scheduled", dbstate.RoleAgent
}

// followSession moves the session along with its execution: running starts it, an end finishes it.
// Both only touch a session nobody else has moved.
func (s *Service) followSession(ctx context.Context, q *dbq.Queries, id, state string, e *kagentapi.ScheduledRunExecution) error {
	switch state {
	case stateRunning:
		return q.StartScheduledRunSession(ctx, id)
	case stateSucceeded:
		return q.EndScheduledRunSession(ctx, dbq.EndScheduledRunSessionParams{ID: id, Status: dbstate.SessionCompleted})
	case stateFailed, stateTimedOut:
		why := e.GetFailureReason()
		if why == "" {
			why = state
		}
		return q.EndScheduledRunSession(ctx, dbq.EndScheduledRunSessionParams{ID: id, Status: dbstate.SessionFailed, Error: &why})
	}
	return nil
}

func (s *Service) openAttention(
	ctx context.Context, tx pgx.Tx, row dbq.ScheduledRunSchedule, name string,
	e *kagentapi.ScheduledRunExecution, state string, sessionID *string,
) ([]events.Draft, error) {
	if name == "" {
		name = "a scheduled run"
	}
	what := "failed"
	if state == stateTimedOut {
		what = "timed out"
	}
	why := e.GetFailureReason()
	content := fmt.Sprintf("Scheduled run %s %s.", name, what)
	if why != "" {
		content += " " + why
	}
	ctxMap := map[string]any{"schedule_id": *row.ScheduleID, "execution_id": e.GetId(), "state": state}
	if sessionID != nil {
		ctxMap["session_id"] = *sessionID
	}
	// The item is not tied to the session: items of a session that ended are resolved with it, and
	// this one is about that ending.
	return attention.Open(ctx, tx, s.OwnerID, attention.NewQueueItem{
		ID: "sched_exec_" + e.GetId(), ItemType: api.QueueItemTypeError, Priority: api.High,
		Title: "Scheduled run " + what, Content: content, Needed: content, Context: ctxMap,
	})
}
