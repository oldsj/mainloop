package sessions

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/preview"
)

// IsLiveAgent reports whether the named Agent runs the Agent SDK runner, whose client port
// GET /sessions/{id}/live relays.
func (s *Service) IsLiveAgent(name string) bool {
	return config.Sessions{LiveAgents: s.opts().LiveAgents}.IsLiveAgent(name)
}

// LiveTarget is the kagent Session behind the owner's session when its Agent runs the SDK runner.
// preview.ErrLiveNotFound covers another owner's session, one without a kagent Session, and one whose Agent
// is not live.
func (s *Service) LiveTarget(ctx context.Context, sessionID string) (string, error) {
	b, err := dbq.New(s.Pool).GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", preview.ErrLiveNotFound
	}
	if err != nil {
		return "", err
	}
	if b.KagentSessionID == nil || *b.KagentSessionID == "" {
		return "", preview.ErrLiveNotFound
	}
	name := ""
	if b.Role == dbstate.RoleMain {
		name = s.Agent.Name
	}
	if ref, err := s.bindingAgentOf(b); err == nil {
		name = ref.Name
	}
	if !s.IsLiveAgent(name) {
		return "", preview.ErrLiveNotFound
	}
	return *b.KagentSessionID, nil
}
