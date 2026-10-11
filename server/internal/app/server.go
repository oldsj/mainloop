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

// Server implements the strict server interface by embedding one *XAPI per domain over the 501
// fallback. A slice adds its field here, builds it in New, and deletes nothing.
type Server struct {
	*HealthAPI

	fallback
}

var _ api.StrictServerInterface = (*Server)(nil)

// Deps are what the domains need. Slices add fields as they need them.
type Deps struct {
	Config *config.Config
	Log    *slog.Logger
	DB     Pinger
}

// New builds the Server.
func New(d Deps) *Server {
	return &Server{
		HealthAPI: &HealthAPI{DB: d.DB},
	}
}

// Handler returns the owner API: the strict handlers behind request validation, then the Host
// guard, Origin guard, CORS, panic recovery, request id and access log.
func (s *Server) Handler(d Deps) (http.Handler, error) {
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
		requestValidator(spec),
	), nil
}
