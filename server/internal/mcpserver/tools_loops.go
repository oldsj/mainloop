package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// loopTools arm, list and stop loops (docs/specs/loops.md): durable wake-ups for main and
// supervisors, so they can end their turn instead of polling.
func (s *Server) loopTools() []Tool {
	id := stringSchema(1, 100)
	minutes := map[string]any{"type": "integer", "minimum": 1, "maximum": tasks.MaxLoopMinutes}
	statuses := make([]any, 0, 7)
	for _, st := range []dbstate.TaskStatus{
		dbstate.TaskQueued, dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskBlocked,
		dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled,
	} {
		statuses = append(statuses, string(st))
	}
	conditions := make([]any, 0, len(dbstate.PRConditions))
	for _, c := range dbstate.PRConditions {
		conditions = append(conditions, string(c))
	}
	return []Tool{
		{
			Name: agentauth.ToolLoopArm,
			Description: "Arm a loop: a durable wake-up that sends you a message when something happens, so you can end your " +
				"turn instead of polling. trigger=task fires when a task you manage changes status or reason (statuses limits " +
				"it to those statuses; a task already in one fires at once). trigger=pull_request fires when a code task's pull " +
				"request changes: opened, its state, or CI's result (pr_conditions ci_success, ci_failure, merged, closed limits " +
				"it; CI counts at the pull request's head; a condition that already holds fires at once). trigger=quiet fires " +
				"when a task you manage has not changed for quiet_minutes (no status, report, pull request or turn activity), " +
				"then again after each further quiet period. trigger=time fires after after_minutes, then every " +
				"every_minutes if given; with task_id its message says how that task stands. expect_change=true (quiet, or " +
				"time with task_id) is for a loop waiting on progress, such as a repair: three firings in a row that find the " +
				"task unchanged stop the loop and ask the owner. Leave it false for a periodic check-in that may well see no " +
				"change. note is what the message reminds you to do. A firing waits for your turn to end and comes " +
				"with your children's reports as one message; what happens meanwhile folds into it. Limits: max_firings " +
				"(default 20), min_interval_seconds between firings (default 60), expires_minutes (default 1440, at most 10080), " +
				"at most 20 armed loops. A supervisor's loops end with its task. Reuse request_id to retry.",
			InputSchema: objectSchema(map[string]any{
				"request_id": id,
				"trigger":    map[string]any{"type": "string", "enum": []any{"task", "pull_request", "quiet", "time"}},
				"task_id":    optional(id),
				"statuses": map[string]any{
					"type": "array", "maxItems": len(statuses), "default": []any{},
					"items": map[string]any{"type": "string", "enum": statuses},
				},
				"pr_conditions": map[string]any{
					"type": "array", "maxItems": len(conditions), "default": []any{},
					"items": map[string]any{"type": "string", "enum": conditions},
				},
				"quiet_minutes":        optional(minutes),
				"expect_change":        map[string]any{"type": "boolean", "default": false},
				"after_minutes":        optional(minutes),
				"every_minutes":        optional(minutes),
				"note":                 stringSchema(1, tasks.MaxLoopNote),
				"max_firings":          optional(map[string]any{"type": "integer", "minimum": 1, "maximum": tasks.MaxLoopFirings}),
				"min_interval_seconds": optional(map[string]any{"type": "integer", "minimum": 0, "maximum": int(tasks.MaxLoopMinInterval.Seconds())}),
				"expires_minutes":      optional(minutes),
			}, "request_id", "trigger", "note"),
			Handle: s.loopArm,
		},
		{
			Name:        agentauth.ToolLoopList,
			Description: "List your loops, armed ones first, with each loop's firing count and next timer.",
			InputSchema: objectSchema(map[string]any{}),
			Handle:      s.loopList,
		},
		{
			Name:        agentauth.ToolLoopStop,
			Description: "Stop one of your armed loops. A firing it already made is still delivered.",
			InputSchema: objectSchema(map[string]any{"loop_id": id}, "loop_id"),
			Handle:      s.loopStop,
		},
	}
}

