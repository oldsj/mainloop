package schedules

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// SchedulesAPI serves the /schedules routes. The type name is unique because the app embeds every
// domain's API in one struct.
type SchedulesAPI struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *SchedulesAPI { return &SchedulesAPI{Svc: svc} }

var errUnavailable = httpx.NewError(http.StatusServiceUnavailable, "Scheduled runs are not available")

func invalid(msg string) error { return httpx.NewError(http.StatusUnprocessableEntity, msg) }

// refusal maps a kagent failure to the contract's status. The error bodies are the contract's
// HTTPError, which every 4xx and 502 of these routes allows, so they are returned as errors rather
// than as one typed response per status. creating says a missing Agent is a missing resource of the
// request, not a deleted schedule.
func (s *Service) refusal(err error, creating bool) error {
	var se *kagent.SessionError
	var cfg *kagent.ConfigError
	switch {
	case errors.As(err, &se):
		switch se.Code {
		case connect.CodeNotFound:
			return httpx.NewError(http.StatusNotFound, "Schedule not found")
		case connect.CodeAlreadyExists:
			return httpx.NewError(http.StatusConflict, "request_id was already used with a different request")
		case connect.CodeAborted:
			return httpx.NewError(http.StatusPreconditionFailed, "The schedule changed since it was read; read it again and retry")
		case connect.CodeFailedPrecondition:
			if creating {
				return httpx.NewError(http.StatusNotFound, "Agent not found")
			}
			return httpx.NewError(http.StatusConflict, "The schedule was deleted")
		case connect.CodeInvalidArgument:
			return invalid(se.Detail)
		}
	case errors.As(err, &cfg) && !cfg.Credentials:
		return httpx.NewError(http.StatusForbidden, "kagent refused this request")
	}
	s.log().Warn("kagent schedule call failed", "error", err)
	return httpx.NewError(http.StatusBadGateway, "kagent is unavailable")
}

func scheduleID(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return invalid("schedule_id must be a UUID")
	}
	return nil
}

func quote(etag string) *string {
	q := `"` + etag + `"`
	return &q
}

// ifMatch is the etag in an If-Match header: a quoted UUID.
func ifMatch(h *string) (string, error) {
	if h == nil || strings.TrimSpace(*h) == "" {
		return "", httpx.NewError(http.StatusPreconditionRequired, "If-Match is required; send the etag from the last read")
	}
	v := strings.TrimPrefix(strings.TrimSpace(*h), "W/")
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", invalid("If-Match must be a quoted etag")
	}
	v = v[1 : len(v)-1]
	if _, err := uuid.Parse(v); err != nil {
		return "", invalid("If-Match must hold the schedule's etag")
	}
	return v, nil
}

func pageSize(p *int) int {
	if p == nil {
		return 50
	}
	return *p
}

func nextToken(t string) *string {
	if t == "" {
		return nil
	}
	return &t
}

func (a *SchedulesAPI) ready() error {
	if a.Svc == nil || a.Svc.Kagent == nil || a.Svc.Pool == nil {
		return errUnavailable
	}
	return nil
}

func updated(owner, id string) events.Draft {
	return events.Draft{UserID: owner, Type: events.TypeScheduleUpdated, Payload: api.EventScheduleUpdated{ScheduleId: id}}
}

func ptr[T any](v T) *T { return &v }

var errNotFound = httpx.NewError(http.StatusNotFound, "Schedule not found")

// registered returns the registry row of a schedule this instance made on one of its own Agents.
// Anything else (another instance's schedule, one this instance never made, an Agent that left this
// deployment) does not exist as far as the routes are concerned.
func (s *Service) registered(ctx context.Context, id string) (dbq.ScheduledRunSchedule, error) {
	row, err := dbq.New(s.Pool).GetScheduledRunSchedule(ctx, dbq.GetScheduledRunScheduleParams{ScheduleID: &id, OwnerID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errNotFound
	}
	if err != nil {
		return row, err
	}
	owned, err := s.ownAgents(ctx)
	if err != nil {
		return row, err
	}
	if !owned[agentKey(row.AgentNamespace, row.AgentName)] {
		return row, errNotFound
	}
	return row, nil
}

// touch publishes schedule:updated for a schedule that changed, marking it deleted in the same
// transaction when it was.
func (s *Service) touch(ctx context.Context, id string, deleted bool) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		if deleted {
			if err := dbq.New(tx).MarkScheduledRunScheduleDeleted(ctx, &id); err != nil {
				return nil, err
			}
		}
		return []events.Draft{updated(s.OwnerID, id)}, nil
	})
}

