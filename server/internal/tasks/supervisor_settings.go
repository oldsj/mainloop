package tasks

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
)

// SupervisorChoice is a project's own provider, model and effort for its standing supervisor
// (docs/specs/project-supervisor.md#settings). Nil is the default: the project's default provider,
// and the owner's worker session default model and effort for it.
type SupervisorChoice struct {
	ProviderProfileID, Model, Effort *string
}

func (c SupervisorChoice) same(o SupervisorChoice) bool {
	eq := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return eq(c.ProviderProfileID, o.ProviderProfileID) && eq(c.Model, o.Model) && eq(c.Effort, o.Effort)
}

// SupervisorSettings is a project's choice and what it resolves to.
type SupervisorSettings struct {
	ProjectID string
	SupervisorChoice
	// EffectiveProfileID is the provider the supervisor runs on; AllowedModels and
	// AllowedEffortLevels are what that provider offers.
	EffectiveProfileID                 string
	AllowedModels, AllowedEffortLevels []string
	// OperationID is the reassign a provider change started, on the answer to that change.
	OperationID *string
}

func choiceOf(r dbq.GetProjectSupervisorSettingsRow) SupervisorChoice {
	return SupervisorChoice{ProviderProfileID: r.SupervisorProfileID, Model: r.SupervisorModel, Effort: r.SupervisorEffort}
}

// standingInput is the create input of the project's standing supervisor with the project's choice.
func standingInput(c SupervisorChoice, in CreateInput) CreateInput {
	in.ProviderProfileID, in.Model, in.Effort = deref(c.ProviderProfileID), deref(c.Model), deref(c.Effort)
	return in
}

// resolveSupervisor resolves and validates a choice as delegate would for the project's standing
// supervisor: the provider (explicit, or the project's default, or the installation's), and the
// model and effort against it.
func (s *Service) resolveSupervisor(ctx context.Context, q *dbq.Queries, project string, c SupervisorChoice) (SupervisorSettings, error) {
	in := standingInput(c, CreateInput{Mode: dbstate.TaskModeCoordination, standing: true})
	sel, err := s.selectProfile(ctx, q, in, &project, nil)
	if err != nil {
		return SupervisorSettings{}, err
	}
	if err := validateModelEffort(in, sel.profile); err != nil {
		return SupervisorSettings{}, err
	}
	return SupervisorSettings{
		ProjectID: project, SupervisorChoice: c, EffectiveProfileID: sel.profile.ID,
		AllowedModels: modelIDs(sel.profile.Models), AllowedEffortLevels: append([]string{}, sel.profile.EffortLevels...),
	}, nil
}

// SupervisorSettings reads the owner's project's supervisor settings.
func (s *Service) SupervisorSettings(ctx context.Context, owner, project string) (SupervisorSettings, error) {
	q := dbq.New(s.Pool)
	row, err := q.GetProjectSupervisorSettings(ctx, dbq.GetProjectSupervisorSettingsParams{ID: project, UserID: owner})
	if err != nil {
		return SupervisorSettings{}, notFoundOr(err, ReasonProjectNotFound)
	}
	c := choiceOf(row)
	out, err := s.resolveSupervisor(ctx, q, project, c)
	var e *Error
	if errors.As(err, &e) && e.Status == 422 {
		// A choice the providers no longer offer (a profile disabled since) is still the project's;
		// it is shown with the provider it names, and the next start or change refuses it.
		return SupervisorSettings{ProjectID: project, SupervisorChoice: c, EffectiveProfileID: deref(c.ProviderProfileID)}, nil
	}
	return out, err
}

