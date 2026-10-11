// Package app wires the HTTP API: one field and one constructor line per domain.
package app

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// fallback is nested one struct deeper than the domain APIs, so a method a domain defines is
// found at a shallower depth and wins over Unimplemented's 501.
type fallback struct {
	api.Unimplemented
}

// Server implements the strict server interface by embedding one pointer per domain over the 501
// fallback. A slice adds its field here, builds it in New, and deletes nothing.
//
// An embedded field is named after its bare type name, so two domains cannot both export `API`.
// Give each domain type a unique name (`sessions.SessionsAPI`), or declare an alias here
// (`type SessionsAPI = sessions.API`).
type Server struct {
	*HealthAPI

	fallback

	deps Deps
}

var _ api.StrictServerInterface = (*Server)(nil)

// Deps are what the domains need. Slices add fields as they need them.
type Deps struct {
	Config *config.Config
	Log    *slog.Logger
	DB     Pinger
}

// maxBodyBytes caps request bodies. The validator buffers the whole body before any handler runs,
// so the cap has to sit in this chain.
const maxBodyBytes = 4 << 20

// New builds the Server.
func New(d Deps) *Server {
	return &Server{
		HealthAPI: &HealthAPI{DB: d.DB, Log: d.Log},
		deps:      d,
	}
}

// Handler returns the owner API: the strict handlers behind request validation, the body cap, then the
// Host guard, Origin guard, CORS, panic recovery, request id and access log.
func (s *Server) Handler() (http.Handler, error) {
	d := s.deps
	spec, err := api.GetSpec()
	if err != nil {
		return nil, err
	}
	cfg := d.Config
	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// Request bodies are validated before this runs, so this is a decode failure.
			httpx.WriteError(w, http.StatusBadRequest, "Invalid request body")
			d.Log.Debug("request decode failed", "error", err)
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var he *httpx.Error
			if !errors.As(err, &he) {
				d.Log.ErrorContext(r.Context(), "handler error", "request_id", httpx.RequestID(r.Context()), "error", err)
			}
			httpx.WriteErr(w, err)
		},
	})
	routes := api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		Middlewares: []api.MiddlewareFunc{},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
			httpx.WriteError(w, http.StatusBadRequest, "Invalid request")
		},
	})
	return httpx.Chain(routes,
		httpx.AccessLog(d.Log),
		httpx.RequestIDMiddleware,
		httpx.Recover(d.Log),
		httpx.HostGuard(cfg.AllowedAPIHosts(), cfg.IsDev(), "/health"),
		httpx.OriginGuard(cfg.FrontendOrigin(), cfg.IsDev()),
		httpx.CORS(cfg.FrontendOrigin(), cfg.IsDev()),
		httpx.MaxBody(maxBodyBytes),
		requestValidator(spec),
	), nil
}