// confirm records kagent's answer to a create: the intent becomes the registry entry for the
// schedule, with schedule:updated in the same transaction. A replay of a confirmed create changes
// nothing and publishes nothing.
func (s *Service) confirm(ctx context.Context, requestID string, sc *kagentapi.ScheduledRun) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		n, err := q.ConfirmScheduledRunCreate(ctx, dbq.ConfirmScheduledRunCreateParams{RequestID: requestID, ScheduleID: ptr(sc.GetId())})
		if err != nil {
			return nil, err
		}
		if sc.GetDeletedAt() != nil {
			if err := q.MarkScheduledRunScheduleDeleted(ctx, ptr(sc.GetId())); err != nil {
				return nil, err
			}
		}
		if n == 0 {
			return nil, nil
		}
		return []events.Draft{updated(s.OwnerID, sc.GetId())}, nil
	})
}

// definite says kagent answered and refused, so a create it refused made nothing.
func definite(err error) bool {
	var se *kagent.SessionError
	return errors.As(err, &se)
}

var errRequestIDReused = httpx.NewError(http.StatusConflict, "request_id was already used with a different request")

// beginCreate writes the create intent before anything is sent. A row that already holds the
// request id must hold the same Agent and config, or the request is a different one under a used key.
func (s *Service) beginCreate(
	ctx context.Context, requestID string, agent *kagentapi.ResourceReference, cfg *kagentapi.ScheduledRunConfig,
) error {
	raw, err := protojson.Marshal(cfg)
	if err != nil {
		return err
	}
	q := dbq.New(s.Pool)
	if _, err := q.InsertScheduledRunIntent(ctx, dbq.InsertScheduledRunIntentParams{
		RequestID: requestID, OwnerID: s.OwnerID, AgentNamespace: agent.GetNamespace(), AgentName: agent.GetName(), CreateConfig: raw,
	}); err != nil {
		return err
	}
	row, err := q.GetScheduledRunByRequest(ctx, dbq.GetScheduledRunByRequestParams{RequestID: requestID, OwnerID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errRequestIDReused // the key belongs to another owner
	}
	if err != nil {
		return err
	}
	var had kagentapi.ScheduledRunConfig
	if err := protojson.Unmarshal(row.CreateConfig, &had); err != nil {
		return err
	}
	if row.AgentNamespace != agent.GetNamespace() || row.AgentName != agent.GetName() || !proto.Equal(&had, cfg) {
		return errRequestIDReused
	}
	return nil
}

// maxListPages bounds how many kagent pages one list call reads looking for this instance's schedules.
const maxListPages = 20

// ListSchedules implements api.StrictServerInterface. kagent lists every schedule of its principal,
// so each page is cut down to this instance's registry; a page can hold fewer items than the limit.
func (a *SchedulesAPI) ListSchedules(ctx context.Context, r api.ListSchedulesRequestObject) (api.ListSchedulesResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	token := ""
	if r.Params.PageToken != nil {
		token = *r.Params.PageToken
	}
	owned, err := a.Svc.ownAgents(ctx)
	if err != nil {
		return nil, err
	}
	page := api.ScheduledRunPage{Items: []api.ScheduledRunOutput{}}
	seen := map[string]bool{}
	// Keep reading while a whole page was foreign, so a client is not handed empty pages for as
	// long as another instance's schedules sort first.
	for range maxListPages {
		list, next, err := a.Svc.Kagent.ListScheduledRunsPage(ctx, token, pageSize(r.Params.Limit))
		if err != nil {
			return nil, a.Svc.refusal(err, false)
		}
		ids := make([]string, len(list))
		for i, sc := range list {
			ids[i] = sc.GetId()
		}
		rows, err := dbq.New(a.Svc.Pool).ListScheduledRunSchedulesByID(ctx, dbq.ListScheduledRunSchedulesByIDParams{
			OwnerID: a.Svc.OwnerID, ScheduleIds: ids,
		})
		if err != nil {
			return nil, err
		}
		mine := map[string]bool{}
		for _, row := range rows {
			if row.ScheduleID != nil && owned[agentKey(row.AgentNamespace, row.AgentName)] {
				mine[*row.ScheduleID] = true
			}
		}
		for _, sc := range list {
			if mine[sc.GetId()] {
				page.Items = append(page.Items, fromSchedule(sc))
			}
		}
		page.NextPageToken = nextToken(next)
		if len(page.Items) > 0 || next == "" || seen[next] {
			break
		}
		seen[next] = true
		token = next
	}
	return api.ListSchedules200JSONResponse(page), nil
}

// CreateSchedule implements api.StrictServerInterface. The intent row is written first and the
// request id goes to kagent as it is, so a retry (or the sweep) replays the same create and finds
// the schedule the first call made.
func (a *SchedulesAPI) CreateSchedule(ctx context.Context, r api.CreateScheduleRequestObject) (api.CreateScheduleResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("the body is required")
	}
	if strings.TrimSpace(r.Body.RequestId) == "" {
		return nil, invalid("request_id is required")
	}
	cfg, err := toConfig(r.Body.Config)
	if err != nil {
		return nil, invalid(err.Error())
	}
	owned, err := a.Svc.ownAgents(ctx)
	if err != nil {
		return nil, err
	}
	if !owned[agentKey(r.Body.Agent.Namespace, r.Body.Agent.Name)] {
		return nil, httpx.NewError(http.StatusForbidden, "That Agent does not belong to this deployment")
	}
	agent := &kagentapi.ResourceReference{Namespace: r.Body.Agent.Namespace, Name: r.Body.Agent.Name}
	if err := a.Svc.beginCreate(ctx, r.Body.RequestId, agent, cfg); err != nil {
		return nil, err
	}
	sc, err := a.Svc.Kagent.CreateScheduledRun(ctx, &kagentapi.CreateScheduledRunRequest{
		Agent: agent, RequestId: r.Body.RequestId, Config: cfg,
	})
	if err != nil {
		if definite(err) {
			if derr := dbq.New(a.Svc.Pool).DeleteScheduledRunIntent(ctx, r.Body.RequestId); derr != nil {
				a.Svc.log().WarnContext(ctx, "dropping a refused schedule intent failed", "error", derr)
			}
		}
		return nil, a.Svc.refusal(err, true)
	}
	if err := a.Svc.confirm(ctx, r.Body.RequestId, sc); err != nil {
		return nil, err
	}
	if sc.GetDeletedAt() != nil {
		// A replay of a create whose schedule was deleted since: kagent still returns it.
		return nil, httpx.NewError(http.StatusConflict, "The schedule made by this request_id was deleted")
	}
	return api.CreateSchedule201JSONResponse{
		Body: fromSchedule(sc), Headers: api.CreateSchedule201ResponseHeaders{ETag: quote(sc.GetEtag())},
	}, nil
}

