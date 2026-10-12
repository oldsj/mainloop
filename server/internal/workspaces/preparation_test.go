package workspaces

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/kagent"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

const prepSHA = "0123456789abcdef0123456789abcdef01234567"

// preparedWorkspace creates a running workspace and gives it the preparation row a workspace made
// with a development environment gets, pinned to a commit.
func preparedWorkspace(t *testing.T, h *harness) wsRow {
	t.Helper()
	h.fake.Actors = true
	h.cfg.Workspaces.OperationWait = 10e6
	w, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("feature")})
	if err != nil {
		t.Fatal(err)
	}
	r := h.row(w.WorkspaceId)
	h.exec(`UPDATE workspaces SET ref=$2 WHERE id=$1`, r.ID, prepSHA)
	h.exec(`INSERT INTO workspace_preparations (id, idempotency_key, action_id) VALUES ($1, 'k:'||$1, 'prep:'||$2)`, r.ID, r.RequestID)
	// The create op was only queued; make the Session.
	claims, err := Table().Claim(bg, h.svc.Pool, "create-test", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("create claim: %v, %v", claims, err)
	}
	if _, err = h.svc.handle(bg, claims[0]); err != nil {
		t.Fatal(err)
	}
	return h.row(r.ID)
}

// prepareOnce runs one pass of the preparation loop and returns the stored state.
func prepareOnce(t *testing.T, h *harness, id string) string {
	t.Helper()
	claims, err := PreparationTable().Claim(bg, h.svc.Pool, "prep-test", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("preparation claim: %v, %v", claims, err)
	}
	out, err := h.svc.prepare(bg, claims[0])
	if err != nil {
		t.Logf("prepare: %v", err)
	}
	if st := h.prepState(id); st == dbstate.PreparationPlanned || st == dbstate.PreparationRequested {
		_ = out
		if err := claims[0].RetryAfter(bg, h.svc.Pool, 0); err != nil {
			t.Fatal(err)
		}
	}
	return h.prepState(id)
}

func (h *harness) prepState(id string) string {
	h.t.Helper()
	p, err := h.dq.GetWorkspacePreparation(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return p.State
}

func lastPrepare(t *testing.T, h *harness) *ka.PrepareSessionWorkspaceRequest {
	t.Helper()
	for _, c := range h.fake.PrepareRequests() {
		return c
	}
	t.Fatal("kagent was never asked to prepare the workspace")
	return nil
}

func TestPreparationConfirmsOnceWithTheAgentProfile(t *testing.T) {
	h := newHarness(t, nil)
	r := preparedWorkspace(t, h)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
		t.Fatalf("state = %s", got)
	}
	req := lastPrepare(t, h)
	if req.GetSessionId() != *r.KagentSessionID || req.GetActionId() != "prep:"+r.RequestID || req.GetCreateRequestId() != r.RequestID ||
		req.GetSetupProfile() != "agent" || req.GetSetupDigest() != setupDigests["agent"] ||
		req.GetGenerationId() == "" || req.GetActorUid() == "" || req.GetWorkspace().GetRef() == "" {
		t.Fatalf("request = %v", req)
	}
	if n := len(h.fake.PrepareRequests()); n != 1 {
		t.Fatalf("prepare calls = %d", n)
	}
}

func TestPreparationUsesTheAttemptRole(t *testing.T) {
	h := newHarness(t, nil)
	r := preparedWorkspace(t, h)
	h.exec(`INSERT INTO tasks (id,owner_id,project_id,root_task_id,title,brief,mode,assigned_profile_id,selection_source,status,checkout_branch,checkout_ref,checkout_depth)
	    VALUES ('task',$1,$2,'task','test','test','code','claude','explicit','queued',$3,'main',0)`, owner, r.ProjectID, r.Branch)
	h.exec(`INSERT INTO task_attempts (id,task_id,number,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,workspace_request_id,writer_generation)
	    VALUES ('attempt','task',1,'claude','claude','rev',$1,$2,'supervisor',1,$3,1)`, r.AgentNamespace, r.AgentName, r.RequestID)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
		t.Fatalf("state = %s", got)
	}
	if req := lastPrepare(t, h); req.GetSetupProfile() != "supervisor" || req.GetSetupDigest() != setupDigests["supervisor"] {
		t.Fatalf("request = %v", req)
	}
}

func TestPreparationWakesAnIdleSessionBeforeAsking(t *testing.T) {
	h := newHarness(t, nil)
	h.fake.IdleActors = true
	r := preparedWorkspace(t, h)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationPlanned {
		t.Fatalf("first pass state = %s", got)
	}
	if n := h.fake.Count(kagent.ProcResumeSession); n != 1 || len(h.fake.PrepareRequests()) != 0 {
		t.Fatalf("resumes = %d, prepares = %d", n, len(h.fake.PrepareRequests()))
	}
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
		t.Fatalf("second pass state = %s", got)
	}
}