// SetSupervisorSettings replaces the owner's project's supervisor settings and records who changed
// them. A standing supervisor that runs takes them now: its task's model and effort change, and its
// current session's override with them; a provider change is a reassign handoff that keeps its
// children (refused, and nothing changed, when it cannot hand off now). One that has no attempt yet
// starts on them. Setting what the project already has changes nothing.
func (s *Service) SetSupervisorSettings(ctx context.Context, owner, project string, c SupervisorChoice, by LimitChanger) (SupervisorSettings, error) {
	var out SupervisorSettings
	var session *string
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		// The per-project lock a first use and a retire take, so the supervisor they make or end is
		// the one this changes.
		if err := q.LockTaskRequest(ctx, "standing\x1f"+project); err != nil {
			return nil, err
		}
		row, err := q.LockProjectSupervisorSettings(ctx, dbq.LockProjectSupervisorSettingsParams{ID: project, UserID: owner})
		if err != nil {
			return nil, notFoundOr(err, ReasonProjectNotFound)
		}
		old := SupervisorSettings{SupervisorChoice: SupervisorChoice{
			ProviderProfileID: row.SupervisorProfileID, Model: row.SupervisorModel, Effort: row.SupervisorEffort,
		}}
		if out, err = s.resolveSupervisor(ctx, q, project, c); err != nil {
			return nil, err
		}
		if old.same(c) {
			return nil, nil
		}
		if err := q.SetProjectSupervisorSettings(ctx, dbq.SetProjectSupervisorSettingsParams{
			ID: project, SupervisorProfileID: c.ProviderProfileID, SupervisorModel: c.Model, SupervisorEffort: c.Effort,
		}); err != nil {
			return nil, err
		}
		var drafts []events.Draft
		drafts, session, err = s.applySupervisorChoice(ctx, q, owner, project, &out, by)
		if err != nil {
			return nil, err
		}
		return drafts, q.InsertSupervisorSettingsAudit(ctx, dbq.InsertSupervisorSettingsAuditParams{
			ProjectID: project, OwnerID: owner, ChangedBy: by.by(), SessionID: ptr(by.SessionID),
			OldProfileID: old.ProviderProfileID, NewProfileID: c.ProviderProfileID, OldModel: old.Model, NewModel: c.Model,
			OldEffort: old.Effort, NewEffort: c.Effort, OperationID: out.OperationID,
		})
	})
	if err != nil {
		s.noteRefusal(ctx, err)
		return SupervisorSettings{}, err
	}
	if session != nil && s.Sessions != nil {
		// The session's own /model and /effort override, as the owner's composer sets it: the next
		// turn runs on them. nil clears it to the session default. Setting them again is a no-op,
		// so a PUT repeated after a failure here applies them.
		if _, err := s.Sessions.SetModelOverride(ctx, *session, c.Model); err != nil {
			return out, err
		}
		if _, err := s.Sessions.SetEffortOverride(ctx, *session, c.Effort); err != nil {
			return out, err
		}
	}
	return out, nil
}

// applySupervisorChoice moves the project's standing supervisor, if it has one, to the resolved
// settings in out: a new provider by a reassign handoff (out.OperationID), or, before its first
// attempt, by its assignment; and its model and effort on its task. session is the running session
// that takes the new model and effort as its override, when the provider stays.
func (s *Service) applySupervisorChoice(ctx context.Context, q *dbq.Queries, owner, project string, out *SupervisorSettings, by LimitChanger) (drafts []events.Draft, session *string, err error) {
	st, err := q.GetStandingTask(ctx, &project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: st.ID, OwnerID: owner})
	if err != nil {
		return nil, nil, err
	}
	version := t.Version
	moved := t.AssignedProfileID != out.EffectiveProfileID
	if moved && t.CurrentAttemptID == nil {
		// Not provisioned yet: its create starts it on the new provider.
		if t, err = q.SetTaskProfile(ctx, dbq.SetTaskProfileParams{ID: t.ID, AssignedProfileID: out.EffectiveProfileID}); err != nil {
			return nil, nil, err
		}
		moved = false
	}
	if t, err = q.SetTaskModelEffort(ctx, dbq.SetTaskModelEffortParams{ID: t.ID, Model: out.Model, Effort: out.Effort}); err != nil {
		return nil, nil, err
	}
	if !moved {
		after, err := q.TouchTask(ctx, dbq.TouchTaskParams{ID: t.ID, Version: version})
		if err != nil {
			return nil, nil, err
		}
		if after.CurrentAttemptID != nil {
			a, err := q.GetTaskAttempt(ctx, *after.CurrentAttemptID)
			if err != nil {
				return nil, nil, err
			}
			if a.State == dbstate.AttemptActive {
				session = a.SessionID
			}
		}
		return []events.Draft{s.updated(after)}, session, nil
	}
	p := OwnerPrincipal(owner)
	if by.SessionID != "" {
		p = Principal{OwnerID: owner, Role: dbstate.RoleMain, BindingID: by.SessionID}
	}
	in := ActionInput{RequestID: "supervisor-settings:" + uuid.NewString(), TaskID: t.ID, TargetProfileID: out.EffectiveProfileID}
	dig, payload, err := digest(dbstate.TaskOpReassign, t.ID, in.payload(dbstate.TaskOpReassign))
	if err != nil {
		return nil, nil, err
	}
	op, drafts, err := s.record(ctx, q, p, dbstate.TaskOpReassign, in, t, dig, payload)
	if err != nil {
		return nil, nil, err
	}
	out.OperationID = &op.ID
	return drafts, nil, nil
}
