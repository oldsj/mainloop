package kagent

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

const maxPageSize = 100

// CreateSession creates a Session. Calling again with the same RequestID returns the same
// Session. The reply must echo the create contract, or the outcome is unknown.
func (c *Client) CreateSession(ctx context.Context, req *api.CreateSessionRequest) (*api.Session, error) {
	const op = "SessionService CreateSession"
	resp, err := unary(ctx, c, op, c.sessionCl.CreateSession, req)
	if err != nil {
		return nil, err
	}
	s := resp.GetSession()
	if s.GetId() == "" || s.GetContextId() != s.GetId() || !echoesCreate(s, req) {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply differs from the create contract"}
	}
	return s, nil
}

// PrepareSessionWorkspace asks kagent to prepare the Session's workspace.
func (c *Client) PrepareSessionWorkspace(
	ctx context.Context, req *api.PrepareSessionWorkspaceRequest,
) (*api.WorkspacePreparationReceipt, error) {
	const op = "SessionService PrepareSessionWorkspace"
	resp, err := unary(ctx, c, op, c.sessionCl.PrepareSessionWorkspace, req)
	if err != nil {
		return nil, err
	}
	// A repeat call after confirmation opens a new challenge and closes task admission, so the
	// receipt must be proven to belong to this action.
	r := resp.GetReceipt()
	if r == nil || r.GetContextId() != req.GetSessionId() || !proto.Equal(r.GetOriginal(), req) {
		return nil, &OutcomeUnknown{Op: op, Detail: "receipt does not match the request"}
	}
	return r, nil
}

// echoesCreate reports whether a created Session carries the contract it was created with. It
// compares the fields this client knows, so a newer kagent that adds a field does not break it.
func echoesCreate(s *api.Session, req *api.CreateSessionRequest) bool {
	a, ra := AgentOf(s), req.GetAgent()
	w, rw := s.GetWorkspace(), req.GetWorkspace()
	d, rd := s.GetDevelopmentEnvironment(), req.GetDevelopmentEnvironment()
	return a.GetNamespace() == ra.GetNamespace() && a.GetName() == ra.GetName() &&
		w.GetRepo() == rw.GetRepo() && w.GetRef() == rw.GetRef() &&
		w.GetBranch() == rw.GetBranch() && w.GetDepth() == rw.GetDepth() &&
		d.GetImage() == rd.GetImage() && d.GetPlatform() == rd.GetPlatform() &&
		d.GetPolicyIdentity() == rd.GetPolicyIdentity()
}

// sessionOf checks the reply is for the Session that was asked about.
func sessionOf(op, id string, s *api.Session) (*api.Session, error) {
	if s.GetId() != id || s.GetContextId() != id {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is for a different session"}
	}
	return s, nil
}

// GetSession reads one Session.
func (c *Client) GetSession(ctx context.Context, id string) (*api.Session, error) {
	const op = "SessionService GetSession"
	if id == "" {
		return nil, emptyArg(op, "session id")
	}
	resp, err := unary(ctx, c, op, c.sessionCl.GetSession, &api.GetSessionRequest{SessionId: id})
	if err != nil {
		return nil, err
	}
	return sessionOf(op, id, resp.GetSession())
}

// ObserveSession reads one Session for display only. It differs from GetSession in one way: a reply
// that states no context id is accepted, with the context left empty, so a caller can show the
// Session as having an unconfirmed context. The reply must still be for this Session, and a context
// id that names anything else is refused. Nothing that acts on a Session (delivery, lifecycle,
// readiness) uses it; those keep GetSession's requirement that the context is confirmed.
func (c *Client) ObserveSession(ctx context.Context, id string) (*api.Session, error) {
	const op = "SessionService GetSession"
	if id == "" {
		return nil, emptyArg(op, "session id")
	}
	resp, err := unary(ctx, c, op, c.sessionCl.GetSession, &api.GetSessionRequest{SessionId: id})
	if err != nil {
		return nil, err
	}
	s := resp.GetSession()
	if s.GetId() != id || (s.GetContextId() != "" && s.GetContextId() != id) {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is for a different session"}
	}
	return s, nil
}

