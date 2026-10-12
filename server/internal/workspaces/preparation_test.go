package workspaces

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
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
	for _, role := range []string{"supervisor", "child"} {
		t.Run(role, func(t *testing.T) {
			h := newHarness(t, nil)
			r := preparedWorkspace(t, h)
			task := func(id string, parent *string) {
				h.exec(`INSERT INTO tasks (id,owner_id,project_id,parent_task_id,root_task_id,title,brief,mode,assigned_profile_id,selection_source,status,checkout_branch,checkout_ref,checkout_depth)
				    VALUES ($1,$2,$3,$4,'root','test','test','code','claude','explicit','queued',$5,'main',0)`, id, owner, r.ProjectID, parent, r.Branch)
			}
			taskID, depth := "root", 1
			task("root", nil)
			if role == "child" {
				taskID, depth = "kid", 2
				task("kid", ptr("root"))
			}
			h.exec(`INSERT INTO task_attempts (id,task_id,number,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,workspace_request_id,writer_generation)
			    VALUES ('attempt',$1,1,'claude','claude','rev',$2,$3,$4,$5,$6,1)`, taskID, r.AgentNamespace, r.AgentName, role, depth, r.RequestID)
			if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
				t.Fatalf("state = %s", got)
			}
			if req := lastPrepare(t, h); req.GetSetupProfile() != role || req.GetSetupDigest() != setupDigests[role] {
				t.Fatalf("request = %v", req)
			}
		})
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

// kagent counts a Git proxy credential for preparation only when its header is spelled exactly
// "authorization"; with any other spelling it refuses every pass with "Current prepared runtime
// identity is unavailable" and the preparation can never be confirmed.
func TestPreparationOfASessionWithAGitReadReferenceIsNotRefusedAsAnUnavailableIdentity(t *testing.T) {
	h := newHarness(t, nil)
	h.withGitRead()
	r := preparedWorkspace(t, h)
	var read int
	for _, c := range h.fake.CreateRequests()[0].GetCredentials() {
		if strings.HasPrefix(c.GetOrigin(), "http://mainloop-git-") {
			read++
		}
	}
	if read != 1 {
		t.Fatalf("git proxy references on the create = %d, want 1", read)
	}
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
		t.Fatalf("state = %s, want confirmed\n%s", got, h.logs.String())
	}
}

// A reference frozen on a workspace row before the header was spelled lowercase is sent lowercase.
func TestAFrozenGitReadReferenceIsSentWithALowercaseHeader(t *testing.T) {
	ref, err := frozenReference([]byte(`{"origin":"http://mainloop-git-read.mainloop.svc.cluster.local","header":"Authorization","secretRef":{"name":"n","key":"authorization"}}`))
	if err != nil || ref.GetHeader() != "authorization" {
		t.Fatalf("ref = %v, %v", ref, err)
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

func TestPreparationRetriesAreSpacedAndAlreadyExistsIsFinal(t *testing.T) {
	h := newHarness(t, nil)
	r := preparedWorkspace(t, h)
	h.fake.FailNext(kagent.ProcPrepareSessionWorkspace, grpcFault(connect.CodeUnavailable))
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationRequested {
		t.Fatalf("after a transient error: %s", got)
	}
	h.fake.FailNext(kagent.ProcPrepareSessionWorkspace, grpcFault(connect.CodeAlreadyExists))
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationFailed {
		t.Fatalf("after AlreadyExists: %s", got)
	}
}

func TestPreparationFailsAtOnceOnAHistoricalReceipt(t *testing.T) {
	h := newHarness(t, nil)
	h.fake.PrepareClassification = "pending"
	r := preparedWorkspace(t, h)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationRequested {
		t.Fatalf("state = %s", got)
	}
	base := h.fake.TamperSession
	h.fake.TamperSession = func(proc string, s *ka.Session) {
		base(proc, s)
		if proc == kagent.ProcGetSession && s.GetWorkspacePreparation() != nil {
			s.WorkspacePreparation.Historical = true
		}
	}
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationFailed {
		t.Fatalf("state = %s", got)
	}
}

func TestPreparationWindowIsCheckedAfterReadingTheReceipt(t *testing.T) {
	// Confirmed by kagent after the window: still counts.
	h := newHarness(t, nil)
	h.fake.PrepareClassification = "pending"
	r := preparedWorkspace(t, h)
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationRequested {
		t.Fatalf("state = %s", got)
	}
	h.exec(`UPDATE workspace_preparations SET created_at = now() - interval '2 hours' WHERE id=$1`, r.ID)
	base := h.fake.TamperSession
	h.fake.TamperSession = func(proc string, s *ka.Session) {
		base(proc, s)
		if proc == kagent.ProcGetSession && s.GetWorkspacePreparation() != nil {
			s.WorkspacePreparation.Classification = "confirmed"
		}
	}
	if got := prepareOnce(t, h, r.ID); got != dbstate.PreparationConfirmed {
		t.Fatalf("late confirmation: %s", got)
	}

	// Still pending after the window: failed.
	h2 := newHarness(t, nil)
	h2.fake.PrepareClassification = "pending"
	r2 := preparedWorkspace(t, h2)
	if got := prepareOnce(t, h2, r2.ID); got != dbstate.PreparationRequested {
		t.Fatalf("state = %s", got)
	}
	h2.exec(`UPDATE workspace_preparations SET created_at = now() - interval '2 hours' WHERE id=$1`, r2.ID)
	if got := prepareOnce(t, h2, r2.ID); got != dbstate.PreparationFailed {
		t.Fatalf("expired: %s", got)
	}
}

func TestCreateRetryReusesTheStoredCommitAndAnOutageIsRetryable(t *testing.T) {
	h := newHarness(t, nil)
	refs := &fakeRefs{sha: prepSHA}
	h.svc.Refs = refs
	if _, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("first")}); err != nil {
		t.Fatal(err)
	}
	selectEnvironment(h)
	id := newID()
	w, err := h.svc.CreateWithRequestID(bg, id, api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop"), Branch: ptr("second")})
	if err != nil {
		t.Fatal(err)
	}
	asked := len(refs.got)
	// The remote ref has moved and GitHub is down, but a repeat of the same request finds the
	// workspace without planning again.
	refs.sha, refs.err = "", errors.New("github is down")
	again, err := h.svc.CreateWithRequestID(bg, id, api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop"), Branch: ptr("second")})
	if err != nil || again.WorkspaceId != w.WorkspaceId || len(refs.got) != asked || h.row(w.WorkspaceId).Ref != prepSHA {
		t.Fatalf("repeat: %v, workspace %s vs %s, asks %d vs %d", err, again.WorkspaceId, w.WorkspaceId, len(refs.got), asked)
	}
	// An outage on a new create is not a refusal of the ref.
	_, err = h.create(api.CreateWorkspaceRequest{Branch: ptr("third")})
	var inv *InvalidError
	if err == nil || errors.As(err, &inv) {
		t.Fatalf("outage err = %v", err)
	}
}

func TestCreateWithAnEnvironmentRefusesAnUnpinnableRef(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("first")}); err != nil {
		t.Fatal(err)
	}
	selectEnvironment(h)
	n := h.workspaceCount()
	_, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("second")})
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(inv.Error(), "checkout_ref_unavailable") || h.workspaceCount() != n {
		t.Fatalf("err = %v, workspaces %d -> %d", err, n, h.workspaceCount())
	}
	// An explicit commit needs no resolver.
	if _, err = h.create(api.CreateWorkspaceRequest{Branch: ptr("third"), Ref: ptr(prepSHA)}); err != nil {
		t.Fatal(err)
	}
}

