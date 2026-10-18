package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/progress"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Stalled turns (issue #265). A delivered turn whose task has said nothing for a long time may be
// wedged inside kagent (a harness that lost its task state, a tool that never returns) or may just
// be working quietly. The delivery loop records when it last saw a live event of the turn's task and
// what that event left it doing (activity, below), and a sweep marks a turn stalled once it has been
// quiet longer than its allowance: Opts.StallAfter, or the longer Opts.StallToolAfter while a tool
// call is running, since a web search or a long build emits nothing until it ends. A task parked for
// the owner is never stalled. The next event clears the stall. Stalled is an observation shown to
// the owner, with stop and restart; it changes no delivery state, and nothing is replayed.

// activity is what the delivery loop has seen a turn's task do, as far as stalls go.
type activity struct {
	// open holds the tool calls that started and have no result yet.
	open map[string]bool
	// kind and stalled are as last stored, and at is the activity time last stored (or the time the
	// turn was marked delivered, before any).
	kind    dbstate.ActivityKind
	stalled bool
	at      time.Time
	// written is when this handler last wrote the activity time.
	written time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// noteActivity folds one task event into the turn's activity and stores it when it matters: what
// the turn is doing changed, the turn was stalled (a live event ends that at once), or the stored
// activity time is older than Opts.ActivityWriteEvery. live is false for a task snapshot (the first
// event of a subscription, or a read), which shows what the task is doing but is no sign of new
// activity unless its status was recorded after the activity already stored (the task moved while
// no stream was open). A failed write is logged and dropped: the stall view is derived, and must
// never hold a turn up.
func (t *turn) noteActivity(ctx context.Context, ev a2a.Event, live bool) {
	a := &t.act
	if a.open == nil {
		a.open = map[string]bool{}
	}
	parked := false
	switch e := ev.(type) {
	case *a2a.Task:
		clear(a.open)
		for _, art := range e.Artifacts {
			if art != nil {
				a.toolParts(art.Parts)
			}
		}
		parked = waits(e.Status.State)
	case *a2a.TaskStatusUpdateEvent:
		parked = waits(e.Status.State)
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact != nil {
			a.toolParts(e.Artifact.Parts)
		}
	default:
		return
	}
	kind := dbstate.ActivityEvent
	switch {
	case parked:
		kind = dbstate.ActivityParked
	case len(a.open) > 0:
		kind = dbstate.ActivityTool
	}
	now := t.s.now()
	var at *time.Time
	switch task, ok := ev.(*a2a.Task); {
	case live:
		at = &now
	case ok && task.Status.Timestamp != nil && task.Status.Timestamp.After(a.at):
		ts := *task.Status.Timestamp
		if ts.After(now) {
			ts = now
		}
		at = &ts
	}
	due := at != nil && (a.written.IsZero() || a.stalled || now.Sub(a.written) >= t.s.opts().ActivityWriteEvery)
	if kind == a.kind && !due {
		return
	}
	live = at != nil
	id, owner, attempts := t.fence()
	cleared := false
	err := events.InTxPublish(ctx, t.s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		row, err := dbq.New(tx).NoteDeliveryActivity(ctx, dbq.NoteDeliveryActivityParams{
			ID: id, LeaseOwner: owner, Attempts: attempts, Kind: string(kind), At: at,
		})
		if err != nil {
			return nil, err
		}
		if !row.WasStalled || !live {
			return nil, nil
		}
		cleared = true
		// The page re-reads the session, so the stall notice goes as soon as the turn moves again.
		return []events.Draft{endedEvent(t.d.UserID, t.d.SessionID, t.c.ID)}, nil
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return // the claim moved on, or the turn is no longer running
	case err != nil:
		if ctx.Err() == nil {
			t.s.log().WarnContext(ctx, "record turn activity failed", "message_id", t.c.ID, "error", err)
		}
		return
	}
	a.kind = kind
	if live {
		a.written, a.stalled, a.at = now, false, *at
	}
	if cleared {
		t.s.log().InfoContext(ctx, "stalled turn is active again", "message_id", t.c.ID, "session_id", t.d.SessionID)
	}
}

func (a *activity) toolParts(parts a2a.ContentParts) {
	for _, p := range parts {
		id, starts, ok := progress.ToolPart(p)
		if !ok || id == "" {
			continue
		}
		if starts {
			a.open[id] = true
		} else {
			delete(a.open, id)
		}
	}
}

// waits reports a task state that waits for someone else: quiet, but not stalled.
func waits(st a2a.TaskState) bool {
	switch normState(st) {
	case "input_required", "auth_required":
		return true
	}
	return false
}

// StallSweep marks delivered turns that have been quiet past their allowance.
func (s *Service) StallSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "mark-stalled-turns", Every: s.opts().StallEvery, Run: s.MarkStalled}
}

// MarkStalled runs one pass of [Service.StallSweep]: it marks the turns and publishes
// session:message for each, so an open page shows the stall. It writes nothing else and calls
// nothing outside the database.
func (s *Service) MarkStalled(ctx context.Context) error {
	o := s.opts()
	var marked []dbq.MarkStalledTurnsRow
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		rows, err := dbq.New(tx).MarkStalledTurns(ctx, dbq.MarkStalledTurnsParams{
			Now: s.now(), Seconds: o.StallAfter.Seconds(), ToolSeconds: o.StallToolAfter.Seconds(),
		})
		if err != nil {
			return nil, fmt.Errorf("mark stalled turns: %w", err)
		}
		marked = rows
		drafts := make([]events.Draft, 0, len(rows))
		for _, r := range rows {
			drafts = append(drafts, endedEvent(r.UserID, r.SessionID, r.ID))
		}
		return drafts, nil
	})
	if err != nil {
		return err
	}
	for _, r := range marked {
		s.log().WarnContext(ctx, "turn stalled: no task activity past its allowance; stop or restart it",
			"message_id", r.ID, "session_id", r.SessionID)
	}
	return nil
}

// turnStall is the wire form of a session's stalled turn.
func turnStall(sessionID string, main bool, st dbq.GetStalledTurnRow) *api.TurnStall {
	last := st.UpdatedAt
	if st.ActivityAt != nil {
		last = *st.ActivityAt
	}
	out := &api.TurnStall{
		MessageId: st.ID, SessionId: sessionID, TaskId: st.TaskID, MainThread: main, LastActivityAt: last,
		ToolRunning: st.ActivityKind != nil && dbstate.ActivityKind(*st.ActivityKind) == dbstate.ActivityTool,
	}
	if st.StalledAt != nil {
		out.StalledAt = *st.StalledAt
	}
	return out
}
