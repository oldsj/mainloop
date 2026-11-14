package mcpserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/ci"
)

// checkTools are the CI tools. They read only and write no durable intent row: a read that fails
// is simply retried by the caller. The service is nil in a process that has no GitHub App, and then
// the tools answer that they are unavailable.
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
	}
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