// SessionLiveness is what a liveness sweep needs from a kagent Session: whether kagent still has a
// record of it, whether its runtime can no longer run again, whether that terminal state is still
// on its way out rather than settled, and when kagent last recorded it changing. The zero value is
// a session kagent has no record of.
type SessionLiveness struct {
	Exists   bool
	Terminal bool
	// Deleting is set only when Terminal is set for RUNTIME_STATE_DELETING: the session is on its
	// way out but not yet gone, unlike RUNTIME_STATE_FAILED or RUNTIME_STATE_DELETED, which are
	// settled. A caller deciding whether anything in the session could still be running (
	// [tasks.KagentStops.Stopped]) must not treat Deleting the same as the rest of Terminal.
	Deleting bool
	// Asleep is set when kagent verified no running Actor for a Session that is not Terminal: idle
	// settlement parked it, so nothing in it runs until the next send or attach wakes it. A sleeping
	// Session stays READY; see [Awake].
	Asleep       bool
	LastActivity time.Time
	// Progress is a fingerprint of the session's running turn, set only by internal/tasks.KagentLiveness
	// (never by [Client.ObserveLiveness]): it changes whenever kagent records anything new for a
	// submitted or working task, and is empty when none is running.
	Progress string
}

// terminalRuntimeStates are the Session states nothing can wake: failed outright, or on the way
// out and gone.
func terminalRuntimeStates(s api.RuntimeState) bool {
	switch s {
	case api.RuntimeState_RUNTIME_STATE_FAILED, api.RuntimeState_RUNTIME_STATE_DELETING, api.RuntimeState_RUNTIME_STATE_DELETED:
		return true
	}
	return false
}

// ObserveLiveness reads a Session for liveness only, the same way ObserveSession reads one for
// display: a session kagent has no record of at all is not an error, it is simply not Exists.
// LastActivity is kagent's own updated_at, which only moves on a lifecycle operation (create,
// rename, delete), never on a turn: it is not evidence that a long working turn
// is stalled, and a caller that needs turn-level activity (internal/tasks.KagentLiveness) computes
// its own and ignores this field. The zero Go time, not the 1970 epoch a nil proto timestamp's
// AsTime() would give, means kagent sent no updated_at at all.
func (c *Client) ObserveLiveness(ctx context.Context, id string) (SessionLiveness, error) {
	s, err := c.ObserveSession(ctx, id)
	if err != nil {
		var se *SessionError
		if errors.As(err, &se) && se.Code == connect.CodeNotFound {
			return SessionLiveness{}, nil
		}
		return SessionLiveness{}, err
	}
	var updated time.Time
	if ts := s.GetUpdatedAt(); ts != nil {
		updated = ts.AsTime()
	}
	state := s.GetState()
	return SessionLiveness{
		Exists: true, Terminal: terminalRuntimeStates(state),
		Deleting: state == api.RuntimeState_RUNTIME_STATE_DELETING,
		Asleep:   !terminalRuntimeStates(state) && Asleep(s), LastActivity: updated,
	}, nil
}

// DeleteSession deletes a Session. The reply is the Session as it was being deleted. kagent refuses
// it while a turn is running; see [Client.ForceDeleteSession].
func (c *Client) DeleteSession(ctx context.Context, id string) (*api.Session, error) {
	return c.deleteSession(ctx, id, false)
}

// ForceDeleteSession deletes a Session and terminates its running turns instead of being refused
// while they are active. It is for the paths that end a session on purpose (restart, stop).
func (c *Client) ForceDeleteSession(ctx context.Context, id string) (*api.Session, error) {
	return c.deleteSession(ctx, id, true)
}

func (c *Client) deleteSession(ctx context.Context, id string, force bool) (*api.Session, error) {
	const op = "SessionService DeleteSession"
	resp, err := unary(ctx, c, op, c.sessionCl.DeleteSession, &api.DeleteSessionRequest{SessionId: id, Force: force})
	if err != nil {
		return nil, err
	}
	return sessionOf(op, id, resp.GetSession())
}

// ListSessionsPage reads one page. An empty next token means the last page.
func (c *Client) ListSessionsPage(
	ctx context.Context, token string, limit int,
) (sessions []*api.Session, next string, err error) {
	const op = "SessionService ListSessions"
	limit = min(max(limit, 1), maxPageSize)
	resp, err := unary(ctx, c, op, c.sessionCl.ListSessions, &api.ListSessionsRequest{
		Page: &api.PageRequest{Limit: int32(limit), PageToken: token}, //nolint:gosec // limit is 1..100
	})
	if err != nil {
		return nil, "", err
	}
	return resp.GetSessions(), resp.GetPage().GetNextPageToken(), nil
}