// GetSchedule implements api.StrictServerInterface.
func (a *SchedulesAPI) GetSchedule(ctx context.Context, r api.GetScheduleRequestObject) (api.GetScheduleResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if err := scheduleID(r.ScheduleId); err != nil {
		return nil, err
	}
	if _, err := a.Svc.registered(ctx, r.ScheduleId); err != nil {
		return nil, err
	}
	sc, err := a.Svc.Kagent.GetScheduledRun(ctx, r.ScheduleId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if sc.GetDeletedAt() != nil {
		return nil, errNotFound
	}
	return api.GetSchedule200JSONResponse{
		Body: fromSchedule(sc), Headers: api.GetSchedule200ResponseHeaders{ETag: quote(sc.GetEtag())},
	}, nil
}

// UpdateSchedule implements api.StrictServerInterface. kagent compares the etag; this layer only
// passes it through, so a stale one is refused there and never applied.
func (a *SchedulesAPI) UpdateSchedule(ctx context.Context, r api.UpdateScheduleRequestObject) (api.UpdateScheduleResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if err := scheduleID(r.ScheduleId); err != nil {
		return nil, err
	}
	etag, err := ifMatch(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("the body is required")
	}
	cfg, err := toConfig(*r.Body)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if _, err := a.Svc.registered(ctx, r.ScheduleId); err != nil {
		return nil, err
	}
	sc, err := a.Svc.Kagent.UpdateScheduledRun(ctx, r.ScheduleId, etag, cfg)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if err := a.Svc.touch(ctx, sc.GetId(), false); err != nil {
		return nil, err
	}
	return api.UpdateSchedule200JSONResponse{
		Body: fromSchedule(sc), Headers: api.UpdateSchedule200ResponseHeaders{ETag: quote(sc.GetEtag())},
	}, nil
}

// DeleteSchedule implements api.StrictServerInterface. kagent's delete is idempotent, and so is this.
func (a *SchedulesAPI) DeleteSchedule(ctx context.Context, r api.DeleteScheduleRequestObject) (api.DeleteScheduleResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if err := scheduleID(r.ScheduleId); err != nil {
		return nil, err
	}
	if _, err := a.Svc.registered(ctx, r.ScheduleId); err != nil {
		return nil, err
	}
	sc, err := a.Svc.Kagent.DeleteScheduledRun(ctx, r.ScheduleId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if err := a.Svc.touch(ctx, sc.GetId(), true); err != nil {
		return nil, err
	}
	return api.DeleteSchedule204Response{}, nil
}

// ListScheduleExecutions implements api.StrictServerInterface.
func (a *SchedulesAPI) ListScheduleExecutions(ctx context.Context, r api.ListScheduleExecutionsRequestObject) (api.ListScheduleExecutionsResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if err := scheduleID(r.ScheduleId); err != nil {
		return nil, err
	}
	if _, err := a.Svc.registered(ctx, r.ScheduleId); err != nil {
		return nil, err
	}
	token := ""
	if r.Params.PageToken != nil {
		token = *r.Params.PageToken
	}
	list, next, err := a.Svc.Kagent.ListScheduledRunExecutionsPage(ctx, r.ScheduleId, token, pageSize(r.Params.Limit))
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	ids := make([]string, len(list))
	for i, e := range list {
		ids[i] = e.GetId()
	}
	links, err := dbq.New(a.Svc.Pool).ListScheduledRunLinks(ctx, dbq.ListScheduledRunLinksParams{
		ScheduleID: r.ScheduleId, ExecutionIds: ids,
	})
	if err != nil {
		return nil, err
	}
	sessions := map[string]*string{}
	for _, l := range links {
		sessions[l.ExecutionID] = l.SessionID
	}
	page := api.ScheduledRunExecutionPage{Items: []api.ScheduledRunExecutionOutput{}, NextPageToken: nextToken(next)}
	for _, e := range list {
		page.Items = append(page.Items, fromExecution(e, sessions[e.GetId()]))
	}
	return api.ListScheduleExecutions200JSONResponse(page), nil
}

// TriggerSchedule implements api.StrictServerInterface.
func (a *SchedulesAPI) TriggerSchedule(ctx context.Context, r api.TriggerScheduleRequestObject) (api.TriggerScheduleResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if err := scheduleID(r.ScheduleId); err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("the body is required")
	}
	if _, err := a.Svc.registered(ctx, r.ScheduleId); err != nil {
		return nil, err
	}
	e, err := a.Svc.Kagent.TriggerScheduledRun(ctx, r.ScheduleId, r.Body.RequestId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if err := a.Svc.touch(ctx, r.ScheduleId, false); err != nil {
		return nil, err
	}
	var sessionID *string
	links, err := dbq.New(a.Svc.Pool).ListScheduledRunLinks(ctx, dbq.ListScheduledRunLinksParams{
		ScheduleID: r.ScheduleId, ExecutionIds: []string{e.GetId()},
	})
	if err != nil {
		return nil, err
	}
	if len(links) == 1 {
		sessionID = links[0].SessionID
	}
	return api.TriggerSchedule202JSONResponse(fromExecution(e, sessionID)), nil
}
