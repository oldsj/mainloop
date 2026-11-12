package tasks

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// MaxSteerChars bounds a steer message.
const MaxSteerChars = 8000

// SteerInput is a steer call: a message for a managed task's running turn.
type SteerInput struct {
	TaskID, RequestID, Text string
}

// SteerResult says what became of a steer.
type SteerResult struct {
	TaskID, AttemptID, MessageID string
	// How is "steered" (recorded to join the running turn at its next tool boundary), "queued"
	// (waiting behind the open turn, because the turn could not take a steer) or "sent" (the
	// session had no open turn, so the message starts one).
	How string
}

// Steer sends a message into the running turn of a task the principal manages: main any task,
// a supervisor its own children. It goes through the same send as the owner's composer with steer
// asked for; where the turn cannot take a steer (none running, waiting on input, an Agent without
// steers) the sessions ledger records an ordinary message instead, and the result says which. The
// message id derives from the principal and request id, so a retry finds the first send and never
// sends twice.
func (s *Service) Steer(ctx context.Context, p Principal, in SteerInput) (SteerResult, error) {
	if !p.canManage() || p.Role == "" {
		return SteerResult{}, refusal(403, ReasonManagementDenied)
	}
	// The authority is read in a transaction so a revoked credential is refused before any send.
	var t dbq.Task
	var a dbq.TaskAttempt
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := ensureActive(ctx, q, p); err != nil {
			return err
		}
		var err error
		t, err = q.GetTaskByID(ctx, in.TaskID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && t.OwnerID != p.OwnerID {
			return refusal(404, ReasonTaskNotFound)
		}
		if err != nil {
			return err
		}
		read, manage := p.access(t)
		if !read {
			return refusal(404, ReasonTaskNotFound)
		}
		if !manage {
			return refusal(403, ReasonManagementDenied)
		}
		if TaskFinal(t.Status) {
			return refusal(409, ReasonTaskTerminal)
		}
		if t.CurrentAttemptID == nil {
			return refusal(409, ReasonTaskNotRunning)
		}
		if a, err = q.GetTaskAttempt(ctx, *t.CurrentAttemptID); err != nil {
			return err
		}
		if a.State != dbstate.AttemptActive || a.SessionID == nil {
			return refusal(409, ReasonTaskNotRunning)
		}
		return nil
	})
	if err != nil {
		return SteerResult{}, err
	}
	if s.Sessions == nil {
		return SteerResult{}, errors.New("tasks: no session runtime")
	}
	mid := uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "mainloop:steer:%s:%s", p.Key(), in.RequestID)).String()
	from := "main"
	if p.Role == dbstate.RoleSupervisor {
		from = "supervisor task " + p.TaskID
	}
	text := fmt.Sprintf("[message from %s]\n%s", from, in.Text)
	sent, err := s.Sessions.SendToWith(ctx, sessions.SourceMainloop, *a.SessionID, mid, text, sessions.SendOptions{Steer: true})
	switch {
	case errors.Is(err, sessions.ErrConflict), errors.Is(err, sessions.ErrNotFound):
		return SteerResult{}, refusal(409, ReasonTaskNotRunning)
	case err != nil:
		return SteerResult{}, err
	}
	out := SteerResult{TaskID: t.ID, AttemptID: a.ID, MessageID: mid, How: "sent"}
	switch {
	case sent.Steered:
		out.How = "steered"
	case sent.Queued:
		out.How = "queued"
	}
	return out, nil
}
