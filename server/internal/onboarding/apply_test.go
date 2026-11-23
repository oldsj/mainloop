package onboarding_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/environments/fakeregistry"
	"github.com/oldsj/mainloop/server/internal/onboarding"
	"github.com/oldsj/mainloop/server/internal/projects"
)

type applyEnv struct {
	*env
	applier  *onboarding.Applier
	registry *fakeregistry.Registry
	image    fakeregistry.Image
}

// failingImport fails every import, as an unreachable database or GitHub would.
type failingImport struct{ *projects.Service }

func (failingImport) Import(context.Context, string) (dbq.Project, error) {
	return dbq.Project{}, errors.New("import: connection reset")
}

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	e := newEnv(t)
	reg := fakeregistry.New()
	validator := &environments.Validator{Client: reg, Allowlist: []string{"ghcr.io"}, Budgets: environments.DefaultBudgets()}
	e.svc.Images = validator
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test", "MAINLOOP_OWNER_ID": owner})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Identity.OwnerID != owner {
		t.Fatalf("owner = %q", cfg.Identity.OwnerID)
	}
	projectSvc := projects.NewService(cfg, e.db.Pool, nil, nil, nil, slog.Default())
	a := &applyEnv{env: e, registry: reg, image: reg.AddImage("ghcr.io", "oldsj/opencozy-env", fakeregistry.Options{})}
	a.applier = &onboarding.Applier{
		Pool: e.db.Pool, OwnerID: owner, Projects: projectSvc, Verification: ci.NewService(e.db.Pool, nil),
		Environments: &environments.Service{Pool: e.db.Pool, Validator: validator}, GitHub: e.gh,
		RegistryAllowlist: []string{"ghcr.io"},
	}
	return a
}

