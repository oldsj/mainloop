package hitl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// ObserverSweep is the observer as a reconcile sweep.
func (s *Service) ObserverSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "hitl-observer", Every: s.opts().ObserveEvery, Run: s.Observe}
}

// scope is where an ask lives: the Mainloop session and its kagent session.
type scope struct {
	ownerID         string
	sessionID       string
	kagentSessionID string
	namespace, name string
	contextID       string
}

func scopeOfSession(hs dbq.HitlSession) scope {
	return scope{
		ownerID: hs.OwnerID, sessionID: hs.SessionID, kagentSessionID: hs.KagentSessionID,
		namespace: hs.AgentNamespace, name: hs.AgentName, contextID: hs.ContextID,
	}
}

func scopeOfRequest(r dbq.HitlRequest) scope {
	return scope{
		ownerID: r.OwnerID, sessionID: r.SessionID, kagentSessionID: r.KagentSessionID,
		namespace: r.AgentNamespace, name: r.AgentName, contextID: r.ContextID,
	}
}

// Observe makes one pass: it picks up the bound sessions, reads a few pages of each least recently
// scanned session's tasks, adopts tasks that wait with a valid ask, and re-reads the live requests
// that are due. Everything it learns is written in small transactions after the kagent read that
// produced it, and a cursor moves only after its page was handled, so a pass that dies half way (an
// outage, a restart) repeats work and loses none.
func (s *Service) Observe(ctx context.Context) error {
	o := s.opts()
	q := dbq.New(s.Pool)
	if _, err := q.SyncHITLSessions(ctx, dbq.SyncHITLSessionsParams{
		AgentNamespace: s.Agent.Namespace, AgentName: s.Agent.Name, UserID: s.OwnerID,
	}); err != nil {
		return fmt.Errorf("sync sessions: %w", err)
	}
	sessions, err := q.ListHITLSessionsToScan(ctx, dbq.ListHITLSessionsToScanParams{
		OwnerID: s.OwnerID, EverySeconds: o.ScanEvery.Seconds(), MaxSessions: int32(o.MaxSessions), //nolint:gosec // small bounded option
	})
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	for _, hs := range sessions {
		if err := s.scanSession(ctx, hs); err != nil {
			if isOutage(err) {
				s.diagnose(ctx, hs.KagentSessionID, errDetail(err))
				s.staleOnOutage(ctx) // the rest would fail the same way; retry on the next pass
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.diagnose(ctx, hs.KagentSessionID, errDetail(err))
			s.log().WarnContext(ctx, "hitl scan failed", "kagent_session_id", hs.KagentSessionID, "error", err)
		}
	}
	due, err := q.ListHITLToRefresh(ctx, dbq.ListHITLToRefreshParams{
		OwnerID: s.OwnerID, EverySeconds: o.RefreshEvery.Seconds(), MaxRequests: int32(o.RefreshBatch), //nolint:gosec // small bounded option
	})
	if err != nil {
		return fmt.Errorf("list live requests: %w", err)
	}
	for _, r := range due {
		if err := s.refresh(ctx, r); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isOutage(err) {
				s.diagnose(ctx, r.KagentSessionID, errDetail(err))
				s.noteOutage(ctx, r)
				continue
			}
			s.diagnose(ctx, r.KagentSessionID, errDetail(err))
			s.log().WarnContext(ctx, "hitl refresh failed", "request_id", r.ID, "error", err)
			_ = q.AttemptHITLRequest(ctx, r.ID)
		}
	}
	return nil
}

// scanSession reads up to PagesPerScan pages of one session's tasks from its cursor.
func (s *Service) scanSession(ctx context.Context, hs dbq.HitlSession) error {
	o := s.opts()
	q := dbq.New(s.Pool)
	sc := scopeOfSession(hs)
	agent := s.agentOf(hs.AgentNamespace, hs.AgentName)
	token := ""
	if hs.TaskCursor != nil {
		token = *hs.TaskCursor
	}
	for range o.PagesPerScan {
		rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
		tasks, next, err := s.Kagent.ListTasksPage(rctx, agent, hs.ContextID, token, o.PageSize)
		cancel()
		if err != nil {
			var perr *kagent.ProtocolError
			if token != "" && errors.As(err, &perr) {
				// The cursor no longer works (kagent restarted its listing). Start over; adopting
				// a task twice is a no-op.
				s.diagnose(ctx, hs.KagentSessionID, "the task listing cursor was rejected; restarting the scan")
				return q.SetHITLSessionScan(ctx, dbq.SetHITLSessionScanParams{KagentSessionID: hs.KagentSessionID})
			}
			return err
		}
		for _, t := range tasks {
			if t == nil || t.Status.State != a2a.TaskStateInputRequired {
				continue
			}
			if err := s.adoptTask(ctx, sc, t); err != nil {
				return err
			}
		}
		var cursor *string
		if next != "" {
			cursor = &next
		}
		if err := q.SetHITLSessionScan(ctx, dbq.SetHITLSessionScanParams{
			KagentSessionID: hs.KagentSessionID, Cursor: cursor, Done: next == "",
		}); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		token = next
	}
	return nil
}

