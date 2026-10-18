// Package attention is the owner's inbox: what needs them now (the Needs-you snapshot), the
// quieter Activity log, and the answers and dismissals that go back.
//
// Everything that needs the owner is one attention_items row, keyed by (source_kind, source_id):
//
//   - hitl_request: written by the hitl package when kagent shows a task that waits for input.
//   - task_decision: written by [Service.Sweep] from tasks.decision_*. The tasks package does not
//     write attention rows, so the sweep keeps them honest: it opens one for each blocked task that
//     asks for the owner's decision and resolves it when the decision clears, the attempt is
//     replaced or the task finishes.
//   - task_reconciliation: one item per blocked task or unconfirmed runtime stop, including stops
//     still held by final tasks or older attempts. Confirmation clears the stop's need.
//   - merge_proposal: one item per proposal awaiting approval, linked to its canonical HITL ask.
//   - turn_stalled: one item per stalled turn (no task activity past its allowance) of the main
//     thread or a standalone session, opened and resolved by [Service.SyncTasks] from the sessions
//     ledger's stalled_at. A task-owned session is left to the task liveness sweep.
//   - queue: written by [Open], for questions other slices put to the owner. The owner's answer is
//     stored on the row (detail.response) for that slice to read; nothing is delivered from here.
//
// needs_owner: a session needs the owner when it has a visible attention item. Visibility is one
// rule, the attention_visible view of migration 00070. Reconciliation needs survive session and
// task finality; ordinary items require a live session/current attempt and no live HITL answer.
// The snapshot, its count, the unread count and [Service.NeedsOwner] all read it, so they cannot
// disagree.
//
// Every change that moves the unread count publishes inbox:updated in its own transaction, and the
// sweep publishes it for the moves that have no write of their own, comparing with the count the
// newest inbox:updated carried.
package attention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// The attention sources.
const (
	SourceHITL           = "hitl_request"
	SourceTaskDecision   = "task_decision"
	SourceReconciliation = "task_reconciliation"
	SourceMerge          = "merge_proposal"
	SourceStalledTurn    = "turn_stalled"
	SourceQueue          = "queue"
)

// noticePrefix marks the queue_item_id of an input notice (a hitl_diagnostics row).
const noticePrefix = "diag_"

// maxResponseBytes caps an owner's answer to a queue item.
const maxResponseBytes = 16 << 10

// historyLimit caps Activity.
const historyLimit = 100

// Errors the API maps to the contract's statuses.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// Service reads and changes the owner's attention.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
	Log     *slog.Logger
	// SweepEvery is the period of the task-decision sweep. Zero means 3 seconds.
	SweepEvery time.Duration
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Sweep is the task-decision sweep as a reconcile sweep.
func (s *Service) Sweep() reconcile.Sweep {
	every := s.SweepEvery
	if every <= 0 {
		every = 3 * time.Second
	}
	return reconcile.Sweep{Name: "attention", Every: every, Run: s.SyncTasks}
}

// SyncTasks opens items for owner decisions, reconciliation needs and merge proposals, and resolves the
// items whose need has ended: a decision that cleared, a question of a finished task, an item of a
// finished or archived session, an item whose task has finished or whose attempt was replaced,
// archived or deleted. It publishes one inbox:updated when it changed an item, or when the unread
// count differs from the one the owner's newest inbox:updated carried, since a session, task or
// response that ends hides or shows items without any write here. That event is read from the
// events table in the same transaction, never remembered by the process, so a restart, a second
// replica or a duplicate pass publishes neither too little nor too much. Two sweeps that race in
// the same instant can each publish once.
func (s *Service) SyncTasks(ctx context.Context) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		var changed []string
		for _, step := range []func(context.Context, string) ([]string, error){
			q.OpenTaskDecisionAttention, q.ResolveStaleTaskDecisions, q.ResolveQueueForFinishedTasks,
			q.OpenTaskReconciliationAttention, q.ResolveTaskReconciliationAttention,
			q.OpenMergeProposalAttention, q.ResolveMergeProposalAttention,
			q.OpenStalledTurnAttention, q.ResolveStalledTurnAttention,
			q.ResolveFinishedSessionAttention, q.ResolveAttentionHiddenForGood,
		} {
			ids, err := step(ctx, s.OwnerID)
			if err != nil {
				return nil, err
			}
			changed = append(changed, ids...)
		}
		n, err := q.CountUnreadAttention(ctx, s.OwnerID)
		if err != nil {
			return nil, err
		}
		if len(changed) == 0 {
			last, err := q.LastPublishedUnread(ctx, s.OwnerID)
			if err != nil {
				return nil, err
			}
			if last == n {
				return nil, nil
			}
		}
		item := ""
		if len(changed) > 0 {
			item = changed[0]
		}
		return []events.Draft{inboxEvent(s.OwnerID, item, int(n))}, nil
	})
}

