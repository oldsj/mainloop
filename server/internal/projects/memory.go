package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

func toMemoryEntry(e projectcontext.Entry) api.ProjectMemoryEntry {
	return api.ProjectMemoryEntry{
		Id: e.ID, ProjectId: e.ProjectID, Text: e.Text, State: api.ProjectMemoryState(e.State),
		Author: api.ProjectMemoryEntryAuthor(e.Author), ReplacesId: e.ReplacesID,
		CreatedAt: e.CreatedAt, DecidedAt: e.DecidedAt,
		Source: api.ProjectMemorySource{
			SessionId: e.Source.SessionID, TaskId: e.Source.TaskID,
			AttemptId: e.Source.AttemptID, DeliveryId: e.Source.DeliveryID,
		},
	}
}

// GetProjectMemory implements api.StrictServerInterface.
func (a *ProjectsAPI) GetProjectMemory(ctx context.Context, r api.GetProjectMemoryRequestObject) (api.GetProjectMemoryResponseObject, error) {
	m, err := a.instructions().Memory(ctx, r.ProjectId)
	switch {
	case errors.Is(err, projectcontext.ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case err != nil:
		return nil, err
	}
	out := api.ProjectMemory{
		ProjectId: r.ProjectId, UsedBytes: m.UsedBytes, Entries: make([]api.ProjectMemoryEntry, len(m.Entries)),
		MaxBytes: projectcontext.MaxMemoryBytes, MaxEntryBytes: projectcontext.MaxMemoryEntryBytes,
	}
	for i, e := range m.Entries {
		out.Entries[i] = toMemoryEntry(e)
	}
	return api.GetProjectMemory200JSONResponse(out), nil
}

// DecideProjectMemoryEntry implements api.StrictServerInterface: the owner accepts or rejects a
// proposal, or revokes an active entry.
func (a *ProjectsAPI) DecideProjectMemoryEntry(ctx context.Context, r api.DecideProjectMemoryEntryRequestObject) (api.DecideProjectMemoryEntryResponseObject, error) {
	if !r.Body.Decision.Valid() {
		return nil, apierr.Invalid("decision must be accept, reject or revoke")
	}
	e, err := a.instructions().Decide(ctx, r.ProjectId, r.EntryId, projectcontext.Decision(r.Body.Decision))
	switch {
	case errors.Is(err, projectcontext.ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, projectcontext.ErrEntryNotFound):
		return nil, apierr.NotFound("Memory entry not found")
	case errors.Is(err, projectcontext.ErrEntryState):
		return nil, apierr.Conflict("The memory entry is not in a state that allows this decision")
	case errors.Is(err, projectcontext.ErrMemoryFull):
		return nil, apierr.Conflict(fmt.Sprintf("Project memory would pass %d bytes; revoke an entry first", projectcontext.MaxMemoryBytes))
	case err != nil:
		return nil, err
	}
	return api.DecideProjectMemoryEntry200JSONResponse(toMemoryEntry(e)), nil
}