// adoptTask records the task's ask if it has a valid one. A task that waits for something that is
// not a HITL ask, or whose ask cannot be shown, is only reported.
func (s *Service) adoptTask(ctx context.Context, sc scope, t *a2a.Task) error {
	obs, err := readRequest(t.Status.Message)
	switch {
	case errors.Is(err, errNotHITL):
		return nil
	case err != nil:
		s.diagnose(ctx, sc.kagentSessionID, "task "+string(t.ID)+": "+err.Error())
		return nil
	}
	return s.adopt(ctx, sc, t, obs)
}

func (s *Service) newRequestID(sc scope, taskID string, o *observed) string {
	return requestID(sc.kagentSessionID, taskID, o.statusMessageID, o.hash)
}

// adopt writes the request and its inbox card, and retires an older live ask of the same task.
func (s *Service) adopt(ctx context.Context, sc scope, t *a2a.Task, o *observed) error {
	id := s.newRequestID(sc, string(t.ID), o)
	q := dbq.New(s.Pool)
	if cur, err := q.GetHITLRequest(ctx, dbq.GetHITLRequestParams{ID: id, OwnerID: sc.ownerID}); err == nil {
		if cur.Availability == dbstate.HITLPending || cur.Availability == dbstate.HITLStale {
			return s.confirm(ctx, cur)
		}
		return nil // final: the same ask is not reopened
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	title, needed, kind := s.cardText(ctx, sc, o)
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		replaced, err := q.SupersedeLiveHITLForTask(ctx, dbq.SupersedeLiveHITLForTaskParams{
			KagentSessionID: sc.kagentSessionID, TaskID: string(t.ID), ID: id,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range replaced {
			if _, err := q.ResolveHITLAttention(ctx, r); err != nil {
				return nil, err
			}
		}
		n, err := q.InsertHITLRequest(ctx, dbq.InsertHITLRequestParams{
			ID: id, OwnerID: sc.ownerID, SessionID: sc.sessionID, KagentSessionID: sc.kagentSessionID,
			AgentNamespace: sc.namespace, AgentName: sc.name, ContextID: sc.contextID, TaskID: string(t.ID),
			StatusMessageID: o.statusMessageID, RequestHash: o.hash, Kind: o.kind, Payload: o.payload,
		})
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		attn := attentionID(id)
		sess := sc.sessionID
		taskID := string(t.ID)
		if err := q.UpsertHITLAttention(ctx, dbq.UpsertHITLAttentionParams{
			ID: attn, OwnerID: sc.ownerID, Kind: kind, SourceID: id, SessionID: &sess, TaskID: &taskID,
			Title: title, Needed: needed, Href: "/sessions/" + sc.sessionID,
		}); err != nil {
			return nil, err
		}
		return inboxDraft(ctx, q, sc.ownerID, attn)
	})
}

func attentionID(requestID string) string { return "attn_" + requestID }

func inboxDraft(ctx context.Context, q *dbq.Queries, ownerID, item string) ([]events.Draft, error) {
	n, err := q.CountOpenAttention(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	count := int(n)
	return []events.Draft{{
		UserID: ownerID, Type: events.TypeInboxUpdated,
		Payload: api.EventInboxUpdated{ItemId: &item, UnreadCount: &count},
	}}, nil
}

// cardText is what the inbox shows: the session's title, and one line on what is asked.
func (s *Service) cardText(ctx context.Context, sc scope, o *observed) (title, needed string, kind dbstate.AttentionKind) {
	title = "Agent session"
	if c, err := dbq.New(s.Pool).GetHITLContext(ctx, dbq.GetHITLContextParams{ID: sc.sessionID, UserID: sc.ownerID}); err == nil && c.Title != "" {
		title = c.Title
	}
	switch {
	case o.tools != nil:
		names := make([]string, 0, len(o.tools.Tools))
		for _, t := range o.tools.Tools {
			names = append(names, t.Name)
		}
		needed = "Approve " + strings.Join(names, ", ")
		return title, clip(needed, 200), dbstate.AttentionApproval
	default:
		needed = o.ask.Questions[0].Question
		return title, clip(needed, 200), dbstate.AttentionQuestion
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not cut a UTF-8 sequence
		cut--
	}
	return s[:cut] + "…"
}

// confirm records that kagent still shows the request. A stale request reopens, which the inbox
// hears about.
func (s *Service) confirm(ctx context.Context, r dbq.HitlRequest) error {
	if r.Availability != dbstate.HITLStale {
		return dbq.New(s.Pool).ConfirmHITLRequest(ctx, r.ID)
	}
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		if err := q.ConfirmHITLRequest(ctx, r.ID); err != nil {
			return nil, err
		}
		return inboxDraft(ctx, q, r.OwnerID, attentionID(r.ID))
	})
}

