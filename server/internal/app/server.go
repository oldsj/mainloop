// Package app wires the HTTP API: one field and one constructor line per domain.
package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/buildinfo"
	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/devices"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/privileged"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/schedules"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/transcripts"
	"github.com/oldsj/mainloop/server/internal/workspaces"
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
	*projects.ProjectsAPI
	*providers.ProvidersAPI
	*environments.EnvironmentsAPI
	*events.EventsAPI
	*workspaces.WorkspacesAPI
	*sessions.SessionsAPI
	*tasks.TasksAPI
	*hitl.API
	*attention.AttentionAPI
	*health.DataAPI
	*schedules.SchedulesAPI
	*merge.MergeAPI
	*devloop.DevloopAPI
	*dotfiles.DotfilesAPI
	*transcripts.TranscriptsAPI
	*preview.GrantAPI
	*privileged.PrivilegedAPI
	*devices.DevicesAPI

	fallback

	deps Deps
}

var _ api.StrictServerInterface = (*Server)(nil)

// Deps are what the domains need. Slices add fields as they need them.
type Deps struct {
	Config *config.Config
	Log    *slog.Logger
	DB     Pinger
	// Pool and Hub are nil in processes and tests that do not serve events; GET /events then
	// answers 503.
	Pool *pgxpool.Pool
	Hub  *events.Hub
	// Live, when set, brings a stream that joins mid-turn up to date with the running turns.
	Live events.LiveSource
	// Registry reads image metadata for environments. Nil means the anonymous OCI client.
	Registry environments.RegistryClient

	// Everything below is built by the caller with the services' own constructors, once per process.
	// Providers, Projects, Sessions, HITL, Attention, Health, Schedules and Transcripts are required; New does not
	// build a stand-in for a missing one.

	// Providers is the provider registry behind the provider routes and task profiles.
	Providers *providers.Registry
	// Projects imports and serves projects.
	Projects *projects.Service
	// Tasks serves tasks. Nil answers the task routes with 503 (a process without a database).
	Tasks *tasks.Service
	// Workspaces runs the workspace lifecycle. Nil answers the workspace routes with 503.
	Workspaces *workspaces.Service
	// Sessions is the main thread service, shared with the delivery loop.
	Sessions *sessions.Service
	// Preview routes preview hosts to workspace dev servers. Nil serves no previews, and a
	// preview host is then refused by the Host guard like any unknown host.
	Preview *preview.Proxy
	// Live relays GET /sessions/{id}/live to the session's SDK runner. Nil serves no live sessions.
	LiveRelay *preview.Live
	// HITL observes kagent for tasks that wait for the owner and carries the answers back.
	HITL *hitl.Service
	// Attention is the owner's inbox.
	Attention *attention.Service
	// Health serves orchestration health and the smoke ledger.
	Health *health.Service
	// Schedules serves scheduled runs; its routes answer 503 when it has no kagent client.
	Schedules *schedules.Service
	// Transcripts serves stored session transcripts and their search.
	Transcripts *transcripts.Service
	// Merge serves the owner's read of a merge proposal's details. Nil answers that route with 503.
	Merge *merge.Service
	// Devloop serves the owner's view of the mainloop-dev lease. Nil answers those routes with 501
	// (the fallback), since devloop's own operator flag already covers "not configured".
	Devloop *devloop.LeaseService
	// DevloopTargets serves a project's staging target (GET/PUT /projects/{id}/devloop-target).
	// Nil answers those routes 404, the same as an unconfigured project.
	DevloopTargets *devloop.TargetService
	// Auth verifies who calls the owner API. Nil serves it unauthenticated, which only tests and
	// development do; serve-api always builds one (auth.New refuses a missing mode outside dev).
	Auth *auth.Authenticator
	// CI serves a project's verification setting (GET/PUT /projects/{id}/verification). Nil
	// without a GitHub App.
	CI *ci.Service
}

// missing names the required services Deps lacks. A nil one would otherwise fail as a nil dereference
// on the first request that reaches it.
func (d Deps) missing() error {
	var names []string
	for _, r := range []struct {
		name   string
		absent bool
	}{
		{"Providers", d.Providers == nil}, {"Projects", d.Projects == nil}, {"Sessions", d.Sessions == nil},
		{"HITL", d.HITL == nil}, {"Attention", d.Attention == nil}, {"Health", d.Health == nil}, {"Schedules", d.Schedules == nil},
		{"Transcripts", d.Transcripts == nil},
	} {
		if r.absent {
			names = append(names, r.name)
		}
	}
	if len(names) > 0 {
		return fmt.Errorf("app: Deps lacks required services: %s", strings.Join(names, ", "))
	}
	return nil
}

// maxBodyBytes caps request bodies. The validator buffers the whole body before any handler runs,
// so the cap has to sit in this chain. Sized for POST /chat/images: a base64 image up to
// sessions.MaxImageBytes (5MiB raw, Anthropic's own per-image limit) inflates by about a third,
// plus room for the JSON envelope.
const maxBodyBytes = MaxBodyBytes

// MaxBodyBytes is the request body cap, exported for the device verifier, which hashes the body
// before the cap's middleware runs.
const MaxBodyBytes = 8 << 20

