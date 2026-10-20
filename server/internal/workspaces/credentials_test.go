package workspaces

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

func (h *harness) withCredentials() *authtest.Env {
	h.t.Helper()
	a := authtest.NewEnv(h.svc.Pool)
	h.svc.Credentials = a.Creds
	return a
}

func (h *harness) credentialRows(id string) []dbq.Credential {
	h.t.Helper()
	rows, err := h.dq.ListCredentialsByBinding(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func TestAWorkspaceSessionIsCreatedWithItsMCPCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	a := h.withCredentials()
	w := h.running()

	reqs := h.fake.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 1 {
		t.Fatalf("create requests = %v", reqs)
	}
	c := reqs[0].GetCredentials()[0]
	if want := h.cfg.MCPOrigin(); c.GetOrigin() != want {
		t.Errorf("origin = %q, want %q", c.GetOrigin(), want)
	}
	if c.GetHeader() != "authorization" || c.GetSecretRef().GetName() != "mainloop-mcp-"+w.WorkspaceId || c.GetSecretRef().GetKey() != "authorization" {
		t.Errorf("credential reference = %v", c)
	}
	if a.Cluster.UID(authtest.Namespace, c.GetSecretRef().GetName()) == "" {
		t.Error("the referenced Secret does not exist")
	}
	rows := h.credentialRows(w.WorkspaceId)
	if len(rows) != 1 || rows[0].State != dbstate.CredentialPublished || rows[0].Kind != dbstate.CredentialMCP {
		t.Errorf("credential rows = %+v", rows)
	}
	b, err := h.dq.GetAgentBinding(bg, w.WorkspaceId)
	if err != nil || b.Role != dbstate.RoleAgent || b.MCPGrantKind != dbstate.GrantWorkspace {
		t.Errorf("binding = %+v, %v", b, err)
	}
}

func TestAWorkspaceStillCreatesWithoutAKubernetesStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.svc.Credentials = agentauth.NewCredentials(h.svc.Pool, agentauth.Unconfigured{}, authtest.Key, "mainloop", nil)
	w := h.running()
	if reqs := h.fake.CreateRequests(); len(reqs) != 1 || len(reqs[0].GetCredentials()) != 0 {
		t.Fatalf("create requests = %v", reqs)
	}
	if rows := h.credentialRows(w.WorkspaceId); len(rows) != 0 {
		t.Errorf("credential rows = %+v", rows)
	}
}

func TestDeletingAWorkspaceRevokesItsCredentialWithThePurge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.withCredentials()
	h.run()
	w := h.running()
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 {
		t.Fatal("rows kept")
	}
	rows := h.credentialRows(w.WorkspaceId)
	if len(rows) != 1 || rows[0].State != dbstate.CredentialRevoked {
		t.Fatalf("credential rows after the purge = %+v", rows)
	}
}

// The create that dropRejected removes had already published a credential; it goes with the rows.
func TestDropRejectedRevokesTheCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, slow)
	h.withCredentials()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	stop := h.run()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if rows := h.credentialRows(w.WorkspaceId); len(rows) != 1 || rows[0].State != dbstate.CredentialPublished {
		t.Fatalf("credential rows before the drop = %+v", rows)
	}
	h.exec(`UPDATE workspace_ops SET state = 'rejected', lease_owner = NULL, lease_until = NULL WHERE id = $1`, h.op(w.WorkspaceId, "create").ID)
	if err := h.svc.dropRejected(bg, w.WorkspaceId, h.op(w.WorkspaceId, "create")); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 {
		t.Fatal("rows kept")
	}
	if rows := h.credentialRows(w.WorkspaceId); len(rows) != 1 || rows[0].State != dbstate.CredentialRevoked {
		t.Fatalf("credential rows after the drop = %+v", rows)
	}
}

// A delete that replays the create to find a Session it never saw sends the same credential
// reference and issues nothing new.
func TestADeleteReplayReusesTheCredentialReference(t *testing.T) {
	t.Parallel()
	h := newHarness(t, slow)
	h.withCredentials()
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
	if rows := h.credentialRows(w.WorkspaceId); len(rows) != 1 {
		t.Errorf("credential rows = %+v: the replay issued a new credential", rows)
	}
	names := map[string]bool{}
	for _, r := range reqs {
		if len(r.GetCredentials()) != 1 {
			t.Errorf("a create without its credential reference: %v", r)
		}
		for _, c := range r.GetCredentials() {
			names[c.GetSecretRef().GetName()] = true
		}
	}
	if len(names) != 1 || !names["mainloop-mcp-"+w.WorkspaceId] {
		t.Fatalf("credential references across creates = %v", names)
	}
}

func TestClassifyCredentialErrors(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		err  error
		perm bool
	}{
		"ended":   {agentauth.ErrBindingEnded, true},
		"grant":   {agentauth.ErrInvalidGrant, true},
		"pending": {agentauth.ErrCleanupPending, false},
		"revoked": {agentauth.ErrRevoked, false},
	} {
		t.Run(name, func(t *testing.T) {
			perm, reason := classify(tt.err)
			if perm != tt.perm || reason == "" {
				t.Fatalf("classify = %v, %q", perm, reason)
			}
		})
	}
}
