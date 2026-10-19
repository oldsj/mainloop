// Package mcpserver is Mainloop's MCP listener. Agents call it with a bearer token; it serves
// stateless Streamable HTTP with JSON responses on two paths: /mcp and /mcp/merge-approval.
//
// Every request is authenticated first. The server the MCP SDK then uses is built for that one
// request and carries only the tools the actor may use, so discovery and invocation read the same
// policy table and a tool that is not listed cannot be called.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/issues"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/prs"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/topics"
)

// Paths of the two MCP endpoints.
const (
	PathOrdinary = "/mcp"
	PathApproval = "/mcp/merge-approval"
)

// Server holds what the tools need. Build one with its fields set, then call [Server.Handler].
type Server struct {
	Auth    *agentauth.Authenticator
	Policy  agentauth.Policy
	Queries *dbq.Queries
	Topics  *topics.Service
	// Tasks serves the task tools. With none, those tools answer that tasks are unavailable.
	Tasks *tasks.Service
	// PRs and Merge serve the pull-request and merge tools. With none, those tools answer that
	// they are unavailable.
	PRs   *prs.Service
	Merge *merge.Service
	// Issues serves the read-only issue, Dependabot-alert and check-run tools, over the same
	// GitHub App as PRs and Merge. With none, those tools answer that they are unavailable.
	Issues *issues.Service
	// DevLease and DevOps serve the devloop tools. With none, those tools answer that they are
	// unavailable.
	DevLease *devloop.LeaseService
	DevOps   *devloop.OperationsService
	Log      *slog.Logger
	// AllowedHosts are the Host names the listener answers to; Dev also allows loopback.
	AllowedHosts map[string]bool
	Dev          bool
}

// tools lists every implemented tool, one registration function per domain. A slice that lands
// later adds its function here and nothing else changes.
func (s *Server) tools() map[string]Tool {
	all := map[string]Tool{}
	for _, domain := range [][]Tool{s.coordinationTools(), s.taskTools(), s.pullRequestTools(), s.issueTools(), s.devLoopTools()} {
		for _, t := range domain {
			all[t.Name] = t
		}
	}
	return all
}

// Handler is the whole listener: health, the two MCP paths, and the Host guard. It fails if the
// policy table names a tool the registry does not implement, so a divergence between the two
// stops the server at startup instead of panicking a later call.
func (s *Server) Handler() (http.Handler, error) {
	tools := s.tools()
	if err := checkToolsImplemented(tools, agentauth.AllTools); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})
	mux.Handle(PathOrdinary, s.endpoint(SurfaceOrdinary, tools))
	mux.Handle(PathApproval, s.endpoint(agentauth.SurfaceApproval, tools))
	return httpx.Chain(mux, httpx.Standard(s.Log, s.AllowedHosts, s.Dev, "/health")), nil
}

// checkToolsImplemented confirms that every name the policy table may hand out is registered.
// Policy.Tools never returns a name outside names, so checking names here covers every actor and
// surface at once.
func checkToolsImplemented(implemented map[string]Tool, names []string) error {
	var missing []string
	for _, name := range names {
		if _, ok := implemented[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("mcpserver: policy allows tool(s) %s, which the registry does not implement",
		strings.Join(missing, ", "))
}

// SurfaceOrdinary is the surface of /mcp.
const SurfaceOrdinary = agentauth.SurfaceOrdinary

// requestState is the authenticated actor of one request and the server built for it. The SDK
// asks for the server more than once per request; it is built once.
type requestState struct {
	actor   agentauth.Actor
	surface agentauth.Surface
	once    sync.Once
	server  *mcp.Server
}

type stateKey struct{}

func (s *Server) endpoint(surface agentauth.Surface, tools map[string]Tool) http.Handler {
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		st, _ := r.Context().Value(stateKey{}).(*requestState)
		if st == nil {
			return nil
		}
		st.once.Do(func() { st.server = s.buildServer(st, tools) })
		return st.server
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
		// The Host allowlist (httpx.HostGuard) already runs in front of this handler and is
		// stricter; the SDK check would also refuse a port-forward to the pod.
		DisableLocalhostProtection: true,
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Agents are not browsers and send no Origin; a request that carries one is a web page
		// calling in, which no origin is allowed to do.
		if r.Header.Get("Origin") != "" {
			httpx.WriteError(w, http.StatusForbidden, "Forbidden")
			return
		}
		token, err := agentauth.BearerToken(r.Header.Get("Authorization"))
		if err != nil {
			unauthorized(w)
			return
		}
		actor, err := s.Auth.Authenticate(r.Context(), token)
		if errors.Is(err, agentauth.ErrUnauthorized) {
			unauthorized(w)
			return
		}
		if err != nil {
			s.Log.ErrorContext(r.Context(), "mcp authentication failed", "error", err)
			httpx.WriteError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		if len(s.Policy.Tools(actor, surface)) == 0 {
			httpx.WriteError(w, http.StatusForbidden, "Forbidden")
			return
		}
		st := &requestState{actor: actor, surface: surface}
		sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mainloop"`)
	httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
}

// buildServer registers the tools the actor may use on the surface: the policy table's names.
// Handler already refused to start unless every one of them is implemented, so the lookup below
// cannot miss.
func (s *Server) buildServer(st *requestState, tools map[string]Tool) *mcp.Server {
	name := "mainloop"
	if st.surface == agentauth.SurfaceApproval {
		name = "mainloop-merge-approval"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)
	for _, tn := range s.Policy.Tools(st.actor, st.surface) {
		t := tools[tn]
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema},
			s.call(st.actor, t))
	}
	return srv
}

// call adapts a tool to the SDK. Failures the caller should see come back as error results, the
// way the Python server did; anything else is logged and reported without detail.
func (s *Server) call(actor agentauth.Actor, t Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Every transaction a tool opens re-checks the credential under a lock, so a revoke cannot
		// slip in between the authentication and the write.
		res, err := t.Handle(agentauth.Guarded(ctx, actor), actor, req.Params.Arguments)
		if err != nil {
			if errors.Is(err, agentauth.ErrUnauthorized) {
				err = failure("unauthorized", "the credential is no longer valid")
			}
			msg, ok := asToolError(err)
			if !ok {
				s.Log.ErrorContext(ctx, "mcp tool failed", "tool", t.Name, "session", actor.BindingID, "error", err)
				msg = "[500] internal error"
			} else {
				s.Log.InfoContext(ctx, "mcp tool refused", "tool", t.Name, "session", actor.BindingID, "result", msg)
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
		}
		structured := res.Structured
		if structured == nil {
			structured = map[string]any{}
		}
		structured["text"] = res.Text
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: res.Text}},
			StructuredContent: structured,
		}, nil
	}
}

// Implemented lists the tools this server registers, sorted by name. The contract in
// api/mcp-tools.json is generated from them.
func (s *Server) Implemented() []Tool {
	all := s.tools()
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	return out
}