func TestPreparationWaitsForAPendingReceiptWithoutAskingAgain(t *testing.T) {
	h := newHarness(t, nil)
	h.fake.PrepareClassification = "pending"
	r := preparedWorkspace(t, h)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationRequested {
		t.Fatalf("state = %s", got)
	}
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationRequested {
		t.Fatalf("state = %s", got)
	}
	if n := len(h.fake.PrepareRequests()); n != 1 {
		t.Fatalf("a pending receipt was asked for again: %d calls", n)
	}
	base := h.fake.TamperSession
	h.fake.TamperSession = func(proc string, s *ka.Session) {
		base(proc, s)
		if proc == kagent.ProcGetSession && s.GetWorkspacePreparation() != nil {
			s.WorkspacePreparation.Classification = "confirmed"
		}
	}
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
		t.Fatalf("state = %s", got)
	}
	if n := len(h.fake.PrepareRequests()); n != 1 {
		t.Fatalf("prepare calls = %d", n)
	}
}

func TestPreparationFailsForADefiniteFailureAndAMovingRef(t *testing.T) {
	h := newHarness(t, nil)
	h.fake.PrepareClassification = "definite-failure"
	r := preparedWorkspace(t, h)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationFailed {
		t.Fatalf("state = %s", got)
	}
	p, _ := h.dq.GetWorkspacePreparation(bg, r.ID)
	if !strings.Contains(p.Detail, "failed") {
		t.Fatalf("detail = %q", p.Detail)
	}

	h2 := newHarness(t, nil)
	r2 := preparedWorkspace(t, h2)
	h2.exec(`UPDATE workspaces SET ref='main' WHERE id=$1`, r2.ID)
	if got := prepareOnce(t, h2, r2.ID); got != dbstate.PreparationFailed {
		t.Fatalf("branch ref state = %s", got)
	}
	if n := len(h2.fake.PrepareRequests()); n != 0 {
		t.Fatalf("a branch ref was sent to kagent: %d calls", n)
	}
}

type fakeRefs struct {
	sha string
	err error
	got []string
}

func (f *fakeRefs) CommitSHA(_ context.Context, _ github.Repo, ref string) (string, error) {
	f.got = append(f.got, ref)
	return f.sha, f.err
}

// selectEnvironment gives the harness project a validated environment, as the owner's selection does.
func selectEnvironment(h *harness) {
	h.t.Helper()
	h.exec(`INSERT INTO dev_environments (id, owner_id, name, source_kind) VALUES ('env', $1, 'dev', 'prebuilt_image')`, owner)
	h.exec(`INSERT INTO environment_versions (id, environment_id, registry, repository, index_digest, platform_manifest_digest, config_digest, declared_user, architecture,
	        validation_status, validator_version, provenance_kind)
	    VALUES ('v', 'env', 'example.test', 'dev', 'sha256:` + strings.Repeat("a", 64) + `', 'sha256:` + strings.Repeat("b", 64) + `', 'sha256:` + strings.Repeat("c", 64) + `',
	        '65532:65532', 'arm64', 'static_validated', 'oci-static-v2', 'operator')`)
	h.exec(`INSERT INTO project_environment_selections (project_id, environment_id, version_id, follow_default, revision)
	    SELECT id, 'env', 'v', false, 1 FROM projects`)
}

func TestCreateWithAnEnvironmentPinsTheCheckoutToACommit(t *testing.T) {
	h := newHarness(t, nil)
	refs := &fakeRefs{sha: prepSHA}
	h.svc.Refs = refs
	// The project exists once a workspace has been made for it.
	if _, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("first")}); err != nil {
		t.Fatal(err)
	}
	selectEnvironment(h)

	w, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("second")})
	if err != nil {
		t.Fatal(err)
	}
	r := h.row(w.WorkspaceId)
	if r.Ref != prepSHA || len(refs.got) == 0 {
		t.Fatalf("ref = %q, asked for %v", r.Ref, refs.got)
	}
	if got := h.prepState(r.ID); got != dbstate.PreparationPlanned {
		t.Fatalf("preparation = %s", got)
	}

	refs.err, refs.sha = github.ErrNotFound, ""
	_, err = h.create(api.CreateWorkspaceRequest{Branch: ptr("third"), Ref: ptr("gone")})
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(inv.Error(), "checkout_ref_unavailable") {
		t.Fatalf("err = %v", err)
	}
}
