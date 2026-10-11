package kagent

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

const maxPageSize = 100

type sessionClients struct {
	create  *connect.Client[api.CreateSessionRequest, api.CreateSessionResponse]
	prepare *connect.Client[api.PrepareSessionWorkspaceRequest, api.PrepareSessionWorkspaceResponse]
	get     *connect.Client[api.GetSessionRequest, api.GetSessionResponse]
	list    *connect.Client[api.ListSessionsRequest, api.ListSessionsResponse]
	suspend *connect.Client[api.SuspendSessionRequest, api.SuspendSessionResponse]
	resume  *connect.Client[api.ResumeSessionRequest, api.ResumeSessionResponse]
	del     *connect.Client[api.DeleteSessionRequest, api.DeleteSessionResponse]
}

func newSessionClients(h connect.HTTPClient, base string) sessionClients {
	opt := connect.WithGRPCWeb()
	return sessionClients{
		create: connect.NewClient[api.CreateSessionRequest, api.CreateSessionResponse](
			h, base+ProcCreateSession, opt),
		prepare: connect.NewClient[api.PrepareSessionWorkspaceRequest, api.PrepareSessionWorkspaceResponse](
			h, base+ProcPrepareSessionWorkspace, opt),
		get: connect.NewClient[api.GetSessionRequest, api.GetSessionResponse](
			h, base+ProcGetSession, opt),
		list: connect.NewClient[api.ListSessionsRequest, api.ListSessionsResponse](
			h, base+ProcListSessions, opt),
		suspend: connect.NewClient[api.SuspendSessionRequest, api.SuspendSessionResponse](
			h, base+ProcSuspendSession, opt),
		resume: connect.NewClient[api.ResumeSessionRequest, api.ResumeSessionResponse](
			h, base+ProcResumeSession, opt),
		del: connect.NewClient[api.DeleteSessionRequest, api.DeleteSessionResponse](
			h, base+ProcDeleteSession, opt),
	}
}

// CreateSession creates a Session. Calling again with the same RequestID returns the same
// Session. The reply must echo the create contract, or the outcome is unknown.
func (c *Client) CreateSession(ctx context.Context, req *api.CreateSessionRequest) (*api.Session, error) {
	const op = "SessionService CreateSession"
	resp, err := unary(ctx, c, op, c.sessionCl.create, req)
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
	resp, err := unary(ctx, c, op, c.sessionCl.prepare, req)
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
	a, ra := s.GetAgent(), req.GetAgent()
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
	resp, err := unary(ctx, c, op, c.sessionCl.get, &api.GetSessionRequest{SessionId: id})
	if err != nil {
		return nil, err
	}
	return sessionOf(op, id, resp.GetSession())
}

// SuspendSession suspends a Session's runtime.
func (c *Client) SuspendSession(ctx context.Context, id string) (*api.Session, error) {
	const op = "SessionService SuspendSession"
	resp, err := unary(ctx, c, op, c.sessionCl.suspend, &api.SuspendSessionRequest{SessionId: id})
	if err != nil {
		return nil, err
	}
	return sessionOf(op, id, resp.GetSession())
}

// ResumeSession resumes a suspended Session.
func (c *Client) ResumeSession(ctx context.Context, id string) (*api.Session, error) {
	const op = "SessionService ResumeSession"
	resp, err := unary(ctx, c, op, c.sessionCl.resume, &api.ResumeSessionRequest{SessionId: id})
	if err != nil {
		return nil, err
	}
	return sessionOf(op, id, resp.GetSession())
}

// DeleteSession deletes a Session. The reply is the Session as it was being deleted.
func (c *Client) DeleteSession(ctx context.Context, id string) (*api.Session, error) {
	const op = "SessionService DeleteSession"
	resp, err := unary(ctx, c, op, c.sessionCl.del, &api.DeleteSessionRequest{SessionId: id})
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
	resp, err := unary(ctx, c, op, c.sessionCl.list, &api.ListSessionsRequest{
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
