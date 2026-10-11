package projects

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// defaultListLimit is the list endpoint's page size when the client sends none.
const defaultListLimit = 20

// protectedGlobs are the paths whose changes always need the owner, whatever the policy. The
// list is fixed by the server and versioned.
var protectedGlobs = []string{"k8s/**", ".github/**", "**/migrations/**"}

// ProjectsAPI serves the project operations of the strict server interface.
type ProjectsAPI struct {
	Svc *Service
}

func toProject(p dbq.Project) api.Project {
	return api.Project{
		AvatarUrl: p.AvatarURL, CreatedAt: p.CreatedAt, DefaultBranch: p.DefaultBranch,
		Description: p.Description, FullName: p.FullName, HtmlUrl: p.HTMLURL, Id: p.ID,
		LastUsedAt: p.LastUsedAt, MergePolicy: api.MergePolicy(p.MergePolicy),
		MergePolicyVersion: int(p.MergePolicyVersion), MetadataUpdatedAt: p.MetadataUpdatedAt,
		Name: p.Name, OpenIssueCount: int(p.OpenIssueCount), OpenPrCount: int(p.OpenPRCount),
		Owner: p.Owner, UserId: p.UserID,
	}
}

func (a *ProjectsAPI) view(m MergePolicy) api.MergePolicyView {
	return api.MergePolicyView{
		MergePolicy: api.MergePolicy(m.Policy), MergePolicyVersion: int(m.Version),
		ProtectedGlobs: protectedGlobs, ProtectedGlobsVersion: api.MergePolicyViewProtectedGlobsVersionN1,
		WritesEnabled: a.Svc.PolicyWrites,
	}
}

// ListProjects implements api.StrictServerInterface.
func (a *ProjectsAPI) ListProjects(ctx context.Context, r api.ListProjectsRequestObject) (api.ListProjectsResponseObject, error) {
	limit := int32(defaultListLimit)
	if r.Params.Limit != nil {
		limit = int32(max(min(*r.Params.Limit, 1<<31-1), -1))
	}
	ps, err := a.Svc.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make(api.ListProjects200JSONResponse, 0, len(ps))
	for _, p := range ps {
		out = append(out, toProject(p))
	}
	return out, nil
}

// ImportProject implements api.StrictServerInterface.
func (a *ProjectsAPI) ImportProject(ctx context.Context, r api.ImportProjectRequestObject) (api.ImportProjectResponseObject, error) {
	p, err := a.Svc.Import(ctx, r.Body.Repo)
	switch {
	case errors.Is(err, ErrInvalidRepo):
		return nil, apierr.New(http.StatusBadRequest, "Invalid GitHub repository")
	case err != nil:
		return nil, err
	}
	return api.ImportProject201JSONResponse(toProject(p)), nil
}

// GetProject implements api.StrictServerInterface.
func (a *ProjectsAPI) GetProject(ctx context.Context, r api.GetProjectRequestObject) (api.GetProjectResponseObject, error) {
	p, err := a.Svc.Get(ctx, r.ProjectId)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case err != nil:
		return nil, err
	}
	return api.GetProject200JSONResponse(toProject(p)), nil
}

// GetProjectDetail implements api.StrictServerInterface.
func (a *ProjectsAPI) GetProjectDetail(ctx context.Context, r api.GetProjectDetailRequestObject) (api.GetProjectDetailResponseObject, error) {
	d, err := a.Svc.Detail(ctx, r.ProjectId)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case err != nil:
		return nil, err
	}
	out := api.GetProjectDetail200JSONResponse{
		Project:       toProject(d.Project),
		OpenPrs:       make([]api.ProjectPRSummary, 0, len(d.PRs)),
		RecentCommits: make([]api.CommitSummary, 0, len(d.Commits)),
		Sessions:      make([]api.Session, 0, len(d.Sessions)),
	}
	for _, p := range d.PRs {
		out.OpenPrs = append(out.OpenPrs, api.ProjectPRSummary{
			Author: p.Author, CreatedAt: p.CreatedAt, IsMainloop: p.IsMainloop, Number: p.Number,
			State: p.State, Title: p.Title, UpdatedAt: p.UpdatedAt, Url: p.URL,
		})
	}
	for _, c := range d.Commits {
		out.RecentCommits = append(out.RecentCommits, api.CommitSummary{
			Author: c.Author, Date: c.Date, Message: c.Message, Sha: c.SHA, Url: c.URL,
		})
	}
	for _, s := range d.Sessions {
		out.Sessions = append(out.Sessions, toSession(s, d.NeedsOwner[s.ID]))
	}
	return out, nil
}

func toSession(s dbq.ListProjectSessionsRow, needsOwner bool) api.Session {
	var result *map[string]interface{}
	if len(s.Result) > 0 {
		var m map[string]interface{}
		if json.Unmarshal(s.Result, &m) == nil && m != nil {
			result = &m
		}
	}
	return api.Session{
		AgentKind: s.AgentKind, ArchivedAt: s.ArchivedAt, BaseBranch: s.BaseBranch,
		BranchName: s.BranchName, Color: s.Color, CommitSha: s.CommitSha, CompletedAt: s.CompletedAt,
		CreatedAt: s.CreatedAt, Description: s.Description, Error: s.Error, Id: s.ID, Model: s.Model,
		NeedsOwner: needsOwner, ParentSessionId: s.ParentSessionID, ProjectId: s.ProjectID,
		Prompt: s.Prompt, RepoUrl: s.RepoURL, Result: result, StartedAt: s.StartedAt,
		Status: api.SessionStatus(s.Status), Summary: s.Summary, Title: s.Title, Topic: s.Topic,
		UserId: s.UserID,
	}
}

// GetMergePolicy implements api.StrictServerInterface.
func (a *ProjectsAPI) GetMergePolicy(ctx context.Context, r api.GetMergePolicyRequestObject) (api.GetMergePolicyResponseObject, error) {
	m, err := a.Svc.MergePolicy(ctx, r.ProjectId)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case err != nil:
		return nil, err
	}
	return api.GetMergePolicy200JSONResponse(a.view(m)), nil
}

// UpdateMergePolicy implements api.StrictServerInterface.
func (a *ProjectsAPI) UpdateMergePolicy(ctx context.Context, r api.UpdateMergePolicyRequestObject) (api.UpdateMergePolicyResponseObject, error) {
	m, err := a.Svc.SetMergePolicy(ctx, r.ProjectId, dbstate.MergePolicy(r.Body.MergePolicy), int64(r.Body.ExpectedVersion))
	switch {
	case errors.Is(err, ErrWritesDisabled):
		return nil, apierr.Unavailable("Owner policy writes are disabled")
	case errors.Is(err, ErrInvalidPolicy):
		return nil, apierr.Invalid("Unknown merge policy")
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, ErrVersionConflict):
		return nil, apierr.Conflict("Merge policy version changed")
	case err != nil:
		return nil, err
	}
	return api.UpdateMergePolicy200JSONResponse(a.view(m)), nil
}
