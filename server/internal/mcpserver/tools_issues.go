package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/issues"
)

// issueTools are the GitHub issue and Dependabot-alert tools. The reads write no durable intent
// row: a read that fails is simply retried by the caller. create_issue and comment_issue write an
// intent row first, like open_pull_request, and the API process's loop posts at most once. The
// service is nil in a process that has no GitHub App, and then the tools answer that they are
// unavailable.
func (s *Server) issueTools() []Tool {
	id := stringSchema(1, 100)
	issueNumber := map[string]any{"type": "integer", "exclusiveMinimum": 0}
	return []Tool{
		{
			Name: agentauth.ToolGetIssue,
			Description: "Read one GitHub issue of the bound project's repository: title, body, state and labels. " +
				"Refuses an issue number that names a pull request instead.",
			InputSchema: objectSchema(map[string]any{
				"project_id":   id,
				"issue_number": issueNumber,
			}, "project_id", "issue_number"),
			Handle: s.getIssue,
		},
		s.pullReadTool(),
		{
			Name: agentauth.ToolCreateIssue,
			Description: "File a GitHub issue in a project's repository through Mainloop's GitHub App; returns its number and URL. " +
				"Labels must already exist in the repository. Supply a stable request_id: a retry with it never files twice. " +
				"If the issue already exists, add to it with comment_issue instead.",
			InputSchema: objectSchema(map[string]any{
				"project_id": id,
				"title":      stringSchema(1, 256),
				"body":       stringSchema(0, maxIssueBody),
				"labels": map[string]any{
					"type": "array", "items": stringSchema(1, 50), "maxItems": issues.MaxLabels,
				},
				"request_id": id,
			}, "project_id", "title", "body", "request_id"),
			Handle: s.createIssue,
		},
		{
			Name: agentauth.ToolCommentIssue,
			Description: "Add a comment to an existing GitHub issue (not a pull request) in a project's repository; returns the " +
				"comment's URL. Supply a stable request_id: a retry with it never comments twice.",
			InputSchema: objectSchema(map[string]any{
				"project_id":   id,
				"issue_number": issueNumber,
				"body":         stringSchema(1, maxIssueBody),
				"request_id":   id,
			}, "project_id", "issue_number", "body", "request_id"),
			Handle: s.commentIssue,
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
	if err := issueAuthority(a, in.ProjectID); err != nil {
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

// maxIssueBody bounds an issue or comment body, in bytes. GitHub takes 65536 characters; the rest
// leaves room for the marker line Mainloop appends.
const maxIssueBody = 60000

// issueAuthority is projectAuthority for the issue tools, except that the main thread may name
// any of its owner's projects, as it may for merges: "make an issue for it" in chat is not tied
// to one project. The service still resolves the project by owner, so another owner's project is
// not_found.
func issueAuthority(a agentauth.Actor, projectID string) error {
	if isMain(a) {
		return nil
	}
	return projectAuthority(a, projectID)
}

func (s *Server) createIssue(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string   `json:"project_id"`
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels"`
		RequestID string   `json:"request_id"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return Result{}, err
	}
	if in.Title, err = text(in.Title, 1, 256); err != nil {
		return Result{}, err
	}
	if in.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if len(in.Body) > maxIssueBody || len(in.Labels) > issues.MaxLabels {
		return Result{}, errValidation
	}
	for i := range in.Labels {
		if in.Labels[i], err = text(in.Labels[i], 1, 50); err != nil {
			return Result{}, err
		}
	}
	if s.Issues == nil {
		return Result{}, failure("unavailable", "issues are not available")
	}
	if err := issueAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	res, err := s.Issues.Create(ctx, issues.CreateInput{
		OwnerID: a.UserID, ProjectID: in.ProjectID, SessionID: a.BindingID, RequestID: in.RequestID,
		Title: in.Title, Body: in.Body, Labels: in.Labels,
	})
	if err != nil {
		return Result{}, issuesFailure(err)
	}
	return issueWriteResult(res), nil
}

func (s *Server) commentIssue(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID   string `json:"project_id"`
		IssueNumber int    `json:"issue_number"`
		Body        string `json:"body"`
		RequestID   string `json:"request_id"`
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
	if in.IssueNumber <= 0 || strings.TrimSpace(in.Body) == "" || len(in.Body) > maxIssueBody {
		return Result{}, errValidation
	}
	if s.Issues == nil {
		return Result{}, failure("unavailable", "issues are not available")
	}
	if err := issueAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	res, err := s.Issues.Comment(ctx, issues.CommentInput{
		OwnerID: a.UserID, ProjectID: in.ProjectID, SessionID: a.BindingID, RequestID: in.RequestID,
		Number: in.IssueNumber, Body: in.Body,
	})
	if err != nil {
		return Result{}, issuesFailure(err)
	}
	return issueWriteResult(res), nil
}

// issueWriteResult shows a write's state. A write still pending when the call returns is settled
// by the loop; the same request_id reads it again.
func issueWriteResult(res issues.WriteResult) Result {
	m := map[string]any{"id": res.ID, "kind": string(res.Kind), "state": string(res.State), "repository": res.Repository}
	line := fmt.Sprintf("%s %s: %s", res.Kind, res.ID, res.State)
	if res.Number > 0 {
		m["issue_number"] = res.Number
	}
	if res.CommentID > 0 {
		m["comment_id"] = res.CommentID
	}
	if res.URL != "" {
		m["url"] = res.URL
		line += fmt.Sprintf(" #%d %s", res.Number, res.URL)
	}
	if res.Detail != "" {
		m["detail"] = res.Detail
		line += " (" + res.Detail + ")"
	}
	m["text"] = line
	return Result{Text: line, Structured: m}
}

func issuesFailure(err error) error {
	var r *issues.Refusal
	switch {
	case errors.As(err, &r):
		return failure(r.Code, r.Message)
	case errors.Is(err, issues.ErrNotFound):
		return failure("not_found", "no such project")
	case errors.Is(err, issues.ErrConflict):
		return failure("request_payload_conflict", "this request_id was used with other arguments")
	}
	return err
}