// ListSessions reads every Session the identity can see.
func (c *Client) ListSessions(ctx context.Context) ([]*api.Session, error) {
	return paginate("SessionService ListSessions", func(token string) ([]*api.Session, string, error) {
		return c.ListSessionsPage(ctx, token, maxPageSize)
	})
}

// Awake reports whether kagent verified a running Actor for the Session on this read. kagent
// suspends an idle Session's Actor itself and the Session stays READY, so the runtime state does
// not say whether it sleeps: the runtime association does, and only on GetSession. False means
// asleep or not verified; the next send, workspace preparation or attach through the router wakes it.
func Awake(s *api.Session) bool {
	return s.GetRuntimeAssociation().GetCurrentActive()
}

// Asleep reports whether kagent said, on this read, that the Session has no running Actor. Only a
// runtime association can say so: kagent reports one whenever the Session has an Actor, current
// only while the Actor runs. Without one (no Actor verified on this read) sleep is unknown, which
// is not asleep: a caller must not take a Session it cannot see into for a stopped one.
func Asleep(s *api.Session) bool {
	ra := s.GetRuntimeAssociation()
	return ra != nil && !ra.GetCurrentActive()
}

// actorHostSuffix ends the A2A authority kagent records for a Session's Actor:
// "<actor>.<atespace>.actors.resources.substrate.ate.dev".
const actorHostSuffix = ".actors.resources.substrate.ate.dev"

// ActorOf is the Substrate atespace and name of the Session's Actor, from the A2A authority kagent
// records when it creates the Actor and clears when it deletes it. ok is false when the Session
// has none or it is not in that form.
func ActorOf(s *api.Session) (atespace, name string, ok bool) {
	host, found := strings.CutSuffix(s.GetA2AAuthority(), actorHostSuffix)
	if !found {
		return "", "", false
	}
	name, atespace, found = strings.Cut(host, ".")
	if !found || name == "" || atespace == "" || strings.Contains(atespace, ".") {
		return "", "", false
	}
	return atespace, name, true
}

// Settling reports whether kagent is doing internal lifecycle work on the Session (a brief
// SUSPENDED state or a suspend/resume/revert operation). There is no call that asks for these any
// more; a caller that sees one waits and reads again.
func Settling(s *api.Session) bool {
	if s.GetState() == api.RuntimeState_RUNTIME_STATE_SUSPENDED {
		return true
	}
	switch s.GetOperation() {
	case api.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, api.RuntimeOperation_RUNTIME_OPERATION_RESUME,
		api.RuntimeOperation_RUNTIME_OPERATION_REVERT:
		return true
	}
	return false
}

// AgentOf is the Agent a Session runs on. kagent reports it as provenance (the reference and the
// Agent's uid, captured from the pinned revision); only the reference is compared or stored.
func AgentOf(s *api.Session) *api.ResourceReference {
	return s.GetAgent().GetRef()
}

// ActorName is kagent's stable private Actor name for a Session, the same on every wake.
func ActorName(sessionID string) string { return "session-" + strings.ToLower(sessionID) }

// kagent derives runtime_association.generation_id from the Session's current Actor (the Session id
// and the Actor's uid); there is no separate durable generation record. Mainloop never interprets
// the value: it only echoes it, with actor_uid, into PrepareSessionWorkspace.
//
// StaleRuntime reports whether a PrepareSessionWorkspace error means the runtime the request named
// is not ready for it: the generation or Actor is no longer current, or the runtime is asleep
// (kagent wakes it and refuses). The caller reads the Session again and retries with the new
// association. kagent answers FailedPrecondition "Current prepared runtime identity is
// unavailable" for all of these, and also for a few definite refusals it does not tell apart on
// the wire ("Preparation does not match the Session"); those end at the preparation window.
func StaleRuntime(err error) bool {
	var se *SessionError
	return errors.As(err, &se) && (se.Code == connect.CodeFailedPrecondition || se.Code == connect.CodeAborted)
}
