package onboarding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Apply tuning.
const (
	// MaxApplyAttempts is how many attempts an approved proposal gets before it fails.
	MaxApplyAttempts = 5
	applyLease       = 2 * time.Minute
	firstRetry       = 30 * time.Second
	maxPerPass       = 5
)

// ProjectWriter is the project API the apply step uses.
type ProjectWriter interface {
	Import(ctx context.Context, text string) (dbq.Project, error)
	SetWorkspaceService(ctx context.Context, id string, in api.WorkspaceService) (api.WorkspaceService, error)
}

// EnvironmentWriter is the environment API the apply step uses.
type EnvironmentWriter interface {
	Register(ctx context.Context, owner string, req api.RegisterEnvironment) (api.RegisteredEnvironment, error)
	Select(ctx context.Context, owner, projectID string, req api.SelectEnvironment) (api.ProjectEnvironmentSelection, error)
	Selection(ctx context.Context, owner, projectID string) (api.ProjectEnvironmentSelection, bool, error)
}

// VerificationWriter sets a project's checks mode.
type VerificationWriter interface {
	SetVerificationMode(ctx context.Context, ownerID, projectID string, mode dbstate.VerificationMode) (ci.Verification, error)
}

// Applier records the owner's answers to proposal cards and applies approved proposals through
// the project APIs. Each step reads the current state first and writes only what differs, so an
// attempt that died part way is finished by the next one; no transaction is open across a step.
type Applier struct {
	Pool         *pgxpool.Pool
	OwnerID      string
	Projects     ProjectWriter
	Environments EnvironmentWriter
	Verification VerificationWriter
	// GitHub re-checks the App's installation when applying; nil skips the check.
	GitHub InstallChecker
	// RegistryAllowlist parses a proposed image to find an environment already registered for it.
	RegistryAllowlist []string
	Log               *slog.Logger
}

func (a *Applier) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Sweep runs the applier every five seconds.
func (a *Applier) Sweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "project-proposals", Every: 5 * time.Second, Run: a.Run}
}

