package tasks

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
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
		Model: deref(body.Model), Effort: derefEffort(body.Effort), UnderSupervisor: true,
		WebFetch: body.WebFetch != nil && *body.WebFetch,
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

// FollowUpTask implements api.StrictServerInterface.
func (a *TasksAPI) FollowUpTask(ctx context.Context, req api.FollowUpTaskRequestObject) (api.FollowUpTaskResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	res, err := a.Svc.FollowUp(ctx, a.principal(), FollowUpInput{TaskID: req.TaskId, RequestID: req.Body.RequestId, Text: req.Body.Text})
	if err != nil {
		return nil, taskReply(err, actionRefusals...)
	}
	return api.FollowUpTask202JSONResponse(res.API()), nil
}

func action(taskID string, body api.TaskAction) ActionInput {
	return ActionInput{
		RequestID: body.RequestId, TaskID: taskID, ExpectedVersion: body.ExpectedVersion,
		ExpectedAttemptID: deref(body.ExpectedAttemptId),
	}
}

// loopRefusals are the statuses the loop controls declare.
var loopRefusals = []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity}

// ListTaskLoops implements api.StrictServerInterface.
func (a *TasksAPI) ListTaskLoops(ctx context.Context, req api.ListTaskLoopsRequestObject) (api.ListTaskLoopsResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	loops, err := a.Svc.TaskLoops(ctx, a.principal(), req.TaskId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound)
	}
	return api.ListTaskLoops200JSONResponse(api.TaskLoopList{Loops: loops}), nil
}

// ListMainLoops implements api.StrictServerInterface.
func (a *TasksAPI) ListMainLoops(ctx context.Context, _ api.ListMainLoopsRequestObject) (api.ListMainLoopsResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	loops, err := a.Svc.MainLoops(ctx, a.principal())
	if err != nil {
		return nil, taskReply(err)
	}
	return api.ListMainLoops200JSONResponse(api.TaskLoopList{Loops: loops}), nil
}

// PauseLoop implements api.StrictServerInterface.
func (a *TasksAPI) PauseLoop(ctx context.Context, req api.PauseLoopRequestObject) (api.PauseLoopResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	l, err := a.Svc.PauseLoop(ctx, a.principal(), req.LoopId)
	if err != nil {
		return nil, taskReply(err, loopRefusals...)
	}
	return api.PauseLoop200JSONResponse(l), nil
}

// ResumeLoop implements api.StrictServerInterface.
func (a *TasksAPI) ResumeLoop(ctx context.Context, req api.ResumeLoopRequestObject) (api.ResumeLoopResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	l, err := a.Svc.ResumeLoop(ctx, a.principal(), req.LoopId)
	if err != nil {
		return nil, taskReply(err, loopRefusals...)
	}
	return api.ResumeLoop200JSONResponse(l), nil
}

// DeleteLoop implements api.StrictServerInterface.
func (a *TasksAPI) DeleteLoop(ctx context.Context, req api.DeleteLoopRequestObject) (api.DeleteLoopResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	l, err := a.Svc.DeleteLoop(ctx, a.principal(), req.LoopId)
	if err != nil {
		return nil, taskReply(err, loopRefusals...)
	}
	return api.DeleteLoop200JSONResponse(l), nil
}

// FireLoop implements api.StrictServerInterface.
func (a *TasksAPI) FireLoop(ctx context.Context, req api.FireLoopRequestObject) (api.FireLoopResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	l, err := a.Svc.FireLoop(ctx, a.principal(), req.LoopId, req.Body.RequestId)
	if err != nil {
		return nil, taskReply(err, loopRefusals...)
	}
	return api.FireLoop200JSONResponse(l), nil
}

// EnsureProjectSupervisor implements api.StrictServerInterface: it starts the project's standing
// supervisor on first use and answers its task.
func (a *TasksAPI) EnsureProjectSupervisor(ctx context.Context, req api.EnsureProjectSupervisorRequestObject) (api.EnsureProjectSupervisorResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	view, err := a.Svc.EnsureSupervisor(ctx, a.OwnerID, req.ProjectId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound, http.StatusUnprocessableEntity)
	}
	return api.EnsureProjectSupervisor200JSONResponse(view), nil
}

// RestartProjectSupervisor implements api.StrictServerInterface: a restart (or, fresh, a reset) of
// the project's standing supervisor that keeps its children.
func (a *TasksAPI) RestartProjectSupervisor(ctx context.Context, req api.RestartProjectSupervisorRequestObject) (api.RestartProjectSupervisorResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	fresh := req.Body.Fresh != nil && *req.Body.Fresh
	op, err := a.Svc.RestartSupervisor(ctx, a.OwnerID, req.ProjectId, req.Body.RequestId, fresh)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity)
	}
	return api.RestartProjectSupervisor202JSONResponse(op), nil
}

