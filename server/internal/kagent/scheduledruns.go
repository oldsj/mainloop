package kagent

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

type scheduledRunClients struct {
	create    *connect.Client[api.CreateScheduledRunRequest, api.CreateScheduledRunResponse]
	get       *connect.Client[api.GetScheduledRunRequest, api.GetScheduledRunResponse]
	list      *connect.Client[api.ListScheduledRunsRequest, api.ListScheduledRunsResponse]
	update    *connect.Client[api.UpdateScheduledRunRequest, api.UpdateScheduledRunResponse]
	del       *connect.Client[api.DeleteScheduledRunRequest, api.DeleteScheduledRunResponse]
	trigger   *connect.Client[api.TriggerScheduledRunRequest, api.TriggerScheduledRunResponse]
	getExec   *connect.Client[api.GetScheduledRunExecutionRequest, api.GetScheduledRunExecutionResponse]
	listExecs *connect.Client[api.ListScheduledRunExecutionsRequest, api.ListScheduledRunExecutionsResponse]
}

func newScheduledRunClients(h connect.HTTPClient, base string) scheduledRunClients {
	opt := connect.WithGRPCWeb()
	return scheduledRunClients{
		create: connect.NewClient[api.CreateScheduledRunRequest, api.CreateScheduledRunResponse](
			h, base+ProcCreateScheduledRun, opt),
		get: connect.NewClient[api.GetScheduledRunRequest, api.GetScheduledRunResponse](
			h, base+ProcGetScheduledRun, opt),
		list: connect.NewClient[api.ListScheduledRunsRequest, api.ListScheduledRunsResponse](
			h, base+ProcListScheduledRuns, opt),
		update: connect.NewClient[api.UpdateScheduledRunRequest, api.UpdateScheduledRunResponse](
			h, base+ProcUpdateScheduledRun, opt),
		del: connect.NewClient[api.DeleteScheduledRunRequest, api.DeleteScheduledRunResponse](
			h, base+ProcDeleteScheduledRun, opt),
		trigger: connect.NewClient[api.TriggerScheduledRunRequest, api.TriggerScheduledRunResponse](
			h, base+ProcTriggerScheduledRun, opt),
		getExec: connect.NewClient[api.GetScheduledRunExecutionRequest, api.GetScheduledRunExecutionResponse](
			h, base+ProcGetScheduledRunExecution, opt),
		listExecs: connect.NewClient[api.ListScheduledRunExecutionsRequest, api.ListScheduledRunExecutionsResponse](
			h, base+ProcListScheduledRunExecutions, opt),
	}
}

// Schedule failures that callers tell apart come back as a *SessionError with these codes:
//
//	NotFound            no such schedule or execution, or it belongs to another owner
//	AlreadyExists       a create request id was already used with different inputs
//	Aborted             an update carried a stale etag (see UpdateScheduledRun)
//	FailedPrecondition  the schedule was deleted, or its Agent does not exist
//	InvalidArgument     the schedule or an id was not valid

// scheduleOf checks the reply is for the schedule that was asked about.
func scheduleOf(op, id string, s *api.ScheduledRun) (*api.ScheduledRun, error) {
	if s.GetId() != id || s.GetEtag() == "" {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is for a different schedule"}
	}
	return s, nil
}

// CreateScheduledRun creates a schedule. Calling again with the same RequestID and inputs returns
// the same schedule, whatever became of it since; the same RequestID with other inputs is
// AlreadyExists. The reply must echo the create contract, or the outcome is unknown.
func (c *Client) CreateScheduledRun(ctx context.Context, req *api.CreateScheduledRunRequest) (*api.ScheduledRun, error) {
	const op = "ScheduledRunService CreateScheduledRun"
	resp, err := unary(ctx, c, op, c.scheduleCl.create, req)
	if err != nil {
		return nil, err
	}
	s := resp.GetScheduledRun()
	a, ra := s.GetAgent(), req.GetAgent()
	if s.GetId() == "" || s.GetEtag() == "" ||
		a.GetNamespace() != ra.GetNamespace() || a.GetName() != ra.GetName() {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply differs from the create contract"}
	}
	return s, nil
}

// GetScheduledRun reads one schedule. A deleted schedule is returned with DeletedAt set.
func (c *Client) GetScheduledRun(ctx context.Context, id string) (*api.ScheduledRun, error) {
	const op = "ScheduledRunService GetScheduledRun"
	if id == "" {
		return nil, emptyArg(op, "schedule id")
	}
	resp, err := unary(ctx, c, op, c.scheduleCl.get, &api.GetScheduledRunRequest{ScheduledRunId: id})
	if err != nil {
		return nil, err
	}
	return scheduleOf(op, id, resp.GetScheduledRun())
}

// ListScheduledRunsPage reads one page of the identity's undeleted schedules, in id order. An
// empty next token means the last page.
func (c *Client) ListScheduledRunsPage(
	ctx context.Context, token string, limit int,
) (schedules []*api.ScheduledRun, next string, err error) {
	const op = "ScheduledRunService ListScheduledRuns"
	limit = min(max(limit, 1), maxPageSize)
	resp, err := unary(ctx, c, op, c.scheduleCl.list, &api.ListScheduledRunsRequest{
		Page: &api.PageRequest{Limit: int32(limit), PageToken: token}, //nolint:gosec // limit is 1..100
	})
	if err != nil {
		return nil, "", err
	}
	for _, s := range resp.GetScheduledRuns() {
		if s.GetId() == "" {
			return nil, "", &OutcomeUnknown{Op: op, Detail: "a listed schedule has no id"}
		}
	}
	return resp.GetScheduledRuns(), resp.GetPage().GetNextPageToken(), nil
}