// setAvailability moves a live request to a final or stale state, closes its card when the
// request is final, and tells the inbox. A move that did not happen (the request already
// changed) writes and publishes nothing.
func (s *Service) setAvailability(ctx context.Context, r dbq.HitlRequest, to dbstate.HITLAvailability, reason string, from ...dbstate.HITLAvailability) error {
	var why *string
	if reason != "" {
		why = &reason
	}
	fromStates := make([]string, len(from))
	for i, f := range from {
		fromStates[i] = string(f)
	}
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		n, err := q.SetHITLAvailability(ctx, dbq.SetHITLAvailabilityParams{
			Availability: to, Reason: why, ID: r.ID, FromStates: fromStates,
		})
		if err != nil || n == 0 {
			return nil, err
		}
		if to != dbstate.HITLStale {
			if _, err := q.ResolveHITLAttention(ctx, r.ID); err != nil {
				return nil, err
			}
		}
		return inboxDraft(ctx, q, r.OwnerID, attentionID(r.ID))
	})
}

// noteOutage notes a failed look at a live request: it stays pending until kagent has been silent
// for StaleAfter, then it is shown as stale (not answerable) until a read succeeds again.
func (s *Service) noteOutage(ctx context.Context, r dbq.HitlRequest) {
	o := s.opts()
	if r.Availability == dbstate.HITLPending && time.Since(r.CheckedAt) > o.StaleAfter {
		if err := s.setAvailability(ctx, r, dbstate.HITLStale, "kagent has not confirmed this request recently",
			dbstate.HITLPending); err != nil {
			s.log().WarnContext(ctx, "mark hitl request stale", "request_id", r.ID, "error", err)
		}
		return
	}
	_ = dbq.New(s.Pool).AttemptHITLRequest(ctx, r.ID)
}

// staleOnOutage applies the staleness rule to live requests when the listing itself is down and no
// per-request read is tried.
func (s *Service) staleOnOutage(ctx context.Context) {
	o := s.opts()
	due, err := dbq.New(s.Pool).ListHITLToRefresh(ctx, dbq.ListHITLToRefreshParams{
		OwnerID: s.OwnerID, EverySeconds: o.RefreshEvery.Seconds(), MaxRequests: int32(o.RefreshBatch), //nolint:gosec // small bounded option
	})
	if err != nil {
		return
	}
	for _, r := range due {
		s.noteOutage(ctx, r)
	}
}

// refresh re-reads a live request's task.
func (s *Service) refresh(ctx context.Context, r dbq.HitlRequest) error {
	o := s.opts()
	rctx, cancel := context.WithTimeout(ctx, o.ReadTimeout)
	defer cancel()
	task, err := s.Kagent.GetTask(rctx, s.agentOf(r.AgentNamespace, r.AgentName), r.TaskID)
	var gone *kagent.TaskNotFound
	if errors.As(err, &gone) {
		return s.setAvailability(ctx, r, dbstate.HITLUnavailable, "kagent no longer has this task",
			dbstate.HITLPending, dbstate.HITLStale)
	}
	if err != nil {
		return err
	}
	if task.Status.State == a2a.TaskStateInputRequired {
		obs, rerr := readRequest(task.Status.Message)
		if rerr != nil {
			return s.setAvailability(ctx, r, dbstate.HITLUnavailable, "the task no longer shows a valid request",
				dbstate.HITLPending, dbstate.HITLStale)
		}
		sc := scopeOfRequest(r)
		if s.newRequestID(sc, r.TaskID, obs) == r.ID {
			return s.confirm(ctx, r)
		}
		return s.adopt(ctx, sc, task, obs) // a newer ask replaces this one
	}
	// The task is no longer waiting. If our response is what moved it, the request was answered;
	// otherwise the task moved on without us.
	if s.answeredIn(ctx, r, task) {
		return s.setAvailability(ctx, r, dbstate.HITLAnswered, "", dbstate.HITLPending, dbstate.HITLStale)
	}
	return s.setAvailability(ctx, r, dbstate.HITLUnavailable, "the task moved on",
		dbstate.HITLPending, dbstate.HITLStale)
}

// answeredIn reports whether a live response of the request is in the task's history.
func (s *Service) answeredIn(ctx context.Context, r dbq.HitlRequest, task *a2a.Task) bool {
	resp, err := dbq.New(s.Pool).GetLiveHITLResponse(ctx, r.ID)
	if err != nil {
		return false
	}
	return hasMessage(task, resp.OutboundMessageID)
}

// hasMessage reports whether the task's history shows a message with this id.
func hasMessage(t *a2a.Task, messageID string) bool {
	if t == nil {
		return false
	}
	return slices.ContainsFunc(t.History, func(m *a2a.Message) bool { return m != nil && m.ID == messageID })
}

func (s *Service) diagnose(ctx context.Context, runtimeSessionID, detail string) {
	q := dbq.New(s.Pool)
	if err := q.InsertHITLDiagnostic(ctx, dbq.InsertHITLDiagnosticParams{
		OwnerID: s.OwnerID, RuntimeSessionID: runtimeSessionID, Detail: clip(detail, 500),
		RepeatSeconds: s.opts().DiagnosticRepeat.Seconds(),
	}); err != nil {
		s.log().WarnContext(ctx, "record hitl diagnostic", "error", err)
		return
	}
	_ = q.PruneHITLDiagnostics(ctx, s.OwnerID)
}
