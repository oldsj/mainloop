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
// the kagent Session (see liveactor.Of).
// liveactor.ErrNotFound covers another owner's session, one without a kagent Session, and one
// whose Agent is not live. liveactor.ErrWaking is a Session whose actor cannot be named yet; the
// client retries.
func (s *Service) LiveTarget(ctx context.Context, sessionID string) (liveactor.Target, error) {
	ls, err := s.liveSession(ctx, sessionID)
	if err != nil {
		return liveactor.Target{}, err
	}
	ks, err := s.Kagent.ObserveSession(ctx, ls.kagentSessionID)
	if err != nil {
		return liveactor.Target{}, err
	}
	// A sleeping Session is not resumed here: kagent has no resume call, and the relay's connection
	// through the Substrate router is what wakes its Actor.
	actor, ok := liveactor.Of(ks, s.ActorAtespace)
	if !ok {
		return liveactor.Target{}, liveactor.ErrWaking
	}
	return liveactor.Target{KagentSessionID: ls.kagentSessionID, Actor: actor}, nil
}

// liveSession is the owner's live session.
type liveSession struct {
	kagentSessionID string
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
	return liveSession{kagentSessionID: *b.KagentSessionID}, nil
}
