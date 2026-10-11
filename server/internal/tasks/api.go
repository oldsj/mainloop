package tasks

import (
	"context"
	"errors"
	"net/http"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// TasksAPI serves the task operations of the strict server interface. All requests are the
// owner's own.
type TasksAPI struct {
	Svc *Service
	// OwnerID is the one owner this instance serves.
	OwnerID string
}

var errUnavailable = httpx.NewError(http.StatusServiceUnavailable, "Tasks unavailable")

func (a *TasksAPI) principal() Principal { return OwnerPrincipal(a.OwnerID) }

func taskError(reason string) api.TaskError {
	return api.TaskError{Detail: api.TaskErrorDetail{Reason: reason}}
}

func invalid(msg string) api.HTTPValidationError {
	return api.HTTPValidationError{Detail: &[]api.ValidationError{{
		Loc: []api.ValidationError_Loc_Item{}, Msg: msg, Type: "value_error",
	}}}
}

// refusalOf extracts a domain refusal; any other error is an internal failure.
func refusalOf(err error) (*Error, bool) {
	var e *Error
	return e, errors.As(err, &e)
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
	if e, ok := refusalOf(err); ok && e.Status == http.StatusNotFound {
		var b api.GetTask404JSONResponseBody
		if err := b.FromTaskError(taskError(e.Reason)); err != nil {
			return nil, err
		}
		return api.GetTask404JSONResponse(b), nil
	} else if err != nil {
		return nil, err
	}
	return api.GetTask200JSONResponse(view), nil
}

// GetTaskOperation implements api.StrictServerInterface.
func (a *TasksAPI) GetTaskOperation(ctx context.Context, req api.GetTaskOperationRequestObject) (api.GetTaskOperationResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	op, err := a.Svc.Operation(ctx, a.principal(), req.OperationId)
	if e, ok := refusalOf(err); ok && e.Status == http.StatusNotFound {
		var b api.GetTaskOperation404JSONResponseBody
		if err := b.FromTaskError(taskError(e.Reason)); err != nil {
			return nil, err
		}
		return api.GetTaskOperation404JSONResponse(b), nil
	} else if err != nil {
		return nil, err
	}
	return api.GetTaskOperation200JSONResponse(op), nil
}

// CreateTask implements api.StrictServerInterface.
func (a *TasksAPI) CreateTask(ctx context.Context, req api.CreateTaskRequestObject) (api.CreateTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		var b api.CreateTask422JSONResponseBody
		if err := b.FromHTTPValidationError(invalid("Request body is required")); err != nil {
			return nil, err
		}
		return api.CreateTask422JSONResponse(b), nil
	}
	body := req.Body
	in := CreateInput{
		RequestID: body.RequestId, Title: body.Title, Brief: body.Brief, Mode: dbstate.TaskMode(body.Mode),
		ProjectID: deref(body.ProjectId), TopicID: deref(body.TopicId), ProviderProfileID: deref(body.ProviderProfileId),
	}
	if c := body.Checkout; c != nil {
		in.Checkout = &Checkout{Branch: c.Branch, Ref: deref(c.Ref)}
		if c.Depth != nil {
			in.Checkout.Depth = *c.Depth
		}
	}
	op, err := a.Svc.Create(ctx, a.principal(), in)
	if err != nil {
		e, ok := refusalOf(err)
		if !ok {
			return nil, err
		}
		te := taskError(e.Reason)
		switch e.Status {
		case http.StatusForbidden:
			var b api.CreateTask403JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.CreateTask403JSONResponse(b), nil
		case http.StatusNotFound:
			var b api.CreateTask404JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.CreateTask404JSONResponse(b), nil
		case http.StatusConflict:
			return api.CreateTask409JSONResponse(te), nil
		case http.StatusUnprocessableEntity:
			var b api.CreateTask422JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.CreateTask422JSONResponse(b), nil
		}
		return nil, err
	}
	return api.CreateTask202JSONResponse(op), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CancelTask implements api.StrictServerInterface.
func (a *TasksAPI) CancelTask(ctx context.Context, req api.CancelTaskRequestObject) (api.CancelTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		var b api.CancelTask422JSONResponseBody
		if err := b.FromHTTPValidationError(invalid("Request body is required")); err != nil {
			return nil, err
		}
		return api.CancelTask422JSONResponse(b), nil
	}
	op, err := a.Svc.Cancel(ctx, a.principal(), action(req.TaskId, *req.Body))
	if err != nil {
		e, ok := refusalOf(err)
		if !ok {
			return nil, err
		}
		te := taskError(e.Reason)
		switch e.Status {
		case http.StatusForbidden:
			var b api.CancelTask403JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.CancelTask403JSONResponse(b), nil
		case http.StatusNotFound:
			var b api.CancelTask404JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.CancelTask404JSONResponse(b), nil
		case http.StatusConflict:
			return api.CancelTask409JSONResponse(te), nil
		case http.StatusUnprocessableEntity:
			var b api.CancelTask422JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.CancelTask422JSONResponse(b), nil
		}
		return nil, err
	}
	return api.CancelTask202JSONResponse(op), nil
}

