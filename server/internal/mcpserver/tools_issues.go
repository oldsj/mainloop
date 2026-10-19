package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/issues"
)

// issueTools are the read-only GitHub issue and Dependabot-alert tools. Unlike the pull-request
// tools, these write no durable intent row: a read that fails is simply retried by the caller.
// The services are nil in a process that has no GitHub App, and then the tools answer that they
// are unavailable.
func (s *Server) issueTools() []Tool {
	id := stringSchema(1, 100)
	return []Tool{
		{
			Name: agentauth.ToolGetIssue,
			Description: "Read one GitHub issue of the bound project's repository: title, body, state and labels. " +
				"Refuses an issue number that names a pull request instead.",
			InputSchema: objectSchema(map[string]any{
				"project_id":   id,
				"issue_number": map[string]any{"type": "integer", "exclusiveMinimum": 0},
			}, "project_id", "issue_number"),
			Handle: s.getIssue,
		},
		{
			Name:        agentauth.ToolListDependabotAlerts,
			Description: "List open Dependabot security alerts for the bound project's repository.",
			InputSchema: objectSchema(map[string]any{"project_id": id}, "project_id"),
			Handle:      s.listDependabotAlerts,
		},
	}
}

func (s *Server) getIssue(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID   string `json:"project_id"`
		IssueNumber int    `json:"issue_number"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return Result{}, err
	}
	if in.IssueNumber <= 0 {
		return Result{}, errValidation
	}
	if s.Issues == nil {
		return Result{}, failure("unavailable", "issues are not available")
	}
	if err := projectAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	issue, err := s.Issues.GetIssue(ctx, a.UserID, in.ProjectID, in.IssueNumber)
	if err != nil {
		return Result{}, issuesFailure(err)
	}
	m := map[string]any{
		"number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State,
		"labels": issue.Labels, "url": issue.HTMLURL,
	}
	line := fmt.Sprintf("issue #%d: %s (%s)", issue.Number, issue.Title, issue.State)
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) listDependabotAlerts(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string `json:"project_id"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return Result{}, err
	}
	if s.Issues == nil {
		return Result{}, failure("unavailable", "issues are not available")
	}
	if err := projectAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	alerts, err := s.Issues.ListDependabotAlerts(ctx, a.UserID, in.ProjectID)
	if err != nil {
		return Result{}, issuesFailure(err)
	}
	list := make([]map[string]any, 0, len(alerts))
	for _, al := range alerts {
		list = append(list, map[string]any{
			"package": al.PackageName, "ecosystem": al.Ecosystem, "severity": al.Severity,
			"vulnerable_version_range": al.VulnerableVersionRange, "patched_version": al.PatchedVersion,
			"advisory_ghsa_id": al.AdvisoryGHSAID, "advisory_summary": al.AdvisorySummary, "url": al.HTMLURL,
		})
	}
	m := map[string]any{"alerts": list}
	line := fmt.Sprintf("%d open Dependabot alert(s)", len(list))
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func issuesFailure(err error) error {
	var r *issues.Refusal
	switch {
	case errors.As(err, &r):
		return failure(r.Code, r.Message)
	case errors.Is(err, issues.ErrNotFound):
		return failure("not_found", "no such project")
	}
	return err
}
