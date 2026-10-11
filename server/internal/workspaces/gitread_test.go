package workspaces

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// withGitRead turns on the git read capability the way the server does when the transport and a
// GitHub App are configured.
func (h *harness) withGitRead() *authtest.Env {
	h.t.Helper()
	a := h.withCredentials()
	h.svc.GitRead = &GitRead{
		Enroller: &gittransport.Enroller{Pool: h.svc.Pool, Creds: a.Creds},
		Origin:   h.cfg.Git.ReadOrigin,
	}
	return a
}

func gitReadRefs(creds []*kagentapi.SessionCredential) (refs []*kagentapi.SessionCredential) {
	for _, c := range creds {
		if c.GetSecretRef().GetName() != "" && c.GetOrigin() != "" && c.GetHeader() == "Authorization" &&
			len(c.GetSecretRef().GetName()) > len("mainloop-git-read-") && c.GetSecretRef().GetName()[:len("mainloop-git-read-")] == "mainloop-git-read-" {
			refs = append(refs, c)
		}
	}
	return refs
}

func TestAWorkspaceIsEnrolledForGitReadAndItsSessionCarriesTheReference(t *testing.T) {
	h := newHarness(t, nil)
	a := h.withGitRead()
	w := h.running()

	reqs := h.fake.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 2 {
		t.Fatalf("create requests = %v", reqs)
	}
	refs := gitReadRefs(reqs[0].GetCredentials())
	if len(refs) != 1 {
		t.Fatalf("no git read reference in %v", reqs[0].GetCredentials())
	}
	ref := refs[0]
	if ref.GetOrigin() != h.cfg.Git.ReadOrigin || ref.GetSecretRef().GetName() != a.Creds.SecretName(dbstate.CredentialGitRead, w.WorkspaceId) || ref.GetSecretRef().GetKey() != "authorization" {
		t.Errorf("reference = %v", ref)
	}
	if a.Cluster.UID(authtest.Namespace, ref.GetSecretRef().GetName()) == "" {
		t.Error("the referenced Secret does not exist")
	}
	en, err := h.dq.GetGitEnrollment(bg, w.WorkspaceId)
	if err != nil || en.State != dbstate.GitEnrollmentPublished || en.Repository != "oldsj/mainloop" {
		t.Errorf("enrollment = %+v, %v", en, err)
	}
	kinds := map[dbstate.CredentialKind]bool{}
	for _, c := range h.credentialRows(w.WorkspaceId) {
		kinds[c.Kind] = c.State == dbstate.CredentialPublished
	}
	if !kinds[dbstate.CredentialMCP] || !kinds[dbstate.CredentialGitRead] || len(kinds) != 2 {
		t.Errorf("credential kinds = %v", kinds)
	}
}

// A delete that replays an unrecorded create sends the credential set the create carries, and
// issues nothing.
func TestADeleteReplayCarriesTheGitReadReferenceAndIssuesNothing(t *testing.T) {
	h := newHarness(t, slow)
	a := h.withGitRead()
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Config.Workspaces.OperationWait = 5e9
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	reqs := h.fake.CreateRequests()
	if len(reqs) == 0 {
		t.Fatal("the delete did not replay the create")
	}
	want := []string{"mainloop-mcp-" + w.WorkspaceId, a.Creds.SecretName(dbstate.CredentialGitRead, w.WorkspaceId)}
	for i, r := range reqs {
		var got []string
		for _, c := range r.GetCredentials() {
			got = append(got, c.GetSecretRef().GetName())
		}
		if !slices.Equal(got, want) {
			t.Errorf("request %d credentials = %v, want %v", i, got, want)
		}
	}
	if rows := h.credentialRows(w.WorkspaceId); len(rows) != 2 {
		t.Errorf("credential rows = %+v: the replay issued something", rows)
	}
}

