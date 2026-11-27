package ownermemory

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// OwnerMemoryAPI serves the owner's view of their memory (GET /owner/memory and its decisions).
type OwnerMemoryAPI struct {
	Pool    *pgxpool.Pool
	OwnerID string
}

func (a *OwnerMemoryAPI) svc() *Service { return &Service{Pool: a.Pool, OwnerID: a.OwnerID} }

// GetOwnerMemory implements api.StrictServerInterface.
func (a *OwnerMemoryAPI) GetOwnerMemory(ctx context.Context, _ api.GetOwnerMemoryRequestObject) (api.GetOwnerMemoryResponseObject, error) {
	m, err := a.svc().Memory(ctx)
	if err != nil {
		return nil, err
	}
	out := api.OwnerMemory{
		Entries: make([]api.OwnerMemoryEntry, len(m.Entries)), UsedBytes: m.UsedBytes,
		MaxBytes: MaxBytes, MaxEntryBytes: MaxEntryBytes, ReviewAfterDays: m.ReviewAfterDays, GraceDays: GraceDays,
	}
	for i, e := range m.Entries {
		out.Entries[i] = toWire(e, m.ReviewAfterDays)
	}
	return api.GetOwnerMemory200JSONResponse(out), nil
}

// SetOwnerMemorySettings implements api.StrictServerInterface. The review window is an owner
// setting, not policy, so it is not gated on owner policy writes.
func (a *OwnerMemoryAPI) SetOwnerMemorySettings(ctx context.Context, r api.SetOwnerMemorySettingsRequestObject) (api.SetOwnerMemorySettingsResponseObject, error) {
	days, err := a.svc().SetReviewAfterDays(ctx, r.Body.ReviewAfterDays)
	if errors.Is(err, ErrReviewDays) {
		return nil, apierr.Invalid(fmt.Sprintf("review_after_days must be %d to %d", MinReviewDays, MaxReviewDays))
	}
	if err != nil {
		return nil, err
	}
	return api.SetOwnerMemorySettings200JSONResponse(api.OwnerMemorySettings{ReviewAfterDays: days}), nil
}

// DecideOwnerMemoryEntry implements api.StrictServerInterface.
func (a *OwnerMemoryAPI) DecideOwnerMemoryEntry(ctx context.Context, r api.DecideOwnerMemoryEntryRequestObject) (api.DecideOwnerMemoryEntryResponseObject, error) {
	if !r.Body.Decision.Valid() {
		return nil, apierr.Invalid("decision must be accept, reject, keep, forget, moved or move_to_project")
	}
	project := ""
	if r.Body.ProjectId != nil {
		project = *r.Body.ProjectId
	}
	d := Decision(r.Body.Decision)
	if (d == DecisionMoveToProject) != (project != "") {
		return nil, apierr.Invalid("project_id goes with move_to_project, and only with it")
	}
	svc := a.svc()
	e, err := svc.Decide(ctx, r.EntryId, d, project)
	switch {
	case errors.Is(err, ErrEntryNotFound):
		return nil, apierr.NotFound("Owner memory entry not found")
	case errors.Is(err, ErrProjectNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, ErrEntryState):
		return nil, apierr.Conflict("The owner memory entry is not in a state that allows this decision")
	case errors.Is(err, ErrMemoryFull):
		return nil, apierr.Conflict(fmt.Sprintf("Owner memory would pass %d bytes; forget an entry first", MaxBytes))
	case errors.Is(err, ErrProjectMemoryFull):
		return nil, apierr.Conflict(fmt.Sprintf("The project's memory would pass %d bytes; revoke one of its entries first", projectcontext.MaxMemoryBytes))
	case err != nil:
		return nil, err
	}
	m, err := svc.Memory(ctx)
	if err != nil {
		return nil, err
	}
	return api.DecideOwnerMemoryEntry200JSONResponse(toWire(e, m.ReviewAfterDays)), nil
}

func toWire(e Entry, reviewDays int) api.OwnerMemoryEntry {
	return api.OwnerMemoryEntry{
		Id: e.ID, Text: e.Text, State: api.OwnerMemoryState(e.State), Author: api.OwnerMemoryEntryAuthor(e.Author),
		Source: api.ProjectMemorySource{
			SessionId: e.Source.SessionID, TaskId: e.Source.TaskID,
			AttemptId: e.Source.AttemptID, DeliveryId: e.Source.DeliveryID,
		},
		ReplacesId: e.ReplacesID, CreatedAt: e.CreatedAt, LastConfirmedAt: e.LastConfirmedAt,
		DecidedAt: e.DecidedAt, DueAt: e.DueAt(reviewDays), LapsedAt: e.LapsedAt,
		InDotfilesCommit: e.InDotfilesCommit, MovedToProjectId: e.MovedToProjectID, MovedToEntryId: e.MovedToEntryID,
	}
}
