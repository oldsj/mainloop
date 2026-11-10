package sessions

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/preview"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// IsLiveAgent reports whether the named Agent runs the Agent SDK runner, whose client port
// GET /sessions/{id}/live relays.
func (s *Service) IsLiveAgent(name string) bool {
	return config.Sessions{LiveAgents: s.opts().LiveAgents}.IsLiveAgent(name)
}

// LiveTarget is the actor behind the owner's session when its Agent runs the SDK runner, read from
// the kagent Session's runtime association (the actor name changes with each runtime generation).
// preview.ErrLiveNotFound covers another owner's session, one without a kagent Session, and one
// whose Agent is not live. preview.ErrLiveWaking is a Session with no actor yet; a suspended one
// is asked to resume, so the client's retry finds it.
func (s *Service) LiveTarget(ctx context.Context, sessionID string) (preview.LiveTarget, error) {
	kid, err := s.liveSession(ctx, sessionID)
	if err != nil {
		return preview.LiveTarget{}, err
	}
	ks, err := s.Kagent.ObserveSession(ctx, kid)
	if err != nil {
		return preview.LiveTarget{}, err
	}
	ra := ks.GetRuntimeAssociation()
	if ra.GetAtespace() == "" || ra.GetActorName() == "" {
		if ks.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
			if _, err := s.Kagent.ResumeSession(ctx, kid); err != nil {
				s.Log.WarnContext(ctx, "live resume failed", "kagent_session", kid, "error", err)
			}
		}
		return preview.LiveTarget{}, preview.ErrLiveWaking
	}
	return preview.LiveTarget{KagentSessionID: kid, Actor: ra.GetAtespace() + "/" + ra.GetActorName()}, nil
}

func (s *Service) liveSession(ctx context.Context, sessionID string) (string, error) {
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