func (s *Server) loopArm(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		RequestID      string   `json:"request_id"`
		Trigger        string   `json:"trigger"`
		TaskID         *string  `json:"task_id"`
		Statuses       []string `json:"statuses"`
		PRConditions   []string `json:"pr_conditions"`
		AfterMinutes   *int     `json:"after_minutes"`
		EveryMinutes   *int     `json:"every_minutes"`
		QuietMinutes   *int     `json:"quiet_minutes"`
		ExpectChange   bool     `json:"expect_change"`
		Note           string   `json:"note"`
		MaxFirings     *int     `json:"max_firings"`
		MinInterval    *int     `json:"min_interval_seconds"`
		ExpiresMinutes *int     `json:"expires_minutes"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	var li tasks.LoopInput
	var err error
	if li.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if li.Note, err = text(in.Note, 1, tasks.MaxLoopNote); err != nil {
		return Result{}, err
	}
	li.Trigger = dbstate.LoopTrigger(in.Trigger)
	if in.TaskID != nil {
		if li.TaskID, err = text(*in.TaskID, 1, 100); err != nil {
			return Result{}, err
		}
	}
	for _, st := range in.Statuses {
		li.Statuses = append(li.Statuses, dbstate.TaskStatus(st))
	}
	for _, c := range in.PRConditions {
		li.PRConditions = append(li.PRConditions, dbstate.PRCondition(c))
	}
	positive := func(p *int) (int, error) {
		if p == nil {
			return 0, nil
		}
		if *p < 1 {
			return 0, errValidation
		}
		return *p, nil
	}
	if li.AfterMinutes, err = positive(in.AfterMinutes); err != nil {
		return Result{}, err
	}
	if li.EveryMinutes, err = positive(in.EveryMinutes); err != nil {
		return Result{}, err
	}
	if li.QuietMinutes, err = positive(in.QuietMinutes); err != nil {
		return Result{}, err
	}
	li.ExpectChange = in.ExpectChange
	if li.MaxFirings, err = positive(in.MaxFirings); err != nil {
		return Result{}, err
	}
	if li.ExpiresMinutes, err = positive(in.ExpiresMinutes); err != nil {
		return Result{}, err
	}
	if in.MinInterval != nil {
		d := time.Duration(*in.MinInterval) * time.Second
		li.MinInterval = &d
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	l, err := s.Tasks.Arm(ctx, p, li)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	m := loopView(l)
	line := loopLine(l)
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) loopList(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct{}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	ls, err := s.Tasks.Loops(ctx, p)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	views := make([]any, 0, len(ls))
	lines := make([]string, 0, len(ls))
	for _, l := range ls {
		views = append(views, loopView(l))
		lines = append(lines, loopLine(l))
	}
	line := "no loops"
	if len(lines) > 0 {
		line = strings.Join(lines, "\n")
	}
	return Result{Text: line, Structured: map[string]any{"loops": views, "text": line}}, nil
}

func (s *Server) loopStop(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		LoopID string `json:"loop_id"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	id, err := text(in.LoopID, 1, 100)
	if err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	l, err := s.Tasks.StopLoop(ctx, p, id)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	m := loopView(l)
	line := loopLine(l)
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func loopView(l dbq.TaskLoop) map[string]any {
	m := map[string]any{
		"id": l.ID, "state": string(l.State), "trigger": string(l.Trigger), "task_id": l.SubjectTaskID,
		"statuses": l.Statuses, "pr_conditions": l.PrConditions, "note": l.Note, "firings": l.Firings, "max_firings": l.MaxFirings,
		"min_interval_seconds": l.MinIntervalSeconds, "expires_at": l.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at": l.CreatedAt.UTC().Format(time.RFC3339), "every_minutes": nil, "next_fire_at": nil,
		"quiet_minutes": nil, "expect_change": l.ExpectChange, "stop_reason": l.StopReason,
	}
	if l.QuietSeconds != nil {
		m["quiet_minutes"] = *l.QuietSeconds / 60
	}
	if l.EverySeconds != nil {
		m["every_minutes"] = *l.EverySeconds / 60
	}
	if l.FireAt != nil && l.State == dbstate.LoopArmed {
		m["next_fire_at"] = l.FireAt.UTC().Format(time.RFC3339)
	}
	return m
}

func loopLine(l dbq.TaskLoop) string {
	what := "timer"
	switch l.Trigger {
	case dbstate.LoopTriggerTask:
		what = "task " + *l.SubjectTaskID
		if len(l.Statuses) > 0 {
			what += " reaching " + strings.Join(l.Statuses, "/")
		}
	case dbstate.LoopTriggerQuiet:
		what = fmt.Sprintf("task %s quiet for %d minutes", *l.SubjectTaskID, *l.QuietSeconds/60)
	case dbstate.LoopTriggerPullRequest:
		what = "pull request of task " + *l.SubjectTaskID
		if len(l.PrConditions) > 0 {
			what += " on " + strings.Join(l.PrConditions, "/")
		}
	}
	line := fmt.Sprintf("loop %s (%s) is %s: %d of %d firings", l.ID, what, l.State, l.Firings, l.MaxFirings)
	if l.FireAt != nil && l.State == dbstate.LoopArmed {
		line += ", next at " + l.FireAt.UTC().Format(time.RFC3339)
	}
	if l.StopReason != nil {
		line += ", stopped for " + *l.StopReason
	}
	return line
}
