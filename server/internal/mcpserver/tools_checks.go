package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/ci"
)

// checkTools are the CI tools. check_status reads only; watch_checks records a durable watch (the
// idempotent request_id makes a retry find the same one) that the watch loop in serve-api settles
// and delivers. The service is nil in a process that has no GitHub App, and then the tools answer
// that they are unavailable.
func (s *Server) checkTools() []Tool {
	return []Tool{
		{
			Name: agentauth.ToolCheckStatus,
			Description: "Read the CI checks on a commit of the bound project's repository, now, without waiting: " +
				"give pr_number (its current head) or commit_sha (40 hex; works before a pull request exists). " +
				"Returns state green, failed, pending or no_checks, the checks that are not green, and for up to five " +
				"failed checks their own output and annotations. It cannot rerun, cancel or trigger a check.",
			InputSchema: objectSchema(map[string]any{
				"project_id": stringSchema(1, 100),
				"pr_number":  map[string]any{"type": "integer", "exclusiveMinimum": 0},
				"commit_sha": map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"},
			}, "project_id"),
			Handle: s.checkStatus,
		},
		{
			Name: agentauth.ToolWatchChecks,
			Description: "Wait for CI without holding your turn: give pr_number or commit_sha and a stable request_id. " +
				"Returns at once; when nothing is pending any more (or at the timeout, default 30 minutes) Mainloop sends you " +
				"one message with the outcome (green, failed, pending_timeout, no_checks or not_found) and why checks failed. " +
				"End your turn after calling it. A newer watch of the same ref replaces an older one.",
			InputSchema: objectSchema(map[string]any{
				"project_id":      stringSchema(1, 100),
				"pr_number":       map[string]any{"type": "integer", "exclusiveMinimum": 0},
				"commit_sha":      map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"},
				"request_id":      stringSchema(1, 100),
				"timeout_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": int(ci.MaxWatchTimeout.Minutes())},
			}, "project_id", "request_id"),
			Handle: s.watchChecks,
		},
	}
}

func (s *Server) watchChecks(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID      string `json:"project_id"`
		PRNumber       int    `json:"pr_number"`
		CommitSHA      string `json:"commit_sha"`
		RequestID      string `json:"request_id"`
		TimeoutMinutes int    `json:"timeout_minutes"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return Result{}, err
	}
	if in.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	ref := ci.Ref{PR: in.PRNumber, SHA: in.CommitSHA}
	if !ref.Valid() || in.TimeoutMinutes < 0 || in.TimeoutMinutes > int(ci.MaxWatchTimeout.Minutes()) {
		return Result{}, errValidation
	}
	if s.CI == nil {
		return Result{}, failure("unavailable", "CI checks are not available")
	}
	if err := projectAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	w, err := s.CI.StartWatch(ctx, ci.WatchInput{
		OwnerID: a.UserID, ProjectID: in.ProjectID, SessionID: a.BindingID, Ref: ref, RequestID: in.RequestID,
		Timeout: time.Duration(in.TimeoutMinutes) * time.Minute,
	})
	if err != nil {
		return Result{}, ciFailure(err)
	}
	line := fmt.Sprintf("Watching CI (watch %s). One message arrives when it settles, by %s at the latest. End your turn now.",
		w.ID, w.Deadline.UTC().Format(time.RFC3339))
	m := map[string]any{
		"watch_id": w.ID, "state": string(w.State), "deadline": w.Deadline.UTC().Format(time.RFC3339),
		"superseded": w.Superseded, "text": line,
	}
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) checkStatus(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string `json:"project_id"`
		PRNumber  int    `json:"pr_number"`
		CommitSHA string `json:"commit_sha"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return Result{}, err
	}
	ref := ci.Ref{PR: in.PRNumber, SHA: in.CommitSHA}
	if !ref.Valid() {
		return Result{}, errValidation
	}
	if s.CI == nil {
		return Result{}, failure("unavailable", "CI checks are not available")
	}
	if err := projectAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	st, err := s.CI.Status(ctx, a.UserID, in.ProjectID, ref)
	if err != nil {
		return Result{}, ciFailure(err)
	}
	line := ci.Render(st)
	return Result{Text: line, Structured: statusMap(st, line)}, nil
}

// statusMap is a status as the tools' structured answer.
func statusMap(st ci.Status, line string) map[string]any {
	checks := make([]map[string]any, 0, len(st.Checks))
	for _, c := range st.Checks {
		checks = append(checks, map[string]any{
			"name": c.Name, "kind": c.Kind, "app": c.App, "status": c.Status, "result": c.Result, "url": c.URL,
			"started_at": c.StartedAt, "completed_at": c.CompletedAt, "summary": c.Summary,
		})
	}
	failures := make([]map[string]any, 0, len(st.Failures))
	for _, f := range st.Failures {
		ann := make([]map[string]any, 0, len(f.Annotations))
		for _, x := range f.Annotations {
			ann = append(ann, map[string]any{
				"path": x.Path, "start_line": x.StartLine, "end_line": x.EndLine, "level": x.Level,
				"title": x.Title, "message": x.Message,
			})
		}
		failures = append(failures, map[string]any{
			"name": f.Name, "url": f.URL, "summary": f.Summary, "text": f.Text, "annotations": ann, "log_tail": f.LogTail,
		})
	}
	m := map[string]any{
		"state": string(st.State), "sha": st.SHA,
		"counts":   map[string]any{"passed": st.Counts.Passed, "failed": st.Counts.Failed, "pending": st.Counts.Pending},
		"checks":   checks,
		"failures": failures, "failures_omitted": st.FailuresOmitted,
		"text": line,
	}
	if st.PR > 0 {
		m["pr_number"] = st.PR
	}
	return m
}

func ciFailure(err error) error {
	var r *ci.Refusal
	switch {
	case errors.As(err, &r):
		return failure(r.Code, r.Message)
	case errors.Is(err, ci.ErrProjectNotFound):
		return failure("not_found", "no such project")
	}
	return err
}
