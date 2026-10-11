package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/prs"
)

// pullRequestTools are the pull-request and merge tools. Each writes or reads a durable intent
// row; GitHub is called from the reconcile loops, so a tool call that is cut off or repeated never
// sends a second pull request or merge. The services are nil in a process that has no GitHub App,
// and then the tools answer that they are unavailable.
func (s *Server) pullRequestTools() []Tool {
	id := stringSchema(1, 100)
	sha := map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"}
	prCall := objectSchema(map[string]any{
		"project_id":   id,
		"pr_number":    map[string]any{"type": "integer", "exclusiveMinimum": 0},
		"expected_sha": sha,
		"request_id":   id,
	}, "project_id", "pr_number", "expected_sha", "request_id")
	proposalCall := objectSchema(map[string]any{"proposal_id": id, "request_id": id}, "proposal_id", "request_id")
	return []Tool{
		{
			Name: agentauth.ToolOpenPullRequest,
			Description: "Open a same-repository feature-branch PR against the current default branch. Supply a stable " +
				"request_id for retries; uncertain outcomes are reconciled without resending.",
			InputSchema: objectSchema(map[string]any{
				"project_id":   id,
				"branch":       stringSchema(1, 255),
				"expected_sha": sha,
				"title":        stringSchema(1, 256),
				"body":         stringSchema(0, 60000),
				"request_id":   id,
			}, "project_id", "branch", "expected_sha", "title", "body", "request_id"),
			Handle: s.openPullRequest,
		},
		{
			Name:        agentauth.ToolPreparePullRequestMerge,
			Description: "Prepare immutable PR merge facts and required route; creates no approval card.",
			InputSchema: prCall, Handle: s.prepareMerge,
		},
		{
			Name:        agentauth.ToolMergePullRequest,
			Description: "Merge an auto-policy unprotected PR or return an approval proposal. Reuse request_id to reconcile.",
			InputSchema: prCall, Handle: s.mergePullRequest,
		},
		{
			Name: agentauth.ToolMergePullRequestApproval,
			Description: "Evaluate an exact approved merge once; Mainloop continues pending CI under that consent. " +
				"Do not call again while evaluating; read get_pull_request_merge_status instead.",
			InputSchema: proposalCall, Handle: s.mergeApproved,
		},
		{
			Name:        agentauth.ToolGetPullRequestMergeStatus,
			Description: "Read stored merge state using the original proposal_id and request_id, without approval or execution.",
			InputSchema: proposalCall, Handle: s.mergeStatus,
		},
	}
}

// projectAuthority fails closed: the actor must belong to the project it names. An agent without a
// project, or in another one, cannot open or merge there.
func projectAuthority(a agentauth.Actor, projectID string) error {
	if a.ProjectID == "" || a.ProjectID != projectID {
		return failure("forbidden", "this session has no authority over that project")
	}
	return nil
}

func (s *Server) openPullRequest(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID   string `json:"project_id"`
		Branch      string `json:"branch"`
		ExpectedSHA string `json:"expected_sha"`
		Title       string `json:"title"`
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
	if in.Branch, err = text(in.Branch, 1, 255); err != nil {
		return Result{}, err
	}
	if in.Title, err = text(in.Title, 1, 256); err != nil {
		return Result{}, err
	}
	if in.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return Result{}, err
	}
	if !isSHA(in.ExpectedSHA) || len(in.Body) > 60000 {
		return Result{}, errValidation
	}
	if s.PRs == nil {
		return Result{}, failure("unavailable", "pull requests are not available")
	}
	if err := projectAuthority(a, in.ProjectID); err != nil {
		return Result{}, err
	}
	res, err := s.PRs.Open(ctx, prs.OpenInput{
		OwnerID: a.UserID, ProjectID: in.ProjectID, SessionID: a.BindingID,
		Branch: in.Branch, ExpectedSHA: in.ExpectedSHA, Title: in.Title, Body: in.Body, RequestID: in.RequestID,
	})
	if err != nil {
		return Result{}, prFailure(err)
	}
	m := map[string]any{
		"id": res.ID, "state": string(res.State), "repository": res.Repository, "head": res.Head, "base": res.Base,
	}
	line := fmt.Sprintf("pull request %s: %s", res.ID, res.State)
	if res.Number > 0 {
		m["pr_number"], m["url"] = res.Number, res.URL
		line += fmt.Sprintf(" #%d %s", res.Number, res.URL)
	}
	if res.Detail != "" {
		m["detail"] = res.Detail
		line += " (" + res.Detail + ")"
	}
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

type prInput struct {
	ProjectID   string `json:"project_id"`
	PRNumber    int    `json:"pr_number"`
	ExpectedSHA string `json:"expected_sha"`
	RequestID   string `json:"request_id"`
}