// New builds the Server from services the caller has already built.
func New(d Deps) *Server {
	devicesAPI := &devices.DevicesAPI{Pool: d.Pool, OwnerID: d.Config.Identity.OwnerID}
	if d.Auth != nil && d.Auth.Devices != nil {
		devicesAPI.OnRevoke = d.Auth.Devices.Revoked
	}
	if d.Attention != nil {
		// A registration's Needs-you item: Revoke revokes the device.
		d.Attention.OnAnswer(devices.RegistrationItemPrefix, devicesAPI.AnswerRegistration)
	}
	return &Server{
		HealthAPI:       &HealthAPI{DB: d.DB, Log: d.Log, Revision: buildinfo.Revision},
		ProjectsAPI:     &projects.ProjectsAPI{Svc: d.Projects, Devloop: d.DevloopTargets, CI: d.CI},
		ProvidersAPI:    providers.NewAPI(d.Config, d.Pool, d.Providers),
		EnvironmentsAPI: environments.NewAPI(d.Config, d.Pool, d.Registry),
		EventsAPI:       events.NewAPI(d.Pool, d.Hub, d.Log, d.Config.Identity.OwnerID, events.WithLive(d.Live)),
		WorkspacesAPI:   &workspaces.WorkspacesAPI{Svc: d.Workspaces},
		SessionsAPI:     sessions.NewAPI(d.Sessions),
		TasksAPI:        &tasks.TasksAPI{Svc: d.Tasks, OwnerID: d.Config.Identity.OwnerID},
		API:             hitl.NewAPI(d.HITL),
		AttentionAPI:    attention.NewAPI(d.Attention),
		DataAPI:         health.NewAPI(d.Health),
		SchedulesAPI:    schedules.NewAPI(d.Schedules),
		MergeAPI:        &merge.MergeAPI{Svc: d.Merge, OwnerID: d.Config.Identity.OwnerID},
		DevloopAPI:      devloop.NewAPI(d.Devloop),
		DotfilesAPI:     dotfiles.NewAPI(d.Pool, d.Config.Identity.OwnerID),
		TranscriptsAPI:  transcripts.NewAPI(d.Transcripts),
		GrantAPI:        &preview.GrantAPI{Proxy: d.Preview, AuthOff: d.Auth == nil || d.Auth.Mode == auth.ModeOff},
		PrivilegedAPI:   &privileged.PrivilegedAPI{Pool: d.Pool, OwnerID: d.Config.Identity.OwnerID},
		DevicesAPI:      devicesAPI,
		deps:            d,
	}
}

// Handler returns the owner API: the strict handlers behind request validation, the body cap,
// authentication, response compression, then the Host guard, Origin guard and CORS, with preview
// hosts routed ahead of them, all inside panic recovery, request id and access log.
func (s *Server) Handler() (http.Handler, error) {
	d := s.deps
	if err := d.missing(); err != nil {
		return nil, err
	}
	spec, err := api.GetSpec()
	if err != nil {
		return nil, err
	}
	cfg := d.Config
	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: requestDecodeError(d.Log),
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var he *apierr.Error
			if !errors.As(err, &he) {
				d.Log.ErrorContext(r.Context(), "handler error", "request_id", httpx.RequestID(r.Context()), "error", err)
			}
			apierr.Write(w, err)
		},
	})
	routes := api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		Middlewares: []api.MiddlewareFunc{},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
			httpx.WriteError(w, http.StatusBadRequest, "Invalid request")
		},
	})
	// Authentication sits inside CORS, so a refusal still carries the CORS headers the browser needs
	// to read it, and ahead of the body cap and validator, so an unauthenticated caller learns
	// nothing from a 422. It covers the live relay and the SSE streams, which are routes like any other.
	authn, err := d.Auth.Middleware(spec)
	if err != nil {
		return nil, err
	}
	inner := routes
	if d.LiveRelay != nil {
		inner = d.LiveRelay.Route(routes)
	}
	ownerAPI := httpx.Chain(inner,
		httpx.HostGuard(cfg.AllowedAPIHosts(), cfg.IsDev(), "/health"),
		httpx.OriginGuard(cfg.FrontendOrigin(), cfg.IsDev()),
		httpx.CORS(cfg.FrontendOrigin(), cfg.IsDev()),
		// Inside CORS, so a compressed answer carries the same CORS headers, and around
		// everything that writes JSON, refusals included.
		httpx.Compress,
		authn,
		httpx.MaxBody(maxBodyBytes),
		requestValidator(spec),
	)
	// Preview hosts are routed before the API's Host guard, which would refuse them.
	routed := ownerAPI
	if d.Preview != nil {
		routed = d.Preview.Route(ownerAPI)
	}
	return httpx.Chain(routed, httpx.Observed(d.Log)), nil
}

// requestDecodeError is the strict handler's body-decode failure callback: a body past the cap
// (even wrapped by the JSON decoder) is 413, anything else is a 400 without the decoder's detail.
func requestDecodeError(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		if httpx.IsBodyTooLarge(err) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "Invalid request body")
		log.Debug("request decode failed", "error", err)
	}
}