// RetryTask implements api.StrictServerInterface.
func (a *TasksAPI) RetryTask(ctx context.Context, req api.RetryTaskRequestObject) (api.RetryTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		var b api.RetryTask422JSONResponseBody
		if err := b.FromHTTPValidationError(invalid("Request body is required")); err != nil {
			return nil, err
		}
		return api.RetryTask422JSONResponse(b), nil
	}
	op, err := a.Svc.Retry(ctx, a.principal(), action(req.TaskId, *req.Body))
	if err != nil {
		e, ok := refusalOf(err)
		if !ok {
			return nil, err
		}
		te := taskError(e.Reason)
		switch e.Status {
		case http.StatusForbidden:
			var b api.RetryTask403JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.RetryTask403JSONResponse(b), nil
		case http.StatusNotFound:
			var b api.RetryTask404JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.RetryTask404JSONResponse(b), nil
		case http.StatusConflict:
			return api.RetryTask409JSONResponse(te), nil
		case http.StatusUnprocessableEntity:
			var b api.RetryTask422JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.RetryTask422JSONResponse(b), nil
		}
		return nil, err
	}
	return api.RetryTask202JSONResponse(op), nil
}

// ReassignTask implements api.StrictServerInterface.
func (a *TasksAPI) ReassignTask(ctx context.Context, req api.ReassignTaskRequestObject) (api.ReassignTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		var b api.ReassignTask422JSONResponseBody
		if err := b.FromHTTPValidationError(invalid("Request body is required")); err != nil {
			return nil, err
		}
		return api.ReassignTask422JSONResponse(b), nil
	}
	in := action(req.TaskId, api.TaskAction{
		ExpectedAttemptId: req.Body.ExpectedAttemptId, ExpectedVersion: req.Body.ExpectedVersion, RequestId: req.Body.RequestId,
	})
	in.TargetProfileID = req.Body.TargetProfileId
	op, err := a.Svc.Reassign(ctx, a.principal(), in)
	if err != nil {
		e, ok := refusalOf(err)
		if !ok {
			return nil, err
		}
		te := taskError(e.Reason)
		switch e.Status {
		case http.StatusForbidden:
			var b api.ReassignTask403JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.ReassignTask403JSONResponse(b), nil
		case http.StatusNotFound:
			var b api.ReassignTask404JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.ReassignTask404JSONResponse(b), nil
		case http.StatusConflict:
			return api.ReassignTask409JSONResponse(te), nil
		case http.StatusUnprocessableEntity:
			var b api.ReassignTask422JSONResponseBody
			if err := b.FromTaskError(te); err != nil {
				return nil, err
			}
			return api.ReassignTask422JSONResponse(b), nil
		}
		return nil, err
	}
	return api.ReassignTask202JSONResponse(op), nil
}

func action(taskID string, body api.TaskAction) ActionInput {
	return ActionInput{
		RequestID: body.RequestId, TaskID: taskID, ExpectedVersion: body.ExpectedVersion,
		ExpectedAttemptID: deref(body.ExpectedAttemptId),
	}
}
