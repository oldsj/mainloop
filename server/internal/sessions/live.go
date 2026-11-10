package sessions

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/liveactor"
)

// IsLiveAgent reports whether the named Agent runs the Agent SDK runner, whose client port
// GET /sessions/{id}/live relays.
func (s *Service) IsLiveAgent(name string) bool {
	return config.Sessions{LiveAgents: s.opts().LiveAgents}.IsLiveAgent(name)
}

// LiveTarget is the actor behind the owner's session when its Agent runs the SDK runner, read from
// the kagent Session's runtime association (the actor name changes with each runtime generation).
// liveactor.ErrNotFound covers another owner's session, one without a kagent Session, and one
// whose Agent is not live. liveactor.ErrWaking is a Session whose actor did not come back yet;
// the client retries.
func (s *Service) LiveTarget(ctx context.Context, sessionID string) (liveactor.Target, error) {
	ls, err := s.liveSession(ctx, sessionID)
	if err != nil {
		return liveactor.Target{}, err
	}
	t, ok, err := s.liveActor(ctx, ls.kagentSessionID)
	if err != nil || ok {
		return t, err
	}
	// No running actor. A workspace's Session is only ever resumed through the workspace service,
	// never directly; a standalone native or main session is resumed like a send resumes it.
	// ResumeSession wakes the current actor of a READY Session that idle quiescence suspended, and
	// resumes an explicitly suspended one. Then read it again.
	if err := s.wakeLive(ctx, ls); err != nil {
		s.Log.WarnContext(ctx, "live wake failed", "session", sessionID, "kagent_session", ls.kagentSessionID, "error", err)
		return liveactor.Target{}, liveactor.ErrWaking
	}
	t, ok, err = s.liveActor(ctx, ls.kagentSessionID)
	if err != nil || ok {
		return t, err
	}
	return liveactor.Target{}, liveactor.ErrWaking
}

func (s *Service) wakeLive(ctx context.Context, ls liveSession) error {
	if ls.workspaceID == "" {
		_, err := s.Kagent.ResumeSession(ctx, ls.kagentSessionID)
		return err
	}
	if s.Waker == nil {
		return errors.New("no workspace waker")
	}
	return s.Waker.Wake(ctx, ls.workspaceID)
}

// liveActor reads the kagent Session's running actor; ok is false while it has none.
func (s *Service) liveActor(ctx context.Context, kid string) (liveactor.Target, bool, error) {
	ks, err := s.Kagent.ObserveSession(ctx, kid)
	if err != nil {
		return liveactor.Target{}, false, err
	}
	actor, ok := liveactor.Of(ks)
	if !ok {
		return liveactor.Target{}, false, nil
	}
	return liveactor.Target{KagentSessionID: kid, Actor: actor}, true, nil
}

// liveSession is the owner's live session: its kagent Session and, for a workspace session, the
// workspace (whose id is the session's).
type liveSession struct {
	kagentSessionID string
	workspaceID     string
}

func (s *Service) liveSession(ctx context.Context, sessionID string) (liveSession, error) {
	b, err := dbq.New(s.Pool).GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return liveSession{}, liveactor.ErrNotFound
	}
	if err != nil {
		return liveSession{}, err
	}
	if b.KagentSessionID == nil || *b.KagentSessionID == "" {
		return liveSession{}, liveactor.ErrNotFound
	}
	name := ""
	if b.Role == dbstate.RoleMain {
		name = s.Agent.Name
	}
	if ref, err := s.bindingAgentOf(b); err == nil {
		name = ref.Name
	}
	if !s.IsLiveAgent(name) {
		return liveSession{}, liveactor.ErrNotFound
	}
	ls := liveSession{kagentSessionID: *b.KagentSessionID}
	// workspaces.agent_name is NOT NULL, so the join matched exactly when the session has a workspace.
	if b.WorkspaceAgentName != nil {
		ls.workspaceID = b.SessionID
	}
	return ls, nil
}
