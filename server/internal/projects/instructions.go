package projects

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// instructions is the owner's project-context service (instructions and memory), over the
// projects service's pool.
func (a *ProjectsAPI) instructions() *projectcontext.Service {
	return &projectcontext.Service{Pool: a.Svc.Pool, OwnerID: a.Svc.OwnerID}
}

func toInstructions(in projectcontext.Instructions) api.ProjectInstructions {
	return api.ProjectInstructions{
		ProjectId: in.ProjectID, Version: in.Version, Text: in.Text,
		MaxBytes: projectcontext.MaxInstructionsBytes, UpdatedAt: in.UpdatedAt,
	}
}

// GetProjectInstructions implements api.StrictServerInterface.
func (a *ProjectsAPI) GetProjectInstructions(ctx context.Context, r api.GetProjectInstructionsRequestObject) (api.GetProjectInstructionsResponseObject, error) {
	in, err := a.instructions().Instructions(ctx, r.ProjectId)
	switch {
	case errors.Is(err, projectcontext.ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case err != nil:
		return nil, err
	}
	return api.GetProjectInstructions200JSONResponse(toInstructions(in)), nil
}

// UpdateProjectInstructions implements api.StrictServerInterface: owner-only, and versioned like
// the merge policy, so a save made from a stale read is refused rather than lost.
func (a *ProjectsAPI) UpdateProjectInstructions(ctx context.Context, r api.UpdateProjectInstructionsRequestObject) (api.UpdateProjectInstructionsResponseObject, error) {
	if r.Body.ExpectedVersion < 0 {
		return nil, apierr.Invalid("expected_version must not be negative")
	}
	in, err := a.instructions().SetInstructions(ctx, r.ProjectId, r.Body.Text, r.Body.ExpectedVersion)
	switch {
	case errors.Is(err, projectcontext.ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, projectcontext.ErrVersionConflict):
		return nil, apierr.Conflict("Project instructions changed since they were read")
	case errors.Is(err, projectcontext.ErrTooLarge):
		return nil, apierr.Invalid("Project instructions are longer than 8192 bytes")
	case errors.Is(err, projectcontext.ErrInvalidText):
		return nil, apierr.Invalid("Project instructions must be UTF-8 text without NUL")
	case err != nil:
		return nil, err
	}
	return api.UpdateProjectInstructions200JSONResponse(toInstructions(in)), nil
}

// ListProjectInstructionVersions implements api.StrictServerInterface.
func (a *ProjectsAPI) ListProjectInstructionVersions(ctx context.Context, r api.ListProjectInstructionVersionsRequestObject) (api.ListProjectInstructionVersionsResponseObject, error) {
	rows, err := a.instructions().Versions(ctx, r.ProjectId)
	switch {
	case errors.Is(err, projectcontext.ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case err != nil:
		return nil, err
	}
	out := api.ProjectInstructionVersionList{Versions: make([]api.ProjectInstructionVersion, len(rows))}
	for i, v := range rows {
		out.Versions[i] = api.ProjectInstructionVersion{Version: v.Version, Text: v.Text, CreatedAt: *v.UpdatedAt}
	}
	return api.ListProjectInstructionVersions200JSONResponse(out), nil
}
