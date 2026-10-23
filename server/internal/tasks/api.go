package tasks

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// TasksAPI serves the task operations of the strict server interface. All requests are the
// owner's own.
type TasksAPI struct {
	Svc *Service
	// OwnerID is the one owner this instance serves.
	OwnerID string
}

var errUnavailable = apierr.Unavailable("Tasks unavailable")

func (a *TasksAPI) principal() Principal { return OwnerPrincipal(a.OwnerID) }

// actionRefusals are the statuses the task actions declare.
var actionRefusals = []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity}

// taskReply answers a domain refusal whose status the operation declares with the task error body. A
// refusal with another status, or any other error, is an internal failure.
func taskReply(err error, declared ...int) error {
	var e *Error
	if errors.As(err, &e) && slices.Contains(declared, e.Status) {
		return apierr.ReasonDetail(e.Status, e.Reason, e.Detail)
	}
	return err
}

// ListTasks implements api.StrictServerInterface.
func (a *TasksAPI) ListTasks(ctx context.Context, req api.ListTasksRequestObject) (api.ListTasksResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	var project, parent string
	if req.Params.ProjectId != nil {
		project = *req.Params.ProjectId
	}
	if req.Params.ParentTaskId != nil {
		parent = *req.Params.ParentTaskId
	}
	views, err := a.Svc.List(ctx, a.principal(), project, parent)
	if err != nil {
		return nil, err
	}
	return api.ListTasks200JSONResponse(views), nil
}

// GetTask implements api.StrictServerInterface.
func (a *TasksAPI) GetTask(ctx context.Context, req api.GetTaskRequestObject) (api.GetTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	view, err := a.Svc.Get(ctx, a.principal(), req.TaskId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound)
	}
	return api.GetTask200JSONResponse(view), nil
}

// GetTaskOperation implements api.StrictServerInterface.
func (a *TasksAPI) GetTaskOperation(ctx context.Context, req api.GetTaskOperationRequestObject) (api.GetTaskOperationResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	op, err := a.Svc.Operation(ctx, a.principal(), req.OperationId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound)
	}
	return api.GetTaskOperation200JSONResponse(op), nil
}

// CreateTask implements api.StrictServerInterface.
func (a *TasksAPI) CreateTask(ctx context.Context, req api.CreateTaskRequestObject) (api.CreateTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	body := req.Body
	in := CreateInput{
		RequestID: body.RequestId, Title: body.Title, Brief: body.Brief, Mode: dbstate.TaskMode(body.Mode),
		ProjectID: deref(body.ProjectId), TopicID: deref(body.TopicId), ProviderProfileID: deref(body.ProviderProfileId),
		Model: deref(body.Model), Effort: derefEffort(body.Effort),
	}
	if c := body.Checkout; c != nil {
		in.Checkout = &Checkout{Branch: c.Branch, Ref: deref(c.Ref)}
		if c.Depth != nil {
			in.Checkout.Depth = *c.Depth
		}
	}
	op, err := a.Svc.Create(ctx, a.principal(), in)
	if err != nil {
		return nil, taskReply(err, actionRefusals...)
	}
	return api.CreateTask202JSONResponse(op), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefEffort(e *api.EffortLevel) string {
	if e == nil {
		return ""
	}
	return string(*e)
}

// CancelTask implements api.StrictServerInterface.
func (a *TasksAPI) CancelTask(ctx context.Context, req api.CancelTaskRequestObject) (api.CancelTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	op, err := a.Svc.Cancel(ctx, a.principal(), action(req.TaskId, *req.Body))
	if err != nil {
		return nil, taskReply(err, actionRefusals...)
	}
	return api.CancelTask202JSONResponse(op), nil
}

// RetryTask implements api.StrictServerInterface.
func (a *TasksAPI) RetryTask(ctx context.Context, req api.RetryTaskRequestObject) (api.RetryTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	op, err := a.Svc.Retry(ctx, a.principal(), action(req.TaskId, *req.Body))
	if err != nil {
		return nil, taskReply(err, actionRefusals...)
	}
	return api.RetryTask202JSONResponse(op), nil
}

// ReassignTask implements api.StrictServerInterface.
func (a *TasksAPI) ReassignTask(ctx context.Context, req api.ReassignTaskRequestObject) (api.ReassignTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	in := action(req.TaskId, api.TaskAction{
		ExpectedAttemptId: req.Body.ExpectedAttemptId, ExpectedVersion: req.Body.ExpectedVersion, RequestId: req.Body.RequestId,
	})
	in.TargetProfileID = req.Body.TargetProfileId
	op, err := a.Svc.Reassign(ctx, a.principal(), in)
	if err != nil {
		return nil, taskReply(err, actionRefusals...)
	}
	return api.ReassignTask202JSONResponse(op), nil
}

func action(taskID string, body api.TaskAction) ActionInput {
	return ActionInput{
		RequestID: body.RequestId, TaskID: taskID, ExpectedVersion: body.ExpectedVersion,
		ExpectedAttemptID: deref(body.ExpectedAttemptId),
	}
}
