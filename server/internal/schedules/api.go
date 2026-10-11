package schedules

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
		case connect.CodePermissionDenied:
			return httpx.NewError(http.StatusForbidden, "kagent refused this request")
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

// record notes a schedule kagent answered for and publishes schedule:updated, in one transaction.
// The registry row is what the adoption sweep walks, so a schedule made here is never missed.
// quiet publishes only when the row changed (a replay of a create changes nothing).
func (s *Service) record(ctx context.Context, sc *kagentapi.ScheduledRun, requestID *string, deleted, quiet bool) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		n, err := q.UpsertScheduledRunSchedule(ctx, dbq.UpsertScheduledRunScheduleParams{
			ScheduleID: sc.GetId(), OwnerID: s.OwnerID,
			AgentNamespace: sc.GetAgent().GetNamespace(), AgentName: sc.GetAgent().GetName(),
			RequestID: requestID, AdoptFrom: sc.GetCreatedAt().AsTime(),
		})
		if err != nil {
			return nil, err
		}
		if deleted {
			if err := q.MarkScheduledRunScheduleDeleted(ctx, sc.GetId()); err != nil {
				return nil, err
			}
		}
		if quiet && n == 0 {
			return nil, nil
		}
		return []events.Draft{updated(s.OwnerID, sc.GetId())}, nil
	})
}

// ListSchedules implements api.StrictServerInterface.
func (a *SchedulesAPI) ListSchedules(ctx context.Context, r api.ListSchedulesRequestObject) (api.ListSchedulesResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	token := ""
	if r.Params.PageToken != nil {
		token = *r.Params.PageToken
	}
	list, next, err := a.Svc.Kagent.ListScheduledRunsPage(ctx, token, pageSize(r.Params.Limit))
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	page := api.ScheduledRunPage{Items: []api.ScheduledRunOutput{}, NextPageToken: nextToken(next)}
	for _, sc := range list {
		page.Items = append(page.Items, fromSchedule(sc))
	}
	return api.ListSchedules200JSONResponse(page), nil
}

// CreateSchedule implements api.StrictServerInterface. The request id goes to kagent as it is, so a
// retry finds the schedule the first call made.
func (a *SchedulesAPI) CreateSchedule(ctx context.Context, r api.CreateScheduleRequestObject) (api.CreateScheduleResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, invalid("the body is required")
	}
	cfg, err := toConfig(r.Body.Config)
	if err != nil {
		return nil, invalid(err.Error())
	}
	sc, err := a.Svc.Kagent.CreateScheduledRun(ctx, &kagentapi.CreateScheduledRunRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: r.Body.Agent.Namespace, Name: r.Body.Agent.Name},
		RequestId: r.Body.RequestId, Config: cfg,
	})
	if err != nil {
		return nil, a.Svc.refusal(err, true)
	}
	if err := a.Svc.record(ctx, sc, &r.Body.RequestId, false, true); err != nil {
		return nil, err
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
	sc, err := a.Svc.Kagent.GetScheduledRun(ctx, r.ScheduleId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if sc.GetDeletedAt() != nil {
		return nil, httpx.NewError(http.StatusNotFound, "Schedule not found")
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
	sc, err := a.Svc.Kagent.UpdateScheduledRun(ctx, r.ScheduleId, etag, cfg)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if err := a.Svc.record(ctx, sc, nil, false, false); err != nil {
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
	sc, err := a.Svc.Kagent.DeleteScheduledRun(ctx, r.ScheduleId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if err := a.Svc.record(ctx, sc, nil, true, false); err != nil {
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
	e, err := a.Svc.Kagent.TriggerScheduledRun(ctx, r.ScheduleId, r.Body.RequestId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	// The trigger needs the schedule in the registry, so the sweep adopts the execution; the Agent
	// and creation time come from the schedule.
	sc, err := a.Svc.Kagent.GetScheduledRun(ctx, r.ScheduleId)
	if err != nil {
		return nil, a.Svc.refusal(err, false)
	}
	if err := a.Svc.record(ctx, sc, nil, sc.GetDeletedAt() != nil, false); err != nil {
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