func (a *applyEnv) propose(t *testing.T, requestID string, spec onboarding.Spec) onboarding.Proposal {
	t.Helper()
	p, err := a.svc.Propose(context.Background(), main, requestID, spec)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (a *applyEnv) answer(t *testing.T, p onboarding.Proposal, answer string) {
	t.Helper()
	if err := a.inbox.Respond(context.Background(), p.QueueItemID, api.QueueItemResponse{Response: answer}); err != nil {
		t.Fatal(err)
	}
}

func (a *applyEnv) run(t *testing.T) {
	t.Helper()
	if err := a.applier.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (a *applyEnv) state(t *testing.T, id string) (state string, projectID, lastError *string) {
	t.Helper()
	if err := a.db.Pool.QueryRow(context.Background(), `SELECT state, project_id, last_error FROM project_proposals WHERE id = $1`, id).Scan(&state, &projectID, &lastError); err != nil {
		t.Fatal(err)
	}
	return state, projectID, lastError
}

func (a *applyEnv) steps(t *testing.T, id string) string {
	t.Helper()
	rows, err := a.db.Pool.Query(context.Background(), `SELECT step FROM project_proposal_events WHERE proposal_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func (a *applyEnv) spec() onboarding.Spec {
	s := fullSpec()
	s.EnvironmentImage = a.image.Reference()
	return s
}

// Given an approved proposal, when the applier runs, then the repository is a project with the
// proposed environment, checks mode and workspace service, every step is audited, and a second run
// changes nothing.
func TestApproveAppliesTheProposal(t *testing.T) {
	a := newApplyEnv(t)
	ctx := context.Background()
	p := a.propose(t, "req-1", a.spec())
	a.answer(t, p, onboarding.AnswerApprove)

	a.run(t)

	state, projectID, lastError := a.state(t, p.ID)
	if state != onboarding.StateApplied || projectID == nil || lastError != nil {
		t.Fatalf("state = %s project %v error %v", state, projectID, lastError)
	}
	var fullName, mode string
	var command []string
	if err := a.db.Pool.QueryRow(ctx, `SELECT full_name, verification_mode, workspace_service_command FROM projects WHERE id = $1 AND user_id = $2`,
		*projectID, owner).Scan(&fullName, &mode, &command); err != nil {
		t.Fatal(err)
	}
	if fullName != "oldsj/opencozy-next" || mode != "ci" || strings.Join(command, " ") != "pnpm dev" {
		t.Fatalf("project = %s %s %v", fullName, mode, command)
	}
	var selected string
	if err := a.db.Pool.QueryRow(ctx, `SELECT v.repository FROM project_environment_selections s
		JOIN environment_versions v ON v.id = s.version_id WHERE s.project_id = $1`, *projectID).Scan(&selected); err != nil || selected != "oldsj/opencozy-env" {
		t.Fatalf("selected = %q, %v", selected, err)
	}
	want := "decided,import,github_app,environment,verification,workspace_service,applied"
	if got := a.steps(t, p.ID); got != want {
		t.Fatalf("steps = %s, want %s", got, want)
	}

	a.run(t)
	if got := a.steps(t, p.ID); got != want {
		t.Errorf("a second run wrote %s", got)
	}

	// The proposer reads the outcome back with the same request id.
	again, err := a.svc.Propose(ctx, main, "req-1", a.spec())
	if err != nil || again.State != onboarding.StateApplied || again.ProjectID == nil || *again.ProjectID != *projectID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
}

// Given an approved proposal whose attempt died after selecting the environment, when the applier
// runs it again, then each step finds its work done and the proposal is applied once.
func TestApplyingAgainFindsTheWorkDone(t *testing.T) {
	a := newApplyEnv(t)
	ctx := context.Background()
	p := a.propose(t, "req-1", a.spec())
	a.answer(t, p, onboarding.AnswerApprove)
	a.run(t)
	if _, err := a.db.Pool.Exec(ctx, `UPDATE project_proposals SET state = 'approved', finished_at = NULL, next_attempt_at = now() WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}

	a.run(t)

	if state, _, _ := a.state(t, p.ID); state != onboarding.StateApplied {
		t.Fatalf("state = %s", state)
	}
	var detail string
	if err := a.db.Pool.QueryRow(ctx, `SELECT detail FROM project_proposal_events WHERE proposal_id = $1 AND step = 'environment' ORDER BY id DESC LIMIT 1`, p.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(detail, "already selected") {
		t.Errorf("second environment step = %q", detail)
	}
	if n := a.count(t, `SELECT count(*) FROM projects WHERE user_id = $1`, owner); n != 1 {
		t.Errorf("projects = %d", n)
	}
	if n := a.count(t, `SELECT count(*) FROM dev_environments WHERE owner_id = $1`, owner); n != 1 {
		t.Errorf("environments = %d", n)
	}
}

// Given an environment the owner already registered for the image, when another repository's
// proposal names that image, then it reuses that environment.
func TestApplyReusesARegisteredEnvironment(t *testing.T) {
	a := newApplyEnv(t)
	first := a.propose(t, "req-1", a.spec())
	a.answer(t, first, onboarding.AnswerApprove)
	a.run(t)
	other := a.spec()
	other.Repository = "oldsj/another"
	second := a.propose(t, "req-2", other)
	a.answer(t, second, onboarding.AnswerApprove)

	a.run(t)

	if state, _, _ := a.state(t, second.ID); state != onboarding.StateApplied {
		t.Fatalf("state = %s", state)
	}
	if n := a.count(t, `SELECT count(*) FROM dev_environments WHERE owner_id = $1`, owner); n != 1 {
		t.Errorf("environments = %d, want the first one reused", n)
	}
}

// Given a rejected proposal, when the applier runs, then nothing is imported, the decision is
// audited, and the repository can be proposed again.
func TestRejectAppliesNothing(t *testing.T) {
	a := newApplyEnv(t)
	p := a.propose(t, "req-1", a.spec())
	a.answer(t, p, onboarding.AnswerReject)

	a.run(t)

	if state, _, _ := a.state(t, p.ID); state != onboarding.StateRejected {
		t.Fatalf("state = %s", state)
	}
	if n := a.count(t, `SELECT count(*) FROM projects`); n != 0 {
		t.Errorf("projects = %d", n)
	}
	if got := a.steps(t, p.ID); got != "decided" {
		t.Errorf("steps = %s", got)
	}
	again := a.propose(t, "req-2", a.spec())
	if again.ID == p.ID || again.Replayed {
		t.Errorf("proposing again returned %+v", again)
	}
}

// Given an import that keeps failing, when the applier runs, then it retries with a backoff and,
// after the last attempt, fails the proposal and tells the owner in Needs you.
func TestApplyRetriesThenFails(t *testing.T) {
	a := newApplyEnv(t)
	ctx := context.Background()
	a.applier.Projects = failingImport{}
	p := a.propose(t, "req-1", onboarding.Spec{Repository: "oldsj/opencozy-next", Why: "pilot"})
	a.answer(t, p, onboarding.AnswerApprove)

	for i := 1; i <= onboarding.MaxApplyAttempts; i++ {
		a.run(t)
		state, _, lastError := a.state(t, p.ID)
		if lastError == nil || !strings.Contains(*lastError, "connection reset") {
			t.Fatalf("attempt %d: last error %v", i, lastError)
		}
		if i < onboarding.MaxApplyAttempts {
			if state != onboarding.StateApproved {
				t.Fatalf("attempt %d: state %s", i, state)
			}
			a.run(t) // not due yet: nothing happens
			if _, err := a.db.Pool.Exec(ctx, `UPDATE project_proposals SET next_attempt_at = now() WHERE id = $1`, p.ID); err != nil {
				t.Fatal(err)
			}
		} else if state != onboarding.StateFailed {
			t.Fatalf("after the last attempt: state %s", state)
		}
	}
	if got, want := a.steps(t, p.ID), "decided,retry,retry,retry,retry,failed"; got != want {
		t.Fatalf("steps = %s, want %s", got, want)
	}
	items := a.items(t)
	if len(items) != 1 || items[0].Title != "Onboarding oldsj/opencozy-next failed" ||
		!strings.Contains(items[0].QueueItem.Content, "connection reset") {
		t.Fatalf("items = %+v", items)
	}
}

// Given an image the registry no longer serves, when the applier registers it, then the refusal
// fails the proposal on the first attempt: no retry can fix it.
func TestApplyFailsAtOnceOnARefusal(t *testing.T) {
	a := newApplyEnv(t)
	spec := a.spec()
	spec.EnvironmentImage = "ghcr.io/oldsj/gone@sha256:" + strings.Repeat("c", 64)
	a.svc.Images = &fakeImages{} // it passed when proposed
	p := a.propose(t, "req-1", spec)
	a.answer(t, p, onboarding.AnswerApprove)

	a.run(t)

	state, _, lastError := a.state(t, p.ID)
	if state != onboarding.StateFailed || lastError == nil || !strings.HasPrefix(*lastError, "environment:") {
		t.Fatalf("state = %s, error %v", state, lastError)
	}
	if got := a.steps(t, p.ID); got != "decided,import,github_app,failed" {
		t.Errorf("steps = %s", got)
	}
}
