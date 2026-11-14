package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
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
				"returns the blocked operation. Role and tool guidance prefixes the brief. " +
				"Choosing model and effort: match the tier to the task's scope, risk and reversibility, not habit. " +
				"Mechanical or bounded, easily-reversible work takes the cheapest model and lowest effort likely to " +
				"succeed; design, debugging, security-sensitive review, or anything hard to undo takes the strongest " +
				"model and highest effort the profile offers. When re-delegating a failed attempt, prefer a higher " +
				"tier over repeating the same one. Read the resolved provider profile's own models and effort_levels " +
				"(see the providers list) rather than assuming any provider's names; leaving both unset keeps the " +
				"profile's defaults.",
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
				"model": optional(map[string]any{
					"type": "string", "pattern": "^[A-Za-z0-9._:/-]{1,200}$",
				}),
				"effort": optional(map[string]any{
					"type": "string", "enum": []any{"minimal", "low", "medium", "high", "xhigh", "max"},
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
		{
			Name: agentauth.ToolTaskHold,
			Description: "Put a task on hold: it leaves the owner's Working and Needs you views until the hold ends, " +
				"then Mainloop sends you one message to look at it again. Give minutes (1 to 10080) or until (RFC 3339, " +
				"within 7 days), and a short reason; clear: true ends a hold now. The task keeps running. A hold ends early, " +
				"with no message from this tool, when the task fails, blocks or ends or anything needs the owner about it; " +
				"a task in one of those states cannot be held.",
			InputSchema: objectSchema(map[string]any{
				"task_id": id,
				"minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": int(tasks.MaxHold.Minutes())},
				"until":   map[string]any{"type": "string", "format": "date-time"},
				"reason":  stringSchema(1, tasks.MaxHoldReason),
				"clear":   map[string]any{"type": "boolean"},
			}, "task_id"),
			Handle: s.taskHold,
		},
		{
			Name: agentauth.ToolSteer,
			Description: "Send a message into a managed task's running turn; it joins at the next tool boundary. " +
				"Where the turn cannot take it (none running, waiting on input, an agent without steering) it is queued " +
				"as the task's next message instead; the result says steered, queued or sent. Reuse request_id to retry.",
			InputSchema: objectSchema(map[string]any{
				"task_id":    id,
				"request_id": id,
				"text":       stringSchema(1, tasks.MaxSteerChars),
			}, "task_id", "request_id", "text"),
			Handle: s.taskSteer,
		},
	}
}

func (s *Server) taskSteer(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		TaskID    string `json:"task_id"`
		RequestID string `json:"request_id"`
		Text      string `json:"text"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	var st tasks.SteerInput
	var err error
	if st.TaskID, err = text(in.TaskID, 1, 100); err != nil {
		return Result{}, err
	}
	if st.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if st.Text, err = text(in.Text, 1, tasks.MaxSteerChars); err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	res, err := s.Tasks.Steer(ctx, p, st)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	line := fmt.Sprintf("message %s to task %s: %s", res.MessageID, res.TaskID, res.How)
	return Result{Text: line, Structured: map[string]any{
		"task_id": res.TaskID, "attempt_id": res.AttemptID, "message_id": res.MessageID, "delivery": res.How, "text": line,
	}}, nil
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
		reason := r.Reason
		switch reason {
		case tasks.ReasonEnvironmentRequired:
			reason += ": the owner must select a development environment on the project page (Environment section), then delegate again"
		case tasks.ReasonModelUnavailable:
			reason += fmt.Sprintf(": allowed models are %v", r.Detail["allowed_models"])
		case tasks.ReasonEffortUnavailable:
			reason += fmt.Sprintf(": allowed effort levels are %v", r.Detail["allowed_effort_levels"])
		}
		return failure(strconv.Itoa(r.Status), reason)
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
		Model             *string `json:"model"`
		Effort            *string `json:"effort"`
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
	create := tasks.CreateInput{
		ProjectID: str(in.ProjectID), TopicID: str(in.TopicID), ProviderProfileID: str(in.ProviderProfileID),
		Model: str(in.Model), Effort: str(in.Effort),
	}
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

func (s *Server) taskHold(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		TaskID  string `json:"task_id"`
		Minutes int    `json:"minutes"`
		Until   string `json:"until"`
		Reason  string `json:"reason"`
		Clear   bool   `json:"clear"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	var h tasks.HoldInput
	var err error
	if h.TaskID, err = text(in.TaskID, 1, 100); err != nil {
		return Result{}, err
	}
	// Exactly one of minutes, until and clear.
	given := 0
	for _, b := range []bool{in.Minutes != 0, in.Until != "", in.Clear} {
		if b {
			given++
		}
	}
	if given != 1 || in.Minutes < 0 || len(in.Reason) > tasks.MaxHoldReason || in.Clear && in.Reason != "" {
		return Result{}, errValidation
	}
	switch {
	case in.Minutes > 0:
		h.Until = time.Now().Add(time.Duration(in.Minutes) * time.Minute)
	case in.Until != "":
		if h.Until, err = time.Parse(time.RFC3339, in.Until); err != nil {
			return Result{}, errValidation
		}
	}
	h.Reason = in.Reason
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	t, err := s.Tasks.Hold(ctx, p, h)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	m, err := structured(t)
	if err != nil {
		return Result{}, err
	}
	line := fmt.Sprintf("task %s is no longer held", t.Id)
	if t.HeldUntil != nil {
		line = fmt.Sprintf("task %s is held until %s; you will get a message then", t.Id, t.HeldUntil.UTC().Format(time.RFC3339))
	}
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
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
