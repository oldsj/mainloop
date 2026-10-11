// Package kagent is the Go client for the kagent controller: SessionService and AgentService
// over gRPC-web, and the A2A JSON-RPC and SSE endpoint of each agent.
//
// It never replays an uncertain send. A send that may have reached kagent fails with
// *OutcomeUnknown, and the caller observes with GetTask or FindTaskForMessage.
package kagent

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/config"
	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Procedure names, exactly as the fork's service definitions spell them.
const (
	sessionService = "/kagent.api.v1alpha1.SessionService/"
	agentService   = "/kagent.api.v1alpha1.AgentService/"

	ProcCreateSession           = sessionService + "CreateSession"
	ProcPrepareSessionWorkspace = sessionService + "PrepareSessionWorkspace"
	ProcGetSession              = sessionService + "GetSession"
	ProcListSessions            = sessionService + "ListSessions"
	ProcSuspendSession          = sessionService + "SuspendSession"
	ProcResumeSession           = sessionService + "ResumeSession"
	ProcDeleteSession           = sessionService + "DeleteSession"
	ProcGetAgent                = agentService + "GetAgent"
)

const (
	defaultRequestTimeout    = 30 * time.Second
	defaultStreamIdleTimeout = 30 * time.Minute
	defaultSendRetryBudget   = 30 * time.Second
	sendBackoffCap           = 2 * time.Second
)

// Config is how to reach kagent. Zero durations take the defaults.
type Config struct {
	GatewayURL string
	// UserID is sent as x-user-id when ControlTokenFile is empty.
	UserID string
	// ControlTokenFile names a bearer token file, re-read when it changes.
	ControlTokenFile string
	// RequestTimeout bounds unary calls and the non-streaming A2A calls.
	RequestTimeout time.Duration
	// StreamIdleTimeout ends a stream that sends nothing for this long. It also bounds the wait
	// for a stream's first byte, which can come late because kagent sends headers with the first
	// event.
	StreamIdleTimeout time.Duration
	// SendRetryBudget bounds how long a refused send is retried.
	SendRetryBudget time.Duration

	// Transport is the base transport. It must be a *http.Transport: "never sent" is inferred from
	// net/http's connection tracing, which another RoundTripper may not provide. The default is a
	// clone of http.DefaultTransport.
	Transport *http.Transport
	// Sleep and Now replace the clock in tests.
	Sleep func(context.Context, time.Duration) error
	Now   func() time.Time
}

// FromSettings builds a Config from the process settings.
func FromSettings(k config.Kagent) Config {
	return Config{
		GatewayURL:        k.GatewayURL,
		UserID:            k.UserID,
		ControlTokenFile:  k.ControlTokenFile,
		RequestTimeout:    k.RequestTimeout.Duration(),
		StreamIdleTimeout: k.TurnTimeout.Duration(),
		SendRetryBudget:   k.SendRetryBudget.Duration(),
	}
}

// AgentRef names a kagent Agent.
type AgentRef struct{ Namespace, Name string }

func (a AgentRef) path() string { return "/agents/" + a.Namespace + "/" + a.Name }

func (a AgentRef) proto() *api.ResourceReference {
	return &api.ResourceReference{Namespace: a.Namespace, Name: a.Name}
}

// Client talks to one kagent gateway as one service identity. It is safe for concurrent use.
type Client struct {
	base       string
	http       *http.Client
	sessionCl  sessionClients
	agentGet   *connect.Client[api.GetAgentRequest, api.GetAgentResponse]
	reqTimeout time.Duration
	idle       time.Duration
	budget     time.Duration
	sleep      func(context.Context, time.Duration) error
	now        func() time.Time
}

// New builds a Client. It does not contact kagent.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(cfg.GatewayURL, "/")
	if base == "" {
		return nil, &ConfigError{Msg: "kagent gateway URL is empty"}
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, &ConfigError{Msg: "kagent gateway URL must be an absolute http or https URL"}
	}
	transport := cfg.Transport
	if transport == nil {
		if def, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = def.Clone()
		} else {
			transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
	}
	auth := &authTransport{base: transport, userID: cfg.UserID, host: u.Host}
	switch {
	case cfg.ControlTokenFile != "":
		auth.tokens = &tokenSource{path: cfg.ControlTokenFile}
	case cfg.UserID == "":
		return nil, &ConfigError{Msg: "kagent needs a user id or a control token file"}
	}
	c := &Client{
		base: base,
		http: &http.Client{
			Transport: auth,
			// Never follow a redirect: the gateway does not redirect, and a hop would resend the
			// message (307 and 308) and could carry credentials elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		reqTimeout: orDefault(cfg.RequestTimeout, defaultRequestTimeout),
		idle:       orDefault(cfg.StreamIdleTimeout, defaultStreamIdleTimeout),
		budget:     orDefault(cfg.SendRetryBudget, defaultSendRetryBudget),
		sleep:      cfg.Sleep,
		now:        cfg.Now,
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.sessionCl = newSessionClients(doer{c.http}, base)
	c.agentGet = connect.NewClient[api.GetAgentRequest, api.GetAgentResponse](
		doer{c.http}, base+ProcGetAgent, connect.WithGRPCWeb())
	return c, nil
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// unary runs one unary call under the request timeout and classifies its failure.
func unary[Req, Res any](
	ctx context.Context, c *Client, op string, cl *connect.Client[Req, Res], req *Req,
) (*Res, error) {
	ctx, cancel := context.WithTimeout(ctx, c.reqTimeout)
	defer cancel()
	resp, err := cl.CallUnary(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, classifyConnect(op, err)
	}
	if resp.Msg == nil {
		return nil, &OutcomeUnknown{Op: op, Detail: "empty reply", Err: errors.New("empty reply")}
	}
	return resp.Msg, nil
}

// GetAgent reads one Agent.
func (c *Client) GetAgent(ctx context.Context, ref AgentRef) (*api.Agent, error) {
	if ref.Namespace == "" || ref.Name == "" {
		return nil, emptyArg("AgentService GetAgent", "agent namespace and name")
	}
	resp, err := unary(ctx, c, "AgentService GetAgent", c.agentGet, &api.GetAgentRequest{Ref: ref.proto()})
	if err != nil {
		return nil, err
	}
	got := resp.GetAgent().GetRef()
	if got.GetNamespace() != ref.Namespace || got.GetName() != ref.Name {
		// A read has no side effect, so a wrong answer is a definite failure, not an unknown outcome.
		return nil, &SessionError{
			Op: "AgentService GetAgent", Code: connect.CodeUnknown, Detail: "reply is for a different agent",
		}
	}
	return resp.GetAgent(), nil
}

// paginate reads every page of a listing. A server that hands back a page token it already
// gave would loop forever, and stopping quietly would hide rows, so a repeat is an error.
func paginate[T any](op string, fetch func(token string) ([]T, string, error)) ([]T, error) {
	var all []T
	token := ""
	seen := map[string]bool{}
	for {
		page, next, err := fetch(token)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, &OutcomeUnknown{Op: op, Detail: "page token repeated"}
		}
		seen[next] = true
		token = next
	}
}
