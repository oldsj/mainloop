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

// turnMetadata is the A2A message metadata that carries a turn's model and effort override, keyed
// by the binding's kind (provider profile id). Claude sends only what is actually overridden, and
// omits a key reset to default: absent already means the harness default to kagent, and Claude's
// native session carries no override across turns for the key to leave behind. Codex is the
// exception: once touched is true (the session has had either override, ever, even if later reset)
// every turn names both keys explicitly, because kagent's Codex harness keeps the last explicit
// value as a turn/start override for its later turns, and an empty value does not clear it, only a
// concrete one does — so resetting to default sends the profile's own DefaultModel/DefaultEffort,
// not an empty string.
func (s *Service) turnMetadata(kind string, model, effort *string, touched bool) map[string]any {
	profile, _ := s.profileFor(kind)
	if profile.NativeProvider == ProviderCodex && touched {
		resolvedModel, resolvedEffort := profile.DefaultModel, profile.DefaultEffort
		if model != nil {
			resolvedModel = *model
		}
		if effort != nil {
			resolvedEffort = *effort
		}
		return map[string]any{kagent.ModelMetadataKey: resolvedModel, kagent.EffortMetadataKey: resolvedEffort}
	}
	var out map[string]any
	if model != nil {
		out = map[string]any{kagent.ModelMetadataKey: *model}
	}
	if effort != nil {
		if out == nil {
			out = map[string]any{}
		}
		out[kagent.EffortMetadataKey] = *effort
	}
	return out
}
