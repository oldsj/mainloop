package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// taskTools are the tools of the task domain: delegate, report, task_get, task_history,
// task_list and task_cancel. They record and read durable task state; provisioning an attempt and
// delivering a report are the reconciler's work, not these handlers'.
func (s *Server) taskTools() []Tool {
	id := stringSchema(1, 100)
	taskOnly := objectSchema(map[string]any{"task_id": id}, "task_id")
	return []Tool{
		{
			Name: agentauth.ToolDelegate,
			Description: "Create a durable task and scoped attempt. Reuse request_id for lost or uncertain responses. " +
				"After a confirmed terminal blocked create, use a NEW request_id for a new create; reusing the old id " +
				"returns the blocked operation. Role and tool guidance prefixes the brief.",
			InputSchema: objectSchema(map[string]any{
				"request_id": id,
				"title":      stringSchema(1, 200),
				"brief":      stringSchema(1, 16384),
				"mode":       map[string]any{"type": "string", "enum": []any{"code", "coordination"}},
				"project_id": optional(stringSchema(1, 100)),
				"topic_id":   optional(stringSchema(1, 100)),
				"provider_profile_id": optional(map[string]any{
					"type": "string", "minLength": 1, "maxLength": 80, "pattern": "^[a-z][a-z0-9_-]*$",
				}),
				"checkout": optional(objectSchema(map[string]any{
					"branch": stringSchema(1, 255),
					"ref":    map[string]any{"type": "string", "maxLength": 255, "default": ""},
					"depth":  map[string]any{"type": "integer", "minimum": 0, "maximum": 1000, "default": 0},
				}, "branch")),
			}, "request_id", "title", "brief", "mode"),
			Handle: s.delegate,
		},
		{
			Name: agentauth.ToolReport,
			Description: "Record progress or an explicit result claim with durable parent notification. For a blocker " +
				"requiring an owner decision, use outcome=blocked and owner_action to state the specific question or " +
				"choice. Routine results are stored for task reads and Activity.",
			InputSchema: objectSchema(map[string]any{
				"task_id": id, "attempt_id": id, "request_id": id,
				"summary":      stringSchema(1, 4000),
				"outcome":      map[string]any{"type": "string", "enum": []any{"progress", "completed", "failed", "blocked"}},
				"owner_action": optional(stringSchema(1, 4000)),
				"evidence_refs": map[string]any{
					"type": "array", "maxItems": 64, "default": []any{}, "items": stringSchema(1, 2048),
				},
			}, "task_id", "attempt_id", "request_id", "summary", "outcome"),
			Handle: s.report,
		},
		{
			Name:        agentauth.ToolTaskGet,
			Description: "Read a scoped task and its stored projections without a native turn.",
			InputSchema: taskOnly, Handle: s.taskRead,
		},
		{
			Name:        agentauth.ToolTaskHistory,
			Description: "Read stored attempts, operations and reports.",
			InputSchema: taskOnly, Handle: s.taskRead,
		},
		{
			Name:        agentauth.ToolTaskList,
			Description: "List tasks visible within your persisted authority.",
			InputSchema: objectSchema(map[string]any{
				"project_id":     optional(stringSchema(1, 100)),
				"parent_task_id": optional(stringSchema(1, 100)),
			}),
			Handle: s.taskList,
		},
		{
			Name:        agentauth.ToolTaskCancel,
			Description: "Request cancellation of a managed task using its current version/attempt.",
			InputSchema: objectSchema(map[string]any{
				"request_id":          id,
				"expected_version":    map[string]any{"type": "integer", "minimum": 1},
				"expected_attempt_id": nullable(stringSchema(1, 100)),
				"task_id":             id,
			}, "request_id", "expected_version", "expected_attempt_id", "task_id"),
			Handle: s.taskCancel,
		},
	}
}

// taskPrincipal turns the authenticated actor into a task principal, re-checking a supervisor's
// or child's attempt against the database.
func (s *Server) taskPrincipal(ctx context.Context, a agentauth.Actor) (tasks.Principal, error) {
	if s.Tasks == nil {
		return tasks.Principal{}, failure("unavailable", "tasks are not available")
	}
	p, err := s.Tasks.Resolve(ctx, tasks.Principal{OwnerID: a.UserID, Role: a.Role, BindingID: a.BindingID})
	return p, taskFailure(err)
}

// taskFailure shows a task refusal as "[status] reason", as the Python server did. Other errors
// are internal and pass through.
func taskFailure(err error) error {
	var r *tasks.Error
	if errors.As(err, &r) {
		return failure(strconv.Itoa(r.Status), r.Reason)
	}
	return err
}