func (s *Server) decodePR(a agentauth.Actor, raw json.RawMessage) (merge.PrepareInput, error) {
	var in prInput
	if err := decode(raw, &in); err != nil {
		return merge.PrepareInput{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return merge.PrepareInput{}, err
	}
	if in.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return merge.PrepareInput{}, err
	}
	if in.PRNumber <= 0 || !isSHA(in.ExpectedSHA) {
		return merge.PrepareInput{}, errValidation
	}
	if s.Merge == nil {
		return merge.PrepareInput{}, failure("unavailable", "merges are not available")
	}
	if err := projectAuthority(a, in.ProjectID); err != nil {
		return merge.PrepareInput{}, err
	}
	return merge.PrepareInput{
		OwnerID: a.UserID, ProjectID: in.ProjectID, SessionID: a.BindingID,
		PRNumber: in.PRNumber, ExpectedSHA: in.ExpectedSHA, RequestID: in.RequestID,
	}, nil
}

func (s *Server) prepareMerge(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	in, err := s.decodePR(a, raw)
	if err != nil {
		return Result{}, err
	}
	v, err := s.Merge.Prepare(ctx, in)
	return mergeResult(v, err)
}

func (s *Server) mergePullRequest(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	in, err := s.decodePR(a, raw)
	if err != nil {
		return Result{}, err
	}
	v, err := s.Merge.Merge(ctx, in)
	return mergeResult(v, err)
}

type proposalInput struct {
	ProposalID string `json:"proposal_id"`
	RequestID  string `json:"request_id"`
}

func (s *Server) decodeProposal(raw json.RawMessage) (proposalInput, error) {
	var in proposalInput
	if err := decode(raw, &in); err != nil {
		return in, err
	}
	var err error
	if in.ProposalID, err = text(in.ProposalID, 1, 100); err != nil {
		return in, err
	}
	if in.RequestID, err = text(in.RequestID, 1, 100); err != nil {
		return in, err
	}
	if s.Merge == nil {
		return in, failure("unavailable", "merges are not available")
	}
	return in, nil
}

func (s *Server) mergeApproved(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	in, err := s.decodeProposal(raw)
	if err != nil {
		return Result{}, err
	}
	v, err := s.Merge.MergeApproved(ctx, merge.ApprovedInput{
		OwnerID: a.UserID, SessionID: a.BindingID, ProposalID: in.ProposalID, RequestID: in.RequestID,
	})
	return mergeResult(v, err)
}

func (s *Server) mergeStatus(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	in, err := s.decodeProposal(raw)
	if err != nil {
		return Result{}, err
	}
	v, err := s.Merge.Status(ctx, a.UserID, in.ProposalID, in.RequestID)
	return mergeResult(v, err)
}

func isSHA(v string) bool {
	if len(v) != 40 {
		return false
	}
	return strings.Trim(v, "0123456789abcdef") == ""
}

// mergeResult shows a proposal's state to the agent. A refusal that still carries a view (the
// evidence was unavailable) is an error result; the agent reads the code.
func mergeResult(v merge.View, err error) (Result, error) {
	if err != nil {
		return Result{}, mergeFailure(err)
	}
	summary, err := structured(v.Summary)
	if err != nil {
		return Result{}, err
	}
	m := map[string]any{
		"proposal_id": v.ProposalID, "status": v.Status, "state": string(v.State), "route": string(v.Route),
		"repository": v.Repository, "pr_number": v.PRNumber, "head_sha": v.HeadSHA, "summary": summary,
		"summary_digest": v.Digest,
	}
	if v.Detail != "" {
		m["detail"] = v.Detail
	}
	if v.MergeSHA != "" {
		m["merge_sha"] = v.MergeSHA
	}
	if v.Deadline != nil {
		m["deadline"] = v.Deadline.UTC().Format("2006-01-02T15:04:05Z")
	}
	line := fmt.Sprintf("merge %s of %s#%d: %s", v.ProposalID, v.Repository, v.PRNumber, v.Status)
	if v.Detail != "" {
		line += " (" + v.Detail + ")"
	}
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func mergeFailure(err error) error {
	var r *merge.Refusal
	switch {
	case errors.As(err, &r):
		return failure(r.Code, r.Message)
	case errors.Is(err, merge.ErrNotFound):
		return failure("not_found", "no such project or proposal")
	case errors.Is(err, merge.ErrConflict):
		return failure("request_payload_conflict", "this request_id was used with other arguments")
	}
	return err
}

func prFailure(err error) error {
	var r *prs.Refusal
	switch {
	case errors.As(err, &r):
		return failure(r.Code, r.Message)
	case errors.Is(err, prs.ErrNotFound):
		return failure("not_found", "no such project")
	case errors.Is(err, prs.ErrConflict):
		return failure("request_payload_conflict", "this request_id was used with other arguments")
	}
	return err
}