// ListScheduledRuns reads every undeleted schedule.
func (c *Client) ListScheduledRuns(ctx context.Context) ([]*api.ScheduledRun, error) {
	return paginate("ScheduledRunService ListScheduledRuns", func(token string) ([]*api.ScheduledRun, string, error) {
		return c.ListScheduledRunsPage(ctx, token, maxPageSize)
	})
}

// UpdateScheduledRun replaces a schedule's config if etag is still current. kagent answers a stale
// etag with Aborted, which for every other call is an unknown outcome; here it is definite (the
// update did not apply), so it comes back as a *SessionError with CodeAborted.
func (c *Client) UpdateScheduledRun(
	ctx context.Context, id, etag string, config *api.ScheduledRunConfig,
) (*api.ScheduledRun, error) {
	const op = "ScheduledRunService UpdateScheduledRun"
	if id == "" {
		return nil, emptyArg(op, "schedule id")
	}
	resp, err := unary(ctx, c, op, c.scheduleCl.update, &api.UpdateScheduledRunRequest{
		ScheduledRunId: id, Etag: etag, Config: config,
	})
	if err != nil {
		var unknown *OutcomeUnknown
		var ce *connect.Error
		if errors.As(err, &unknown) && errors.As(unknown.Err, &ce) && ce.Code() == connect.CodeAborted {
			return nil, &SessionError{Op: op, Code: connect.CodeAborted, Detail: ce.Message()}
		}
		return nil, err
	}
	return scheduleOf(op, id, resp.GetScheduledRun())
}

// DeleteScheduledRun deletes a schedule; its executions stay readable. The reply is the schedule
// as a tombstone.
func (c *Client) DeleteScheduledRun(ctx context.Context, id string) (*api.ScheduledRun, error) {
	const op = "ScheduledRunService DeleteScheduledRun"
	if id == "" {
		return nil, emptyArg(op, "schedule id")
	}
	resp, err := unary(ctx, c, op, c.scheduleCl.del, &api.DeleteScheduledRunRequest{ScheduledRunId: id})
	if err != nil {
		return nil, err
	}
	s := resp.GetScheduledRun()
	if s.GetId() != id {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is for a different schedule"}
	}
	return s, nil
}

// TriggerScheduledRun reserves a manual execution. Calling again with the same requestID returns
// the same execution, even after the schedule was deleted.
func (c *Client) TriggerScheduledRun(ctx context.Context, id, requestID string) (*api.ScheduledRunExecution, error) {
	const op = "ScheduledRunService TriggerScheduledRun"
	if id == "" {
		return nil, emptyArg(op, "schedule id")
	}
	if requestID == "" {
		return nil, emptyArg(op, "request id")
	}
	resp, err := unary(ctx, c, op, c.scheduleCl.trigger, &api.TriggerScheduledRunRequest{
		ScheduledRunId: id, RequestId: requestID,
	})
	if err != nil {
		return nil, err
	}
	e := resp.GetExecution()
	if e.GetId() == "" || e.GetScheduledRunId() != id || e.GetManualRequestId() != requestID {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is not the execution for this trigger"}
	}
	return e, nil
}

// GetScheduledRunExecution reads one execution.
func (c *Client) GetScheduledRunExecution(ctx context.Context, id string) (*api.ScheduledRunExecution, error) {
	const op = "ScheduledRunService GetScheduledRunExecution"
	if id == "" {
		return nil, emptyArg(op, "execution id")
	}
	resp, err := unary(ctx, c, op, c.scheduleCl.getExec, &api.GetScheduledRunExecutionRequest{ExecutionId: id})
	if err != nil {
		return nil, err
	}
	e := resp.GetExecution()
	if e.GetId() != id {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is for a different execution"}
	}
	return e, nil
}

// ListScheduledRunExecutionsPage reads one page of a schedule's executions, newest first. History
// stays readable after the schedule is deleted. An empty next token means the last page.
func (c *Client) ListScheduledRunExecutionsPage(
	ctx context.Context, scheduleID, token string, limit int,
) (executions []*api.ScheduledRunExecution, next string, err error) {
	const op = "ScheduledRunService ListScheduledRunExecutions"
	if scheduleID == "" {
		return nil, "", emptyArg(op, "schedule id")
	}
	limit = min(max(limit, 1), maxPageSize)
	resp, err := unary(ctx, c, op, c.scheduleCl.listExecs, &api.ListScheduledRunExecutionsRequest{
		ScheduledRunId: scheduleID,
		Page:           &api.PageRequest{Limit: int32(limit), PageToken: token}, //nolint:gosec // limit is 1..100
	})
	if err != nil {
		return nil, "", err
	}
	for _, e := range resp.GetExecutions() {
		if e.GetId() == "" || e.GetScheduledRunId() != scheduleID {
			return nil, "", &OutcomeUnknown{Op: op, Detail: "a listed execution is for a different schedule"}
		}
	}
	return resp.GetExecutions(), resp.GetPage().GetNextPageToken(), nil
}
