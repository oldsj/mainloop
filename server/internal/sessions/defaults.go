package sessions

import (
	"context"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Session default roles: the main thread, and every other session (task sessions, supervisors).
const (
	DefaultRoleMain   = "main"
	DefaultRoleWorker = "worker"
)

// SessionDefault is the owner's default model and effort for one role and provider profile. A nil
// field leaves it to the provider.
type SessionDefault struct {
	Role, Provider string
	Model, Effort  *string
}

// SessionDefaults lists the owner's defaults.
func (s *Service) SessionDefaults(ctx context.Context) ([]SessionDefault, error) {
	rows, err := dbq.New(s.Pool).ListSessionDefaults(ctx, s.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("list session defaults: %w", err)
	}
	out := make([]SessionDefault, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionDefault{Role: r.Role, Provider: r.Provider, Model: r.Model, Effort: r.Effort})
	}
	return out, nil
}

// SetSessionDefault records one default, or removes it when both fields are nil. The provider must
// be a configured profile, and the effort one of the levels it offers.
func (s *Service) SetSessionDefault(ctx context.Context, d SessionDefault) ([]SessionDefault, error) {
	if d.Role != DefaultRoleMain && d.Role != DefaultRoleWorker {
		return nil, fmt.Errorf("%w: role must be main or worker", ErrInvalid)
	}
	profile, ok := s.profileFor(d.Provider)
	if !ok || d.Provider == "" {
		return nil, fmt.Errorf("%w: no provider profile %q", ErrInvalid, d.Provider)
	}
	if d.Effort != nil && !slices.Contains(profile.EffortLevels, *d.Effort) {
		return nil, fmt.Errorf("%w: effort %q is not offered by provider %q", ErrInvalid, *d.Effort, d.Provider)
	}
	q := dbq.New(s.Pool)
	var err error
	if d.Model == nil && d.Effort == nil {
		err = q.DeleteSessionDefault(ctx, dbq.DeleteSessionDefaultParams{UserID: s.OwnerID, Role: d.Role, Provider: d.Provider})
	} else {
		err = q.UpsertSessionDefault(ctx, dbq.UpsertSessionDefaultParams{
			UserID: s.OwnerID, Role: d.Role, Provider: d.Provider, Model: d.Model, Effort: d.Effort,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("set session default: %w", err)
	}
	return s.SessionDefaults(ctx)
}

// turnModelEffort is the model and effort a turn names: the session's override, else the owner's
// default for its role and provider. A default effort its provider no longer offers (the profile
// changed since it was set) is left out rather than failing the turn.
func (s *Service) turnModelEffort(d dbq.GetDeliveryContextRow) (model, effort *string) {
	model, effort = d.ModelOverride, d.EffortOverride
	if model == nil {
		model = d.DefaultModel
	}
	if effort == nil && d.DefaultEffort != nil {
		kind := ""
		if d.BindingKind != nil {
			kind = *d.BindingKind
		}
		if profile, ok := s.profileFor(kind); !ok || slices.Contains(profile.EffortLevels, *d.DefaultEffort) {
			effort = d.DefaultEffort
		}
	}
	return model, effort
}
