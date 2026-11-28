package sessions

import (
	"context"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// Session default roles: the main thread, supervisors (tasks without a parent), and every other
// session. A supervisor with no supervisor default for a field takes the worker default.
const (
	DefaultRoleMain       = string(dbstate.DefaultRoleMain)
	DefaultRoleSupervisor = string(dbstate.DefaultRoleSupervisor)
	DefaultRoleWorker     = string(dbstate.DefaultRoleWorker)
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
	if d.Role != DefaultRoleMain && d.Role != DefaultRoleSupervisor && d.Role != DefaultRoleWorker {
		return nil, fmt.Errorf("%w: role must be main, supervisor or worker", ErrInvalid)
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
// default for its role and provider (a supervisor's falls back, field by field, to the worker
// default; the delivery context query resolves that). An effort its provider no longer offers (the profile changed
// since it was set, as when the built-in Codex profile dropped minimal) is left out rather than
// failing every turn: the session's override falls back to the default, and the default to none.
func (s *Service) turnModelEffort(d dbq.GetDeliveryContextRow) (model, effort *string) {
	model = d.ModelOverride
	if model == nil {
		model = d.DefaultModel
	}
	kind := ""
	if d.BindingKind != nil {
		kind = *d.BindingKind
	}
	offered := func(e *string) bool {
		if e == nil {
			return false
		}
		profile, ok := s.profileFor(kind)
		return !ok || slices.Contains(profile.EffortLevels, *e)
	}
	switch {
	case offered(d.EffortOverride):
		effort = d.EffortOverride
	case offered(d.DefaultEffort):
		effort = d.DefaultEffort
	}
	return model, effort
}