// The replay's credential set is the original's byte for byte: kagent answers it with the same
// Session. A different set (the git reference missing) is refused.
func TestAReplayOfASuccessfulCreateCarriesTheIdenticalCredentialSet(t *testing.T) {
	h := newHarness(t, nil)
	h.withGitRead()
	w := h.running()
	row := h.row(w.WorkspaceId)
	b, err := h.dq.GetAgentBinding(bg, w.WorkspaceId)
	if err != nil {
		t.Fatal(err)
	}
	replay := row.createRequest()
	replay.Credentials = h.svc.Credentials.ReplayCredentials(row.ID, h.cfg.MCPOrigin(), b.Role, b.MCPGrantKind)
	replay.Credentials = append(replay.Credentials, h.svc.GitRead.forReplay(row))
	orig := h.fake.CreateRequests()[0]
	if len(replay.Credentials) != len(orig.GetCredentials()) {
		t.Fatalf("replay credentials = %v, original %v", replay.Credentials, orig.GetCredentials())
	}
	for i, c := range replay.Credentials {
		if !proto.Equal(c, orig.GetCredentials()[i]) {
			t.Errorf("credential %d = %v, original %v", i, c, orig.GetCredentials()[i])
		}
	}
	before := len(h.credentialRows(row.ID))
	sess, err := h.client.CreateSession(bg, replay)
	if err != nil || sess.GetId() != *row.KagentSessionID {
		t.Fatalf("replay = %v, %v; want the original Session", sess, err)
	}
	if len(h.sessions()) != 1 || len(h.credentialRows(row.ID)) != before {
		t.Error("the replay made a Session or issued a credential")
	}
	short := row.createRequest()
	short.Credentials = replay.Credentials[:1]
	if _, err := h.client.CreateSession(bg, short); err == nil {
		t.Error("kagent accepted a replay without the git reference")
	}
}

func TestDeletingAWorkspaceRevokesItsGitReadCredential(t *testing.T) {
	h := newHarness(t, nil)
	h.withGitRead()
	h.run()
	w := h.running()
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	rows := h.credentialRows(w.WorkspaceId)
	if len(rows) != 2 {
		t.Fatalf("credential rows = %+v", rows)
	}
	for _, r := range rows {
		if r.State != dbstate.CredentialRevoked {
			t.Errorf("%s credential is %s after the delete", r.Kind, r.State)
		}
	}
}

func TestWithoutGitReadWorkspacesAreUnchanged(t *testing.T) {
	h := newHarness(t, nil)
	h.withCredentials()
	w := h.running()
	reqs := h.fake.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 1 || len(gitReadRefs(reqs[0].GetCredentials())) != 0 {
		t.Fatalf("create requests = %v", reqs)
	}
	if _, err := h.dq.GetGitEnrollment(bg, w.WorkspaceId); err == nil {
		t.Error("a workspace was enrolled with git read off")
	}
	for _, r := range h.credentialRows(w.WorkspaceId) {
		if r.Kind == dbstate.CredentialGitRead {
			t.Errorf("a git read credential was issued: %+v", r)
		}
	}
}

// Only a GitHub repository can be served; one that is not gets no enrollment and no reference, on
// the create and on its replay alike.
func TestAWorkspaceOutsideGitHubGetsNoGitReadReference(t *testing.T) {
	h := newHarness(t, nil)
	h.withGitRead()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspaces SET repo_url = 'https://git.example.com/o/r' WHERE id = $1`, w.WorkspaceId)
	h.run()
	eventually(t, "the create", func() bool { return len(h.fake.CreateRequests()) == 1 })
	reqs := h.fake.CreateRequests()
	if len(reqs[0].GetCredentials()) != 1 || len(gitReadRefs(reqs[0].GetCredentials())) != 0 {
		t.Errorf("credentials = %v", reqs[0].GetCredentials())
	}
	if h.svc.GitRead.forReplay(h.row(w.WorkspaceId)) != nil {
		t.Error("a replay would carry a reference the original did not")
	}
	if _, err := h.dq.GetGitEnrollment(bg, w.WorkspaceId); err == nil {
		t.Error("enrolled")
	}
}