// NewQueueItem is a question put to the owner by another slice.
type NewQueueItem struct {
	// ID is the queue item's id; opening the same id again changes nothing.
	ID       string
	ItemType api.QueueItemType
	Priority api.QueueItemPriority
	Title    string
	// Content is the full question; Needed is the one line the inbox shows (Content when empty).
	Content string
	Needed  string
	Context map[string]any
	Options []string
	// SessionID and TaskID say what the question is about; both are optional.
	SessionID *string
	TaskID    *string
}

type queueDetail struct {
	ItemType    api.QueueItemType     `json:"item_type"`
	Priority    api.QueueItemPriority `json:"priority"`
	Content     string                `json:"content"`
	Context     map[string]any        `json:"context,omitempty"`
	Options     []string              `json:"options,omitempty"`
	Response    *string               `json:"response,omitempty"`
	RespondedAt *time.Time            `json:"responded_at,omitempty"`
	Metadata    map[string]any        `json:"metadata,omitempty"`
}

// Open puts a question to the owner in tx, and returns the inbox event to publish with it
// (events.InTxPublish does that). The question is a no-op when it is already open or was answered.
func Open(ctx context.Context, tx pgx.Tx, ownerID string, in NewQueueItem) ([]events.Draft, error) {
	if in.ID == "" || in.Title == "" {
		return nil, fmt.Errorf("%w: a queue item needs an id and a title", ErrInvalid)
	}
	if strings.HasPrefix(in.ID, noticePrefix) {
		return nil, fmt.Errorf("%w: a queue item id cannot start with %q", ErrInvalid, noticePrefix)
	}
	if in.ItemType == "" {
		in.ItemType = api.QueueItemTypeQuestion
	}
	if in.Priority == "" {
		in.Priority = api.Normal
	}
	needed := in.Needed
	if needed == "" {
		needed = in.Content
	}
	kind := dbstate.AttentionQuestion
	if in.ItemType == api.QueueItemTypeApproval {
		kind = dbstate.AttentionApproval
	}
	href := "/"
	if in.TaskID != nil {
		href = "/tasks/" + *in.TaskID
	} else if in.SessionID != nil {
		href = "/sessions/" + *in.SessionID
	}
	detail, err := json.Marshal(queueDetail{
		ItemType: in.ItemType, Priority: in.Priority, Content: in.Content, Context: in.Context, Options: in.Options,
	})
	if err != nil {
		return nil, err
	}
	q := dbq.New(tx)
	n, err := q.InsertQueueAttention(ctx, dbq.InsertQueueAttentionParams{
		ID: "attn_queue_" + in.ID, OwnerID: ownerID, Kind: kind, SourceID: in.ID, SessionID: in.SessionID,
		TaskID: in.TaskID, Title: in.Title, Needed: needed, Href: href, Detail: detail,
	})
	if err != nil || n == 0 {
		return nil, err
	}
	return inboxDraft(ctx, q, ownerID, "attn_queue_"+in.ID)
}