// TestSetupDigestsMatchTheKagentPin fails when the vendored kagent API moves to another commit
// without the setup digests being re-read there.
func TestSetupDigestsMatchTheKagentPin(t *testing.T) {
	raw, err := os.ReadFile("../../third_party/kagentapi/SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	var commit string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "commit: "); ok {
			commit = strings.TrimSpace(v)
		}
	}
	if commit != setupDigestsSource {
		t.Fatalf("third_party/kagentapi/SOURCE pins kagent %s but the setup digests in preparation.go were read at %s. "+
			"Read the supervisor, child and agent digests in go/harness/runtime/workspace/preparation_test.go at the new commit, "+
			"update setupDigests and setupDigestsSource, and keep the controller and harness image pins in step.", commit, setupDigestsSource)
	}
}

// gatedWorkspace makes a workspace with a selected environment against a kagent that refuses every
// send until its preparation is confirmed, with the real delivery ledger and both loops running.
// The preparation loop is left to the caller.
func gatedWorkspace(t *testing.T) (*harness, *sessions.Service, api.WorkspaceLifecycle) {
	t.Helper()
	h := newHarness(t, nil)
	h.fake.Actors, h.fake.RequirePreparation = true, true
	h.svc.Refs = &fakeRefs{sha: prepSHA}
	if _, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("first")}); err != nil {
		t.Fatal(err)
	}
	selectEnvironment(h)
	w, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("second")})
	if err != nil {
		t.Fatal(err)
	}
	ss := h.ledger()
	h.runBoth(ss)
	eventually(t, "the Session exists", func() bool { return h.row(w.WorkspaceId).KagentSessionID != nil })
	return h, ss, w
}

func (h *harness) runPreparation() {
	h.t.Helper()
	r := reconcile.NewRunner(h.svc.Pool, h.svc.Log, nil)
	l := h.svc.preparationLoop()
	l.Interval = 10 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-done })
}

func TestABriefWaitsForPreparationAndIsSentOnceItIsConfirmed(t *testing.T) {
	h, ss, w := gatedWorkspace(t)
	id := newID()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "brief"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the delivery to wait", func() bool { return strings.Contains(h.logs.String(), "delivery is waiting for its workspace") })
	if n := h.fake.Count("SendStreamingMessage"); n != 0 || h.deliveryState(id) == dbstate.DeliveryCompleted {
		t.Fatalf("sent before preparation: %d sends, state %s", n, h.deliveryState(id))
	}
	h.runPreparation()
	eventually(t, "the preparation to be confirmed", func() bool { return h.prepState(w.WorkspaceId) == dbstate.PreparationConfirmed })
	eventually(t, "the delivery to complete", func() bool { return h.deliveryState(id) == dbstate.DeliveryCompleted })
	if n := h.fake.Count("SendStreamingMessage"); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

func TestABriefForAWorkspaceWhosePreparationFailedSettlesWithTheDetail(t *testing.T) {
	h, ss, w := gatedWorkspace(t)
	id := newID()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "brief"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the delivery to wait", func() bool { return strings.Contains(h.logs.String(), "delivery is waiting for its workspace") })
	h.exec(`UPDATE workspace_preparations SET state='failed', detail='kagent refused the preparation: bad digest' WHERE id=$1`, w.WorkspaceId)
	eventually(t, "the delivery to fail", func() bool { return h.deliveryState(id) == dbstate.DeliveryFailed })
	d, err := dbq.New(h.svc.Pool).GetDelivery(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Detail == nil || !strings.Contains(*d.Detail, "bad digest") || !strings.Contains(*d.Detail, "could not prepare the kagent session") {
		t.Fatalf("detail = %v", d.Detail)
	}
	if n := h.fake.Count("SendStreamingMessage"); n != 0 {
		t.Fatalf("sends = %d, want 0", n)
	}
}
