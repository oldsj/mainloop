package providers

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Task error reasons for the default-provider routes.
const (
	reasonProjectNotFound = "project_not_found"
	reasonStalePreference = "stale_provider_preference"
)

// ProvidersAPI serves GET /providers and the per-project default provider.
type ProvidersAPI struct {
	Registry *Registry
	Pool     *pgxpool.Pool
	// OwnerID is the one owner this instance serves; projects of anyone else are not found.
	OwnerID string
}

// ListProviders implements the contract operation.
func (a *ProvidersAPI) ListProviders(context.Context, api.ListProvidersRequestObject) (api.ListProvidersResponseObject, error) {
	profiles := a.Registry.Profiles()
	out := make(api.ListProviders200JSONResponse, len(profiles))
	for i, p := range profiles {
		out[i] = toAPI(p)
	}
	return out, nil
}

// GetProjectDefaultProvider implements the contract operation. A project with no stored
// preference reads as profile_id null at version 0.
func (a *ProvidersAPI) GetProjectDefaultProvider(ctx context.Context, req api.GetProjectDefaultProviderRequestObject) (api.GetProjectDefaultProviderResponseObject, error) {
	q := dbq.New(a.Pool)
	if err := a.requireOwnedProject(ctx, q, req.ProjectId); err != nil {
		if errors.Is(err, errProjectNotFound) {
			return nil, apierr.Reason(http.StatusNotFound, reasonProjectNotFound)
		}
		return nil, err
	}
	pref, err := readPreference(ctx, q, req.ProjectId)
	if err != nil {
		return nil, err
	}
	return api.GetProjectDefaultProvider200JSONResponse(pref), nil
}

// UpdateProjectDefaultProvider implements the contract operation: a compare-and-swap on the
// preference version. A profile ID must resolve to an enabled profile with a supervisor Agent.
func (a *ProvidersAPI) UpdateProjectDefaultProvider(ctx context.Context, req api.UpdateProjectDefaultProviderRequestObject) (api.UpdateProjectDefaultProviderResponseObject, error) {
	body := req.Body
	if body.ProfileId != nil {
		if _, err := a.Registry.Resolve(*body.ProfileId, RoleSupervisor, true); err != nil {
			return nil, apierr.New(http.StatusUnprocessableEntity, err.Error())
		}
	}
	var pref api.ProjectProviderPreference
	err := db.InTx(ctx, a.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := a.requireOwnedProject(ctx, q, req.ProjectId); err != nil {
			return err
		}
		var row dbq.ProjectProviderPreference
		var err error
		if body.ExpectedVersion == 0 {
			row, err = q.InsertProviderPreference(ctx, dbq.InsertProviderPreferenceParams{
				ProjectID: req.ProjectId, ProfileID: body.ProfileId,
			})
		} else {
			row, err = q.UpdateProviderPreference(ctx, dbq.UpdateProviderPreferenceParams{
				ProjectID: req.ProjectId, ProfileID: body.ProfileId, Version: int64(body.ExpectedVersion),
			})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errStalePreference
		}
		if err != nil {
			return err
		}
		pref = preferenceFromRow(row)
		return nil
	})
	switch {
	case errors.Is(err, errProjectNotFound):
		return nil, apierr.Reason(http.StatusNotFound, reasonProjectNotFound)
	case errors.Is(err, errStalePreference):
		return nil, apierr.Reason(http.StatusConflict, reasonStalePreference)
	case err != nil:
		return nil, err
	}
	return api.UpdateProjectDefaultProvider200JSONResponse(pref), nil
}

var (
	errProjectNotFound = errors.New("project not found")
	errStalePreference = errors.New("stale provider preference")
)

// requireOwnedProject returns errProjectNotFound for a missing project and for one that belongs
// to another owner, so the two cannot be told apart.
func (a *ProvidersAPI) requireOwnedProject(ctx context.Context, q *dbq.Queries, id string) error {
	owner, err := q.GetProjectOwner(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != a.OwnerID) {
		return errProjectNotFound
	}
	return err
}

func readPreference(ctx context.Context, q *dbq.Queries, projectID string) (api.ProjectProviderPreference, error) {
	row, err := q.GetProviderPreference(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProjectProviderPreference{ProjectId: projectID}, nil
	}
	if err != nil {
		return api.ProjectProviderPreference{}, err
	}
	return preferenceFromRow(row), nil
}

func preferenceFromRow(r dbq.ProjectProviderPreference) api.ProjectProviderPreference {
	return api.ProjectProviderPreference{ProjectId: r.ProjectID, ProfileId: r.ProfileID, Version: int(r.Version)}
}

func toAPI(p config.ProviderProfile) api.ProviderProfile {
	agents := make(map[string]api.ProviderAgentRef, len(p.Agents))
	for role, ref := range p.Agents {
		agents[role] = api.ProviderAgentRef{Namespace: ref.Namespace, Name: ref.Name}
	}
	caps := make([]api.CapabilityResult, len(p.Capabilities))
	for i, c := range p.Capabilities {
		caps[i] = api.CapabilityResult{
			Capability: c.Capability, Detail: c.Detail, EvidenceRef: c.EvidenceRef,
			Scope: api.CapabilityResultScope(c.Scope), State: api.CapabilityState(c.State),
		}
	}
	aliases := p.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	return api.ProviderProfile{
		Agents:                agents,
		Aliases:               aliases,
		Capabilities:          caps,
		ConfigurationRevision: p.ConfigurationRevision,
		DisplayName:           p.DisplayName,
		Enabled:               p.Enabled,
		Id:                    p.ID,
		NativeProvider:        api.ProviderProfileNativeProvider(p.NativeProvider),
		RuntimeAdapter:        api.ProviderProfileRuntimeAdapter(p.RuntimeAdapter),
	}
}

// NewAPI builds the API over the process's one registry (see [NewRegistry]).
func NewAPI(cfg *config.Config, pool *pgxpool.Pool, reg *Registry) *ProvidersAPI {
	return &ProvidersAPI{Registry: reg, Pool: pool, OwnerID: cfg.Identity.OwnerID}
}