func inboxDraft(ctx context.Context, q *dbq.Queries, ownerID, item string) ([]events.Draft, error) {
	n, err := q.CountUnreadAttention(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	return []events.Draft{inboxEvent(ownerID, item, int(n))}, nil
}

func inboxEvent(ownerID, item string, count int) events.Draft {
	ev := api.EventInboxUpdated{UnreadCount: &count}
	if item != "" {
		ev.ItemId = &item
	}
	return events.Draft{UserID: ownerID, Type: events.TypeInboxUpdated, Payload: ev}
}

// NeedsOwner reports whether the session has an open item the owner can see. It is the
// projects.Service hook; a failed read answers false and is logged.
func (s *Service) NeedsOwner(ctx context.Context, sessionID string) bool {
	if s.Pool == nil {
		return false
	}
	ok, err := dbq.New(s.Pool).SessionNeedsOwner(ctx, dbq.SessionNeedsOwnerParams{OwnerID: s.OwnerID, SessionID: &sessionID})
	if err != nil {
		s.log().WarnContext(ctx, "attention: needs_owner read failed", "session_id", sessionID, "error", err)
		return false
	}
	return ok
}

// Snapshot is the Needs-you view: the open items, their count, and the owner's session list. The
// sessions are every unfinished session plus the newest finished ones (ListSessionRows), each with
// its own needs_owner; a session reads waiting_on_user only while a native input request waits on it.
// Items, sessions and the sessions needing the owner come from one read-only transaction, so they
// agree. Every session behind a visible item is unarchived and unfinished, so the session list
// already holds it, apart from the owner's main thread, which is not a session of the list.
func (s *Service) Snapshot(ctx context.Context) (api.AttentionSnapshot, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return api.AttentionSnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := dbq.New(tx)
	rows, err := q.ListOpenAttention(ctx, s.OwnerID)
	if err != nil {
		return api.AttentionSnapshot{}, err
	}
	needing, err := q.ListSessionsNeedingOwner(ctx, s.OwnerID)
	if err != nil {
		return api.AttentionSnapshot{}, err
	}
	listed, err := q.ListSessionRows(ctx, dbq.ListSessionRowsParams{UserID: s.OwnerID})
	if err != nil {
		return api.AttentionSnapshot{}, err
	}
	snap := api.AttentionSnapshot{Items: []api.AttentionItem{}, Sessions: make([]api.Session, 0, len(listed))}
	for _, r := range rows {
		if item, ok := s.item(r); ok {
			snap.Items = append(snap.Items, item)
		}
	}
	snap.Count = len(snap.Items)
	waiting := make(map[string]bool, len(needing)) // session id -> behind a native input request
	for _, n := range needing {
		waiting[n.SessionID] = n.WaitingOnInput
	}
	for _, r := range listed {
		onInput, needs := waiting[r.ID]
		snap.Sessions = append(snap.Sessions, sessionOf(dbq.GetSessionRowRow(r), needs, needs && onInput))
	}
	return snap, nil
}

// item is the wire shape of an attention row. A row of a source this build does not know is
// skipped, not guessed at.
func (s *Service) item(r dbq.AttentionItem) (api.AttentionItem, bool) {
	out := api.AttentionItem{
		Kind: api.AttentionItemKind(r.Kind), Title: r.Title, Needed: r.Needed, Href: r.Href,
		SessionIds: []string{},
	}
	if r.SessionID != nil {
		out.SessionIds = append(out.SessionIds, *r.SessionID)
	}
	if r.ReasonCode != nil {
		reason := api.AttentionItemReasonCode(*r.ReasonCode)
		out.ReasonCode = &reason
	}
	switch r.SourceKind {
	case SourceHITL:
		// The attention row's task_id is kagent's task, not a Mainloop task, so it is not exposed.
		out.Id = "hitl:" + r.SourceID
		out.QueueItem = s.hitlQueueItem(r)
	case SourceTaskDecision:
		if r.TaskID == nil {
			return out, false
		}
		out.Id, out.TaskId = "task:"+*r.TaskID, r.TaskID
	case SourceReconciliation:
		out.Id, out.TaskId = "reconciliation:"+r.SourceID, r.TaskID
	case SourceStalledTurn:
		out.Id = "stalled:" + r.SourceID
	case SourceMerge:
		var detail struct {
			RequestID string `json:"hitl_request_id"`
		}
		if json.Unmarshal(r.Detail, &detail) != nil || detail.RequestID == "" {
			return out, false
		}
		out.Id = "merge:" + r.SourceID
		item := r
		item.SourceID = detail.RequestID
		out.QueueItem = s.hitlQueueItem(item)
	case SourceQueue:
		out.Id, out.TaskId = "queue:"+r.SourceID, r.TaskID
		qi, ok := s.queueItem(r)
		if !ok {
			return out, false
		}
		out.QueueItem = qi
	default:
		return out, false
	}
	return out, true
}

// mainThreadID is the QueueItem.main_thread_id: the owner's inbox has no separate main-thread
// row, so the id names the owner's one thread.
func (s *Service) mainThreadID() string { return "main-" + s.OwnerID }

func (s *Service) hitlQueueItem(r dbq.AttentionItem) *api.QueueItem {
	reqID := r.SourceID
	ctx := map[string]any{}
	if r.SessionID != nil {
		ctx["session_id"] = *r.SessionID
	}
	item := s.baseQueueItem(r)
	item.Id, item.HitlRequestId, item.ItemType = reqID, &reqID, api.QueueItemTypeHitlRequest
	item.Content, item.Context = r.Needed, ctx
	return &item
}

func (s *Service) queueItem(r dbq.AttentionItem) (*api.QueueItem, bool) {
	var d queueDetail
	if err := json.Unmarshal(r.Detail, &d); err != nil {
		s.log().Warn("attention: unreadable queue item", "id", r.ID, "error", err)
		return nil, false
	}
	item := s.baseQueueItem(r)
	item.Id, item.TaskId, item.ItemType, item.Priority = r.SourceID, r.TaskID, d.ItemType, d.Priority
	item.Content, item.Context = d.Content, d.Context
	if item.Context == nil {
		item.Context = map[string]any{}
	}
	if len(d.Options) > 0 {
		item.Options = &d.Options
	}
	item.Response, item.RespondedAt = d.Response, d.RespondedAt
	if r.State != dbstate.AttentionOpen {
		item.Status = api.QueueItemStatusCancelled
		if d.Response != nil {
			item.Status = api.QueueItemStatusResponded
		}
	}
	return &item, true
}

func (s *Service) baseQueueItem(r dbq.AttentionItem) api.QueueItem {
	return api.QueueItem{
		MainThreadId: s.mainThreadID(), UserId: r.OwnerID, Priority: api.Normal, Title: r.Title,
		Status: api.QueueItemStatusPending, ReadAt: r.ReadAt, CreatedAt: r.CreatedAt, Context: map[string]any{},
	}
}

// sessionOf is the wire shape of a session row. needsOwner is the session's needs_owner; waitingOnUser
// is whether a native input request waits on it, which makes an unfinished session read
// waiting_on_user.
func sessionOf(r dbq.GetSessionRowRow, needsOwner, waitingOnUser bool) api.Session {
	sess := api.Session{
		AgentKind: r.AgentKind, ArchivedAt: r.ArchivedAt, BaseBranch: r.BaseBranch, BranchName: r.BranchName,
		Color: r.Color, CommitSha: r.CommitSha, CompletedAt: r.CompletedAt, CreatedAt: r.CreatedAt,
		Description: r.Description, Error: r.Error, Id: r.ID, Model: r.Model, NeedsOwner: needsOwner,
		ParentSessionId: r.ParentSessionID, ProjectId: r.ProjectID, Prompt: r.Prompt, RepoUrl: r.RepoURL,
		StartedAt: r.StartedAt, Status: api.SessionStatus(r.Status), Summary: r.Summary, Title: r.Title,
		Topic: r.Topic, UserId: r.UserID,
	}
	if len(r.Result) > 0 {
		var m map[string]any
		if json.Unmarshal(r.Result, &m) == nil && m != nil {
			sess.Result = &m
		}
	}
	if waitingOnUser {
		switch r.Status {
		case dbstate.SessionCompleted, dbstate.SessionFailed, dbstate.SessionCancelled, dbstate.SessionSuperseded:
		default:
			sess.Status = api.SessionStatusWaitingOnUser
		}
	}
	return sess
}

// Activity is the log: provider reports, answered questions and input notices, newest first. Each
// source is capped in SQL, so the read never loads more than the history needs.
func (s *Service) Activity(ctx context.Context) ([]api.ActivityItem, error) {
	q := dbq.New(s.Pool)
	reports, err := q.ListActivityReports(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	answered, err := q.ListActivityQueue(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	notices, err := q.ListActivityNotices(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	out := make([]api.ActivityItem, 0, len(reports)+len(answered)+len(notices))
	for _, r := range reports {
		href := "/tasks/" + r.TaskID
		out = append(out, api.ActivityItem{
			Id: "report:" + r.ID, Title: r.Title, Href: &href, CreatedAt: r.CreatedAt,
			Content: fmt.Sprintf("Provider report (%s; unverified): %s", r.Outcome, r.Summary),
		})
	}
	for _, r := range answered {
		var d queueDetail
		_ = json.Unmarshal(r.Detail, &d)
		content := "Closed: " + r.Needed
		if d.Response != nil {
			content = r.Needed + "\nYou answered: " + *d.Response
		}
		href, id := r.Href, r.SourceID
		out = append(out, api.ActivityItem{
			Id: "queue:" + r.SourceID, Title: r.Title, Content: content, Href: &href, CreatedAt: r.HappenedAt,
			QueueItemId: &id,
		})
	}
	for _, n := range notices {
		id := fmt.Sprintf("%s%d", noticePrefix, n.ID)
		out = append(out, api.ActivityItem{
			Id: "queue:" + id, Title: "Agent input notice", Content: n.Detail, CreatedAt: n.ObservedAt,
			QueueItemId: &id, Dismissible: true,
		})
	}
	sortNewestFirst(out)
	if len(out) > historyLimit {
		out = out[:historyLimit]
	}
	return out, nil
}

func sortNewestFirst(items []api.ActivityItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && newer(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func newer(a, b api.ActivityItem) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.Id > b.Id
}

// Respond records the owner's answer to a queue item. It is fenced by the item's state: the first
// answer resolves the item, the same answer again reads back as done without a second event, and a
// different answer is a conflict. A native request is answered on its own route; a notice takes no
// answer.
func (s *Service) Respond(ctx context.Context, itemID string, body api.QueueItemResponse) error {
	if strings.TrimSpace(body.Response) == "" {
		return fmt.Errorf("%w: the response is required", ErrInvalid)
	}
	if len(body.Response) > maxResponseBytes {
		return fmt.Errorf("%w: the response is too long", ErrInvalid)
	}
	meta := []byte("{}")
	if body.Metadata != nil {
		b, err := json.Marshal(*body.Metadata)
		if err != nil {
			return fmt.Errorf("%w: metadata is not valid JSON", ErrInvalid)
		}
		meta = b
	}
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		id, err := q.RespondQueueAttention(ctx, dbq.RespondQueueAttentionParams{
			Response: body.Response, Metadata: meta, OwnerID: s.OwnerID, SourceID: itemID,
		})
		if err == nil {
			return inboxDraft(ctx, q, s.OwnerID, id)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, s.whyNotAnswered(ctx, q, itemID, body.Response)
	})
}

// whyNotAnswered explains an answer that matched no open queue item: nil when the item already
// holds this very answer (a repeat), otherwise the reason it cannot take one.
func (s *Service) whyNotAnswered(ctx context.Context, q *dbq.Queries, itemID, response string) error {
	row, err := q.GetAttentionBySource(ctx, dbq.GetAttentionBySourceParams{OwnerID: s.OwnerID, SourceKind: SourceQueue, SourceID: itemID})
	switch {
	case err == nil:
		var d queueDetail
		_ = json.Unmarshal(row.Detail, &d)
		if d.Response != nil && *d.Response == response {
			return nil
		}
		if d.Response != nil {
			return fmt.Errorf("%w: this item was already answered", ErrConflict)
		}
		return fmt.Errorf("%w: this item is no longer waiting for an answer", ErrConflict)
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if _, err := q.GetAttentionBySource(ctx, dbq.GetAttentionBySourceParams{OwnerID: s.OwnerID, SourceKind: SourceHITL, SourceID: itemID}); err == nil {
		return fmt.Errorf("%w: use the structured HITL response route", ErrConflict)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, ok := noticeID(itemID); ok {
		return fmt.Errorf("%w: an input notice takes no response", ErrConflict)
	}
	return ErrNotFound
}

func noticeID(itemID string) (int64, bool) {
	rest, ok := strings.CutPrefix(itemID, noticePrefix)
	if !ok || rest == "" {
		return 0, false
	}
	var n int64
	for _, c := range rest {
		if c < '0' || c > '9' || n > (1<<62)/10 {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// Dismiss puts an input notice away. Dismissing it again is not an error and publishes nothing. A
// native request stays in attention until its task moves on.
func (s *Service) Dismiss(ctx context.Context, itemID string) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		id, ok := noticeID(itemID)
		if !ok {
			return nil, s.whyNotDismissed(ctx, q, itemID)
		}
		if _, err := q.DismissNotice(ctx, dbq.DismissNoticeParams{OwnerID: s.OwnerID, ID: id}); err == nil {
			return inboxDraft(ctx, q, s.OwnerID, itemID)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if _, err := q.GetNotice(ctx, dbq.GetNoticeParams{OwnerID: s.OwnerID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
		return nil, nil // already dismissed
	})
}

func (s *Service) whyNotDismissed(ctx context.Context, q *dbq.Queries, itemID string) error {
	for _, kind := range []string{SourceHITL, SourceQueue} {
		_, err := q.GetAttentionBySource(ctx, dbq.GetAttentionBySourceParams{OwnerID: s.OwnerID, SourceKind: kind, SourceID: itemID})
		if err == nil {
			return fmt.Errorf("%w: native requests remain in task attention", ErrConflict)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return ErrNotFound
}

// NewService builds the service over the database. Its task-decision sweep runs only where the
// caller registers it.
func NewService(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, OwnerID: cfg.Identity.OwnerID, Log: log}
}
