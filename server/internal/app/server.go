// Package app wires the HTTP API: one field and one constructor line per domain.
package app

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/schedules"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
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

	fallback

	deps Deps
	// initErr is a domain that failed to build from configuration; Handler reports it.
	initErr error
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
	// Registry reads image metadata for environments. Nil means the anonymous OCI client.
	Registry environments.RegistryClient
	// GitHub reads GitHub for projects. Nil means no GitHub App is configured.
	GitHub projects.Remote
	// Workspaces runs the workspace lifecycle. Nil answers the workspace routes with 503.
	Workspaces *workspaces.Service
	// Sessions is the main thread service, shared with the delivery loop. Nil builds one with no
	// kagent client, which records sends but does not deliver them.
	Sessions *sessions.Service
	// Credentials issues and revokes agent bearers; there is one per process. The sessions and
	// workspaces services receive it from serve-api. Nil in tests that issue no credentials.
	Credentials *agentauth.Credentials
	// Preview routes preview hosts to workspace dev servers. Nil serves no previews, and a
	// preview host is then refused by the Host guard like any unknown host.
	Preview *preview.Proxy
	// HITL observes kagent for tasks that wait for the owner and carries the answers back. Nil builds
	// one with no kagent client: it serves what is recorded but observes and sends nothing.
	HITL *hitl.Service
	// Attention is the owner's inbox. Nil builds one over Pool; the task-decision sweep runs only
	// where the caller registers it.
	Attention *attention.Service
	// Health serves orchestration health and the smoke ledger. Nil builds one over Pool with no
	// kagent client: it serves what is stored and collects nothing.
	Health *health.Service
	// Schedules serves scheduled runs. Nil builds one over Pool with no kagent client: the routes
	// answer 503 and nothing is adopted.
	Schedules *schedules.Service
}

// maxBodyBytes caps request bodies. The validator buffers the whole body before any handler runs,
// so the cap has to sit in this chain.
const maxBodyBytes = 4 << 20

// New builds the Server.
func New(d Deps) *Server {
	prov, err := providers.NewAPI(d.Config, d.Pool)
	var taskSvc *tasks.Service
	if d.Pool != nil && prov != nil {
		taskSvc = &tasks.Service{
			Pool: d.Pool, Providers: prov.Registry, DefaultProfile: d.Config.Tasks.DefaultProviderProfileID,
			MergeTools: bool(d.Config.MergeToolsEnabled),
			Limits: tasks.Limits{
				MaxChildrenPerParent: d.Config.Tasks.MaxChildrenPerParent, MaxChildrenGlobal: d.Config.Tasks.MaxChildrenGlobal,
				ProvisionWindow: d.Config.Tasks.ProvisionWindow.Duration(), Interval: d.Config.Tasks.ReconcileInterval.Duration(),
				ProjectionEvery: d.Config.Tasks.ProjectionEvery.Duration(),
			},
			Log: d.Log,
		}
		// An interface holding a nil pointer is not nil: set the runtimes only when they exist.
		if d.Workspaces != nil {
			taskSvc.Workspaces = d.Workspaces
		}
		if d.Sessions != nil {
			taskSvc.Sessions = d.Sessions
			// A nil kagent client means nothing can run, so an unconfirmed stop has nothing to resolve.
			if d.Sessions.Kagent != nil {
				taskSvc.StopObserver = &tasks.KagentStops{
					Pool: d.Pool, OwnerID: d.Config.Identity.OwnerID, Sessions: d.Sessions, Kagent: d.Sessions.Kagent,
				}
			}
		}
	}
	att := attentionService(d)
	return &Server{
		HealthAPI: &HealthAPI{DB: d.DB, Log: d.Log},
		ProjectsAPI: &projects.ProjectsAPI{Svc: &projects.Service{
			Pool: d.Pool, Remote: d.GitHub, OwnerID: d.Config.Identity.OwnerID,
			PolicyWrites: bool(d.Config.OwnerPolicyWritesEnabled), Log: d.Log, NeedsOwner: att.NeedsOwner,
		}},
		ProvidersAPI:    prov,
		EnvironmentsAPI: environments.NewAPI(d.Config, d.Pool, d.Registry),
		EventsAPI:       events.NewAPI(d.Pool, d.Hub, d.Log, d.Config.Identity.OwnerID),
		WorkspacesAPI:   &workspaces.WorkspacesAPI{Svc: d.Workspaces},
		SessionsAPI:     sessions.NewAPI(d.Sessions, d.Config, d.Pool, d.Log),
		TasksAPI:        &tasks.TasksAPI{Svc: taskSvc, OwnerID: d.Config.Identity.OwnerID},
		API:             hitl.NewAPI(hitlService(d)),
		AttentionAPI:    attention.NewAPI(att),
		DataAPI:         health.NewAPI(healthService(d)),
		SchedulesAPI:    schedules.NewAPI(schedulesService(d)),
		deps:            d,
		initErr:         err,
	}
}

// Handler returns the owner API: the strict handlers behind request validation, the body cap, then the
// Host guard, Origin guard and CORS, with preview hosts routed ahead of them, all inside panic
// recovery, request id and access log.
func (s *Server) Handler() (http.Handler, error) {
	if s.initErr != nil {
		return nil, s.initErr
	}
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
	ownerAPI := httpx.Chain(routes,
		httpx.HostGuard(cfg.AllowedAPIHosts(), cfg.IsDev(), "/health"),
		httpx.OriginGuard(cfg.FrontendOrigin(), cfg.IsDev()),
		httpx.CORS(cfg.FrontendOrigin(), cfg.IsDev()),
		httpx.MaxBody(maxBodyBytes),
		requestValidator(spec),
	)
	// Preview hosts are routed before the API's Host guard, which would refuse them.
	routed := ownerAPI
	if d.Preview != nil {
		routed = d.Preview.Route(ownerAPI)
	}
	return httpx.Chain(routed,
		httpx.AccessLog(d.Log),
		httpx.RequestIDMiddleware,
		httpx.Recover(d.Log),
	), nil
}

func hitlService(d Deps) *hitl.Service {
	if d.HITL != nil {
		return d.HITL
	}
	return hitl.NewService(d.Config, d.Pool, nil, d.Log)
}

func schedulesService(d Deps) *schedules.Service {
	if d.Schedules != nil {
		return d.Schedules
	}
	return &schedules.Service{Pool: d.Pool, OwnerID: d.Config.Identity.OwnerID, Log: d.Log}
}

func attentionService(d Deps) *attention.Service {
	if d.Attention != nil {
		return d.Attention
	}
	return &attention.Service{Pool: d.Pool, OwnerID: d.Config.Identity.OwnerID, Log: d.Log}
}

func healthService(d Deps) *health.Service {
	if d.Health != nil {
		return d.Health
	}
	return health.NewService(d.Config, d.Pool, d.Log)
}
