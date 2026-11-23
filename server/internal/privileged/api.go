package privileged

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// PrivilegedAPI serves the owner's read-only audit of privileged actions. It reads the table
// directly, so the history stays readable while privileged actions are turned off.
type PrivilegedAPI struct {
	Pool    *pgxpool.Pool
	OwnerID string
}

// ListPrivilegedActions implements api.StrictServerInterface.
func (a *PrivilegedAPI) ListPrivilegedActions(ctx context.Context, _ api.ListPrivilegedActionsRequestObject) (api.ListPrivilegedActionsResponseObject, error) {
	if a.Pool == nil {
		return nil, apierr.Unavailable("privileged actions need the database")
	}
	rows, err := dbq.New(a.Pool).ListRecentPrivilegedActions(ctx, a.OwnerID)
	if err != nil {
		return nil, err
	}
	out := make(api.ListPrivilegedActions200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, summary(r))
	}
	return out, nil
}

// GetPrivilegedAction implements api.StrictServerInterface.
func (a *PrivilegedAPI) GetPrivilegedAction(ctx context.Context, r api.GetPrivilegedActionRequestObject) (api.GetPrivilegedActionResponseObject, error) {
	if a.Pool == nil {
		return nil, apierr.Unavailable("privileged actions need the database")
	}
	q := dbq.New(a.Pool)
	row, err := q.GetPrivilegedAction(ctx, dbq.GetPrivilegedActionParams{OwnerID: a.OwnerID, ID: r.ActionId})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.NotFound("Privileged action not found")
	}
	if err != nil {
		return nil, err
	}
	evs, err := q.ListPrivilegedActionEvents(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	d := api.PrivilegedActionDetail{
		Action: summary(row), Args: object(row.Args), Digest: row.Digest, Attempts: int(row.Attempts),
		DeliveryState: api.PrivilegedActionDetailDeliveryState(row.DeliveryState), Events: []api.PrivilegedActionEvent{},
	}
	if row.FrozenFacts != nil {
		f := object(row.FrozenFacts)
		d.FrozenFacts = &f
		if _, body, _, err := render(row); err == nil {
			d.Body = body
		}
	}
	if row.Result != nil {
		res := object(row.Result)
		d.Result = &res
	}
	for _, e := range evs {
		d.Events = append(d.Events, api.PrivilegedActionEvent{Id: e.ID, Step: e.Step, Detail: e.Detail, CreatedAt: e.CreatedAt})
	}
	return api.GetPrivilegedAction200JSONResponse(d), nil
}

func summary(r dbq.PrivilegedAction) api.PrivilegedActionSummary {
	title := r.Type
	if r.FrozenFacts != nil {
		if t, _, _, err := render(r); err == nil {
			title = t
		}
	}
	return api.PrivilegedActionSummary{
		Id: r.ID, Type: r.Type, State: api.PrivilegedActionState(r.State), Title: title,
		RequesterRole: r.RequesterRole, RequesterSessionId: r.RequesterSessionID, RequesterTaskId: r.RequesterTaskID,
		Reason: r.Reason, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, DecidedAt: r.DecidedAt,
		FinishedAt: r.FinishedAt, DecisionNote: r.DecisionNote, LastError: r.LastError,
	}
}

// object decodes a stored JSON object; anything else (never written) is an empty object.
func object(raw []byte) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}
