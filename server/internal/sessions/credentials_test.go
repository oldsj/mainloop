package sessions_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

const mcpOrigin = "http://mainloop-mcp.test.svc.cluster.local"

// withCredentials gives the service the credential store over a fake cluster.
func (e *env) withCredentials() *authtest.Env {
	e.t.Helper()
	a := authtest.NewEnv(e.pool)
	e.svc.Credentials = a.Creds
	e.svc.MCPOrigin = mcpOrigin
	return a
}

func (e *env) credentialRows(sessionID string) []dbq.Credential {
	e.t.Helper()
	rows, err := dbq.New(e.pool).ListCredentialsByBinding(context.Background(), sessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// grantNative makes a native session's binding a workspace agent and publishes its credential, as
// a session that reaches MCP would have.
func (e *env) grantNative(a *authtest.Env, id string) dbq.Credential {
	e.t.Helper()
	ctx := context.Background()
	if _, err := dbq.New(e.pool).SetBindingGrant(ctx, dbq.SetBindingGrantParams{SessionID: id, MCPGrantKind: dbstate.GrantWorkspace}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := a.Creds.Issue(ctx, id, dbstate.CredentialMCP); err != nil {
		e.t.Fatal(err)
	}
	return e.credentialRows(id)[0]
}

func TestTheMainThreadIsCreatedWithItsMCPCredential(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.withCredentials()
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)

	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row := e.bindingRow(); row.MCPGrantKind != dbstate.GrantCoordination {
		t.Errorf("main grant = %q, want coordination", row.MCPGrantKind)
	}
	reqs := e.srv.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 1 {
		t.Fatalf("create requests = %v", reqs)
	}
	c := reqs[0].GetCredentials()[0]
	if c.GetOrigin() != mcpOrigin || c.GetHeader() != "authorization" ||
		c.GetSecretRef().GetName() != "mainloop-mcp-"+m.SessionID || c.GetSecretRef().GetKey() != "authorization" {
		t.Errorf("credential reference = %v", c)
	}
	if a.Cluster.UID(authtest.Namespace, c.GetSecretRef().GetName()) == "" {
		t.Error("the referenced Secret does not exist")
	}
	rows := e.credentialRows(m.SessionID)
	if len(rows) != 1 || rows[0].State != dbstate.CredentialPublished {
		t.Errorf("credential rows = %+v", rows)
	}
}

func TestANativeSessionCarriesNoMCPCredential(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withProviders()
	e.withCredentials()
	e.run()
	id := e.newSession("first")
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	reqs := e.srv.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 0 {
		t.Fatalf("create requests = %v", reqs)
	}
	if rows := e.credentialRows(id); len(rows) != 0 {
		t.Errorf("credential rows = %+v", rows)
	}
}

// Outside a cluster there is no Secret store: sessions still work, without an MCP credential.
func TestWithoutAKubernetesStoreTheMainThreadStillWorks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.svc.Credentials = agentauth.NewCredentials(e.pool, agentauth.Unconfigured{}, authtest.Key, "mainloop", nil)
	e.svc.MCPOrigin = mcpOrigin
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
	reqs := e.srv.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 0 {
		t.Fatalf("create requests = %v", reqs)
	}
	m, _ := e.svc.EnsureMain(context.Background())
	if rows := e.credentialRows(m.SessionID); len(rows) != 0 {
		t.Errorf("credential rows = %+v", rows)
	}
}

// A previous credential whose Secret is still being deleted holds the name: the first message waits
// for it, however long, without being failed, and goes out once the name is free.
func TestACreateWaitsForTheOldSecretToBeDeletedWithoutFailing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.withCredentials()
	ctx := context.Background()
	m, err := e.svc.EnsureMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, err := a.Creds.Issue(ctx, m.SessionID, dbstate.CredentialMCP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Creds.Revoke(ctx, m.SessionID); err != nil {
		t.Fatal(err)
	}
	e.run()
	id := e.send("hello")
	time.Sleep(500 * time.Millisecond) // many claims, more than the create attempts
	if st := e.delivery(id).State; st != dbstate.DeliveryRecorded {
		t.Fatalf("delivery is %s while the name is held, want recorded", st)
	}
	if n := e.srv.Count("CreateSession"); n != 0 {
		t.Fatalf("%d creates while the old Secret was still there", n)
	}
	// The cleanup finishes.
	if err := a.Cluster.CoreV1().Secrets(authtest.Namespace).Delete(ctx, old.SecretName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE credentials SET state='deleted' WHERE id=$1`, old.CredentialID); err != nil {
		t.Fatal(err)
	}
	e.waitState(id, dbstate.DeliveryCompleted)
	if reqs := e.srv.CreateRequests(); len(reqs) != 1 || len(reqs[0].GetCredentials()) != 1 {
		t.Fatalf("create requests = %v", reqs)
	}
}

func TestCancelRevokesTheSessionsCredentialWithTheStatusChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withProviders()
	a := e.withCredentials()
	id := e.newSession("first")
	cred := e.grantNative(a, id)
	if _, err := e.svc.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got, _ := dbq.New(e.pool).GetCredential(context.Background(), cred.ID); got.State != dbstate.CredentialRevoked {
		t.Fatalf("credential is %s after Cancel", got.State)
	}
}

func TestArchiveRevokesTheSessionsCredentialWithTheArchive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withProviders()
	a := e.withCredentials()
	ctx := context.Background()
	id := e.newSession("first")
	cred := e.grantNative(a, id)
	// A finished session that something else ended without revoking.
	if _, err := e.pool.Exec(ctx, `UPDATE sessions SET status='completed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Archive(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := dbq.New(e.pool).GetCredential(ctx, cred.ID); got.State != dbstate.CredentialRevoked {
		t.Fatalf("credential is %s after Archive", got.State)
	}
}

// An archive that is refused (the session is still live) leaves the credential alone.
func TestRefusedArchiveKeepsTheCredential(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withProviders()
	a := e.withCredentials()
	ctx := context.Background()
	id := e.newSession("first")
	cred := e.grantNative(a, id)
	if err := e.svc.Archive(ctx, id); err == nil {
		t.Fatal("archiving a live session succeeded")
	}
	if got, _ := dbq.New(e.pool).GetCredential(ctx, cred.ID); got.State != dbstate.CredentialPublished {
		t.Fatalf("credential is %s after a refused Archive", got.State)
	}
}