// Run records answered cards, then applies up to a few due proposals.
func (a *Applier) Run(ctx context.Context) error {
	if err := a.decide(ctx); err != nil {
		return err
	}
	for range maxPerPass {
		row, err := dbq.New(a.Pool).ClaimDueProjectProposal(ctx, dbq.ClaimDueProjectProposalParams{
			OwnerID: a.OwnerID, LeaseSeconds: applyLease.Seconds(),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := a.attempt(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (a *Applier) decide(ctx context.Context) error {
	return events.InTxPublish(ctx, a.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		rows, err := q.DecideAnsweredProjectProposals(ctx, a.OwnerID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			detail := "the owner approved"
			if r.State == StateRejected {
				detail = "the owner rejected"
				if r.Response != AnswerReject {
					detail = fmt.Sprintf("the owner answered %q, which is not Approve; nothing is applied", r.Response)
				}
			}
			if err := q.InsertProjectProposalEvent(ctx, dbq.InsertProjectProposalEventParams{ProposalID: r.ID, Step: "decided", Detail: detail}); err != nil {
				return nil, err
			}
			a.log().InfoContext(ctx, "project proposal decided", "proposal_id", r.ID, "state", r.State)
		}
		return nil, nil
	})
}

// attempt applies one claimed proposal and records how it ended: applied, retried later, or failed.
func (a *Applier) attempt(ctx context.Context, row dbq.ProjectProposal) error {
	p, err := fromRow(row)
	if err != nil {
		return a.fail(ctx, row, err.Error())
	}
	projectID, err := a.apply(ctx, p)
	if err == nil {
		return a.finish(ctx, row.ID, StateApplied, &projectID, nil, "project "+projectID+" is set up as proposed")
	}
	if ctx.Err() != nil {
		return ctx.Err() // the sweep is stopping; the lease lets the next run take it
	}
	msg := err.Error()
	if permanent(err) || row.Attempts >= MaxApplyAttempts {
		return a.fail(ctx, row, msg)
	}
	after := firstRetry << (row.Attempts - 1)
	q := dbq.New(a.Pool)
	if err := q.InsertProjectProposalEvent(ctx, dbq.InsertProjectProposalEventParams{
		ProposalID: row.ID, Step: "retry", Detail: fmt.Sprintf("attempt %d failed, retrying in %s: %s", row.Attempts, after, msg),
	}); err != nil {
		return err
	}
	_, err = q.RetryProjectProposal(ctx, dbq.RetryProjectProposalParams{ID: row.ID, LastError: &msg, AfterSeconds: after.Seconds()})
	return err
}

// permanent reports an error no retry can fix: the API refused the request itself.
func permanent(err error) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500 && e.Status != 409 && e.Status != 429
}

// apply runs the steps and returns the project id.
func (a *Applier) apply(ctx context.Context, p Proposal) (string, error) {
	project, err := a.Projects.Import(ctx, p.Repository)
	if err != nil {
		return "", fmt.Errorf("import: %w", err)
	}
	if err := a.event(ctx, p.ID, "import", fmt.Sprintf("project %s (%s)", project.ID, project.FullName)); err != nil {
		return "", err
	}
	if a.GitHub != nil {
		detail := "installed"
		switch ok, err := a.GitHub.Installed(ctx, github.Repo{Owner: project.Owner, Name: project.Name}); {
		case err != nil:
			detail = "not checked: " + err.Error()
		case !ok:
			detail = "NOT installed: " + InstallHint(project.FullName)
		}
		if err := a.event(ctx, p.ID, "github_app", detail); err != nil {
			return "", err
		}
	}
	if p.Spec.EnvironmentImage != "" {
		if err := a.environment(ctx, p, project.ID); err != nil {
			return "", fmt.Errorf("environment: %w", err)
		}
	}
	if m := p.Spec.VerificationMode; m != "" {
		if _, err := a.Verification.SetVerificationMode(ctx, a.OwnerID, project.ID, dbstate.VerificationMode(m)); err != nil {
			return "", fmt.Errorf("checks mode: %w", err)
		}
		if err := a.event(ctx, p.ID, "verification", "checks mode "+m); err != nil {
			return "", err
		}
	}
	if ws := p.Spec.WorkspaceService; ws != nil {
		if _, err := a.Projects.SetWorkspaceService(ctx, project.ID, *ws); err != nil {
			return "", fmt.Errorf("workspace service: %w", err)
		}
		if err := a.event(ctx, p.ID, "workspace_service", "command "+strings.Join(ws.Command, " ")); err != nil {
			return "", err
		}
	}
	return project.ID, nil
}

// environment selects the proposed image for the project: an environment version the owner
// already has for it, or a new environment registered for it.
func (a *Applier) environment(ctx context.Context, p Proposal, projectID string) error {
	ref, err := environments.ParseReference(p.Spec.EnvironmentImage, a.RegistryAllowlist)
	if err != nil {
		return apierr.Invalid(err.Error())
	}
	envID, versionID, how := "", "", "reused"
	found, err := dbq.New(a.Pool).FindOwnedEnvironmentVersionByImage(ctx, dbq.FindOwnedEnvironmentVersionByImageParams{
		OwnerID: a.OwnerID, Registry: &ref.Registry, Repository: &ref.Repository,
		Architecture: &p.Spec.EnvironmentArchitecture, Digest: &ref.Digest,
	})
	switch {
	case err == nil:
		envID, versionID = found.EnvironmentID, found.VersionID
	case errors.Is(err, pgx.ErrNoRows):
		arch := api.RegisterEnvironmentArchitecture(p.Spec.EnvironmentArchitecture)
		reg, err := a.Environments.Register(ctx, a.OwnerID, api.RegisterEnvironment{
			Name: p.Repository, Image: &p.Spec.EnvironmentImage, Architecture: &arch,
		})
		if err != nil {
			return err
		}
		envID, versionID, how = reg.Environment.Id, reg.Version.Id, "registered"
	default:
		return err
	}
	current, ok, err := a.Environments.Selection(ctx, a.OwnerID, projectID)
	if err != nil {
		return err
	}
	if ok && current.EnvironmentId == envID && !current.FollowDefault && current.VersionId != nil && *current.VersionId == versionID {
		return a.event(ctx, p.ID, "environment", fmt.Sprintf("already selected: environment %s version %s", envID, versionID))
	}
	if _, err := a.Environments.Select(ctx, a.OwnerID, projectID, api.SelectEnvironment{
		EnvironmentId: envID, VersionId: &versionID, ExpectedVersion: current.Revision,
	}); err != nil {
		return err
	}
	return a.event(ctx, p.ID, "environment", fmt.Sprintf("%s environment %s version %s and selected it", how, envID, versionID))
}

func (a *Applier) event(ctx context.Context, id, step, detail string) error {
	return dbq.New(a.Pool).InsertProjectProposalEvent(ctx, dbq.InsertProjectProposalEventParams{ProposalID: id, Step: step, Detail: detail})
}

func (a *Applier) finish(ctx context.Context, id, state string, projectID, lastError *string, detail string) error {
	err := events.InTxPublish(ctx, a.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		n, err := q.FinishProjectProposal(ctx, dbq.FinishProjectProposalParams{ID: id, State: state, ProjectID: projectID, LastError: lastError})
		if err != nil || n == 0 {
			return nil, err
		}
		if err := q.InsertProjectProposalEvent(ctx, dbq.InsertProjectProposalEventParams{ProposalID: id, Step: state, Detail: detail}); err != nil {
			return nil, err
		}
		if state != StateFailed {
			return nil, nil
		}
		row, err := q.GetProjectProposal(ctx, dbq.GetProjectProposalParams{OwnerID: a.OwnerID, ID: id})
		if err != nil {
			return nil, err
		}
		return attention.Open(ctx, tx, a.OwnerID, attention.NewQueueItem{
			ID: "project_proposal_failed_" + id, ItemType: api.QueueItemTypeError, Priority: api.QueueItemPriorityHigh,
			Title:   "Onboarding " + row.Repository + " failed",
			Content: fmt.Sprintf("You approved onboarding %s, but applying it failed: %s", row.Repository, detail),
			Context: map[string]any{"project_proposal_id": id, "repository": row.Repository},
		})
	})
	if err == nil {
		a.log().InfoContext(ctx, "project proposal finished", "proposal_id", id, "state", state)
	}
	return err
}

func (a *Applier) fail(ctx context.Context, row dbq.ProjectProposal, msg string) error {
	return a.finish(ctx, row.ID, StateFailed, row.ProjectID, &msg, msg)
}
