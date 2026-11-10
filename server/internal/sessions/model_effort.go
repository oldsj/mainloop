package sessions

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// ModelEffortState is a session's current /model and /effort override: nil means the provider's
// default, in force since the owner last set or reset it.
type ModelEffortState struct {
	SessionID     string
	Model, Effort *string
}

// profileFor resolves kind's provider profile, for /effort validation and turn metadata. A
// binding with no kind (a main predating the registry) is the Claude provider, as bindingAgent
// treats it. ok is false when the registry has nothing for kind: the caller then assumes nothing
// about the profile's native provider or its defaults.
func (s *Service) profileFor(kind string) (config.ProviderProfile, bool) {
	if kind == "" {
		kind = ProviderClaude
	}
	if s.Providers == nil {
		return config.ProviderProfile{}, false
	}
	return s.Providers.Profile(kind)
}

// SetModelOverride sets, or clears (nil), the session's /model override. The format is already
// checked at the API boundary; kagent itself is the real gate on whether the model exists, and
// rejects an unknown one when the next turn starts (see turnMetadata and server/internal/kagent).
func (s *Service) SetModelOverride(ctx context.Context, sessionID string, model *string) (ModelEffortState, error) {
	if _, err := s.requireOwnedBinding(ctx, sessionID); err != nil {
		return ModelEffortState{}, err
	}
	row, err := dbq.New(s.Pool).SetBindingModelOverride(ctx, dbq.SetBindingModelOverrideParams{
		SessionID: sessionID, ModelOverride: model,
	})
	if err != nil {
		return ModelEffortState{}, fmt.Errorf("set model override: %w", err)
	}
	return ModelEffortState{SessionID: row.SessionID, Model: row.ModelOverride, Effort: row.EffortOverride}, nil
}

// SetEffortOverride sets, or clears (nil), the session's /effort override. A non-nil effort must
// be one of the session's current provider profile's offered effort_levels.
func (s *Service) SetEffortOverride(ctx context.Context, sessionID string, effort *string) (ModelEffortState, error) {
	kind, err := s.requireOwnedBinding(ctx, sessionID)
	if err != nil {
		return ModelEffortState{}, err
	}
	if effort != nil {
		profile, _ := s.profileFor(kind)
		if !slices.Contains(profile.EffortLevels, *effort) {
			return ModelEffortState{}, fmt.Errorf("%w: effort %q is not offered by this session's provider", ErrInvalid, *effort)
		}
	}
	row, err := dbq.New(s.Pool).SetBindingEffortOverride(ctx, dbq.SetBindingEffortOverrideParams{
		SessionID: sessionID, EffortOverride: effort,
	})
	if err != nil {
		return ModelEffortState{}, fmt.Errorf("set effort override: %w", err)
	}
	return ModelEffortState{SessionID: row.SessionID, Model: row.ModelOverride, Effort: row.EffortOverride}, nil
}

// requireOwnedBinding confirms the session is the owner's and returns its binding kind (the
// provider profile id), for /model and /effort.
func (s *Service) requireOwnedBinding(ctx context.Context, sessionID string) (string, error) {
	kind, err := dbq.New(s.Pool).GetBindingKindOwned(ctx, dbq.GetBindingKindOwnedParams{
		SessionID: sessionID, UserID: s.OwnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get binding kind: %w", err)
	}
	return kind, nil
}

// turnMetadata is the A2A message metadata that names a turn's model and effort, keyed by the
// binding's kind (provider profile id). Every turn, for every provider, names both keys explicitly:
// the override when one is set, otherwise the profile's DefaultModel/DefaultEffort. Mainloop then
// does not depend on how a harness treats an absent key or a value from an earlier turn (kagent's
// re-ported harnesses differ: Codex no longer carries an override across turns, while the Claude
// SDK driver has kept one), and resetting to default sends the real default, which clears a
// carried value where an empty or absent key would not. A key is omitted only when there is
// nothing to name: no override and a profile without that default (an unknown kind, or a profile
// that offers no models or effort levels), where absent means kagent's harness default.
func (s *Service) turnMetadata(kind string, model, effort *string) map[string]any {
	profile, _ := s.profileFor(kind)
	resolvedModel, resolvedEffort := profile.DefaultModel, profile.DefaultEffort
	if model != nil {
		resolvedModel = *model
	}
	if effort != nil {
		resolvedEffort = *effort
	}
	out := map[string]any{}
	if resolvedModel != "" {
		out[kagent.ModelMetadataKey] = resolvedModel
	}
	if resolvedEffort != "" {
		out[kagent.EffortMetadataKey] = resolvedEffort
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