// RetireProjectSupervisor implements api.StrictServerInterface: the owner ends the project's
// standing supervisor and its children.
func (a *TasksAPI) RetireProjectSupervisor(ctx context.Context, req api.RetireProjectSupervisorRequestObject) (api.RetireProjectSupervisorResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	op, err := a.Svc.RetireSupervisor(ctx, a.OwnerID, req.ProjectId, req.Body.RequestId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity)
	}
	return api.RetireProjectSupervisor202JSONResponse(op), nil
}

func supervisorSettingsView(v SupervisorSettings) api.SupervisorSettings {
	out := api.SupervisorSettings{
		ProjectId: v.ProjectID, ProviderProfileId: v.ProviderProfileID, Model: v.Model,
		EffectiveProviderProfileId: v.EffectiveProfileID, OperationId: v.OperationID,
		AllowedModels: append([]string{}, v.AllowedModels...), AllowedEffortLevels: append([]string{}, v.AllowedEffortLevels...),
	}
	if v.Effort != nil {
		e := api.EffortLevel(*v.Effort)
		out.Effort = &e
	}
	return out
}

// GetProjectSupervisorSettings implements api.StrictServerInterface.
func (a *TasksAPI) GetProjectSupervisorSettings(ctx context.Context, req api.GetProjectSupervisorSettingsRequestObject) (api.GetProjectSupervisorSettingsResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	v, err := a.Svc.SupervisorSettings(ctx, a.OwnerID, req.ProjectId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound)
	}
	return api.GetProjectSupervisorSettings200JSONResponse(supervisorSettingsView(v)), nil
}

// UpdateProjectSupervisorSettings implements api.StrictServerInterface: the owner replaces the
// project's supervisor provider, model and effort.
func (a *TasksAPI) UpdateProjectSupervisorSettings(ctx context.Context, req api.UpdateProjectSupervisorSettingsRequestObject) (api.UpdateProjectSupervisorSettingsResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	c := SupervisorChoice{ProviderProfileID: req.Body.ProviderProfileId, Model: req.Body.Model}
	if e := req.Body.Effort; e != nil {
		v := string(*e)
		c.Effort = &v
	}
	v, err := a.Svc.SetSupervisorSettings(ctx, a.OwnerID, req.ProjectId, c, LimitChanger{})
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity)
	}
	return api.UpdateProjectSupervisorSettings200JSONResponse(supervisorSettingsView(v)), nil
}

func workerLimitView(w WorkerLimit) api.WorkerLimit {
	return api.WorkerLimit{
		ProjectId: w.ProjectID, MaxWorkers: w.Max, DefaultMaxWorkers: w.Default,
		EffectiveMaxWorkers: w.Effective, GlobalMaxWorkers: w.Global, WorkersRunning: w.Running,
	}
}

// GetProjectWorkerLimit implements api.StrictServerInterface.
func (a *TasksAPI) GetProjectWorkerLimit(ctx context.Context, req api.GetProjectWorkerLimitRequestObject) (api.GetProjectWorkerLimitResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.WorkerLimit(ctx, a.OwnerID, req.ProjectId)
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound)
	}
	return api.GetProjectWorkerLimit200JSONResponse(workerLimitView(w)), nil
}

// UpdateProjectWorkerLimit implements api.StrictServerInterface: the owner sets the project's own
// worker limit, or with null returns it to the default.
func (a *TasksAPI) UpdateProjectWorkerLimit(ctx context.Context, req api.UpdateProjectWorkerLimitRequestObject) (api.UpdateProjectWorkerLimitResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return nil, apierr.Invalid("Request body is required")
	}
	w, err := a.Svc.SetWorkerLimit(ctx, a.OwnerID, req.ProjectId, req.Body.MaxWorkers, LimitChanger{})
	if err != nil {
		return nil, taskReply(err, http.StatusNotFound, http.StatusUnprocessableEntity)
	}
	return api.UpdateProjectWorkerLimit200JSONResponse(workerLimitView(w)), nil
}

// GetTaskScreenshot implements api.StrictServerInterface.
func (a *TasksAPI) GetTaskScreenshot(ctx context.Context, req api.GetTaskScreenshotRequestObject) (api.GetTaskScreenshotResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	s, err := dbq.New(a.Svc.Pool).GetTaskReportScreenshot(ctx, dbq.GetTaskReportScreenshotParams{
		ID: req.ScreenshotId, OwnerID: a.OwnerID, TaskID: req.TaskId,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.NotFound("Screenshot not found")
	}
	if err != nil {
		return nil, err
	}
	// A screenshot never changes once stored.
	cache := "private, max-age=31536000, immutable"
	return api.GetTaskScreenshot200JSONResponse{
		Body: api.TaskScreenshot{
			Id: s.ID, MediaType: api.TaskScreenshotMediaType(s.MediaType), Data: s.Data,
			Viewport: api.TaskScreenshotViewport(s.Viewport), Port: int(s.Port), Path: s.Path,
			Width: int(s.Width), Height: int(s.Height), CreatedAt: s.CreatedAt,
		},
		Headers: api.GetTaskScreenshot200ResponseHeaders{CacheControl: &cache},
	}, nil
}