// structured turns an API value into the map MCP returns as structured content.
func structured(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func operationResult(op api.TaskOperation) (Result, error) {
	m, err := structured(op)
	if err != nil {
		return Result{}, err
	}
	line := fmt.Sprintf("task operation %s: %s", op.Id, op.State)
	if op.Reason != nil {
		line += fmt.Sprintf(" (%s)", *op.Reason)
	}
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) delegate(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		RequestID         string  `json:"request_id"`
		Title             string  `json:"title"`
		Brief             string  `json:"brief"`
		Mode              string  `json:"mode"`
		ProjectID         *string `json:"project_id"`
		TopicID           *string `json:"topic_id"`
		ProviderProfileID *string `json:"provider_profile_id"`
		Checkout          *struct {
			Branch string `json:"branch"`
			Ref    string `json:"ref"`
			Depth  int    `json:"depth"`
		} `json:"checkout"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	var err error
	create := tasks.CreateInput{ProjectID: str(in.ProjectID), TopicID: str(in.TopicID), ProviderProfileID: str(in.ProviderProfileID)}
	if create.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if create.Title, err = text(in.Title, 1, 200); err != nil {
		return Result{}, err
	}
	if create.Brief, err = text(in.Brief, 1, 16384); err != nil {
		return Result{}, err
	}
	switch in.Mode {
	case "code", "coordination":
		create.Mode = dbstate.TaskMode(in.Mode)
	default:
		return Result{}, errValidation
	}
	if c := in.Checkout; c != nil {
		if c.Branch == "" || len(c.Branch) > 255 || len(c.Ref) > 255 || c.Depth < 0 || c.Depth > 1000 {
			return Result{}, errValidation
		}
		create.Checkout = &tasks.Checkout{Branch: c.Branch, Ref: c.Ref, Depth: c.Depth}
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	op, err := s.Tasks.Create(ctx, p, create)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	return operationResult(op)
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

func (s *Server) report(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		TaskID       string   `json:"task_id"`
		AttemptID    string   `json:"attempt_id"`
		RequestID    string   `json:"request_id"`
		Summary      string   `json:"summary"`
		Outcome      string   `json:"outcome"`
		OwnerAction  *string  `json:"owner_action"`
		EvidenceRefs []string `json:"evidence_refs"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	var err error
	rep := tasks.ReportInput{OwnerAction: str(in.OwnerAction), EvidenceRefs: in.EvidenceRefs}
	if rep.TaskID, err = text(in.TaskID, 1, 100); err != nil {
		return Result{}, err
	}
	if rep.AttemptID, err = text(in.AttemptID, 1, 100); err != nil {
		return Result{}, err
	}
	if rep.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if rep.Summary, err = text(in.Summary, 1, 4000); err != nil {
		return Result{}, err
	}
	if in.OwnerAction != nil && rep.OwnerAction == "" || len(rep.OwnerAction) > 4000 || len(rep.EvidenceRefs) > 64 {
		return Result{}, errValidation
	}
	for _, ref := range rep.EvidenceRefs {
		if ref == "" || len(ref) > 2048 {
			return Result{}, errValidation
		}
	}
	switch o := dbstate.ReportOutcome(in.Outcome); o {
	case dbstate.OutcomeProgress, dbstate.OutcomeCompleted, dbstate.OutcomeFailed, dbstate.OutcomeBlocked:
		rep.Outcome = o
	default:
		return Result{}, errValidation
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	done, err := s.Tasks.Report(ctx, p, rep)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	m, err := structured(done.Report)
	if err != nil {
		return Result{}, err
	}
	line := fmt.Sprintf("report %s recorded: task %s is %s at version %d", rep.RequestID, rep.TaskID, done.Status, done.Version)
	m["text"], m["task_version"], m["task_status"], m["replayed"] = line, done.Version, string(done.Status), done.Replayed
	return Result{Text: line, Structured: m}, nil
}

// taskRead serves task_get and task_history: one read of the task view. The history is the part
// of it that records what happened.
func (s *Server) taskRead(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		TaskID string `json:"task_id"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	id, err := text(in.TaskID, 1, 100)
	if err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	v, err := s.Tasks.Get(ctx, p, id)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	m, err := structured(v)
	if err != nil {
		return Result{}, err
	}
	line := fmt.Sprintf("%s: %s", v.Task.Title, v.Task.Status)
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) taskList(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		ProjectID    *string `json:"project_id"`
		ParentTaskID *string `json:"parent_task_id"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	for _, v := range []*string{in.ProjectID, in.ParentTaskID} {
		if v != nil {
			if _, err := text(*v, 1, 100); err != nil {
				return Result{}, err
			}
		}
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	views, err := s.Tasks.List(ctx, p, str(in.ProjectID), str(in.ParentTaskID))
	if err != nil {
		return Result{}, taskFailure(err)
	}
	m, err := structured(map[string]any{"tasks": views})
	if err != nil {
		return Result{}, err
	}
	lines := make([]string, len(views))
	for i, v := range views {
		lines[i] = fmt.Sprintf("%s %s: %s", v.Task.Id, v.Task.Title, v.Task.Status)
	}
	line := strings.Join(lines, "\n")
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) taskCancel(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		RequestID         string  `json:"request_id"`
		ExpectedVersion   int     `json:"expected_version"`
		ExpectedAttemptID *string `json:"expected_attempt_id"`
		TaskID            string  `json:"task_id"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	var err error
	act := tasks.ActionInput{ExpectedVersion: in.ExpectedVersion, ExpectedAttemptID: str(in.ExpectedAttemptID)}
	if in.ExpectedVersion < 1 || in.ExpectedAttemptID != nil && act.ExpectedAttemptID == "" {
		return Result{}, errValidation
	}
	if act.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if act.TaskID, err = text(in.TaskID, 1, 100); err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	op, err := s.Tasks.Cancel(ctx, p, act)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	return operationResult(op)
}

// taskIdentity is the task part of whoami for a supervisor or child.
func (s *Server) taskIdentity(ctx context.Context, a agentauth.Actor) (map[string]any, error) {
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return nil, err
	}
	id, err := s.Tasks.Identity(ctx, p)
	if err != nil {
		return nil, taskFailure(err)
	}
	return map[string]any{
		"task_id": id.TaskID, "attempt_id": id.AttemptID, "root_task_id": id.RootTaskID,
		"parent_task_id": id.ParentTaskID, "workspace_id": id.WorkspaceID,
		"writer_generation": id.WriterGeneration, "attempt_number": id.AttemptNumber,
	}, nil
}
